package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
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

// startReplay subscribes a marketdata.ReplayFeed over the runspec marks —
// one tick per proposal line, TS = clock_start + index × tick_seconds — and
// returns the pump draining it into the Store and running the per-tick OMS
// trigger sweep. With no mark series or no proposals there is nothing to
// stream and the pump is empty.
func startReplay(ctx context.Context, spec *RunSpec, marks map[string][]decimal.Decimal, store *marketdata.Store, oms *paper.OMS, states map[string]*strategyState, out io.Writer, ticks int) (*tickPump, error) {
	pump := &tickPump{store: store, oms: oms, states: states, out: out}
	if len(marks) == 0 || ticks == 0 {
		return pump, nil
	}
	symbols := make([]string, 0, len(marks))
	for sym := range marks {
		symbols = append(symbols, sym)
	}
	feed, err := marketdata.NewReplayFeed(marks, spec.ClockStart.Time(), spec.TickSeconds, ticks)
	if err != nil {
		return nil, err
	}
	ch, err := feed.Subscribe(ctx, symbols)
	if err != nil {
		return nil, err
	}
	pump.ch = ch
	return pump, nil
}

// tickPump is the e2e Store writer: advance drains every ReplayFeed tick
// timestamped at or before now (the index-based loop clock) into the Store,
// so the Store always holds exactly the current tick's marks — never future
// ones — and staleness stays wall-clock-free. Exhausted series repeat their
// last element inside the feed; unknown symbols yield no tick, so their
// mark stays zero and market entries reject MARK_PRICE_UNAVAILABLE.
type tickPump struct {
	ch      <-chan marketdata.Tick
	store   *marketdata.Store
	oms     *paper.OMS
	states  map[string]*strategyState
	out     io.Writer
	pending *marketdata.Tick
}

// advance drains ticks into the Store. Every Store write MUST run the OMS
// stop/limit/TP trigger checks (market-data.md §Fill model v2): the marks
// written for one tick timestamp are collected and oms.ProcessTick runs once
// per tick, with the booked fills written as order records.
func (p *tickPump) advance(now time.Time) error {
	marks := make(map[string]decimal.Decimal)
	var tickTS time.Time
	for {
		if p.pending != nil {
			if p.pending.TS.After(now) {
				break
			}
			if len(marks) > 0 && !p.pending.TS.Equal(tickTS) {
				if err := p.processTick(tickTS, marks); err != nil {
					return err
				}
				marks = make(map[string]decimal.Decimal)
			}
			tickTS = p.pending.TS
			marks[p.pending.Symbol] = p.pending.Mark
			p.store.Put(*p.pending)
			p.pending = nil
			continue
		}
		if p.ch == nil {
			break
		}
		t, ok := <-p.ch
		if !ok {
			p.ch = nil
			break
		}
		p.pending = &t
	}
	if len(marks) == 0 {
		return nil
	}
	return p.processTick(tickTS, marks)
}

// processTick runs the OMS trigger sweep over one tick's fresh marks: queued
// exits, protective stops, take-profits, and resting entry limits fill here
// (ProcessTick's deterministic ordering). Each booked fill is written as an
// order record under a deterministic id derived from the tick timestamp and
// processing order, and the per-strategy open-position count is kept
// consistent: an entry fill increments it, a protective fill that leaves the
// book flat decrements it.
func (p *tickPump) processTick(ts time.Time, marks map[string]decimal.Decimal) error {
	fills, err := p.oms.ProcessTick(marks)
	if err != nil {
		return fmt.Errorf("process tick %s: %w", ts.UTC().Format(time.RFC3339), err)
	}
	for i, ord := range fills {
		if st, ok := p.states[ord.StrategyID]; ok {
			if ord.Class == paper.ClassEntry {
				st.openPositions++
			} else if _, open := p.oms.Position(ord.StrategyID, ord.Symbol); !open {
				st.openPositions--
			}
		}
		name := fmt.Sprintf("order/tick/%s/%d", ts.UTC().Format(time.RFC3339), i)
		if err := writeRecord(p.out, orderToRecord(&ord, DeterministicID(name), "")); err != nil {
			return err
		}
	}
	return nil
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

// executeOpen submits the (possibly clipped) entry under the fill model v2:
// market entries fill at mark ± directional slippage (taker fee); limit
// entries marketable at placement (buy: mark <= limit, sell: mark >= limit)
// fill immediately at the limit price inside SubmitEntry (taker fee);
// non-crossing limits rest open.
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
// With no usable mark the exit is QUEUED (market-data.md §Exits fail-closed):
// the order record stays open, the position and its stops stay intact, and
// the flatten retries via ProcessTick on the next fresh tick.
func executeClose(oms *paper.OMS, st *strategyState, p *contract.Proposal, mark decimal.Decimal, out io.Writer) error {
	ord, err := oms.Flatten(p.StrategyID, p.Symbol, mark)
	if err != nil {
		return fmt.Errorf("flatten %s: %w", p.ProposalID, err)
	}
	if err := writeRecord(out, orderToRecord(&ord, DeterministicID("order/"+p.ProposalID+"/close"), p.ProposalID)); err != nil {
		return err
	}
	if ord.Status != paper.StatusFilled {
		return nil
	}
	st.openPositions--
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

// orderToRecord maps a paper order to its record line, substituting the
// deterministic order id for the OMS-internal random id. Filled orders
// carry the fill price and the separately-recorded fee (fee-EXCLUSIVE:
// fees are never baked into the fill price, market-data.md).
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
		rec.FeeQuote = ord.FeeQuote.String()
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
