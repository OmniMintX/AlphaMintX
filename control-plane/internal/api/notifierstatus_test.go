package api

// notifier-status API tests (alert-notifier.md AN-17 read surface): 404
// when unconfigured, the healthy and degraded response shapes with
// sources never null, and the env-admin-only gate.

import (
	"net/http"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/notifier"
)

// fakeNotifierStatus is the test NotifierStatusProvider (the
// fakeBackupEngine pattern: the RBAC env MUST wire one so the
// requiresNotifier row registers and the DeepEqual(routes, Permissions())
// pin holds).
type fakeNotifierStatus struct {
	st notifier.Status
}

func (f *fakeNotifierStatus) Status() notifier.Status { return f.st }

// notifierEnv is newEnv with only the notifier provider wired.
func notifierEnv(t *testing.T, f *fakeNotifierStatus) *testEnv {
	t.Helper()
	return newEnv(t, func(cfg *Config) { cfg.Notifier = f })
}

// TestNotifierStatusUnconfigured: without a provider the route is
// UNREGISTERED — 404, not 403 (the backup precedent: the surface is
// invisible unless explicitly configured).
func TestNotifierStatusUnconfigured(t *testing.T) {
	e := newEnv(t, nil)
	if rec := e.do(t, "GET", "/api/v1/ops/notifier-status", adminTok, nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestNotifierStatusHealthyShape: zero failures render degraded=false and
// every row with a null last_degraded_at, field for field; sources is []
// never null even for an empty snapshot.
func TestNotifierStatusHealthyShape(t *testing.T) {
	f := &fakeNotifierStatus{st: notifier.Status{Sources: []notifier.SourceStatus{
		{Source: "kill_breaker_events"},
		{Source: "kill_clear_events"},
		{Source: "safety_alerts"},
	}}}
	e := notifierEnv(t, f)
	rec := e.do(t, "GET", "/api/v1/ops/notifier-status", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var got struct {
		Degraded bool             `json:"degraded"`
		Sources  []map[string]any `json:"sources"`
	}
	decodeJSON(t, rec, &got)
	if got.Degraded {
		t.Error("degraded = true, want false")
	}
	if len(got.Sources) != 3 {
		t.Fatalf("sources = %v, want 3 rows", got.Sources)
	}
	for i, name := range []string{"kill_breaker_events", "kill_clear_events", "safety_alerts"} {
		row := got.Sources[i]
		want := map[string]any{
			"source": name, "consecutive_failed_ticks": float64(0),
			"degraded": false, "last_degraded_at": nil,
		}
		for k, v := range want {
			if rv, ok := row[k]; !ok || rv != v {
				t.Errorf("sources[%d].%s = %v, want %v", i, k, rv, v)
			}
		}
		if len(row) != len(want) {
			t.Errorf("sources[%d] has %d fields %v, want exactly %d", i, len(row), row, len(want))
		}
	}

	empty := notifierEnv(t, &fakeNotifierStatus{})
	rec = empty.do(t, "GET", "/api/v1/ops/notifier-status", adminTok, nil)
	var emptyGot struct {
		Sources []any `json:"sources"`
	}
	decodeJSON(t, rec, &emptyGot)
	if emptyGot.Sources == nil {
		t.Errorf("empty body = %q, want sources [] never null", rec.Body.String())
	}
}

// TestNotifierStatusDegradedShape: a degraded row carries its counter and
// the RFC 3339 UTC last_degraded_at; the aggregate flag follows.
func TestNotifierStatusDegradedShape(t *testing.T) {
	f := &fakeNotifierStatus{st: notifier.Status{Degraded: true, Sources: []notifier.SourceStatus{
		{Source: "safety_alerts", ConsecutiveFailedTicks: 12, Degraded: true, LastDegradedAt: testNow},
	}}}
	e := notifierEnv(t, f)
	rec := e.do(t, "GET", "/api/v1/ops/notifier-status", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var got struct {
		Degraded bool             `json:"degraded"`
		Sources  []map[string]any `json:"sources"`
	}
	decodeJSON(t, rec, &got)
	if !got.Degraded {
		t.Error("degraded = false, want true")
	}
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %v, want 1 row", got.Sources)
	}
	row := got.Sources[0]
	want := map[string]any{
		"source": "safety_alerts", "consecutive_failed_ticks": float64(12),
		"degraded": true, "last_degraded_at": testNow.UTC().Format(time.RFC3339),
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %v, want %v", k, row[k], v)
		}
	}
}

// TestNotifierStatusEnvAdminOnly: env-admin class ONLY — the full matrix
// runs in TestRBACMatrix; this pins the tier for the common env tokens.
func TestNotifierStatusEnvAdminOnly(t *testing.T) {
	e := notifierEnv(t, &fakeNotifierStatus{})
	wantError(t, e.do(t, "GET", "/api/v1/ops/notifier-status", "", nil), 401, codeUnauthorized)
	for _, tok := range []string{readTok, opTok, agent1Tok} {
		wantError(t, e.do(t, "GET", "/api/v1/ops/notifier-status", tok, nil), 403, codeForbidden)
	}
}
