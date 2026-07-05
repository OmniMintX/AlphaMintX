package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// fire is step 6 (persisted-then-executed, invariant 1): append the
// breaker row FIRST — kind='breaker', scope='strategy', kill_epoch NULL (a
// breaker is not a kill and never bumps the epoch), flatten=1 (the breaker
// ALWAYS flattens), trigger_ref = the monitor sample as JSON — THEN invoke
// DriveSafetyEffects asynchronously so effects never block the loop.
func (m *Monitor) fire(ctx context.Context, row store.Strategy, pnl, limit decimal.Decimal, now time.Time) {
	trigger, err := json.Marshal(map[string]string{
		"daily_pnl":    pnl.String(),
		"limit":        limit.String(),
		"evaluated_at": formatTime(now),
	})
	if err != nil {
		m.logf("safety: monitor: trigger_ref for %s: %v", row.StrategyID, err)
		return
	}
	strategyID, tenantID, triggerRef := row.StrategyID, row.TenantID, string(trigger)
	flatten := true
	ev := store.KillBreakerEvent{
		EventID:    uuid.NewString(),
		Kind:       "breaker",
		Scope:      "strategy",
		StrategyID: &strategyID,
		TenantID:   &tenantID,
		Flatten:    &flatten,
		TriggerRef: &triggerRef,
		ActorID:    "breaker-monitor",
		RecordedAt: formatTime(now),
	}
	if err := m.st.AppendKillBreakerEvent(ev); err != nil {
		m.logf("safety: monitor: append breaker row for %s: %v", strategyID, err)
		return
	}
	m.logf("safety: BREAKER fired for strategy %s: daily_pnl %s <= -%s (event %s)",
		strategyID, pnl.String(), limit.String(), ev.EventID)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				m.logf("safety: monitor: drive panic: %v", p)
			}
		}()
		if err := m.driver.DriveSafetyEffects(ctx); err != nil {
			m.logf("safety: monitor: drive: %v", err)
		}
	}()
}

// stallScan is §Safety-effects driver step 5: any unserved event row older
// than the stall threshold appends a safety_effect_stalled alert (ref_id =
// event_id, strategy_id NULL, at most once per event per UTC day) plus an
// operator log line, and the drives KEEP retrying.
func (m *Monitor) stallScan(now time.Time) {
	events, err := m.st.ListUnservedSafetyEvents()
	if err != nil {
		m.logf("safety: monitor: stall scan: %v", err)
		return
	}
	for _, ev := range events {
		recorded, err := time.Parse(time.RFC3339, ev.RecordedAt)
		if err != nil {
			m.logf("safety: monitor: stall scan: event %s recorded_at %q: %v",
				ev.EventID, ev.RecordedAt, err)
			continue
		}
		age := now.Sub(recorded)
		if age <= m.stall {
			continue
		}
		m.alertDaily("safety_effect_stalled", "", ev.EventID,
			fmt.Sprintf(`{"kind":%q,"age_seconds":%d}`, ev.Kind, int64(age/time.Second)), now)
	}
}

// alertStaleMark appends the step-4 stale-mark alert (cause encoded as
// ref_id so the daily dedupe keys on (kind, cause, strategy, UTC day)).
func (m *Monitor) alertStaleMark(strategyID, symbol string, now time.Time) {
	m.alertDaily("breaker_mark_stale", strategyID, "stale_mark",
		fmt.Sprintf(`{"cause":"stale_mark","symbol":%q}`, symbol), now)
}

// alertPnLError appends the step-4 pnl_error alert.
func (m *Monitor) alertPnLError(strategyID string, err error, now time.Time) {
	m.logf("safety: monitor: DailyPnL(%s): %v", strategyID, err)
	m.alertDaily("breaker_mark_stale", strategyID, "pnl_error",
		fmt.Sprintf(`{"cause":"pnl_error","error":%q}`, err.Error()), now)
}

// alertDaily appends one safety_alerts row deduped per
// (kind, strategy_id, ref_id, utcDate) — HasSafetyAlertToday's
// empty-matches-NULL rule — and logs a line on EVERY occurrence (spec
// §Alerts registry).
func (m *Monitor) alertDaily(kind, strategyID, refID, detailsJSON string, now time.Time) {
	m.logf("safety: ALERT %s strategy=%q ref=%q %s", kind, strategyID, refID, detailsJSON)
	dup, err := m.st.HasSafetyAlertToday(kind, strategyID, refID, utcDate(now))
	if err != nil {
		m.logf("safety: monitor: alert dedupe read (%s): %v", kind, err)
		return
	}
	if dup {
		return
	}
	m.appendAlert(kind, strategyID, refID, detailsJSON, now)
}

// appendAlert appends one safety_alerts row; an empty strategyID/refID
// persists NULL. The error return (already logged) lets the watchdog's
// alert-before-effect rule fail closed for the tick (WD-17); other
// callers ignore it.
func (m *Monitor) appendAlert(kind, strategyID, refID, detailsJSON string, now time.Time) error {
	a := store.SafetyAlert{
		AlertID: uuid.NewString(), Kind: kind,
		DetailsJSON: detailsJSON, RecordedAt: formatTime(now),
	}
	if strategyID != "" {
		a.StrategyID = &strategyID
	}
	if refID != "" {
		a.RefID = &refID
	}
	if err := m.st.AppendSafetyAlert(a); err != nil {
		m.logf("safety: monitor: append alert (%s): %v", kind, err)
		return err
	}
	return nil
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// utcDate is the YYYY-MM-DD UTC day of t (00:00 UTC boundary).
func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
