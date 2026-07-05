package safety

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// candidate is one monitored strategy plus its open (nonzero-qty)
// positions.
type candidate struct {
	row  store.Strategy
	open []store.Position
}

// safeTick runs one tick with panic isolation (invariant 14): a panic is
// recovered, logged, and recorded as a monitor_tick_panic alert; the
// monitor continues on the next tick at the ACTIVE cadence.
func (m *Monitor) safeTick(ctx context.Context) (next time.Duration) {
	next = m.active
	defer func() {
		if p := recover(); p != nil {
			m.logf("safety: monitor tick panic: %v", p)
			m.appendAlert("monitor_tick_panic", "", "",
				fmt.Sprintf(`{"panic":%q}`, fmt.Sprint(p)), m.now())
		}
	}()
	return m.tick(ctx)
}

// tick is one evaluation pass (spec §Evaluation loop). The stall scan runs
// on EVERY tick — pre-reconcile skipped ticks included — so a drive
// suppressed by resetPending or a never-completing reconcile still alerts
// LOUDLY; the returned duration selects the next tick's cadence. The
// watchdog pass ALSO runs on every tick, pre-reconcile included
// (watchdog.md WD-12: a pass placed only after the breaker evaluation
// would be silently recon-gated in full); only its recon-gated actions —
// the rung-1 ENTRY sweep and the unprotected-exposure fast path — are
// skipped while Reconciled() is false.
func (m *Monitor) tick(ctx context.Context) time.Duration {
	now := m.now()
	defer m.stallScan(now)
	cands, err := m.monitored()
	if err != nil {
		m.logf("safety: monitor: monitored set: %v", err)
		return m.active
	}
	reconciled := m.recon.Reconciled()
	if !reconciled {
		// Step 1 — reconcile gate: local positions and PnL are unverified
		// before the first completed startup reconcile. Skip the breaker
		// evaluation and alert per strategy with the same daily dedupe as
		// step 4 (ref_id = cause).
		for _, c := range cands {
			m.alertDaily("breaker_mark_stale", c.row.StrategyID, "not_reconciled",
				`{"cause":"not_reconciled"}`, now)
		}
	} else {
		for _, c := range cands {
			m.evaluate(ctx, c, now)
		}
	}
	m.watchdogPass(ctx, cands, now, reconciled)
	return m.interval(cands)
}

// monitored resolves the monitored set (spec §Monitored set): every
// strategy in a live_* lifecycle state, plus any strategy holding a
// nonzero live position regardless of state (paused/killed books with
// residual exposure stay monitored).
func (m *Monitor) monitored() ([]candidate, error) {
	var out []candidate
	for page := 1; ; page++ {
		rows, total, err := m.st.ListStrategies(page, store.MaxPageLimit)
		if err != nil {
			return nil, err
		}
		for _, s := range rows {
			positions, err := m.st.ListPositions(s.StrategyID)
			if err != nil {
				return nil, err
			}
			var open []store.Position
			for _, p := range positions {
				qty, err := decimal.NewFromString(p.QtyBase)
				if err != nil {
					return nil, fmt.Errorf("safety: positions.qty_base %q: %w", p.QtyBase, err)
				}
				if !qty.IsZero() {
					open = append(open, p)
				}
			}
			if len(open) > 0 || strings.HasPrefix(s.LifecycleState, "live_") {
				out = append(out, candidate{row: s, open: open})
			}
		}
		if page*store.MaxPageLimit >= total || len(rows) == 0 {
			return out, nil
		}
	}
}

// interval selects the next tick's cadence (spec §Cadence): ACTIVE while
// ANY monitored strategy has a nonzero position or a non-terminal live
// order, IDLE when all are flat and quiet. Errors fail toward the ACTIVE
// (more watchful) cadence.
func (m *Monitor) interval(cands []candidate) time.Duration {
	ids := make(map[string]bool, len(cands))
	for _, c := range cands {
		if len(c.open) > 0 {
			return m.active
		}
		ids[c.row.StrategyID] = true
	}
	if len(ids) == 0 {
		return m.idle
	}
	live, err := m.st.ListNonTerminalLiveOrders()
	if err != nil {
		m.logf("safety: monitor: list non-terminal orders: %v", err)
		return m.active
	}
	for _, o := range live {
		if ids[o.StrategyID] {
			return m.active
		}
	}
	return m.idle
}

// evaluate runs per-tick steps 2-6 for one monitored strategy.
func (m *Monitor) evaluate(ctx context.Context, c candidate, now time.Time) {
	sid := c.row.StrategyID
	// Step 2 — the dedupe AND the latch (invariant 7): at most one
	// breaker row per strategy per UTC day, auto-re-armed at 00:00 UTC.
	active, err := m.st.BreakerActiveToday(sid, utcDate(now))
	if err != nil {
		m.logf("safety: monitor: BreakerActiveToday(%s): %v", sid, err)
		return
	}
	if active {
		return
	}
	// Step 3 — limit guard: daily_loss_limit_quote unset, zero, or
	// negative means the monitor NEVER fires for the strategy; a zero
	// limit must not instantly kill a misconfigured book — fail loud
	// instead, once per strategy per UTC day.
	limit := m.limits.Limits(sid).DailyLossLimitQuote
	if limit.Sign() <= 0 {
		m.alertDaily("breaker_limit_unset", sid, "",
			fmt.Sprintf(`{"daily_loss_limit_quote":%q}`, limit.String()), now)
		return
	}
	m.evaluatePnL(ctx, c, limit, now)
}

// evaluatePnL runs steps 4-6: the mark-freshness check against the mark
// source, the PnL fold, the freshness RE-VERIFY (the TOCTOU same-snapshot
// rule: a mark cannot expire between check and fold and silently
// contribute zero), the trigger comparison, and the fire. Stale marks and
// PnL errors are fail-open for firing but LOUD (invariant 8): no fire, no
// all-clear, a breaker_mark_stale alert with the cause.
func (m *Monitor) evaluatePnL(ctx context.Context, c candidate, limit decimal.Decimal, now time.Time) {
	sid := c.row.StrategyID
	stale, err := m.staleSymbol(sid, now)
	if err != nil {
		m.alertPnLError(sid, err, now)
		return
	}
	if stale != "" {
		m.alertStaleMark(sid, stale, now)
		return
	}
	pnl, err := m.pnl.DailyPnL(sid, now)
	if err != nil {
		m.alertPnLError(sid, err, now)
		return
	}
	if stale, err = m.staleSymbol(sid, now); err != nil {
		m.alertPnLError(sid, err, now)
		return
	} else if stale != "" {
		m.alertStaleMark(sid, stale, now)
		return
	}
	// Step 5 — trigger: the identical decimal predicate the approval
	// preflight uses (DailyPnL <= -daily_loss_limit_quote).
	if pnl.GreaterThan(limit.Neg()) {
		return
	}
	m.fire(ctx, c.row, pnl, limit, now)
}

// staleSymbol returns the first open-position symbol lacking a FRESH mark
// at now ("" when every open position is freshly marked), re-reading the
// positions so a book that changed mid-tick is still covered.
func (m *Monitor) staleSymbol(strategyID string, now time.Time) (string, error) {
	positions, err := m.st.ListPositions(strategyID)
	if err != nil {
		return "", err
	}
	for _, p := range positions {
		qty, err := decimal.NewFromString(p.QtyBase)
		if err != nil {
			return "", fmt.Errorf("safety: positions.qty_base %q: %w", p.QtyBase, err)
		}
		if qty.IsZero() {
			continue
		}
		if _, _, ok := m.marks.Mark(p.Symbol, now); !ok {
			return p.Symbol, nil
		}
	}
	return "", nil
}
