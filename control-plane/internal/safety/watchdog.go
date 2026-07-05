package safety

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// watchdogPass is the per-tick watchdog evaluation (docs/specs/watchdog.md
// §Placement and cadence): the watch set is STRICTLY the live_* subset of
// the Monitor's candidate scan (WD-10 — paper/paused/killed silence is not
// a live-money event); leaving the set removes a strategy from evaluation
// with no cleanup — evaluation keys off the CURRENT watch set, and the
// in-memory maps may retain stale entries harmlessly (WD-11).
func (m *Monitor) watchdogPass(ctx context.Context, cands []candidate, now time.Time, reconciled bool) {
	if m.watchdogOff {
		return
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.row.LifecycleState, "live_") {
			continue
		}
		m.watchOne(ctx, c, now, reconciled)
	}
}

// watchOne runs the evaluation ladder for ONE watched strategy: the WD-16
// standing-kill skip BEFORE any rung, then age against the WD-8/WD-9
// baseline — quiet (WD-15), rung 2 (WD-19: pure 10 min, never recon-gated,
// or the recon-gated unprotected-exposure fast path), or rung 1 (WD-17).
func (m *Monitor) watchOne(ctx context.Context, c candidate, now time.Time, reconciled bool) {
	sid := c.row.StrategyID
	lastSeen, seen := m.observe(sid, now)
	epoch, err := m.st.GlobalMaxKillEpoch(sid)
	if err != nil {
		m.logf("safety: watchdog: GlobalMaxKillEpoch(%s): %v", sid, err)
		return
	}
	if epoch > 0 {
		// A standing kill (any tier) owns the sweep and the lock: never a
		// second kill row (invariant 4); repair a crash-lost escalation
		// alert while here (WD-16 back-fill).
		m.backfillEscalationAlert(sid, now)
		return
	}
	age, fromBeat := m.silenceAge(sid, lastSeen, seen, now)
	if age <= WatchdogSilenceThreshold {
		return // WD-15 quiet: no action, no "recovered" bookkeeping
	}
	switch {
	case age > WatchdogEscalationThreshold:
		m.escalate(ctx, sid, "silence_10m", lastSeen, fromBeat, age, now)
	case reconciled && m.unprotectedExposure(c, now):
		m.escalate(ctx, sid, "unprotected_exposure", lastSeen, fromBeat, age, now)
	default:
		m.rungOneSilence(ctx, sid, lastSeen, fromBeat, age, now, reconciled)
	}
}

// observe stamps firstWatched on watch-set entry (WD-11) and returns the
// strategy's lastSeen beat (seen=false when none since start).
func (m *Monitor) observe(strategyID string, now time.Time) (time.Time, bool) {
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	if _, ok := m.firstWatched[strategyID]; !ok {
		m.firstWatched[strategyID] = now
	}
	last, seen := m.lastSeen[strategyID]
	return last, seen
}

// silenceAge is now − max(lastSeen, baseline) with baseline =
// max(monitor start, firstWatched) (WD-9: the accepted, documented
// restart liveness gap — a fresh, bounded window). fromBeat reports
// whether the beat set the base — false means the baseline applies and
// the WD-21 details carry an empty last_seen.
func (m *Monitor) silenceAge(strategyID string, lastSeen time.Time, seen bool, now time.Time) (age time.Duration, fromBeat bool) {
	m.hbMu.Lock()
	baseline := m.firstWatched[strategyID]
	m.hbMu.Unlock()
	if m.startedAt.After(baseline) {
		baseline = m.startedAt
	}
	if seen && lastSeen.After(baseline) {
		return now.Sub(lastSeen), true
	}
	return now.Sub(baseline), false
}

// rungOneSilence is WD-17: derive-from-state, alert FIRST (deduped once
// per strategy per UTC day; an append failure fails the tick closed — the
// breaker's fire precedent), then the ENTRY-cancel sweep, recon-gated
// (WD-14) and repeated undeduped every tick while silence persists —
// idempotent by the sweep's own semantics, self-healing with no persisted
// checkpoint.
func (m *Monitor) rungOneSilence(ctx context.Context, strategyID string, lastSeen time.Time, seen bool, age time.Duration, now time.Time, reconciled bool) {
	m.logf("safety: WATCHDOG silence for strategy %s (%ds)",
		strategyID, int64(age/time.Second))
	dup, err := m.st.HasSafetyAlertToday("watchdog_silence", strategyID, "silence", utcDate(now))
	if err != nil {
		m.logf("safety: watchdog: alert dedupe read (watchdog_silence): %v", err)
		return // fail closed for the tick; silence is durably re-observable
	}
	if !dup {
		details := watchdogDetails("silence", lastSeen, seen, age)
		if m.appendAlert("watchdog_silence", strategyID, "silence", details, now) != nil {
			return // alert before effect (WD-17): no sweep this tick
		}
	}
	if !reconciled {
		return // WD-14: the sweep reads unverified local state pre-reconcile
	}
	m.logf("safety: WATCHDOG ENTRY sweep engaged for strategy %s", strategyID)
	if err := m.entries.CancelOpenEntries(ctx, strategyID); err != nil {
		// Invariant-17 error isolation: log, continue; the next tick's
		// re-detection re-runs the sweep.
		m.logf("safety: watchdog: entry sweep for %s: %v (next tick retries)", strategyID, err)
	}
}

// escalate is WD-19 rung 2: the strategy-tier kill, persist-then-execute —
// AppendStrategyKill (actor 'watchdog', flatten=false: the watchdog reacts
// to ABSENCE of information; stops remain), the watchdog_kill_escalation
// alert (ref_id = the kill event_id; a crash before it is repaired by the
// WD-16 back-fill), then DriveSafetyEffects asynchronously in a
// panic-recovered goroutine — the breaker fire pattern. Every downstream
// semantic is safety-wiring.md machinery, reused not respecified.
func (m *Monitor) escalate(ctx context.Context, strategyID, cause string, lastSeen time.Time, seen bool, age time.Duration, now time.Time) {
	eventID := uuid.NewString()
	epoch, err := m.st.AppendStrategyKill(eventID, strategyID, "watchdog", formatTime(now), false)
	if err != nil {
		m.logf("safety: watchdog: kill append for %s: %v (next tick retries)", strategyID, err)
		return
	}
	m.logf("safety: WATCHDOG KILL escalation for strategy %s: cause %s, silence %ds (event %s, epoch %d)",
		strategyID, cause, int64(age/time.Second), eventID, epoch)
	m.appendAlert("watchdog_kill_escalation", strategyID, eventID,
		watchdogDetails(cause, lastSeen, seen, age), now)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				m.logf("safety: watchdog: drive panic: %v", p)
			}
		}()
		if err := m.driver.DriveSafetyEffects(ctx); err != nil {
			m.logf("safety: watchdog: drive: %v", err)
		}
	}()
}

// backfillEscalationAlert is the WD-16 crash repair: when the newest
// strategy-scope kill row carries actor_id='watchdog' and no
// watchdog_kill_escalation alert references its event_id, the skip path
// appends the missing alert (idempotent late append). Its details_json is
// {"cause":"backfill"} EXACTLY — the original cause and silence figures
// did not survive the crash and MUST NOT be fabricated (WD-21).
func (m *Monitor) backfillEscalationAlert(strategyID string, now time.Time) {
	eventID, actorID, ok, err := m.st.LatestStrategyKillEvent(strategyID)
	if err != nil {
		m.logf("safety: watchdog: LatestStrategyKillEvent(%s): %v", strategyID, err)
		return
	}
	if !ok || actorID != "watchdog" {
		return
	}
	dup, err := m.st.HasSafetyAlert("watchdog_kill_escalation", strategyID, eventID)
	if err != nil {
		m.logf("safety: watchdog: alert dedupe read (watchdog_kill_escalation): %v", err)
		return
	}
	if dup {
		return
	}
	m.appendAlert("watchdog_kill_escalation", strategyID, eventID, `{"cause":"backfill"}`, now)
}

// unprotectedExposure is the WD-20 computable predicate: TRUE iff some
// open position has no non-terminal PROTECTIVE-class order for
// (strategy, symbol) — Class alone, type- and origin-agnostic — after the
// dust carve-out EXCLUDES positions the OMS itself could never protect:
// |qty| below the venue minQty, notional at a FRESH mark below
// minNotional, no fresh mark to price the test, or a Filters miss — each
// fails toward PROTECTED. The fast path is only an ACCELERATOR; the
// unconditional 10-minute rung backstops whatever the carve-out misjudges.
func (m *Monitor) unprotectedExposure(c candidate, now time.Time) bool {
	if len(c.open) == 0 {
		return false
	}
	live, err := m.st.ListNonTerminalLiveOrders()
	if err != nil {
		m.logf("safety: watchdog: list non-terminal orders: %v", err)
		return false // fail toward PROTECTED
	}
	for _, p := range c.open {
		qty, err := decimal.NewFromString(p.QtyBase)
		if err != nil {
			m.logf("safety: watchdog: positions.qty_base %q: %v", p.QtyBase, err)
			continue
		}
		abs := qty.Abs()
		minQty, minNotional, ok := m.filters.MinFilters(p.Symbol)
		if !ok {
			continue // filters unloaded/expired/unconfigured: EXCLUDE
		}
		if abs.LessThan(minQty) {
			continue // dust below minQty: no protective is possible
		}
		mark, _, fresh := m.marks.Mark(p.Symbol, now)
		if !fresh {
			continue // no fresh mark to price the notional test: EXCLUDE
		}
		if abs.Mul(mark).LessThan(minNotional) {
			continue // dust below minNotional at the fresh mark
		}
		if !hasProtective(live, c.row.StrategyID, p.Symbol) {
			return true
		}
	}
	return false
}

// hasProtective reports a non-terminal PROTECTIVE-class order for
// (strategy, symbol): resting stop or in-flight reduce-only close alike —
// findLiveProtective's Class scan WITHOUT its type filter (WD-20).
func hasProtective(live []store.LiveOrder, strategyID, symbol string) bool {
	for i := range live {
		o := &live[i]
		if o.StrategyID == strategyID && o.Symbol == symbol && o.Class == "PROTECTIVE" {
			return true
		}
	}
	return false
}

// watchdogDetails renders the WD-21 details_json {cause, last_seen,
// silence_seconds}; last_seen is empty when the baseline applies.
func watchdogDetails(cause string, lastSeen time.Time, seen bool, age time.Duration) string {
	last := ""
	if seen {
		last = formatTime(lastSeen)
	}
	b, _ := json.Marshal(map[string]any{
		"cause": cause, "last_seen": last, "silence_seconds": int64(age / time.Second),
	})
	return string(b)
}
