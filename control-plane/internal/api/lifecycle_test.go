package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/papergate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func lcBody(to, reason string) map[string]string {
	return map[string]string{"to": to, "reason": reason}
}

func lifecyclePath(strategyID string) string {
	return "/api/v1/strategies/" + strategyID + "/lifecycle"
}

// stubCanceler records EntryCanceler invocations (tests inject failures).
type stubCanceler struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (c *stubCanceler) CancelOpenEntries(_ context.Context, strategyID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, strategyID)
	return c.err
}

// TestLifecycleValidation pins LC-3/LC-4/LC-5: resolution before body
// semantics, the strict REQUIRED body, unknown states, empty reasons, and
// the killed redirect — none of which writes anything.
func TestLifecycleValidation(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	wantError(t, e.do(t, "POST", lifecyclePath(uid(99)), adminTok, lcBody("paused", "r")),
		404, codeUnknownStrategy)
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, nil),
		400, codeSchemaInvalid) // body REQUIRED
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("warp", "r")),
		400, codeInvalidLifecycleState)
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("paused", "")),
		400, codeSchemaInvalid) // reason REQUIRED
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok,
		map[string]any{"to": "paused", "reason": "r", "bogus": 1}), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("killed", "r")),
		422, codeUseKillEndpoint)

	st, err := e.store.GetStrategy(strat1)
	if err != nil || st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q (%v), want paper untouched", st.LifecycleState, err)
	}
}

// TestLifecyclePauseAndResume drives paper -> paused -> paper by a tenant
// trader: the LC-13 response shape, the snapshot advance, the nil-seam
// LC-12 alert on pause, and the paused-provenance resume rule (LC-7).
func TestLifecyclePauseAndResume(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	trader := seedUserToken(t, e.store, "tenant-1", RoleTrader, "db-trader")

	rec := e.do(t, "POST", lifecyclePath(strat1), trader, lcBody("paused", "maintenance"))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp lifecycleResponse
	decodeJSON(t, rec, &resp)
	if resp.StrategyID != strat1 || resp.FromState != "paper" || resp.ToState != "paused" ||
		resp.TransitionID == "" || resp.RecordedAt != formatTime(testNow) {
		t.Fatalf("pause response = %+v", resp)
	}
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paused" {
		t.Fatalf("lifecycle_state = %q, want paused", st.LifecycleState)
	}
	// Nil EntryCanceler: 200 regardless, with the LC-12 alert row keyed
	// on the transition.
	dup, err := e.store.HasSafetyAlert("lifecycle_entry_cancel_failed", strat1, resp.TransitionID)
	if err != nil || !dup {
		t.Errorf("lifecycle_entry_cancel_failed alert = %v (%v), want recorded", dup, err)
	}

	// Paused resumes ONLY to its previous state (LC-7 provenance from the
	// audit trail): live_l1 is illegal, paper succeeds.
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), trader, lcBody("live_l1", "r")),
		422, codeIllegalTransition)
	rec = e.do(t, "POST", lifecyclePath(strat1), trader, lcBody("paper", "resume"))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper", st.LifecycleState)
	}
}

// TestLifecycleEntryCancelerInvoked: a wired seam runs on entry into
// paused (LC-12), and a seam failure still answers 200 with the alert.
func TestLifecycleEntryCancelerInvoked(t *testing.T) {
	canceler := &stubCanceler{}
	e := gatedEnv(t, func(cfg *Config) { cfg.EntryCanceler = canceler })
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	rec := e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("paused", "r"))
	if rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if len(canceler.calls) != 1 || canceler.calls[0] != strat1 {
		t.Fatalf("canceler calls = %v, want [%s]", canceler.calls, strat1)
	}
	var resp lifecycleResponse
	decodeJSON(t, rec, &resp)
	if dup, _ := e.store.HasSafetyAlert("lifecycle_entry_cancel_failed", strat1, resp.TransitionID); dup {
		t.Error("alert recorded on a successful cancel; want none")
	}
}

// TestLifecycleLiveTargetKillGuard pins LC-8's carve-out (test 15): any
// live target under any active kill binding the strategy is 422
// ILLEGAL_TRANSITION with the API-authored "kill tier active" — never
// PAPER_GATE_FAILED, nothing written.
func TestLifecycleLiveTargetKillGuard(t *testing.T) {
	e := gatedEnv(t, func(cfg *Config) { cfg.ExchangeKeysConfigured = true })
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	if _, err := e.store.AppendTenantKill(uid(90), "tenant-1", "test", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}

	rec := e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("live_l1", "promote"))
	wantError(t, rec, 422, codeIllegalTransition)
	var body errorBody
	decodeJSON(t, rec, &body)
	if body.Message != "kill tier active" {
		t.Fatalf("message = %q, want \"kill tier active\"", body.Message)
	}
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper untouched", st.LifecycleState)
	}
	// A tenant or platform kill still allows pause and resume-to-paper.
	if rec := e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("paused", "r")); rec.Code != http.StatusOK {
		t.Fatalf("pause under tenant kill = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("paper", "r")); rec.Code != http.StatusOK {
		t.Fatalf("resume under tenant kill = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
}

// TestLifecyclePaperGateFailed pins LC-11: a paper -> live_* attempt
// failing ONLY on the paper-gate answers 422 PAPER_GATE_FAILED embedding
// the full LC-23 report; a second failing guard keeps the machine's
// ILLEGAL_TRANSITION instead.
func TestLifecyclePaperGateFailed(t *testing.T) {
	e := gatedEnv(t, func(cfg *Config) { cfg.ExchangeKeysConfigured = true })
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	rec := e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("live_l1", "promote"))
	if rec.Code != 422 {
		t.Fatalf("status = %d (body %q), want 422", rec.Code, rec.Body.String())
	}
	var body struct {
		Code      string           `json:"code"`
		PaperGate papergate.Report `json:"paper_gate"`
	}
	decodeJSON(t, rec, &body)
	if body.Code != codePaperGateFailed {
		t.Fatalf("code = %q, want PAPER_GATE_FAILED", body.Code)
	}
	if body.PaperGate.Passed || len(body.PaperGate.Conditions) != 5 {
		t.Fatalf("paper_gate = %+v, want full failed 5-condition report", body.PaperGate)
	}

	// live_l2 with no L2 envelope ALSO fails the tier guard: the machine's
	// verdict stands (ILLEGAL_TRANSITION), no gate report.
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("live_l2", "r")),
		422, codeIllegalTransition)
}

// TestLifecycleUnlock drives spec test 6's API half: a killed strategy
// unlocks to paper ONLY after its kill is cleared, by Admin+, flat, with a
// recorded reason.
func TestLifecycleUnlock(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "killed")
	trader := seedUserToken(t, e.store, "tenant-1", RoleTrader, "db-trader")
	admin := seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin")
	epoch, err := e.store.AppendStrategyKill(uid(91), strat1, "test", formatTime(testNow), false)
	if err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}

	// Standing kill: the machine rejects the unlock (KillCleared false).
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), admin, lcBody("paper", "resolved")),
		422, codeIllegalTransition)

	rec := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/kill/clear", admin,
		map[string]any{"reason": "root-caused", "observed_epoch": epoch})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d (body %q)", rec.Code, rec.Body.String())
	}

	// Cleared: a trader still cannot unlock (Admin+ only); the admin can.
	wantError(t, e.do(t, "POST", lifecyclePath(strat1), trader, lcBody("paper", "resolved")),
		422, codeIllegalTransition)
	rec = e.do(t, "POST", lifecyclePath(strat1), admin, lcBody("paper", "resolved"))
	if rec.Code != http.StatusOK {
		t.Fatalf("unlock status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper", st.LifecycleState)
	}
}

// TestPaperGateRead pins LC-24: the read-only LC-23 report for readers,
// tenant-scoped resolution, and the shared per-token 60/min bucket
// (test 19 — the burst exhausts to 429).
func TestPaperGateRead(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	gatePath := "/api/v1/strategies/" + strat1 + "/paper-gate"

	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+uid(99)+"/paper-gate", readTok, nil),
		404, codeUnknownStrategy)

	rec := e.do(t, "GET", gatePath, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var rep papergate.Report
	decodeJSON(t, rec, &rep)
	if rep.Passed || len(rep.Conditions) != 5 || rep.EvaluatedAt != formatTime(testNow) {
		t.Fatalf("report = %+v, want a failed 5-condition report at the fixed clock", rep)
	}

	// The GET charges the same per-token bucket every POST charges: the
	// 61st request in the window is 429 (two consumed above — the charge
	// precedes resolution, so the 404 probe paid too).
	for i := 0; i < 58; i++ {
		if rec := e.do(t, "GET", gatePath, readTok, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET #%d: status = %d (body %q)", i+3, rec.Code, rec.Body.String())
		}
	}
	wantError(t, e.do(t, "GET", gatePath, readTok, nil), 429, codeRateLimited)
}

// TestLiveModePaperFloor pins LC-14a (test 10): with PaperSubmitter false
// (a live-OMS Submitter) a paper strategy's approve verdict persists and
// NOTHING submits; the paper-bridge fixture (PaperSubmitter true, the test
// default) submits, and live tiers submit in both.
func TestLiveModePaperFloor(t *testing.T) {
	paperMode := gatedEnv(t)
	createStrategy(t, paperMode.store, strat1, "paper")
	putMark(paperMode, "BTC/USDT", "64000")
	v, env := postProposal(paperMode, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if v.Decision != "approve" {
		t.Fatalf("decision = %s (%v), want approve", v.Decision, v.Reasons)
	}
	if env.Submitted == nil || !*env.Submitted || paperMode.sub.count() != 1 {
		t.Fatalf("paper-bridge mode: envelope %+v, calls %d — want submitted", env, paperMode.sub.count())
	}

	liveMode := gatedEnv(t, func(cfg *Config) { cfg.PaperSubmitter = false })
	createStrategy(t, liveMode.store, strat1, "paper")
	createStrategy(t, liveMode.store, strat2, "live_l3")
	putMark(liveMode, "BTC/USDT", "64000")
	v, env = postProposal(liveMode, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if v.Decision != "approve" {
		t.Fatalf("live-mode decision = %s (%v), want approve (verdict persists)", v.Decision, v.Reasons)
	}
	if env.Submitted != nil || liveMode.sub.count() != 0 {
		t.Fatalf("live mode: envelope %+v, calls %d — want the paper L0 floor", env, liveMode.sub.count())
	}
	// The floor is paper-specific: live_l3 still submits.
	if _, _ = postProposal(liveMode, t, strat2, agent2Tok, 0, openProposal(t, uid(20), strat2, uid(22))); liveMode.sub.count() != 1 {
		t.Fatalf("live_l3 calls = %d, want 1", liveMode.sub.count())
	}
}

// TestLifecycleKilledEmptyReasonTaxonomy pins the LC-4/LC-5 order: the
// schema checks answer FIRST — {"to":"killed","reason":""} is 400
// SCHEMA_INVALID (the absent-required-field convention), never 422
// USE_KILL_ENDPOINT; nothing is written.
func TestLifecycleKilledEmptyReasonTaxonomy(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	wantError(t, e.do(t, "POST", lifecyclePath(strat1), adminTok, lcBody("killed", "")),
		400, codeSchemaInvalid)
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper untouched", st.LifecycleState)
	}
}

// TestLifecycleKillWhileLockWaitBlocksLiveTarget drives the LC-8/LC-9
// guard under the race the in-transaction re-check closes: a kill
// committed while a live-target transition waits on the strategy lock is
// observed — 422 ILLEGAL_TRANSITION "kill tier active", nothing written.
func TestLifecycleKillWhileLockWaitBlocksLiveTarget(t *testing.T) {
	e := gatedEnv(t, func(cfg *Config) { cfg.ExchangeKeysConfigured = true })
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	body, err := json.Marshal(lcBody("live_l1", "promote"))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	lock := e.srv.strategyLock(strat1)
	lock.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.do(t, "POST", lifecyclePath(strat1), adminTok, body) }()
	time.Sleep(50 * time.Millisecond) // let the request reach the lock
	if _, err := e.store.AppendStrategyKill(uid(90), strat1, "op-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	lock.Unlock()

	rec := <-done
	wantError(t, rec, 422, codeIllegalTransition)
	var body2 errorBody
	decodeJSON(t, rec, &body2)
	if body2.Message != "kill tier active" {
		t.Fatalf("message = %q, want \"kill tier active\"", body2.Message)
	}
	if st, _ := e.store.GetStrategy(strat1); st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper untouched", st.LifecycleState)
	}
}

// TestPaperGateReduceOnlyClampThreaded pins the LC-18 reduce-only
// threading store → report: the joined reduce_only flag reaches the
// replay, so an oversized reduce-only fill closes the open long WITHOUT
// minting a phantom short span (unclamped, the later re-entry would
// "cover" it for a second closed trade).
func TestPaperGateReduceOnlyClampThreaded(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper") // bootstrap window at testNow

	orders := []store.Order{
		{OrderID: uid(70), Origin: "kill", StrategyID: strat1, Symbol: "BTC/USDT", Class: "ENTRY",
			Side: "buy", Type: "market", QtyBase: "1", Status: "filled", SubmittedAt: formatTime(testNow)},
		{OrderID: uid(71), Origin: "kill", StrategyID: strat1, Symbol: "BTC/USDT", Class: "PROTECTIVE",
			Side: "sell", Type: "market", ReduceOnly: true, QtyBase: "2", Status: "filled", SubmittedAt: formatTime(testNow)},
		{OrderID: uid(72), Origin: "kill", StrategyID: strat1, Symbol: "BTC/USDT", Class: "ENTRY",
			Side: "buy", Type: "market", QtyBase: "1", Status: "filled", SubmittedAt: formatTime(testNow)},
	}
	for _, o := range orders {
		if err := e.store.InsertOrder(o); err != nil {
			t.Fatalf("InsertOrder(%s): %v", o.OrderID, err)
		}
	}
	fills := []store.Fill{
		{FillID: uid(80), OrderID: uid(70), QtyBase: "1", FillPrice: "1000", FeeQuote: "0", FillTS: formatTime(testNow)},
		{FillID: uid(81), OrderID: uid(71), QtyBase: "2", FillPrice: "1010", FeeQuote: "0", FillTS: formatTime(testNow)},
		{FillID: uid(82), OrderID: uid(72), QtyBase: "1", FillPrice: "990", FeeQuote: "0", FillTS: formatTime(testNow)},
	}
	for _, f := range fills {
		if err := e.store.InsertFill(f); err != nil {
			t.Fatalf("InsertFill(%s): %v", f.FillID, err)
		}
	}

	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/paper-gate", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var rep papergate.Report
	decodeJSON(t, rec, &rep)
	for _, c := range rep.Conditions {
		if c.Name == papergate.CondMinClosedTrades {
			if c.Measured != "1" {
				t.Fatalf("min_closed_trades measured = %q, want \"1\" (clamped — no phantom short trade)", c.Measured)
			}
			return
		}
	}
	t.Fatalf("min_closed_trades missing from report %+v", rep)
}
