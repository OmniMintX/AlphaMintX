package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Safety-wiring persistence surface (docs/specs/safety-wiring.md
// §Store-surface amendment): the strategy/platform kill appenders (epoch
// MAX+1 in tx, same pattern as AppendTenantKill), the served-effect marker,
// the alert journal, the driver's lifecycle lock, and the safety-engine
// reads. safety_effects and safety_alerts are append-only (invariant 13).

// AppendStrategyKill persists a strategy-scope kill event: ONE transaction
// resolves the strategy's tenant_id (recorded for audit; the kill predicate
// matches strategy rows on strategy_id alone), computes
// kill_epoch = MAX(kill_epoch) over the WHOLE table + 1, and inserts the
// row. ErrNotFound for an unknown strategy (defense in depth behind the
// handler's 404). Returns the recorded epoch.
func (s *Store) AppendStrategyKill(eventID, strategyID, actorID, recordedAt string, flatten bool) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	var tenantID string
	err = tx.QueryRow(`SELECT tenant_id FROM strategies WHERE strategy_id = ?`, strategyID).Scan(&tenantID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
	}
	if err != nil {
		return 0, err
	}
	var epoch int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(kill_epoch), 0) + 1 FROM kill_breaker_events`).
		Scan(&epoch); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO kill_breaker_events
		(event_id, kind, scope, strategy_id, tenant_id, kill_epoch, flatten, trigger_ref, actor_id, recorded_at)
		VALUES (?, 'kill', 'strategy', ?, ?, ?, ?, NULL, ?, ?)`,
		eventID, strategyID, tenantID, epoch, flatten, actorID, recordedAt); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

// AppendPlatformKill persists a platform-scope kill event: both scope ids
// NULL — exactly the Phase-1 global shape, so the existing 3-clause kill
// predicate binds every strategy of every tenant with no predicate change.
// Epoch MAX+1 in tx. Returns the recorded epoch.
func (s *Store) AppendPlatformKill(eventID, actorID, recordedAt string, flatten bool) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	var epoch int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(kill_epoch), 0) + 1 FROM kill_breaker_events`).
		Scan(&epoch); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO kill_breaker_events
		(event_id, kind, scope, strategy_id, tenant_id, kill_epoch, flatten, trigger_ref, actor_id, recorded_at)
		VALUES (?, 'kill', 'platform', NULL, NULL, ?, ?, NULL, ?, ?)`,
		eventID, epoch, flatten, actorID, recordedAt); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

// AppendSafetyEffectDone records that eventID's effects completed: the
// insert IS the completion marker. INSERT OR IGNORE semantics — a duplicate
// marker is a silent no-op (PK idempotence); the enforced FOREIGN KEY still
// rejects a marker for a nonexistent event row.
func (s *Store) AppendSafetyEffectDone(eventID, completedAt string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO safety_effects (event_id, completed_at)
		VALUES (?, ?)`, eventID, completedAt)
	return err
}

// SafetyEffectServed reports whether eventID carries a safety_effects
// done-marker — driver-written OR clear-written (lifecycle-api.md LC-38):
// the safety drive re-checks it immediately before executing an event's
// effects, so an in-flight pass can never execute effects a concurrent
// clear has superseded. Read-only.
func (s *Store) SafetyEffectServed(eventID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM safety_effects WHERE event_id = ?`, eventID).Scan(&n)
	return n > 0, err
}

// AppendSafetyAlert appends one safety_alerts row (append-only monitor and
// driver alerts; kind is an OPEN set — no CHECK, §Alerts registry).
func (s *Store) AppendSafetyAlert(a SafetyAlert) error {
	_, err := s.db.Exec(`INSERT INTO safety_alerts
		(alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.AlertID, a.Kind, a.StrategyID, a.RefID, a.DetailsJSON, a.RecordedAt)
	return err
}

// AppendKillLifecycleLock is driver step 3b's mutator: ONE transaction
// reads the strategy's CURRENT lifecycle state (never a stale pre-read); a
// live_* state appends the transition row to 'killed' (actor_role 'system',
// reason referencing the kill event_id) and updates the strategy state.
// Already-killed or non-live states no-op and return locked=false
// (idempotent). ErrNotFound for an unknown strategy.
func (s *Store) AppendKillLifecycleLock(strategyID, eventID, actorID, recordedAt string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	var state string
	err = tx.QueryRow(`SELECT lifecycle_state FROM strategies WHERE strategy_id = ?`, strategyID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
	}
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(state, "live_") {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO lifecycle_transitions
		(transition_id, strategy_id, from_state, to_state, actor_id, actor_role, reason, recorded_at)
		VALUES (?, ?, ?, 'killed', ?, 'system', ?, ?)`,
		uuid.NewString(), strategyID, state, actorID,
		"kill-switch event "+eventID, recordedAt); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE strategies SET lifecycle_state = 'killed', updated_at = ?
		WHERE strategy_id = ?`, recordedAt, strategyID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListUnservedSafetyEvents returns every kill AND breaker row — at ANY age —
// with no safety_effects marker, in tier precedence order
// platform → tenant → strategy (derived from the id columns' NULL-ness, so
// Phase-1 global rows sort with platform), then insertion (rowid) order.
func (s *Store) ListUnservedSafetyEvents() ([]KillBreakerEvent, error) {
	rows, err := s.db.Query(`SELECT e.event_id, e.kind, e.scope, e.strategy_id, e.tenant_id,
		e.kill_epoch, e.flatten, e.trigger_ref, e.actor_id, e.recorded_at
		FROM kill_breaker_events e
		LEFT JOIN safety_effects se ON se.event_id = e.event_id
		WHERE se.event_id IS NULL
		ORDER BY CASE
			WHEN e.strategy_id IS NULL AND e.tenant_id IS NULL THEN 0
			WHEN e.strategy_id IS NULL THEN 1
			ELSE 2 END, e.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KillBreakerEvent
	for rows.Next() {
		var e KillBreakerEvent
		var strategyID, tenantID, triggerRef sql.NullString
		var killEpoch sql.NullInt64
		var flatten sql.NullBool
		if err := rows.Scan(&e.EventID, &e.Kind, &e.Scope, &strategyID, &tenantID,
			&killEpoch, &flatten, &triggerRef, &e.ActorID, &e.RecordedAt); err != nil {
			return nil, err
		}
		e.StrategyID = nullable(strategyID)
		e.TenantID = nullable(tenantID)
		e.TriggerRef = nullable(triggerRef)
		if killEpoch.Valid {
			e.KillEpoch = &killEpoch.Int64
		}
		if flatten.Valid {
			e.Flatten = &flatten.Bool
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SafetyAlertFilter narrows ListSafetyAlerts; zero values mean no
// constraint (Limit <= 0: unbounded).
type SafetyAlertFilter struct {
	Kind       string
	StrategyID string
	RefID      string
	Limit      int
}

// ListSafetyAlerts returns safety_alerts rows matching the filter in
// insertion (rowid) order.
func (s *Store) ListSafetyAlerts(f SafetyAlertFilter) ([]SafetyAlert, error) {
	q := `SELECT alert_id, kind, strategy_id, ref_id, details_json, recorded_at
		FROM safety_alerts`
	var conds []string
	var args []any
	if f.Kind != "" {
		conds, args = append(conds, `kind = ?`), append(args, f.Kind)
	}
	if f.StrategyID != "" {
		conds, args = append(conds, `strategy_id = ?`), append(args, f.StrategyID)
	}
	if f.RefID != "" {
		conds, args = append(conds, `ref_id = ?`), append(args, f.RefID)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY rowid`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SafetyAlert
	for rows.Next() {
		var a SafetyAlert
		var strategyID, refID sql.NullString
		if err := rows.Scan(&a.AlertID, &a.Kind, &strategyID, &refID,
			&a.DetailsJSON, &a.RecordedAt); err != nil {
			return nil, err
		}
		a.StrategyID = nullable(strategyID)
		a.RefID = nullable(refID)
		out = append(out, a)
	}
	return out, rows.Err()
}

// HasSafetyAlertToday is the daily alert dedupe read, keyed
// (kind, strategy_id, ref_id, utcDate). An EMPTY strategyID/refID matches
// rows where the column is NULL (the empty-matches-NULL rule).
func (s *Store) HasSafetyAlertToday(kind, strategyID, refID, utcDate string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM safety_alerts
		WHERE kind = ?
		AND (strategy_id = ? OR (strategy_id IS NULL AND ? = ''))
		AND (ref_id = ? OR (ref_id IS NULL AND ? = ''))
		AND substr(recorded_at, 1, 10) = ?`,
		kind, strategyID, strategyID, refID, refID, utcDate).Scan(&n)
	return n > 0, err
}

// LatestStrategyKillEvent returns the NEWEST kind='kill', scope='strategy'
// row for the strategy — served or not (watchdog.md WD-16: the
// escalation-alert back-fill needs the row's event_id and actor_id, which
// neither GlobalMaxKillEpoch nor ListUnservedSafetyEvents exposes);
// ok=false when no such row exists. Read-only.
func (s *Store) LatestStrategyKillEvent(strategyID string) (eventID, actorID string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT event_id, actor_id FROM kill_breaker_events
		WHERE kind = 'kill' AND scope = 'strategy' AND strategy_id = ?
		ORDER BY rowid DESC LIMIT 1`, strategyID).Scan(&eventID, &actorID)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return eventID, actorID, true, nil
}

// HasSafetyAlert is the any-age alert dedupe read for the one-time
// safety_residue_abandoned rows, keyed (kind, strategy_id, ref_id); same
// empty-matches-NULL rule as HasSafetyAlertToday.
func (s *Store) HasSafetyAlert(kind, strategyID, refID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM safety_alerts
		WHERE kind = ?
		AND (strategy_id = ? OR (strategy_id IS NULL AND ? = ''))
		AND (ref_id = ? OR (ref_id IS NULL AND ? = ''))`,
		kind, strategyID, strategyID, refID, refID).Scan(&n)
	return n > 0, err
}
