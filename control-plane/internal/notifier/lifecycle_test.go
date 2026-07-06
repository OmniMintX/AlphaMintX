package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestLogOnlyMode pins AN-14: no HTTP server involved; each event emits
// exactly one SAFETY-EVENT-prefixed single line carrying the AN-13
// envelope on the EventOut sink, and the watermark advances normally.
func TestLogOnlyMode(t *testing.T) {
	h := newHarness(t)
	e, _ := h.seeded(Config{LogOnly: true, Heartbeat: 0})
	h.alert(40, "logged", `{"a":1}`)
	h.alert(41, "logged_too", "{}")
	e.runPass(context.Background())

	lines := strings.Split(strings.TrimSpace(h.events.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("SAFETY-EVENT lines = %d, want 2:\n%s", len(lines), h.events.String())
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "SAFETY-EVENT ") {
			t.Fatalf("line %d lacks the marker prefix: %q", i, line)
		}
		var env capture
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "SAFETY-EVENT ")), &env); err != nil {
			t.Fatalf("line %d not a single JSON envelope: %v", i, err)
		}
		if env.Schema != Schema || env.Source != store.AlertSourceSafetyAlert ||
			env.ID != uid(40+i) || env.Seq != int64(i+1) {
			t.Errorf("line %d envelope = %+v, want the AN-13 shape", i, env)
		}
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 2 {
		t.Errorf("watermark = %d, want 2", got)
	}
	// Log-only success emits NO per-delivery summary line (AN-14: webhook
	// mode logs those; log-only already carries the envelope itself).
	if h.logs.count("alert dispatched source=") != 0 {
		t.Errorf("log-only logged webhook-style summaries:\n%s", h.logs.String())
	}
}

// TestHeartbeat pins AN-14a: a synthetic envelope with source "notifier",
// the hour-truncated id, and seq 0 — first one on start, then
// interval-anchored; heartbeat_hours 0 disables it entirely.
func TestHeartbeat(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: time.Hour})

	e.runPass(context.Background()) // first heartbeat on start
	hb := r.accepted()
	if len(hb) != 1 || hb[0].Source != "notifier" || hb[0].Seq != 0 ||
		hb[0].ID != "heartbeat-2026-07-05T12:00:00Z" ||
		hb[0].Event["kind"] != "notifier_heartbeat" {
		t.Fatalf("first heartbeat = %+v, want the AN-14a synthetic envelope", hb)
	}
	e.runPass(context.Background()) // not due again within the hour
	if got := len(r.accepted()); got != 1 {
		t.Fatalf("heartbeats within the hour = %d, want 1 (at most once per due pass)", got)
	}
	h.clk.Advance(time.Hour)
	e.runPass(context.Background())
	hb = r.accepted()
	if len(hb) != 2 || hb[1].ID != "heartbeat-2026-07-05T13:00:00Z" {
		t.Fatalf("second heartbeat = %+v, want the next hour id", hb)
	}
}

// TestHeartbeatDisabled: heartbeat_hours 0 sends none, ever.
func TestHeartbeatDisabled(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	e.runPass(context.Background())
	h.clk.Advance(200 * time.Hour)
	e.runPass(context.Background())
	if got := len(r.captures()); got != 0 {
		t.Fatalf("heartbeats with heartbeat_hours=0 = %d, want 0", got)
	}
}

// TestHeartbeatFailureNeverBlocksDispatch pins the AN-14a isolation: a
// receiver rejecting only heartbeats leaves event dispatch untouched;
// the failure feeds the notifier's own counter (source=notifier).
func TestHeartbeatFailureNeverBlocksDispatch(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	r.setStatus(func(c capture) int {
		if c.Source == "notifier" {
			return http.StatusBadGateway
		}
		return http.StatusOK
	})
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: time.Hour})
	h.alert(40, "with_heartbeat", "{}")
	e.runPass(context.Background())
	acc := r.accepted()
	if len(acc) != 1 || acc[0].ID != uid(40) {
		t.Fatalf("accepted = %+v, want the event despite heartbeat failure", acc)
	}
	if h.logs.count("alert dispatch failed source=notifier class=status:502") != 1 {
		t.Fatalf("no notifier-source failure line; logs:\n%s", h.logs.String())
	}
	// The next pass retries the still-due heartbeat without re-delivering
	// the event.
	e.runPass(context.Background())
	if got := len(r.accepted()); got != 1 {
		t.Errorf("events re-delivered on heartbeat retry: %d accepted", got)
	}
}

// TestNoOverlappingPasses pins AN-6a: a slow pass suppresses the
// intervening ticks (skip-never-queue) — the receiver never observes two
// requests in flight.
func TestNoOverlappingPasses(t *testing.T) {
	h := newHarness(t)
	var inflight, maxInflight atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if n <= old || maxInflight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond) // several 10 ms poll ticks long
		inflight.Add(-1)
	}))
	t.Cleanup(srv.Close)
	e, _ := h.seeded(Config{URL: srv.URL, Heartbeat: 0,
		Poll: 10 * time.Millisecond, Now: time.Now})
	for i := 0; i < 4; i++ {
		h.alert(40+i, "slow", "{}")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	<-done
	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("max concurrent POSTs = %d, want 1 (no overlapping passes)", got)
	}
}

// TestShutdownCancelsHungPOST pins AN-18: cancelling the serve context
// during a hung POST exits the dispatcher promptly — bounded by the test,
// not by timeout_seconds.
func TestShutdownCancelsHungPOST(t *testing.T) {
	h := newHarness(t)
	inflight := make(chan struct{}, 1)
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case inflight <- struct{}{}:
		default:
		}
		<-block
	}))
	t.Cleanup(srv.Close)
	e, _ := h.seeded(Config{URL: srv.URL, Heartbeat: 0,
		Timeout: time.Hour, Poll: time.Hour, Now: time.Now})
	h.alert(40, "hung", "{}")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	<-inflight // the POST is provably in flight
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not exit within 2 s of context cancel")
	}
}

// TestNonInterference pins AN-2a/AN-5 by mechanism: with a POST provably
// in flight against a receiver that blocks forever, a concurrent kill
// append on the SAME store completes at normal latency — the dispatcher
// holds no store connection across network I/O.
func TestNonInterference(t *testing.T) {
	h := newHarness(t)
	inflight := make(chan struct{}, 1)
	block := make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(block) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case inflight <- struct{}{}:
		default:
		}
		<-block
	}))
	t.Cleanup(func() { release(); srv.Close() })
	e, _ := h.seeded(Config{URL: srv.URL, Heartbeat: 0,
		Timeout: time.Hour, Now: time.Now})
	h.alert(40, "blocking", "{}")
	passDone := make(chan struct{})
	go func() { e.runPass(context.Background()); close(passDone) }()
	<-inflight // the POST is in flight NOW

	appended := make(chan error, 1)
	go func() {
		_, err := h.st.AppendPlatformKill(uid(20), "op-1", formatTime(testNow), true)
		appended <- err
	}()
	select {
	case err := <-appended:
		if err != nil {
			t.Fatalf("AppendPlatformKill during in-flight POST: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kill append blocked behind an in-flight POST (AN-2a violation)")
	}
	release()
	<-passDone
}
