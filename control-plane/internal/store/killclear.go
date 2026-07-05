package store

// SW-2 kill-clear machinery (docs/specs/lifecycle-api.md LC-25..LC-38):
// append-only kill_clear_events rows, the LC-28 active-kill predicate, and
// the three scope-tier clear appenders. Clearing never mutates
// kill_breaker_events (LC-35); a clear is a new row.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoActiveKill: the clear's scope has no active kill (LC-31, 422
// NO_ACTIVE_KILL); nothing is written.
var ErrNoActiveKill = errors.New("NO_ACTIVE_KILL")

// ErrClearConflict: the body's observed_epoch does not match the scope's
// recomputed MAX(kill_epoch) (LC-27, 409 CLEAR_CONFLICT); nothing is
// written — a kill landing between the operator's read and the clear can
// never be swept away unseen.
var ErrClearConflict = errors.New("CLEAR_CONFLICT")

// ErrKillActive: the LC-9 CAS transaction's live-target re-check found an
// active kill binding the strategy (the LC-8 guard re-evaluated IN the
// transaction); nothing is written — the handler answers 422
// ILLEGAL_TRANSITION, message "kill tier active".
var ErrKillActive = errors.New("KILL_ACTIVE")

// activeKillSQL is the LC-28 ACTIVE-KILL predicate (normative SQL,
// verbatim), shared by ActiveKill and AppendLifecycleTransitionCAS's
// in-transaction live-target re-check.
const activeKillSQL = `SELECT EXISTS (SELECT 1 FROM kill_breaker_events e
  WHERE e.kind = 'kill' AND (
    (e.strategy_id = ?1 AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'strategy' AND strategy_id = ?1))
    OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL
        AND e.tenant_id = (SELECT tenant_id FROM strategies WHERE strategy_id = ?1)
        AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'tenant' AND tenant_id = e.tenant_id))
    OR (e.strategy_id IS NULL AND e.tenant_id IS NULL AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'platform'))))`

// ActiveKill reports whether an UNCLEARED kill binds the strategy at any
// tier (LC-28, normative SQL verbatim): a kill row is active while its
// kill_epoch exceeds the newest cleared_epoch of ITS OWN scope. The three
// clauses extend the multi-tenant-rbac.md 3-clause kill predicate; tenant
// rows still bind only their tenant, both-NULL rows everyone.
func (s *Store) ActiveKill(strategyID string) (bool, error) {
	var active int
	err := s.db.QueryRow(activeKillSQL, strategyID).Scan(&active)
	return active > 0, err
}

// AppendKillClearStrategy appends a strategy-scope clear: ONE transaction
// resolves tenant_id from strategies (audit; ErrNotFound unknown), checks
// the strategy-scope active clause (LC-31 first), CAS-verifies
// observedEpoch against the recomputed scope max (LC-27), inserts the
// clear row, the LC-38 supersede markers, and their alerts. Returns the
// recorded cleared_epoch and the superseded event ids.
func (s *Store) AppendKillClearStrategy(clearID, strategyID, actorID, reason string, observedEpoch int64, recordedAt string) (int64, []string, error) {
	return s.appendKillClear(clearID, "strategy", strategyID, "", actorID, reason, observedEpoch, recordedAt)
}

// AppendKillClearTenant appends a tenant-scope clear — the tenant-scope
// clause of LC-28, otherwise the same shape as AppendKillClearStrategy.
func (s *Store) AppendKillClearTenant(clearID, tenantID, actorID, reason string, observedEpoch int64, recordedAt string) (int64, []string, error) {
	return s.appendKillClear(clearID, "tenant", "", tenantID, actorID, reason, observedEpoch, recordedAt)
}

// AppendKillClearPlatform appends a platform-scope clear — it covers
// platform rows AND Phase-1 global rows (both ids NULL, LC-26).
func (s *Store) AppendKillClearPlatform(clearID, actorID, reason string, observedEpoch int64, recordedAt string) (int64, []string, error) {
	return s.appendKillClear(clearID, "platform", "", "", actorID, reason, observedEpoch, recordedAt)
}

// appendKillClear is the shared clear transaction. The LC-31 active check
// answers before the LC-27 epoch verification (the pinned order); on any
// rejection NOTHING is written — no clear row, no markers, no alerts.
func (s *Store) appendKillClear(clearID, scope, strategyID, tenantID, actorID, reason string, observedEpoch int64, recordedAt string) (int64, []string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer rollback(tx)
	if scope == "strategy" {
		err := tx.QueryRow(`SELECT tenant_id FROM strategies WHERE strategy_id = ?`, strategyID).Scan(&tenantID)
		if err == sql.ErrNoRows {
			return 0, nil, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
		}
		if err != nil {
			return 0, nil, err
		}
	}
	// Per-scope kill-row match (the scope's own LC-28 clause) and the
	// scope's cleared-epoch watermark subquery.
	var match, cleared string
	var matchArgs, clearedArgs []any
	switch scope {
	case "strategy":
		match, matchArgs = `e.strategy_id = ?`, []any{strategyID}
		cleared, clearedArgs = `scope = 'strategy' AND strategy_id = ?`, []any{strategyID}
	case "tenant":
		match, matchArgs = `e.strategy_id IS NULL AND e.tenant_id IS NOT NULL AND e.tenant_id = ?`, []any{tenantID}
		cleared, clearedArgs = `scope = 'tenant' AND tenant_id = ?`, []any{tenantID}
	default: // platform: Phase-1 global rows classify here too (LC-26)
		match = `e.strategy_id IS NULL AND e.tenant_id IS NULL`
		cleared = `scope = 'platform'`
	}
	var active int
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM kill_breaker_events e
		WHERE e.kind = 'kill' AND `+match+` AND e.kill_epoch >
		(SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events WHERE `+cleared+`))`,
		append(append([]any{}, matchArgs...), clearedArgs...)...).Scan(&active); err != nil {
		return 0, nil, err
	}
	if active == 0 {
		return 0, nil, ErrNoActiveKill
	}
	var scopeMax int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(e.kill_epoch), 0) FROM kill_breaker_events e
		WHERE e.kind = 'kill' AND `+match, matchArgs...).Scan(&scopeMax); err != nil {
		return 0, nil, err
	}
	if scopeMax != observedEpoch {
		return 0, nil, fmt.Errorf("observed epoch %d, scope max %d: %w", observedEpoch, scopeMax, ErrClearConflict)
	}
	var sid, tid *string
	if scope == "strategy" {
		sid = &strategyID
	}
	if scope != "platform" {
		tid = &tenantID
	}
	if _, err := tx.Exec(`INSERT INTO kill_clear_events
		(clear_id, scope, strategy_id, tenant_id, cleared_epoch, actor_id, reason, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		clearID, scope, sid, tid, observedEpoch, actorID, reason, recordedAt); err != nil {
		return 0, nil, err
	}
	superseded, err := s.supersedeUnserved(tx, clearID, match, matchArgs, observedEpoch, recordedAt)
	if err != nil {
		return 0, nil, err
	}
	return observedEpoch, superseded, tx.Commit()
}

// supersedeUnserved implements the LC-38 carve-out inside the clear
// transaction: every covered kill event (scope match, kill_epoch <=
// cleared_epoch) lacking a safety_effects marker gets one — recording that
// the CLEAR is the resolution, not that effects executed — plus one
// kill_effects_superseded alert (strategy_id from the event when set,
// ref_id = event_id). Breaker rows are never covered. Returns the
// superseded event ids in insertion (rowid) order.
func (s *Store) supersedeUnserved(tx *sql.Tx, clearID, match string, matchArgs []any, clearedEpoch int64, recordedAt string) ([]string, error) {
	rows, err := tx.Query(`SELECT e.event_id, e.strategy_id FROM kill_breaker_events e
		LEFT JOIN safety_effects se ON se.event_id = e.event_id
		WHERE e.kind = 'kill' AND `+match+` AND e.kill_epoch <= ? AND se.event_id IS NULL
		ORDER BY e.rowid`, append(append([]any{}, matchArgs...), clearedEpoch)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type covered struct {
		eventID    string
		strategyID *string
	}
	var events []covered
	for rows.Next() {
		var c covered
		var sid sql.NullString
		if err := rows.Scan(&c.eventID, &sid); err != nil {
			return nil, err
		}
		c.strategyID = nullable(sid)
		events = append(events, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var ids []string
	for _, c := range events {
		if _, err := tx.Exec(`INSERT INTO safety_effects (event_id, completed_at)
			VALUES (?, ?)`, c.eventID, recordedAt); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO safety_alerts
			(alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
			VALUES (?, 'kill_effects_superseded', ?, ?, ?, ?)`,
			uuid.NewString(), c.strategyID, c.eventID,
			fmt.Sprintf(`{"clear_id":%q}`, clearID), recordedAt); err != nil {
			return nil, err
		}
		ids = append(ids, c.eventID)
	}
	return ids, nil
}
