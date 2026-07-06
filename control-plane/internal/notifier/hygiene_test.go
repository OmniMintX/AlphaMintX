package notifier

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// secretMarker embeds in every hygiene-case URL: it must appear in NO log
// output (AN-11 — the notifier never logs err.Error() from delivery).
const secretMarker = "SECRET-CAPABILITY-TOKEN-MARKER"

// TestSecretHygieneMatrix pins AN-11 across the failure classes: DNS
// failure, connection refusal, TLS failure, timeout, and redirect — each
// against a URL embedding the marker; the marker appears in no log line
// and each failure logs its derived class.
func TestSecretHygieneMatrix(t *testing.T) {
	// A listener that is closed immediately: its port refuses connections.
	refused, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	refusedAddr := refused.Addr().String()
	refused.Close()

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {}))
	t.Cleanup(tlsSrv.Close)

	block := make(chan struct{})
	defer close(block)
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-block
	}))
	t.Cleanup(slowSrv.Close)

	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "http://"+secretMarker+".invalid/next", http.StatusFound)
	}))
	t.Cleanup(redirectSrv.Close)

	cases := []struct {
		name      string
		url       string
		wantClass string
	}{
		{"dns", "http://" + strings.ToLower(secretMarker) + ".invalid/" + secretMarker, "class=dns"},
		{"connect", "http://" + refusedAddr + "/" + secretMarker, "class=connect"},
		{"tls", tlsSrv.URL + "/" + secretMarker, "class=tls"},
		{"timeout", slowSrv.URL + "/" + secretMarker, "class=timeout"},
		{"redirect", redirectSrv.URL + "/" + secretMarker, "class=status:302"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			e, _ := h.seeded(Config{URL: tc.url, Bearer: secretMarker,
				Heartbeat: 0, Timeout: 250 * time.Millisecond})
			h.alert(40, "hygiene", "{}")
			e.runPass(context.Background())
			logs := h.logs.String()
			if strings.Contains(logs, secretMarker) || strings.Contains(strings.ToLower(logs), strings.ToLower(secretMarker)) {
				t.Fatalf("%s: URL/bearer marker leaked into logs:\n%s", tc.name, logs)
			}
			if !strings.Contains(logs, "alert dispatch failed source=safety_alerts "+tc.wantClass) {
				t.Fatalf("%s: expected failure line with %q; logs:\n%s", tc.name, tc.wantClass, logs)
			}
			if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
				t.Errorf("%s: watermark advanced to %d on failure", tc.name, got)
			}
		})
	}
}

// TestRedirectRefusal pins AN-15/AN-16: a 302 is a status:302 failure
// with NO second request — the body (and bearer) are never re-sent to a
// location the operator never vetted.
func TestRedirectRefusal(t *testing.T) {
	h := newHarness(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		http.Redirect(w, req, "http://second-request.invalid/next", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	e, _ := h.seeded(Config{URL: srv.URL, Heartbeat: 0})
	h.alert(40, "redirected", "{}")
	e.runPass(context.Background())
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 (redirect never followed)", got)
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
		t.Errorf("watermark = %d, want 0 (302 is failure)", got)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "class=status:302") {
		t.Fatalf("no status:302 failure line; logs:\n%s", logs)
	}
	if strings.Contains(logs, "second-request.invalid") {
		t.Fatalf("redirect target leaked into logs:\n%s", logs)
	}
}
