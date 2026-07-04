package omsbridge

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestRestartRehydration: a fresh bridge over an existing store re-arms the
// resting limit entry WITH its SL/TP obligations (a post-restart fill still
// places both protectives, linked to the entry's proposal) and restores the
// realized PnL of every book.
func TestRestartRehydration(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "cp.db"))
	createStrategy(t, st, uid(1))
	marks := newMarks(t)
	putMark(marks, "BTC/USDT", "64000", testNow)
	putMark(marks, "ETH/USDT", "3000", testNow)
	clock := testNow
	b1 := newBridge(t, st, marks, &clock)

	// Resting BTC limit entry carrying SL + TP obligations.
	limit, slB, tpB := mustDec(t, "60000"), mustDec(t, "58000"), mustDec(t, "70000")
	pLimit := testProposal(t, uid(20), uid(1), uid(22), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "1200")
		p.Entry = contract.Entry{Type: "limit", LimitPrice: &limit}
		p.StopLoss, p.TakeProfit = &slB, &tpB
	})
	if err := b1.SubmitApproved(insertChain(t, st, 21, 0, pLimit)); err != nil {
		t.Fatalf("SubmitApproved limit: %v", err)
	}
	// Filled ETH market entry: open position with realized = -fee.
	slE := mustDec(t, "2900")
	pEth := testProposal(t, uid(30), uid(1), uid(32), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.Symbol = "ETH/USDT"
		p.SizeQuote = mustDec(t, "300")
		p.StopLoss = &slE
	})
	if err := b1.SubmitApproved(insertChain(t, st, 31, 1, pEth)); err != nil {
		t.Fatalf("SubmitApproved market: %v", err)
	}

	// Restart: rebuild the OMS from the store alone.
	b2 := newBridge(t, st, marks, &clock)

	var wantRealized decimal.Decimal
	positions, err := st.ListPositions(uid(1))
	if err != nil {
		t.Fatalf("ListPositions: %v", err)
	}
	for _, pos := range positions {
		if pos.Symbol == "ETH/USDT" {
			wantRealized = mustParse(t, pos.RealizedPnLQuote)
		}
	}
	if wantRealized.Sign() >= 0 {
		t.Fatalf("persisted ETH realized = %s, want < 0 (entry fee)", wantRealized)
	}
	if got := b2.oms.RealizedPnL(uid(1), "ETH/USDT"); !got.Equal(wantRealized) {
		t.Errorf("restored realized = %s, want %s", got, wantRealized)
	}

	// The re-armed BTC limit fills on the next crossing tick and places
	// BOTH protectives (TP re-armed on the resting limit), all linked to
	// the entry's proposal.
	if err := b2.Sweep(map[string]decimal.Decimal{"BTC/USDT": mustParse(t, "59500")}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	d, err := st.GetRunDetail(uid(1), uid(22))
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if len(d.Orders) != 3 || len(d.Fills) != 1 {
		t.Fatalf("after sweep fill: orders=%d fills=%d, want 3/1", len(d.Orders), len(d.Fills))
	}
	byType := map[string]store.Order{}
	for _, o := range d.Orders {
		byType[o.Type] = o
	}
	if e := byType["limit"]; e.Status != "filled" || e.FillPrice == nil || *e.FillPrice != "60000" {
		t.Errorf("limit entry = %+v, want filled at 60000 (maker, no slippage)", e)
	}
	if s := byType["stop"]; s.Status != "open" || s.StopPrice == nil || *s.StopPrice != "58000" {
		t.Errorf("stop = %+v, want re-armed SL at 58000", s)
	}
	if tp := byType["take_profit"]; tp.Status != "open" || tp.LimitPrice == nil || *tp.LimitPrice != "70000" {
		t.Errorf("take_profit = %+v, want re-armed TP at 70000", tp)
	}
}

// TestStrategyStateDailyRollover: realized PnL on a later UTC day restarts
// the daily figure at zero before the new delta lands; equity accumulates
// across days and peak is monotone.
func TestStrategyStateDailyRollover(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "cp.db"))
	createStrategy(t, st, uid(1))
	marks := newMarks(t)
	putMark(marks, "BTC/USDT", "64000", testNow)
	clock := testNow
	b := newBridge(t, st, marks, &clock)

	sl := mustDec(t, "62000")
	pOpen := testProposal(t, uid(10), uid(1), uid(12), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "1000")
		p.StopLoss = &sl
	})
	if err := b.SubmitApproved(insertChain(t, st, 11, 0, pOpen)); err != nil {
		t.Fatalf("SubmitApproved open: %v", err)
	}
	row1, ok, err := st.GetStrategyState(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetStrategyState day 1: %v %v", ok, err)
	}

	// Next UTC day: close into profit.
	day2 := testNow.Add(24 * time.Hour)
	clock = day2
	putMark(marks, "BTC/USDT", "66000", day2)
	pClose := testProposal(t, uid(40), uid(1), uid(42), contract.ActionClose, nil)
	if err := b.SubmitApproved(insertChain(t, st, 41, 1, pClose)); err != nil {
		t.Fatalf("SubmitApproved close: %v", err)
	}

	row2, ok, err := st.GetStrategyState(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetStrategyState day 2: %v %v", ok, err)
	}
	if row2.UTCDate != "2026-07-05" {
		t.Errorf("utc_date = %q, want 2026-07-05", row2.UTCDate)
	}
	delta := mustParse(t, row2.EquityQuote).Sub(mustParse(t, row1.EquityQuote))
	if delta.Sign() <= 0 {
		t.Fatalf("day-2 realized delta = %s, want > 0 (profitable close)", delta)
	}
	if daily := mustParse(t, row2.DailyRealizedPnLQuote); !daily.Equal(delta) {
		t.Errorf("day-2 daily = %s, want the rolled-over delta %s (day-1 fee excluded)", daily, delta)
	}
	if peak := mustParse(t, row2.PeakEquityQuote); !peak.Equal(mustParse(t, row2.EquityQuote)) {
		t.Errorf("peak = %s, want equity %s (monotone rise above the seed)", peak, row2.EquityQuote)
	}
}
