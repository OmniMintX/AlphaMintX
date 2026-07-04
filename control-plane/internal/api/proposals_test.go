package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/runstate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// gatedEnv is newEnv plus the serve-mode ingestion wiring: RiskLimits and a
// runstate hydrator over the same store and mark cache.
func gatedEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnv(t, func(cfg *Config) {
		limits := riskgate.RiskLimits{
			SymbolWhitelist:             []string{"BTC/USDT", "ETH/USDT"},
			MaxOpenPositions:            3,
			PerPositionNotionalCapQuote: decimal.NewFromInt(2000),
			DailyLossLimitQuote:         decimal.NewFromInt(500),
			MaxDrawdownPct:              decimal.NewFromInt(10),
			MaxLossAtStopQuote:          decimal.NewFromInt(450),
			MinStopDistancePct:          decimal.RequireFromString("0.1"),
			MaxStopDistancePct:          decimal.NewFromInt(25),
			MaxOrdersPerMinute:          60,
			RequireStopLoss:             true,
			AllocatedCapitalQuote:       decimal.NewFromInt(10000),
			AccountingQuote:             "USDT",
			StalenessThresholdSeconds:   riskgate.DefaultStalenessThresholdSeconds,
			L1ApprovalTimeoutSeconds:    riskgate.DefaultL1ApprovalTimeoutSeconds,
		}
		cfg.Limits = &limits
		cfg.RuntimeState = &runstate.Hydrator{
			Store: cfg.Store, Marks: cfg.Marks,
			AllocatedCapitalQuote: limits.AllocatedCapitalQuote,
		}
	})
}

func putMark(e *testEnv, symbol, price string) {
	e.marks.Put(marketdata.Tick{Symbol: symbol, Mark: decimal.RequireFromString(price), TS: testNow})
}

// openProposal is a gate-approvable open_long: fresh created_at, stop in
// range, size under the notional cap.
func openProposal(t *testing.T, proposalID, strategyID, runID string) *contract.Proposal {
	t.Helper()
	p := testProposal(t, proposalID, strategyID, runID)
	p.CreatedAt = mustTime(t, "2026-07-04T12:29:30Z")
	p.Action = contract.ActionOpenLong
	p.SizeQuote = mustDec(t, "1500")
	sl := mustDec(t, "62900")
	p.StopLoss = &sl
	p.Confidence = 0.9
	return p
}

// proposalEnvelope decodes the 200 submission envelope; pointer fields
// distinguish absent keys from false.
type proposalEnvelope struct {
	Verdict         json.RawMessage `json:"verdict"`
	Submitted       *bool           `json:"submitted"`
	SubmitErrorCode string          `json:"submit_error_code"`
	PendingApproval *bool           `json:"pending_approval"`
}

func postProposal(e *testEnv, t *testing.T, strategyID, token string, tick int, p *contract.Proposal) (*contract.Verdict, proposalEnvelope) {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strategyID+"/proposals", token,
		store.ProposalSubmission{TickNumber: tick, Proposal: p})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST proposal: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var env proposalEnvelope
	decodeJSON(t, rec, &env)
	var v contract.Verdict
	if err := json.Unmarshal(env.Verdict, &v); err != nil {
		t.Fatalf("decode envelope verdict %q: %v", env.Verdict, err)
	}
	return &v, env
}

func TestPostProposalApproveSubmitsAtL3(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")
	putMark(e, "BTC/USDT", "64000")

	v, env := postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if v.Decision != contract.DecisionApprove {
		t.Fatalf("decision = %s (%v), want approve", v.Decision, v.Reasons)
	}
	if env.Submitted == nil || !*env.Submitted || env.SubmitErrorCode != "" {
		t.Errorf("envelope = %+v, want submitted=true and no error code", env)
	}
	if env.PendingApproval != nil {
		t.Errorf("pending_approval key present at L3, want absent")
	}
	if e.sub.count() != 1 {
		t.Errorf("submitter calls = %d, want 1 (direct L3 submission)", e.sub.count())
	}
	if _, ok, err := e.store.GetPendingApproval(v.VerdictID); err != nil || ok {
		t.Errorf("pending approval = %v %v, want none at L3", ok, err)
	}
}

func TestPostProposalSubmitFailureReported(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")
	putMark(e, "BTC/USDT", "64000")
	e.sub.err = errors.New("paper OMS unavailable")

	_, env := postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if env.Submitted == nil || *env.Submitted || env.SubmitErrorCode != codeSubmitFailed {
		t.Errorf("envelope = %+v, want submitted=false with SUBMIT_FAILED", env)
	}
	rows, err := e.store.RejectedSubmissions(strat1)
	if err != nil || len(rows) != 1 {
		t.Errorf("rejected_submissions audit rows = %d (%v), want 1", len(rows), err)
	}
}

func TestPostProposalDuplicateReturnsVerdictVerbatim(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")
	putMark(e, "BTC/USDT", "64000")

	p := openProposal(t, uid(10), strat1, uid(12))
	v, _ := postProposal(e, t, strat1, agent1Tok, 0, p)

	// Duplicate: 200 envelope carrying the stored payload byte-for-byte;
	// no re-submit and no re-reported flags.
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: p})
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, body %s", rec.Code, rec.Body.String())
	}
	stored, err := e.store.GetVerdictByProposalID(p.ProposalID)
	if err != nil {
		t.Fatalf("GetVerdictByProposalID: %v", err)
	}
	var env proposalEnvelope
	decodeJSON(t, rec, &env)
	if !bytes.Equal(env.Verdict, stored) {
		t.Errorf("duplicate verdict not the stored bytes verbatim:\n got %s\nwant %s", env.Verdict, stored)
	}
	if env.Submitted != nil || env.SubmitErrorCode != "" || env.PendingApproval != nil {
		t.Errorf("duplicate envelope = %+v, want verdict only", env)
	}
	var dup contract.Verdict
	if err := json.Unmarshal(env.Verdict, &dup); err != nil {
		t.Fatalf("decode duplicate verdict: %v", err)
	}
	if dup.VerdictID != v.VerdictID {
		t.Errorf("duplicate verdict_id = %s, want original %s", dup.VerdictID, v.VerdictID)
	}
	if e.sub.count() != 1 {
		t.Errorf("submitter calls = %d, want 1 (never a second order)", e.sub.count())
	}

	// Same proposal_id, different payload: 409, still one verdict.
	changed := openProposal(t, uid(10), strat1, uid(12))
	changed.Reasoning = "a different payload for the same proposal_id"
	rec = e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: changed})
	wantError(t, rec, http.StatusConflict, codeIdempotencyConflict)

	// Same payload, different tick: the envelope differs, 409 as well.
	rec = e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 1, Proposal: p})
	wantError(t, rec, http.StatusConflict, codeIdempotencyConflict)
}

func TestPostProposalRunTickConflict(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")
	putMark(e, "BTC/USDT", "64000")

	postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))

	// A different proposal from a different run claiming the same
	// (strategy_id, tick_number): 409 RUN_TICK_CONFLICT, no verdict.
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: openProposal(t, uid(20), strat1, uid(22))})
	wantError(t, rec, http.StatusConflict, codeRunTickConflict)
	if _, err := e.store.GetVerdictByProposalID(uid(20)); err == nil {
		t.Errorf("conflicting submission earned a verdict; want none persisted")
	}
}

func TestPostProposalL1ArmsPendingApproval(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat2, "live_l1")
	putMark(e, "BTC/USDT", "64000")

	p := openProposal(t, uid(20), strat2, uid(22))
	v, env := postProposal(e, t, strat2, agent2Tok, 0, p)
	if v.Decision != contract.DecisionApprove {
		t.Fatalf("decision = %s (%v), want approve", v.Decision, v.Reasons)
	}
	if env.PendingApproval == nil || !*env.PendingApproval {
		t.Errorf("envelope = %+v, want pending_approval=true", env)
	}
	pending, ok, err := e.store.GetPendingApproval(v.VerdictID)
	if err != nil || !ok {
		t.Fatalf("pending approval = %v %v, want armed", ok, err)
	}
	if pending.DeadlineAt != "2026-07-04T12:40:00Z" { // created_at + 600 s
		t.Errorf("deadline_at = %q, want 12:40:00Z", pending.DeadlineAt)
	}
	if e.sub.count() != 0 {
		t.Errorf("submitter calls = %d, want 0 (L1 waits for the human)", e.sub.count())
	}

	// The duplicate did not arm the timer, so it reports no flag.
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat2+"/proposals", agent2Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: p})
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, body %s", rec.Code, rec.Body.String())
	}
	var dupEnv proposalEnvelope
	decodeJSON(t, rec, &dupEnv)
	if dupEnv.PendingApproval != nil {
		t.Errorf("duplicate envelope = %+v, want no pending_approval key", dupEnv)
	}
}

func TestPostProposalRejectionsNeverEarnVerdicts(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")

	// Body strategy_id outside the token/path scope: 403 + rejected row.
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: openProposal(t, uid(30), strat2, uid(32))})
	wantError(t, rec, http.StatusForbidden, codeStrategyScopeMismatch)

	// Invalid proposal_id (gate step 0a): 400 + rejected row, NO verdict.
	rec = e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		[]byte(`{"tick_number":0,"proposal":{"proposal_id":"nope"}}`))
	wantError(t, rec, http.StatusBadRequest, codeSchemaInvalid)

	// Missing tick_number: the run row would be mis-keyed — 400, never
	// silently tick 0.
	body, err := json.Marshal(map[string]any{"proposal": testProposal(t, uid(34), strat1, uid(36))})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec = e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok, body)
	wantError(t, rec, http.StatusBadRequest, codeSchemaInvalid)

	rows, err := e.store.RejectedSubmissions(strat1)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rejected_submissions = %d rows (%v), want 3", len(rows), err)
	}
}

func TestPostProposalRateLimitedPerStrategy(t *testing.T) {
	e := gatedEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")

	first := testProposal(t, uid(100), strat1, uid(400))
	for i := 0; i < 30; i++ {
		p := first
		if i > 0 {
			p = testProposal(t, uid(100+i), strat1, uid(400+i))
		}
		rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
			store.ProposalSubmission{TickNumber: i, Proposal: p})
		if rec.Code != http.StatusOK {
			t.Fatalf("POST #%d: status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 30, Proposal: testProposal(t, uid(131), strat1, uid(431))})
	wantError(t, rec, http.StatusTooManyRequests, codeRateLimited)
	if _, err := e.store.GetVerdictByProposalID(uid(131)); err == nil {
		t.Errorf("rate-limited submission earned a verdict; want none persisted")
	}

	// A verbatim duplicate re-POST is answered from the store without
	// charging the exhausted limiter: still 200.
	rec = e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: first})
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate under exhausted limiter: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var env proposalEnvelope
	decodeJSON(t, rec, &env)
	stored, err := e.store.GetVerdictByProposalID(uid(100))
	if err != nil {
		t.Fatalf("GetVerdictByProposalID: %v", err)
	}
	if !bytes.Equal(env.Verdict, stored) {
		t.Errorf("duplicate verdict not the stored bytes verbatim:\n got %s\nwant %s", env.Verdict, stored)
	}
}

func TestPostProposalUnknownStrategyAndDisabledRoute(t *testing.T) {
	e := gatedEnv(t) // token configured, but no strategies row
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: testProposal(t, uid(40), strat1, uid(42))})
	wantError(t, rec, http.StatusNotFound, codeUnknownStrategy)

	// Without Limits/RuntimeState the ingestion route does not exist.
	plain := newEnv(t, nil)
	rec = plain.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: testProposal(t, uid(40), strat1, uid(42))})
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled route status = %d, want 404", rec.Code)
	}
}
