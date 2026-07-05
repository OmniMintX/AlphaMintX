package live

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
)

// S16: live venue fills flow through the IDENTICAL accounting math as the
// paper OMS (invariant 10) — the same fill sequence yields the same entry
// price, fees, realized PnL, strategy_state, VWAP fill_price, and
// filled_at.
func TestAccounting_PaperParity(t *testing.T) {
	e := newEnv(t)
	e.reconcile()

	// Paper baseline: market buy 640 quote at mark 64000 (slippage 0,
	// taker 10 bps => fill 0.01@64000, fee 0.64), then a flatten at 65000
	// (fee 0.65).
	pom, err := paper.New(paper.FillModel{
		MarketSlippageBps: "0", TakerFeeBps: "10", MakerFeeBps: "10",
	})
	if err != nil {
		t.Fatalf("paper.New: %v", err)
	}
	pEntry, err := pom.SubmitEntry(paper.EntryRequest{
		StrategyID: uid(1), Symbol: "BTC/USDT", Side: paper.SideBuy, Type: "market",
		SizeQuote: decimal.NewFromInt(640), MarkPrice: decimal.NewFromInt(64000),
	})
	if err != nil {
		t.Fatalf("paper SubmitEntry: %v", err)
	}
	pPos, ok := pom.Position(uid(1), "BTC/USDT")
	if !ok {
		t.Fatal("paper position missing after the entry fill")
	}

	// Live: the SAME executions arrive as venue fills (quote-asset
	// commissions carry the paper fee figures verbatim).
	if err := e.submitEntryWith(10, func(p *contract.Proposal) {
		p.SizeQuote = mustDec(t, "640")
	}); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.FillWithCommission(idN(1, 0), "0.01", "64000", "0.64", "USDT"); err != nil {
		t.Fatalf("FillWithCommission entry: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01", "0")
	e.reconcile()

	pos, ok := e.position()
	if !ok {
		t.Fatal("live position missing after the entry fill")
	}
	if pos.QtyBase != pPos.QtyBase.String() || pos.EntryPrice != pPos.EntryPrice.String() ||
		pos.FeesQuote != pPos.FeesQuote.String() {
		t.Errorf("live book = qty %s entry %s fees %s, want paper %s / %s / %s",
			pos.QtyBase, pos.EntryPrice, pos.FeesQuote,
			pPos.QtyBase, pPos.EntryPrice, pPos.FeesQuote)
	}
	ord := e.order(idN(1, 0))
	if ord.Status != "filled" || ord.FillPrice == nil || *ord.FillPrice != pEntry.FillPrice.String() {
		t.Errorf("live entry = status %s fill_price %v, want filled at paper %s",
			ord.Status, ord.FillPrice, pEntry.FillPrice)
	}
	if ord.FilledAt == nil || *ord.FilledAt != formatTime(testNow) {
		t.Errorf("filled_at = %v, want %s", ord.FilledAt, formatTime(testNow))
	}

	// Round trip: paper flatten at 65000; live reduce-only flatten filled
	// at the same price and fee.
	if _, err := pom.Flatten(uid(1), "BTC/USDT", decimal.NewFromInt(65000)); err != nil {
		t.Fatalf("paper Flatten: %v", err)
	}
	if err := e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "kill", nil); err != nil {
		t.Fatalf("live Flatten: %v", err)
	}
	if err := e.venue.FillWithCommission(idN(2, 0), "0.01", "65000", "0.65", "USDT"); err != nil {
		t.Fatalf("FillWithCommission exit: %v", err)
	}
	e.reconcile()

	pos, _ = e.position()
	want := pom.RealizedPnL(uid(1), "BTC/USDT")
	if pos.QtyBase != "0" || pos.RealizedPnLQuote != want.String() {
		t.Errorf("flat book = qty %s realized %s, want 0 / paper %s",
			pos.QtyBase, pos.RealizedPnLQuote, want)
	}
	state, ok, err := e.st.GetStrategyState(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetStrategyState: ok=%v err=%v", ok, err)
	}
	wantEquity := decimal.NewFromInt(10000).Add(want)
	if state.EquityQuote != wantEquity.String() || state.DailyRealizedPnLQuote != want.String() {
		t.Errorf("strategy_state = equity %s daily %s, want %s / %s",
			state.EquityQuote, state.DailyRealizedPnLQuote, wantEquity, want)
	}
	exit := e.order(idN(2, 0))
	if exit.Status != "filled" || exit.FillPrice == nil || *exit.FillPrice != "65000" {
		t.Errorf("live exit = status %s fill_price %v, want filled at VWAP 65000",
			exit.Status, exit.FillPrice)
	}
}
