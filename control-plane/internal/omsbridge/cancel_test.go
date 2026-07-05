package omsbridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// TestCancelOpenEntries pins LC-12a: the bridge cancels ONE strategy's
// resting un-filled ENTRY orders and persists the flip; reduce-only
// protectives and other strategies' entries are untouched, and a later
// crossing tick no longer fills the canceled entry.
func TestCancelOpenEntries(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "cp.db"))
	createStrategy(t, st, uid(1))
	createStrategy(t, st, uid(2))
	marks := newMarks(t)
	putMark(marks, "BTC/USDT", "64000", testNow)
	clock := testNow
	b := newBridge(t, st, marks, &clock)

	// strat1: a filled market entry with SL/TP protectives (stop far
	// below so the later 59500 tick cannot trigger it).
	sl, tp := mustDec(t, "55000"), mustDec(t, "70000")
	market := testProposal(t, uid(10), uid(1), uid(12), contract.ActionOpenLong, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "1000")
		p.StopLoss, p.TakeProfit = &sl, &tp
	})
	if err := b.SubmitApproved(insertChain(t, st, 11, 0, market)); err != nil {
		t.Fatalf("SubmitApproved(market): %v", err)
	}
	// strat1 + strat2: resting limit entries at 60000 (mark 64000 rests).
	limit := mustDec(t, "60000")
	resting := func(base int, strategyID, proposalID, runID string, tick int) {
		p := testProposal(t, proposalID, strategyID, runID, contract.ActionOpenLong, func(p *contract.Proposal) {
			p.SizeQuote = mustDec(t, "1200")
			p.Entry = contract.Entry{Type: "limit", LimitPrice: &limit}
			p.StopLoss = &sl
		})
		if err := b.SubmitApproved(insertChain(t, st, base, tick, p)); err != nil {
			t.Fatalf("SubmitApproved(resting %s): %v", strategyID, err)
		}
	}
	resting(21, uid(1), uid(20), uid(22), 1)
	resting(31, uid(2), uid(30), uid(32), 0)

	if err := b.CancelOpenEntries(context.Background(), uid(1)); err != nil {
		t.Fatalf("CancelOpenEntries: %v", err)
	}

	// strat1's resting entry is persisted canceled; protectives survive.
	d, err := st.GetRunDetail(uid(1), uid(22))
	if err != nil || len(d.Orders) != 1 {
		t.Fatalf("strat1 resting orders = %v (%v), want 1", d.Orders, err)
	}
	if d.Orders[0].Status != "canceled" {
		t.Errorf("resting entry status = %q, want canceled", d.Orders[0].Status)
	}
	open, err := st.ListOpenOrders(uid(1))
	if err != nil {
		t.Fatalf("ListOpenOrders: %v", err)
	}
	for _, o := range open {
		if o.Class != "PROTECTIVE" || !o.ReduceOnly {
			t.Errorf("open order after cancel = %+v, want protectives only", o)
		}
	}
	if len(open) != 2 {
		t.Errorf("strat1 open orders = %d, want the SL and TP", len(open))
	}
	// strat2's entry is untouched.
	if open, err = st.ListOpenOrders(uid(2)); err != nil || len(open) != 1 || open[0].Class != "ENTRY" {
		t.Fatalf("strat2 open orders = %+v (%v), want its resting ENTRY", open, err)
	}

	// A crossing tick fills strat2's entry but never the canceled one.
	if err := b.Sweep(map[string]decimal.Decimal{"BTC/USDT": decimal.RequireFromString("59500")}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if d, err = st.GetRunDetail(uid(1), uid(22)); err != nil || len(d.Fills) != 0 {
		t.Errorf("canceled entry fills = %d (%v), want 0", len(d.Fills), err)
	}
	if d, err = st.GetRunDetail(uid(2), uid(32)); err != nil || len(d.Fills) != 1 {
		t.Errorf("strat2 entry fills = %d (%v), want 1", len(d.Fills), err)
	}
}
