package backtest

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/e2e"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
)

// executeOpen submits the (possibly clipped) entry under fill model v2,
// mirroring the e2e executeOpen: market entries fill at mark ± directional
// slippage (taker fee); marketable limits fill at the limit price
// (SubmitEntry); non-crossing limits rest open. Entry-fill fees are realized
// PnL, folded into the state as of the decision time.
func executeOpen(oms *paper.OMS, st *state, p *contract.Proposal, v *contract.Verdict, mark decimal.Decimal, now time.Time, rec *recorder) error {
	size := p.SizeQuote.Decimal()
	if v.ClippedSizeQuote != nil {
		size = v.ClippedSizeQuote.Decimal()
	}
	side := paper.SideBuy
	if p.Action == contract.ActionOpenShort {
		side = paper.SideSell
	}
	req := paper.EntryRequest{
		StrategyID: p.StrategyID,
		Symbol:     p.Symbol,
		Side:       side,
		Type:       p.Entry.Type,
		SizeQuote:  size,
		MarkPrice:  mark,
	}
	if p.StopLoss != nil {
		req.StopPrice = p.StopLoss.Decimal()
	}
	if p.TakeProfit != nil {
		req.TakeProfit = p.TakeProfit.Decimal()
	}
	if p.Entry.LimitPrice != nil {
		req.LimitPrice = p.Entry.LimitPrice.Decimal()
	}
	ord, err := oms.SubmitEntry(req)
	if err != nil {
		return fmt.Errorf("submit entry %s: %w", p.ProposalID, err)
	}
	if ord.Status == paper.StatusFilled {
		st.openPositions++
		st.applyRealized(oms, p.StrategyID, p.Symbol, now)
	}
	if err := rec.write("order", e2e.OrderToRecord(&ord, DeterministicID("order/"+p.ProposalID+"/entry"), p.ProposalID)); err != nil {
		return err
	}
	if pos, ok := oms.Position(p.StrategyID, p.Symbol); ok {
		return rec.write("position", e2e.PositionRecord{
			Kind:       "position",
			StrategyID: p.StrategyID,
			Symbol:     pos.Symbol,
			QtyBase:    pos.QtyBase.String(),
			EntryPrice: pos.EntryPrice.String(),
		})
	}
	return nil
}

// executeClose flattens the strategy's position reduce-only at the tick's
// mark, mirroring the e2e executeClose. With no usable mark the exit is
// QUEUED (market-data.md §Exits fail-closed): the order record stays open,
// the position and its stops stay intact, and the flatten retries via
// ProcessTick on the next fresh sub-tick.
func executeClose(oms *paper.OMS, st *state, p *contract.Proposal, mark decimal.Decimal, now time.Time, rec *recorder) error {
	ord, err := oms.Flatten(p.StrategyID, p.Symbol, mark)
	if err != nil {
		return fmt.Errorf("flatten %s: %w", p.ProposalID, err)
	}
	if err := rec.write("order", e2e.OrderToRecord(&ord, DeterministicID("order/"+p.ProposalID+"/close"), p.ProposalID)); err != nil {
		return err
	}
	if ord.Status != paper.StatusFilled {
		return nil
	}
	st.openPositions--
	st.applyRealized(oms, p.StrategyID, p.Symbol, now)
	// The final position record is read back from the OMS, not fabricated:
	// a correct flatten leaves the book flat (Position reports zero values).
	pos, ok := oms.Position(p.StrategyID, p.Symbol)
	if ok {
		return fmt.Errorf("close %s left a residual position: qty_base %s", p.ProposalID, pos.QtyBase)
	}
	return rec.write("position", e2e.PositionRecord{
		Kind:       "position",
		StrategyID: p.StrategyID,
		Symbol:     p.Symbol,
		QtyBase:    pos.QtyBase.String(),
		EntryPrice: pos.EntryPrice.String(),
	})
}
