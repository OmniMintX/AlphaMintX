package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/runstate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func postLimits(t *testing.T, e *testEnv, token, strategyID string, changes map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, "POST", "/api/v1/strategies/"+strategyID+"/limits", token,
		map[string]any{"changes": changes})
}

// TestLimitChangeAuditAndAtomicReject: one audit row per changed field
// (old -> new, actor, timestamp) in one transaction; ANY invalid field
// rejects the whole request with zero rows appended.
func TestLimitChangeAuditAndAtomicReject(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	rec := postLimits(t, e, adminTok, strat1, map[string]any{
		"max_open_positions": 5, "daily_loss_limit_quote": "250"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	rows, err := e.store.RiskLimitChanges()
	if err != nil {
		t.Fatalf("RiskLimitChanges: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want one per field", len(rows))
	}
	// Rows are field-sorted within the request; old values are the
	// effective (base) values at change time; the env-admin is attributed.
	if rows[0].Field != "daily_loss_limit_quote" || *rows[0].OldValue != "500" || rows[0].NewValue != "250" ||
		rows[1].Field != "max_open_positions" || *rows[1].OldValue != "3" || rows[1].NewValue != "5" ||
		rows[0].ActorID != "env-admin" {
		t.Fatalf("audit rows = %+v, %+v", rows[0], rows[1])
	}

	// Not whitelisted, never changeable, negative/exponent decimals (the
	// contract regex cannot represent them), out-of-bound ints, a decimal
	// sent as a JSON number, and an empty change set — all atomic rejects.
	invalid := []map[string]any{
		{"max_open_positions": 2, "symbol_whitelist": []string{"BTC/USDT"}},
		{"accounting_quote": "USD"},
		{"daily_loss_limit_quote": "-5"},
		{"daily_loss_limit_quote": "1e3"},
		{"max_orders_per_minute": 0},
		{"max_open_positions": -1},
		{"per_position_notional_cap_quote": 2000},
		{},
	}
	for _, changes := range invalid {
		wantError(t, postLimits(t, e, adminTok, strat1, changes), 400, codeInvalidLimitChange)
	}
	if rows, _ := e.store.RiskLimitChanges(); len(rows) != 2 {
		t.Fatalf("audit rows after rejects = %d, want 2 (atomic reject, no partial apply)", len(rows))
	}

	wantError(t, postLimits(t, e, adminTok, uid(9), map[string]any{"max_open_positions": 1}),
		404, codeUnknownStrategy)
}

// TestLimitChangeGateObservesNewValue: the gate reads the provider, so a
// lowered notional cap clips the very next evaluation.
func TestLimitChangeGateObservesNewValue(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	putMark(e, "BTC/USDT", "64000")

	v, _ := postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if v.Decision != "approve" {
		t.Fatalf("baseline decision = %s (%v), want approve under the 2000 cap", v.Decision, v.Reasons)
	}
	if rec := postLimits(t, e, adminTok, strat1, map[string]any{
		"per_position_notional_cap_quote": "1000"}); rec.Code != http.StatusOK {
		t.Fatalf("limit change status = %d (body %q)", rec.Code, rec.Body.String())
	}
	v, _ = postProposal(e, t, strat1, agent1Tok, 1, openProposal(t, uid(20), strat1, uid(22)))
	if v.Decision != "clip" {
		t.Fatalf("post-change decision = %s (%v), want clip at the lowered cap", v.Decision, v.Reasons)
	}
}

// TestLimitChangeOverlayWinsAcrossRestart: rebuilding the provider from the
// store replays the persisted overlay over WHATEVER env base is configured
// — the overlay always wins, even when the base changed since.
func TestLimitChangeOverlayWinsAcrossRestart(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	if rec := postLimits(t, e, adminTok, strat1, map[string]any{
		"max_open_positions": 5, "daily_loss_limit_quote": "250"}); rec.Code != http.StatusOK {
		t.Fatalf("limit change status = %d (body %q)", rec.Code, rec.Body.String())
	}

	changedBase := testRiskLimits()
	changedBase.MaxOpenPositions = 7
	changedBase.DailyLossLimitQuote = decimal.NewFromInt(999)
	changedBase.MaxOrdersPerMinute = 42
	provider, err := NewLimitsProvider(e.store, changedBase)
	if err != nil {
		t.Fatalf("NewLimitsProvider: %v", err)
	}
	got := provider.Limits(strat1)
	if got.MaxOpenPositions != 5 || !got.DailyLossLimitQuote.Equal(decimal.NewFromInt(250)) {
		t.Errorf("overlaid limits = %d/%s, want the persisted 5/250 over the new base", got.MaxOpenPositions, got.DailyLossLimitQuote)
	}
	if got.MaxOrdersPerMinute != 42 {
		t.Errorf("max_orders_per_minute = %d, want the new base 42 (field never overridden)", got.MaxOrdersPerMinute)
	}
	if other := provider.Limits(strat2); other.MaxOpenPositions != 7 {
		t.Errorf("foreign strategy limits = %d, want the bare base (per-strategy overlay)", other.MaxOpenPositions)
	}
}

// TestLimitChangePreflightUsesProvider: the approval preflight daily-loss
// check observes a lowered daily_loss_limit_quote through the provider —
// never a startup capture.
func TestLimitChangePreflightUsesProvider(t *testing.T) {
	var provider *LimitsProvider
	e := newEnv(t, func(cfg *Config) {
		limits := testRiskLimits()
		var err error
		if provider, err = NewLimitsProvider(cfg.Store, limits); err != nil {
			t.Fatalf("NewLimitsProvider: %v", err)
		}
		hyd := &runstate.Hydrator{Store: cfg.Store, Marks: cfg.Marks, AllocatedCapitalQuote: limits.AllocatedCapitalQuote}
		cfg.Limits = &limits
		cfg.LimitsProvider = provider
		cfg.RuntimeState = hyd
		cfg.DailyLossBreached = func(strategyID string, now time.Time) (bool, error) {
			daily, err := hyd.DailyPnL(strategyID, now)
			if err != nil {
				return false, err
			}
			return daily.LessThanOrEqual(provider.Limits(strategyID).DailyLossLimitQuote.Neg()), nil
		}
	})
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "live_l1")
	e.marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("64000"), TS: testNow})
	if err := e.store.UpsertStrategyState(store.StrategyState{
		StrategyID: strat1, EquityQuote: "9900", PeakEquityQuote: "10000",
		DailyRealizedPnLQuote: "-100", UTCDate: "2026-07-04", UpdatedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("UpsertStrategyState: %v", err)
	}

	// Base limit 500: a -100 day passes the preflight.
	_, verdict1, _ := insertChain(t, e.store, 10, strat1, 0)
	if err := e.store.CreatePendingApproval(verdict1, strat1, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	rec := postApproval(t, e, strat1, verdict1, true)
	var first store.Approval
	decodeJSON(t, rec, &first)
	if first.Outcome != store.OutcomeApproved {
		t.Fatalf("baseline outcome = %q (%v), want approved", first.Outcome, first.PreflightReasons)
	}

	// Lower the limit to 50 at runtime: the same -100 day now blocks.
	if rec := postLimits(t, e, adminTok, strat1, map[string]any{
		"daily_loss_limit_quote": "50"}); rec.Code != http.StatusOK {
		t.Fatalf("limit change status = %d (body %q)", rec.Code, rec.Body.String())
	}
	_, verdict2, _ := insertChain(t, e.store, 20, strat1, 1)
	if err := e.store.CreatePendingApproval(verdict2, strat1, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	rec = postApproval(t, e, strat1, verdict2, true)
	var second store.Approval
	decodeJSON(t, rec, &second)
	if second.Outcome != store.OutcomeApprovedButBlocked ||
		!slices.Contains(second.PreflightReasons, reasonDailyLossLimitBreach) {
		t.Fatalf("post-change outcome = %q (%v), want blocked on DAILY_LOSS_LIMIT_BREACHED",
			second.Outcome, second.PreflightReasons)
	}
}
