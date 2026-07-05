package api

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// recordingSink records Beat invocations (the HeartbeatSink seam).
type recordingSink struct {
	mu    sync.Mutex
	beats []struct {
		strategyID string
		at         time.Time
	}
}

func (r *recordingSink) Beat(strategyID string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats = append(r.beats, struct {
		strategyID string
		at         time.Time
	}{strategyID, at})
}

// TestHeartbeatEndpoint pins watchdog.md WD-1..WD-7: 200 {received_at}
// (RFC 3339 Z) for an in-scope agent token — env AND DB variants — with
// empty body and {} both accepted; unknown fields 400 SCHEMA_INVALID; a
// foreign strategy 403 STRATEGY_SCOPE_MISMATCH; a nil Heartbeats sink
// (paper mode) still 200; no lifecycle predicate (a paper strategy's beat
// is accepted); and a heartbeat burst never charges the per-strategy
// proposal limiter.
func TestHeartbeatEndpoint(t *testing.T) {
	sink := &recordingSink{}
	e := gatedEnv(t, func(cfg *Config) { cfg.Heartbeats = sink })
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper") // WD-7: no lifecycle predicate
	dbAgent := seedAgentToken(t, e.store, "tenant-1", strat1, "db-agent-heartbeat")
	path := "/api/v1/strategies/" + strat1 + "/heartbeat"

	// Empty body (env token) and {} (DB token) both 200 {received_at}.
	for _, tc := range []struct {
		name  string
		token string
		body  any
	}{
		{"env token, empty body", agent1Tok, nil},
		{"db token, {} body", dbAgent, []byte(`{}`)},
	} {
		rec := e.do(t, http.MethodPost, path, tc.token, tc.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body %q), want 200", tc.name, rec.Code, rec.Body.String())
		}
		var resp heartbeatResponse
		decodeJSON(t, rec, &resp)
		if resp.ReceivedAt != formatTime(testNow) {
			t.Errorf("%s: received_at = %q, want %q", tc.name, resp.ReceivedAt, formatTime(testNow))
		}
		if _, err := time.Parse(time.RFC3339, resp.ReceivedAt); err != nil {
			t.Errorf("%s: received_at %q is not RFC 3339: %v", tc.name, resp.ReceivedAt, err)
		}
	}
	sink.mu.Lock()
	if len(sink.beats) != 2 || sink.beats[0].strategyID != strat1 || !sink.beats[0].at.Equal(testNow) {
		t.Errorf("sink beats = %+v, want 2 beats for %s at the server clock", sink.beats, strat1)
	}
	sink.mu.Unlock()

	// Unknown field: 400 SCHEMA_INVALID (strict decode, WD-4).
	wantError(t, e.do(t, http.MethodPost, path, agent1Tok, []byte(`{"x":1}`)),
		http.StatusBadRequest, codeSchemaInvalid)
	// Foreign strategy: the guard's 403 STRATEGY_SCOPE_MISMATCH (WD-2).
	wantError(t, e.do(t, http.MethodPost, "/api/v1/strategies/"+strat2+"/heartbeat", agent1Tok, nil),
		http.StatusForbidden, codeStrategyScopeMismatch)
	// No user role, no read/operator/env-admin class may POST heartbeats.
	wantError(t, e.do(t, http.MethodPost, path, readTok, nil), http.StatusForbidden, codeForbidden)
	wantError(t, e.do(t, http.MethodPost, path, adminTok, nil), http.StatusForbidden, codeForbidden)

	// A burst of heartbeats never charges the 30/min proposal limiter
	// (WD-6): 30 beats would exhaust it, yet the next proposal is not 429.
	createStrategy(t, e.store, strat2, "live_l3")
	putMark(e, "BTC/USDT", "64000")
	for i := 0; i < 30; i++ {
		if rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat2+"/heartbeat", agent2Tok, nil); rec.Code != http.StatusOK {
			t.Fatalf("burst beat #%d: status = %d (body %q)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat2+"/proposals", agent2Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: testProposal(t, uid(50), strat2, uid(51))})
	if rec.Code == http.StatusTooManyRequests {
		t.Fatalf("proposal after the heartbeat burst = 429: heartbeats charged the proposal limiter")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("proposal after the heartbeat burst: status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
}

// TestHeartbeatNilSinkPaperMode pins WD-3: with no Heartbeats sink wired
// (paper mode) the route is still registered and answers 200.
func TestHeartbeatNilSinkPaperMode(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/heartbeat", agent1Tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-sink heartbeat: status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var resp heartbeatResponse
	decodeJSON(t, rec, &resp)
	if resp.ReceivedAt != formatTime(testNow) {
		t.Errorf("received_at = %q, want %q", resp.ReceivedAt, formatTime(testNow))
	}
}
