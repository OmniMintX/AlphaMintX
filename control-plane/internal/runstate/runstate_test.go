package runstate

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func TestAutonomyFor(t *testing.T) {
	want := map[string]riskgate.Autonomy{
		"live_l1": riskgate.AutonomyL1, "live_l2": riskgate.AutonomyL2, "live_l3": riskgate.AutonomyL3,
		"paper": riskgate.AutonomyL0, "draft": riskgate.AutonomyL0,
		"paused": riskgate.AutonomyL0, "killed": riskgate.AutonomyL0,
	}
	for state, autonomy := range want {
		if got := AutonomyFor(state); got != autonomy {
			t.Errorf("AutonomyFor(%q) = %v, want %v", state, got, autonomy)
		}
	}
}

// TestStateFoldsUnrealized: a long book underwater at the fresh mark drags
// BOTH equity and the daily figure down (daily loss reflects unrealized,
// risk-limits.md Definitions) while the stored peak stays monotone.
func TestStateFoldsUnrealized(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	upsertPosition(t, s, uid(1), "BTC/USDT", "0.1", "60000")
	upsertState(t, s, uid(1), "10000", "10500", "-50", "2026-07-04")
	h, marks := newHydrator(t, s)
	putMark(marks, "BTC/USDT", "59000", testNow) // unrealized = -100

	state, err := h.State(uid(1), "live_l3", "BTC/USDT", testNow)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.EquityQuote.Equal(decimal.RequireFromString("9900")) {
		t.Errorf("equity = %s, want 9900", state.EquityQuote)
	}
	if !state.DailyRealizedPnLQuote.Equal(decimal.RequireFromString("-150")) {
		t.Errorf("daily = %s, want -150 (realized -50 + unrealized -100)", state.DailyRealizedPnLQuote)
	}
	if !state.PeakEquityQuote.Equal(decimal.RequireFromString("10500")) {
		t.Errorf("peak = %s, want 10500 (monotone)", state.PeakEquityQuote)
	}
	if state.OpenPositionsCount != 1 {
		t.Errorf("open positions = %d, want 1", state.OpenPositionsCount)
	}
	if !state.MarkPrice.Equal(decimal.RequireFromString("59000")) {
		t.Errorf("mark = %s, want 59000", state.MarkPrice)
	}
	if state.Autonomy != riskgate.AutonomyL3 || state.KillActive {
		t.Errorf("autonomy/kill = %v/%v, want L3, false", state.Autonomy, state.KillActive)
	}
}

// TestStatePeakRisesWithEquity: a book in profit lifts equity above the
// stored peak; the hydrated peak follows (max of stored and current).
func TestStatePeakRisesWithEquity(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	upsertPosition(t, s, uid(1), "BTC/USDT", "0.1", "60000")
	upsertState(t, s, uid(1), "10000", "10000", "0", "2026-07-04")
	h, marks := newHydrator(t, s)
	putMark(marks, "BTC/USDT", "65000", testNow) // unrealized = +500

	state, err := h.State(uid(1), "live_l3", "BTC/USDT", testNow)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.EquityQuote.Equal(decimal.RequireFromString("10500")) ||
		!state.PeakEquityQuote.Equal(decimal.RequireFromString("10500")) {
		t.Errorf("equity/peak = %s/%s, want 10500/10500", state.EquityQuote, state.PeakEquityQuote)
	}
}

// TestStateStaleMarkFailsClosed: a stale tick yields a zero MarkPrice (the
// gate then rejects MARK_PRICE_UNAVAILABLE) and contributes NO unrealized
// PnL; the position still occupies its slot. A strategy_state row from an
// earlier UTC day rolls the daily figure over to zero.
func TestStateStaleMarkFailsClosed(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	upsertPosition(t, s, uid(1), "BTC/USDT", "0.1", "60000")
	upsertState(t, s, uid(1), "9950", "10000", "-75", "2026-07-03") // yesterday
	h, marks := newHydrator(t, s)
	putMark(marks, "BTC/USDT", "59000", testNow.Add(-2*time.Minute)) // stale (> 60s)

	state, err := h.State(uid(1), "live_l3", "BTC/USDT", testNow)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if !state.MarkPrice.IsZero() {
		t.Errorf("mark = %s, want 0 (stale never leaks)", state.MarkPrice)
	}
	if !state.EquityQuote.Equal(decimal.RequireFromString("9950")) {
		t.Errorf("equity = %s, want 9950 (no unrealized from a stale mark)", state.EquityQuote)
	}
	if !state.DailyRealizedPnLQuote.IsZero() {
		t.Errorf("daily = %s, want 0 (UTC rollover of yesterday's row)", state.DailyRealizedPnLQuote)
	}
	if state.OpenPositionsCount != 1 {
		t.Errorf("open positions = %d, want 1", state.OpenPositionsCount)
	}
}

// TestStateRateCountPendingAndKill: the sliding-window rate figure counts
// approve/clip verdicts on non-hold proposals only; open ENTRY orders feed
// the pending count; a persisted kill event keeps the gate shut.
func TestStateRateCountPendingAndKill(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	insertVerdictChain(t, s, 10, 0, uid(1), contract.ActionOpenLong, contract.DecisionApprove, "2026-07-04T11:59:30Z") // counts
	insertVerdictChain(t, s, 20, 1, uid(1), contract.ActionClose, contract.DecisionApprove, "2026-07-04T11:59:50Z")    // counts
	insertVerdictChain(t, s, 30, 2, uid(1), contract.ActionOpenLong, contract.DecisionApprove, "2026-07-04T11:58:00Z") // outside window
	insertVerdictChain(t, s, 40, 3, uid(1), contract.ActionHold, contract.DecisionApprove, "2026-07-04T11:59:40Z")     // hold: no order
	insertVerdictChain(t, s, 50, 4, uid(1), contract.ActionOpenLong, contract.DecisionReject, "2026-07-04T11:59:45Z")  // reject: no token
	for i, class := range []string{"ENTRY", "PROTECTIVE"} {
		err := s.InsertOrder(store.Order{
			OrderID: uid(60 + i), Origin: "watchdog", StrategyID: uid(1), Symbol: "BTC/USDT",
			Class: class, Side: "buy", Type: "limit", QtyBase: "0.1", KillEpoch: 0,
			Status: "open", SubmittedAt: formatTime(testNow),
		})
		if err != nil {
			t.Fatalf("InsertOrder: %v", err)
		}
	}
	if err := s.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(70), Kind: "kill", Scope: "strategy", StrategyID: strptr(uid(1)),
		KillEpoch: int64ptr(3), ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	h, _ := newHydrator(t, s)

	state, err := h.State(uid(1), "live_l3", "BTC/USDT", testNow)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.EntryOrdersInLastMinute != 2 {
		t.Errorf("rate count = %d, want 2 (approve open + approve close in window)", state.EntryOrdersInLastMinute)
	}
	if state.PendingEntryOrdersCount != 1 {
		t.Errorf("pending entries = %d, want 1 (PROTECTIVE excluded)", state.PendingEntryOrdersCount)
	}
	if !state.KillActive || state.KillEpoch != 3 {
		t.Errorf("kill = %v/%d, want active epoch 3", state.KillActive, state.KillEpoch)
	}
	if !state.EquityQuote.Equal(decimal.NewFromInt(10000)) {
		t.Errorf("equity = %s, want the 10000 allocated-capital seed (no strategy_state row)", state.EquityQuote)
	}
}

func strptr(s string) *string { return &s }
func int64ptr(i int64) *int64 { return &i }
