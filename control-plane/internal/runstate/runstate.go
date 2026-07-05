// Package runstate hydrates the Risk Gate's RuntimeState from the
// control-plane store and the live mark cache (docs/specs/risk-limits.md
// Definitions): equity = realized equity snapshot + unrealized PnL at the
// current mark, peak is monotone, and the daily figure folds unrealized in
// so an underwater book counts against daily_loss_limit_quote. Fail-closed
// throughout: a stale or missing mark contributes nothing and yields a zero
// MarkPrice (the gate then rejects MARK_PRICE_UNAVAILABLE).
package runstate

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// MarkSource is the freshness-checked last-tick cache;
// *marketdata.Store satisfies it.
type MarkSource interface {
	Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool)
}

// Hydrator builds RuntimeState snapshots for gate evaluation.
type Hydrator struct {
	Store *store.Store
	Marks MarkSource
	// AllocatedCapitalQuote seeds equity/peak for strategies with no
	// strategy_state row yet (equity = allocated capital + cumulative PnL).
	AllocatedCapitalQuote decimal.Decimal
}

// AutonomyFor maps a lifecycle state to its effective autonomy
// (strategy-lifecycle.md): L0 is the floor — paper, draft, paused, and
// killed strategies never auto-execute above it.
func AutonomyFor(lifecycleState string) riskgate.Autonomy {
	switch lifecycleState {
	case "live_l1":
		return riskgate.AutonomyL1
	case "live_l2":
		return riskgate.AutonomyL2
	case "live_l3":
		return riskgate.AutonomyL3
	default:
		return riskgate.AutonomyL0
	}
}

// State hydrates the RuntimeState for one proposal evaluation at now.
// symbol is the proposal's symbol; its mark is zero unless fresh.
func (h *Hydrator) State(strategyID, lifecycleState, symbol string, now time.Time) (riskgate.RuntimeState, error) {
	state := riskgate.RuntimeState{
		Autonomy:              AutonomyFor(lifecycleState),
		EquityQuote:           decimal.Zero,
		PeakEquityQuote:       decimal.Zero,
		DailyRealizedPnLQuote: decimal.Zero,
		MarkPrice:             decimal.Zero,
	}

	// KillEpoch STAYS the raw epoch (lifecycle-api.md LC-34, staleness
	// ordering): a clear never un-stales an intent stamped before the kill.
	epoch, err := h.Store.GlobalMaxKillEpoch(strategyID)
	if err != nil {
		return state, err
	}
	state.KillEpoch = epoch
	// The standing-condition blocker moves to the ActiveKill predicate
	// (LC-34): a kill is a standing condition until its scope's SW-2
	// clear; a killed lifecycle state keeps the gate shut regardless.
	active, err := h.Store.ActiveKill(strategyID)
	if err != nil {
		return state, err
	}
	state.KillActive = active || lifecycleState == "killed"

	// One consistent snapshot: positions, open orders, and strategy_state
	// are read in a single transaction so a concurrent sweep never tears
	// the equity math.
	snap, err := h.Store.StrategySnapshot(strategyID)
	if err != nil {
		return state, err
	}
	unrealized, err := h.foldPositions(&state, snap.Positions, now)
	if err != nil {
		return state, err
	}

	for _, o := range snap.OpenOrders {
		if o.Class == "ENTRY" {
			state.PendingEntryOrdersCount++
		}
	}

	equity, peak, daily := h.AllocatedCapitalQuote, h.AllocatedCapitalQuote, decimal.Zero
	if snap.HasState {
		row := snap.State
		if equity, err = parseDec("strategy_state.equity_quote", row.EquityQuote); err != nil {
			return state, err
		}
		if peak, err = parseDec("strategy_state.peak_equity_quote", row.PeakEquityQuote); err != nil {
			return state, err
		}
		// Daily rollover: a row from an earlier UTC day contributes zero
		// realized PnL to today.
		if row.UTCDate == utcDate(now) {
			if daily, err = parseDec("strategy_state.daily_realized_pnl_quote", row.DailyRealizedPnLQuote); err != nil {
				return state, err
			}
		}
	}
	state.EquityQuote = equity.Add(unrealized)
	state.PeakEquityQuote = decimal.Max(peak, state.EquityQuote)
	state.DailyRealizedPnLQuote = daily.Add(unrealized)

	n, err := h.Store.CountRateVerdictsSince(strategyID, formatTime(now.Add(-time.Minute)))
	if err != nil {
		return state, err
	}
	state.EntryOrdersInLastMinute = n

	if mark, _, ok := h.Marks.Mark(symbol, now); ok {
		state.MarkPrice = mark
	}
	return state, nil
}

// foldPositions counts open positions and sums their unrealized PnL at the
// current fresh marks. A position whose mark is stale or missing counts
// toward the position slots but contributes zero unrealized PnL (the Store
// never leaks a stale price).
func (h *Hydrator) foldPositions(state *riskgate.RuntimeState, positions []store.Position, now time.Time) (decimal.Decimal, error) {
	unrealized := decimal.Zero
	for _, p := range positions {
		qty, err := parseDec("positions.qty_base", p.QtyBase)
		if err != nil {
			return decimal.Zero, err
		}
		if qty.IsZero() {
			continue
		}
		state.OpenPositionsCount++
		mark, _, ok := h.Marks.Mark(p.Symbol, now)
		if !ok {
			continue
		}
		entry, err := parseDec("positions.entry_price", p.EntryPrice)
		if err != nil {
			return decimal.Zero, err
		}
		unrealized = unrealized.Add(mark.Sub(entry).Mul(qty))
	}
	return unrealized, nil
}

// DailyPnL is the folded daily figure (realized after rollover +
// unrealized at fresh marks) backing the approval preflight's daily-loss
// check; the limit is breached when it is at or below the negated
// daily_loss_limit_quote. Positions and strategy_state come from one
// consistent snapshot.
func (h *Hydrator) DailyPnL(strategyID string, now time.Time) (decimal.Decimal, error) {
	snap, err := h.Store.StrategySnapshot(strategyID)
	if err != nil {
		return decimal.Zero, err
	}
	var scratch riskgate.RuntimeState
	unrealized, err := h.foldPositions(&scratch, snap.Positions, now)
	if err != nil {
		return decimal.Zero, err
	}
	daily := decimal.Zero
	if snap.HasState && snap.State.UTCDate == utcDate(now) {
		if daily, err = parseDec("strategy_state.daily_realized_pnl_quote", snap.State.DailyRealizedPnLQuote); err != nil {
			return decimal.Zero, err
		}
	}
	return daily.Add(unrealized), nil
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// utcDate is the YYYY-MM-DD UTC day of t (00:00 UTC boundary).
func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func parseDec(field, v string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("runstate: %s %q: %w", field, v, err)
	}
	return d, nil
}
