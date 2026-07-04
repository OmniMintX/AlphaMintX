package omsbridge

import (
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func mustParse(t *testing.T, v string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(v)
	if err != nil {
		t.Fatalf("NewFromString(%q): %v", v, err)
	}
	return d
}

// TestSubmitApprovedMarketEntryPersists: a market entry fill writes the
// entry order (filled, slipped price), its protective SL/TP orders (open,
// reduce-only), one fill row with the taker fee recorded separately, the
// position snapshot with realized = -fee, and the strategy_state snapshot
// (equity down by the fee, peak monotone at the seed).
func TestSubmitApprovedMarketEntryPersists(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "cp.db"))
	createStrategy(t, st, uid(1))
	marks := newMarks(t)
	putMark(marks, "BTC/USDT", "64000", testNow)
	clock := testNow
	b := newBridge(t, st, marks, &clock)

	sl, tp := mustDec(t, "62000"), mustDec(t, "70000")
	p := testProposal(t, uid(10), uid(1), uid(12), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "1000")
		p.StopLoss, p.TakeProfit = &sl, &tp
	})
	meta := insertChain(t, st, 11, 0, p)
	if err := b.SubmitApproved(meta); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}

	d, err := st.GetRunDetail(uid(1), uid(12))
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if len(d.Orders) != 3 || len(d.Fills) != 1 {
		t.Fatalf("orders/fills = %d/%d, want 3/1", len(d.Orders), len(d.Fills))
	}
	byType := map[string]store.Order{}
	for _, o := range d.Orders {
		byType[o.Type] = o
	}
	entry := byType["market"]
	if entry.Class != "ENTRY" || entry.Status != "filled" ||
		entry.FillPrice == nil || *entry.FillPrice != "64064" || entry.FilledAt == nil {
		t.Errorf("entry order = %+v, want filled ENTRY at 64064 (10 bps slippage)", entry)
	}
	stop := byType["stop"]
	if stop.Class != "PROTECTIVE" || !stop.ReduceOnly || stop.Status != "open" ||
		stop.StopPrice == nil || *stop.StopPrice != "62000" {
		t.Errorf("stop order = %+v, want open reduce-only stop at 62000", stop)
	}
	tpOrd := byType["take_profit"]
	if tpOrd.Class != "PROTECTIVE" || !tpOrd.ReduceOnly || tpOrd.Status != "open" ||
		tpOrd.LimitPrice == nil || *tpOrd.LimitPrice != "70000" {
		t.Errorf("take_profit order = %+v, want open reduce-only TP at 70000", tpOrd)
	}

	positions, err := st.ListPositions(uid(1))
	if err != nil || len(positions) != 1 {
		t.Fatalf("ListPositions = %v, %v, want one row", positions, err)
	}
	pos := positions[0]
	fee := mustParse(t, d.Fills[0].FeeQuote)
	if fee.Sign() <= 0 {
		t.Fatalf("fill fee = %s, want > 0", fee)
	}
	if qty := mustParse(t, pos.QtyBase); qty.Sign() <= 0 {
		t.Errorf("position qty = %s, want > 0", qty)
	}
	if realized := mustParse(t, pos.RealizedPnLQuote); !realized.Equal(fee.Neg()) {
		t.Errorf("realized = %s, want -fee %s (fees realized when paid)", realized, fee.Neg())
	}

	row, ok, err := st.GetStrategyState(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetStrategyState = %v, %v, want a row", ok, err)
	}
	alloc := decimal.NewFromInt(10000)
	if equity := mustParse(t, row.EquityQuote); !equity.Equal(alloc.Sub(fee)) {
		t.Errorf("equity = %s, want %s (allocated - fee)", equity, alloc.Sub(fee))
	}
	if daily := mustParse(t, row.DailyRealizedPnLQuote); !daily.Equal(fee.Neg()) {
		t.Errorf("daily = %s, want %s", daily, fee.Neg())
	}
	if peak := mustParse(t, row.PeakEquityQuote); !peak.Equal(alloc) {
		t.Errorf("peak = %s, want %s (monotone at seed)", peak, alloc)
	}
	if row.UTCDate != "2026-07-04" {
		t.Errorf("utc_date = %q, want 2026-07-04", row.UTCDate)
	}
}

// TestNoFillSweepWritesZeroRows: a sweep whose marks trigger nothing leaves
// orders, fills, positions, and strategy_state untouched.
func TestNoFillSweepWritesZeroRows(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "cp.db"))
	createStrategy(t, st, uid(1))
	marks := newMarks(t)
	putMark(marks, "BTC/USDT", "64000", testNow)
	clock := testNow
	b := newBridge(t, st, marks, &clock)

	limit, sl := mustDec(t, "60000"), mustDec(t, "58000")
	p := testProposal(t, uid(10), uid(1), uid(12), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "1200")
		p.Entry = contract.Entry{Type: "limit", LimitPrice: &limit}
		p.StopLoss = &sl
	})
	meta := insertChain(t, st, 11, 0, p)
	if err := b.SubmitApproved(meta); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}

	// 63000 does not cross the 60000 buy limit: nothing fills.
	if err := b.Sweep(map[string]decimal.Decimal{"BTC/USDT": mustParse(t, "63000")}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	d, err := st.GetRunDetail(uid(1), uid(12))
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if len(d.Orders) != 1 || len(d.Fills) != 0 {
		t.Fatalf("after no-fill sweep: orders=%d fills=%d, want 1, 0", len(d.Orders), len(d.Fills))
	}
	if d.Orders[0].Status != "open" {
		t.Errorf("resting order status = %q, want open", d.Orders[0].Status)
	}
	if positions, _ := st.ListPositions(uid(1)); len(positions) != 0 {
		t.Errorf("positions = %v, want none", positions)
	}
	if _, ok, _ := st.GetStrategyState(uid(1)); ok {
		t.Errorf("strategy_state row written by a no-fill sweep")
	}
}
