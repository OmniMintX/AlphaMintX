package backtest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/e2e"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// recorder tees each record line to records.jsonl and the backtest_records
// table with byte-identical payloads (one marshal, two sinks).
type recorder struct {
	out io.Writer
	db  *DB
	id  string
	seq int
}

func (r *recorder) write(kind string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := r.db.AppendRecord(r.id, r.seq, kind, b); err != nil {
		return err
	}
	r.seq++
	b = append(b, '\n')
	_, err = r.out.Write(b)
	return err
}

// state is the single-strategy runtime state under the virtual clock.
// Unlike the fixed e2e scenario states, equity/peak/daily track the OMS
// realized PnL (fees realized when paid), with the UTC day rollover of the
// runstate convention derived from virtual time only.
type state struct {
	equity, peak, dailyPnL decimal.Decimal
	openPositions          int
	lastRealized           decimal.Decimal
	day                    string
}

// rollover zeroes the daily realized PnL when virtual now crosses 00:00 UTC.
func (st *state) rollover(now time.Time) {
	if d := now.UTC().Format("2006-01-02"); d != st.day {
		st.day = d
		st.dailyPnL = decimal.Zero
	}
}

// applyRealized folds the OMS cumulative realized PnL delta into daily PnL,
// equity, and peak as of the fill's virtual timestamp.
func (st *state) applyRealized(oms *paper.OMS, strategyID, symbol string, at time.Time) {
	st.rollover(at)
	cum := oms.RealizedPnL(strategyID, symbol)
	delta := cum.Sub(st.lastRealized)
	if delta.IsZero() {
		return
	}
	st.lastRealized = cum
	st.dailyPnL = st.dailyPnL.Add(delta)
	st.equity = st.equity.Add(delta)
	if st.equity.GreaterThan(st.peak) {
		st.peak = st.equity
	}
}

// replay is the candle loop: per grid tick, pump the candle's four sub-ticks
// (Store write + full trigger sweep each, e2e discipline), then evaluate the
// tick's proposal at close_time + 1s with the mark resolved from the Store
// under max_age_seconds. Gapped grid slots pump nothing: the stale mark
// fails closed at the decision (MARK_PRICE_UNAVAILABLE for market opens).
func replay(spec *RunSpec, ds *Dataset, lines []ProposalLine, rec *recorder) error {
	gate := riskgate.NewService()
	oms, err := paper.New(spec.FillModel)
	if err != nil {
		return fmt.Errorf("runspec fill_model: %w", err)
	}
	store, err := marketdata.NewStore(time.Duration(spec.MaxAgeSeconds) * time.Second)
	if err != nil {
		return fmt.Errorf("runspec max_age_seconds: %w", err)
	}
	allocated := spec.ParsedLimits.AllocatedCapitalQuote
	st := &state{
		equity: allocated, peak: allocated, dailyPnL: decimal.Zero,
		day: time.UnixMilli(ds.FirstOpenTime()).UTC().Format("2006-01-02"),
	}

	ivlMS := ds.IntervalMS()
	klineIdx := 0
	for tick := 0; tick < ds.Ticks(); tick++ {
		gridOpen := ds.FirstOpenTime() + int64(tick)*ivlMS
		if klineIdx < len(ds.Klines) && ds.Klines[klineIdx].OpenTime == gridOpen {
			if err := pumpCandle(spec, oms, store, st, rec, tick, ds.Klines[klineIdx], ivlMS); err != nil {
				return err
			}
			klineIdx++
		}
		now := time.UnixMilli(gridOpen + ivlMS).UTC().Add(time.Second)
		st.rollover(now)
		p := &lines[tick].Proposal
		// Stale or missing mark => Mark returns zero: the gate's zero-price
		// guard rejects market-entry opens MARK_PRICE_UNAVAILABLE
		// (fail-closed; the Store never leaks a stale price).
		mark, _, _ := store.Mark(p.Symbol, now)
		if err := evaluateOne(gate, oms, spec.ParsedLimits, st, p, mark, now, rec); err != nil {
			return err
		}
	}
	return nil
}

// pumpCandle drains one candle's four sub-ticks into the Store, running the
// full OMS trigger sweep after EVERY write (market-data.md §Fill model v2).
// Booked fills are recorded under ids deterministic in (tick, leg, index) —
// the leg disambiguates the boundary collision close_time(t) == open_time(t+1).
func pumpCandle(spec *RunSpec, oms *paper.OMS, store *marketdata.Store, st *state, rec *recorder, tick int, k Kline, ivlMS int64) error {
	subs, err := SubTicks(spec.Symbol, k, ivlMS)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		store.Put(sub.Tick)
		fills, err := oms.ProcessTick(map[string]decimal.Decimal{spec.Symbol: sub.Tick.Mark})
		if err != nil {
			return fmt.Errorf("process sub-tick %d/%s: %w", tick, sub.Leg, err)
		}
		for i, ord := range fills {
			if ord.Class == paper.ClassEntry {
				st.openPositions++
			} else if _, open := oms.Position(ord.StrategyID, ord.Symbol); !open {
				st.openPositions--
			}
			st.applyRealized(oms, ord.StrategyID, ord.Symbol, sub.Tick.TS)
			name := fmt.Sprintf("order/tick/%d/%s/%d", tick, sub.Leg, i)
			if err := rec.write("order", e2e.OrderToRecord(&ord, DeterministicID(name), "")); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluateOne gates one proposal and routes approved/clipped executions to
// the paper OMS, writing records in the stable order proposal, verdict,
// order(s), position(s) — the e2e evaluateOne discipline. escalate verdicts
// are recorded as-is with NO execution (backtest v1 has no operator).
func evaluateOne(gate *riskgate.Service, oms *paper.OMS, limits riskgate.RiskLimits, st *state, p *contract.Proposal, mark decimal.Decimal, now time.Time, rec *recorder) error {
	rtState := riskgate.RuntimeState{
		Autonomy:              riskgate.AutonomyL3,
		EquityQuote:           st.equity,
		PeakEquityQuote:       st.peak,
		DailyRealizedPnLQuote: st.dailyPnL,
		OpenPositionsCount:    st.openPositions,
		MarkPrice:             mark,
	}
	verdict, err := gate.Evaluate(p, limits, rtState, now)
	if err != nil {
		return fmt.Errorf("gate %s: %w", p.ProposalID, err)
	}
	verdict.VerdictID = DeterministicID("verdict/" + p.ProposalID)
	if err := rec.write("proposal", e2e.ProposalRecord{Kind: "proposal", Proposal: p}); err != nil {
		return err
	}
	if err := rec.write("verdict", e2e.VerdictRecord{Kind: "verdict", Verdict: &verdict}); err != nil {
		return err
	}
	approved := verdict.Decision == contract.DecisionApprove || verdict.Decision == contract.DecisionClip
	switch {
	case approved && p.Action.IsOpen():
		return executeOpen(oms, st, p, &verdict, mark, now, rec)
	case verdict.Decision == contract.DecisionApprove && p.Action == contract.ActionClose:
		return executeClose(oms, st, p, mark, now, rec)
	}
	return nil
}
