package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// seedCloseExempt pre-seeds the close_exempt strategy with an open BTC/USDT
// position (matching the emitter's close proposal symbol) and a daily realized
// loss beyond daily_loss_limit_quote (500), so its close approving proves the
// exit exemption of gate step 3.
func seedCloseExempt(spec *RunSpec, states map[string]*strategyState, oms *paper.OMS) error {
	for _, s := range spec.Strategies {
		if s.Scenario != ScenarioCloseExempt {
			continue
		}
		st := states[s.StrategyID]
		st.dailyPnL = decimal.NewFromInt(-600)
		st.openPositions = 1
		if _, err := oms.SubmitEntry(paper.EntryRequest{
			StrategyID: s.StrategyID,
			Symbol:     "BTC/USDT",
			Side:       paper.SideBuy,
			Type:       "market",
			SizeQuote:  decimal.NewFromInt(1700),
			MarkPrice:  decimal.RequireFromString("64180.1"),
			StopPrice:  decimal.NewFromInt(62900),
		}); err != nil {
			return fmt.Errorf("seed close_exempt position: %w", err)
		}
	}
	return nil
}

// markAt resolves the mark price for a symbol at tick i, falling back to the
// last element when the series is short. Unknown symbols resolve to zero and
// reject at the gate (MARK_PRICE_UNAVAILABLE) for market entries.
func markAt(marks map[string][]decimal.Decimal, symbol string, tick int) decimal.Decimal {
	series := marks[symbol]
	if len(series) == 0 {
		return decimal.Zero
	}
	if tick >= len(series) {
		tick = len(series) - 1
	}
	return series[tick]
}

// evaluateOne gates one proposal and routes approved/clipped executions to
// the paper OMS, writing records in the stable order proposal, verdict,
// order(s), position(s).
func evaluateOne(gate *riskgate.Service, oms *paper.OMS, limits riskgate.RiskLimits, st *strategyState, p *contract.Proposal, mark decimal.Decimal, now time.Time, out io.Writer, got map[string]OutcomeDetail) error {
	state := riskgate.RuntimeState{
		Autonomy:              riskgate.AutonomyL3,
		EquityQuote:           st.equity,
		PeakEquityQuote:       st.peak,
		DailyRealizedPnLQuote: st.dailyPnL,
		OpenPositionsCount:    st.openPositions,
		MarkPrice:             mark,
	}
	verdict, err := gate.Evaluate(p, limits, state, now)
	if err != nil {
		return fmt.Errorf("gate %s: %w", p.ProposalID, err)
	}
	verdict.VerdictID = DeterministicID("verdict/" + p.ProposalID)
	detail := OutcomeDetail{Outcome: string(verdict.Decision)}
	if len(verdict.Reasons) > 0 {
		detail.PrimaryReason = verdict.Reasons[0].Code
	}
	if verdict.ClippedSizeQuote != nil {
		detail.ClippedSizeQuote = verdict.ClippedSizeQuote.String()
	}
	got[p.StrategyID] = detail

	if err := writeRecord(out, proposalRecord{Kind: "proposal", Proposal: p}); err != nil {
		return err
	}
	if err := writeRecord(out, verdictRecord{Kind: "verdict", Verdict: &verdict}); err != nil {
		return err
	}

	approved := verdict.Decision == contract.DecisionApprove || verdict.Decision == contract.DecisionClip
	switch {
	case approved && p.Action.IsOpen():
		return executeOpen(oms, st, p, &verdict, mark, out)
	case verdict.Decision == contract.DecisionApprove && p.Action == contract.ActionClose:
		return executeClose(oms, st, p, mark, out)
	}
	return nil
}

// executeOpen submits the (possibly clipped) entry; limit entries fill via
// FillLimitEntry when the tick's mark crosses the limit price at the same
// tick (buy: mark <= limit, sell: mark >= limit).
func executeOpen(oms *paper.OMS, st *strategyState, p *contract.Proposal, v *contract.Verdict, mark decimal.Decimal, out io.Writer) error {
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
	if p.Entry.LimitPrice != nil {
		req.LimitPrice = p.Entry.LimitPrice.Decimal()
	}
	ord, err := oms.SubmitEntry(req)
	if err != nil {
		return fmt.Errorf("submit entry %s: %w", p.ProposalID, err)
	}
	if ord.Type == "limit" && ord.Status == paper.StatusOpen && limitCrossed(side, mark, ord.LimitPrice) {
		if ord, err = oms.FillLimitEntry(ord.ID); err != nil {
			return fmt.Errorf("fill limit entry %s: %w", p.ProposalID, err)
		}
	}
	if ord.Status == paper.StatusFilled {
		st.openPositions++
	}
	if err := writeRecord(out, orderToRecord(&ord, DeterministicID("order/"+p.ProposalID+"/entry"), p.ProposalID)); err != nil {
		return err
	}
	if pos, ok := oms.Position(p.StrategyID, p.Symbol); ok {
		return writeRecord(out, positionRecord{
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
// mark (the paper OMS cancels the protective stop after the flatten fill).
func executeClose(oms *paper.OMS, st *strategyState, p *contract.Proposal, mark decimal.Decimal, out io.Writer) error {
	ord, err := oms.Flatten(p.StrategyID, p.Symbol, mark)
	if err != nil {
		return fmt.Errorf("flatten %s: %w", p.ProposalID, err)
	}
	st.openPositions--
	if err := writeRecord(out, orderToRecord(&ord, DeterministicID("order/"+p.ProposalID+"/close"), p.ProposalID)); err != nil {
		return err
	}
	// The final position record is read back from the OMS, not fabricated:
	// a correct flatten leaves the book flat (Position reports zero values).
	pos, ok := oms.Position(p.StrategyID, p.Symbol)
	if ok {
		return fmt.Errorf("close %s left a residual position: qty_base %s", p.ProposalID, pos.QtyBase)
	}
	return writeRecord(out, positionRecord{
		Kind:       "position",
		StrategyID: p.StrategyID,
		Symbol:     p.Symbol,
		QtyBase:    pos.QtyBase.String(),
		EntryPrice: pos.EntryPrice.String(),
	})
}

// limitCrossed is the Phase-0 same-tick fill rule for resting limit entries.
func limitCrossed(side paper.Side, mark, limit decimal.Decimal) bool {
	if side == paper.SideBuy {
		return mark.LessThanOrEqual(limit)
	}
	return mark.GreaterThanOrEqual(limit)
}

// orderToRecord maps a paper order to its record line, substituting the
// deterministic order id for the OMS-internal random id.
func orderToRecord(ord *paper.Order, orderID, proposalID string) orderRecord {
	rec := orderRecord{
		Kind:       "order",
		OrderID:    orderID,
		ProposalID: proposalID,
		StrategyID: ord.StrategyID,
		Symbol:     ord.Symbol,
		Class:      string(ord.Class),
		Side:       string(ord.Side),
		Type:       ord.Type,
		ReduceOnly: ord.ReduceOnly,
		QtyBase:    ord.QtyBase.String(),
		Status:     string(ord.Status),
	}
	if ord.Type == "limit" {
		rec.LimitPrice = ord.LimitPrice.String()
	}
	if ord.Status == paper.StatusFilled {
		rec.FillPrice = ord.FillPrice.String()
	}
	return rec
}

// writeRecord appends one compact-JSON record line (LF-terminated; struct
// field order is the stable wire order).
func writeRecord(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
