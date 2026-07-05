package store

// Operator-surface reads (docs/specs/operator-surface.md §Wiring seams):
// the OS-10a single-snapshot SafetyStatus and the two paginated alert
// feeds. All three are READ-ONLY — the append-only surface is untouched.

import (
	"database/sql"
	"fmt"
)

// BoundKillClear is the newest kill_clear_events row COVERING a bound kill
// (OS-9 scope-column watermark). INTERNAL: the api handler maps BoundKill
// to the OS-8a wire DTO; these fields never marshal into a response.
type BoundKillClear struct {
	ClearID      string
	ActorID      string
	Reason       string
	RecordedAt   string
	ClearedEpoch int64
}

// BoundKill is one kind='kill' row binding the strategy plus its newest
// covering clear (nil while the kill stands at its own scope).
type BoundKill struct {
	KillBreakerEvent
	Cleared *BoundKillClear
}

// SafetyStatusRow is the OS-10a snapshot: every field derives from ONE
// read transaction, so the banner inputs and the acting predicate can
// never disagree within a response.
type SafetyStatusRow struct {
	LifecycleState     string
	PausedFrom         *string
	ActiveKill         bool
	Kills              []BoundKill
	BreakerActiveToday bool
	BreakerEvent       *KillBreakerEvent
}

// safetyKillsSQL is the OS-8/OS-9 binding-kills join: the 3-clause scope
// match of the LC-28 predicate (without the epoch-vs-clear condition) over
// kind='kill' rows with a non-NULL kill_epoch (OS-8b), each LEFT-joined to
// its newest covering clear — covering keys on the clear's SCOPE COLUMN
// plus the matching id (OS-9: matching ids alone is WRONG), newest =
// highest cleared_epoch, rowid DESC tiebreak. ?1 = strategy_id,
// ?2 = the strategy's tenant_id (read in the same transaction).
const safetyKillsSQL = `SELECT e.event_id, e.kind, e.scope, e.strategy_id, e.tenant_id,
  e.kill_epoch, e.flatten, e.trigger_ref, e.actor_id, e.recorded_at,
  c.clear_id, c.actor_id, c.reason, c.recorded_at, c.cleared_epoch
  FROM kill_breaker_events e
  LEFT JOIN kill_clear_events c ON c.rowid = (
    SELECT c2.rowid FROM kill_clear_events c2
    WHERE c2.cleared_epoch >= e.kill_epoch AND (
      (e.strategy_id IS NOT NULL AND c2.scope = 'strategy' AND c2.strategy_id = e.strategy_id)
      OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL
          AND c2.scope = 'tenant' AND c2.tenant_id = e.tenant_id)
      OR (e.strategy_id IS NULL AND e.tenant_id IS NULL AND c2.scope = 'platform'))
    ORDER BY c2.cleared_epoch DESC, c2.rowid DESC LIMIT 1)
  WHERE e.kind = 'kill' AND e.kill_epoch IS NOT NULL
  AND (e.strategy_id = ?1
    OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL AND e.tenant_id = ?2)
    OR (e.strategy_id IS NULL AND e.tenant_id IS NULL))
  ORDER BY e.kill_epoch DESC`

// SafetyStatus is the OS-10a single-snapshot read: ONE read transaction
// evaluates the binding-kills join (OS-8/OS-9), the LC-28 activeKillSQL
// predicate verbatim (OS-10 — never re-derived from the join), the
// BreakerActiveToday latch plus the newest matching breaker event (OS-11),
// and the LC-7 paused-provenance read feeding paused_from (OS-7).
// ErrNotFound for an unknown strategy.
func (s *Store) SafetyStatus(strategyID, utcDate string) (SafetyStatusRow, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return SafetyStatusRow{}, err
	}
	defer rollback(tx)
	var row SafetyStatusRow
	var tenantID string
	err = tx.QueryRow(`SELECT tenant_id, lifecycle_state FROM strategies WHERE strategy_id = ?`,
		strategyID).Scan(&tenantID, &row.LifecycleState)
	if err == sql.ErrNoRows {
		return SafetyStatusRow{}, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
	}
	if err != nil {
		return SafetyStatusRow{}, err
	}
	rows, err := tx.Query(safetyKillsSQL, strategyID, tenantID)
	if err != nil {
		return SafetyStatusRow{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k BoundKill
		var sid, tid, trigger sql.NullString
		var epoch sql.NullInt64
		var flatten sql.NullBool
		var clearID, clearActor, clearReason, clearAt sql.NullString
		var clearedEpoch sql.NullInt64
		if err := rows.Scan(&k.EventID, &k.Kind, &k.Scope, &sid, &tid,
			&epoch, &flatten, &trigger, &k.ActorID, &k.RecordedAt,
			&clearID, &clearActor, &clearReason, &clearAt, &clearedEpoch); err != nil {
			return SafetyStatusRow{}, err
		}
		k.StrategyID, k.TenantID, k.TriggerRef = nullable(sid), nullable(tid), nullable(trigger)
		if epoch.Valid {
			k.KillEpoch = &epoch.Int64
		}
		if flatten.Valid {
			k.Flatten = &flatten.Bool
		}
		if clearID.Valid {
			k.Cleared = &BoundKillClear{
				ClearID: clearID.String, ActorID: clearActor.String, Reason: clearReason.String,
				RecordedAt: clearAt.String, ClearedEpoch: clearedEpoch.Int64,
			}
		}
		row.Kills = append(row.Kills, k)
	}
	if err := rows.Err(); err != nil {
		return SafetyStatusRow{}, err
	}
	rows.Close()
	var active int
	if err := tx.QueryRow(activeKillSQL, strategyID).Scan(&active); err != nil {
		return SafetyStatusRow{}, err
	}
	row.ActiveKill = active > 0
	if row.BreakerActiveToday, err = breakerActiveToday(tx, strategyID, utcDate); err != nil {
		return SafetyStatusRow{}, err
	}
	if row.BreakerEvent, err = newestBreakerEventToday(tx, strategyID, utcDate); err != nil {
		return SafetyStatusRow{}, err
	}
	if row.LifecycleState == "paused" {
		from, ok, err := pausedProvenance(tx, strategyID)
		if err != nil {
			return SafetyStatusRow{}, err
		}
		if ok {
			row.PausedFrom = &from
		}
	}
	return row, tx.Commit()
}

// newestBreakerEventToday returns the newest kind='breaker' row on the
// UTC date matching the SAME 3-clause scope match as BreakerActiveToday
// (OS-11: predicate and event agree by construction); nil when none.
func newestBreakerEventToday(q dbtx, strategyID, utcDate string) (*KillBreakerEvent, error) {
	var e KillBreakerEvent
	var sid, tid, trigger sql.NullString
	var epoch sql.NullInt64
	var flatten sql.NullBool
	err := q.QueryRow(`SELECT event_id, kind, scope, strategy_id, tenant_id,
		kill_epoch, flatten, trigger_ref, actor_id, recorded_at
		FROM kill_breaker_events
		WHERE kind = 'breaker' AND substr(recorded_at, 1, 10) = ?
		AND ((strategy_id IS NULL AND tenant_id IS NULL)
			OR strategy_id = ?
			OR (tenant_id = (SELECT tenant_id FROM strategies WHERE strategy_id = ?) AND strategy_id IS NULL))
		ORDER BY recorded_at DESC, rowid DESC LIMIT 1`,
		utcDate, strategyID, strategyID).Scan(&e.EventID, &e.Kind, &e.Scope, &sid, &tid,
		&epoch, &flatten, &trigger, &e.ActorID, &e.RecordedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.StrategyID, e.TenantID, e.TriggerRef = nullable(sid), nullable(tid), nullable(trigger)
	if epoch.Valid {
		e.KillEpoch = &epoch.Int64
	}
	if flatten.Valid {
		e.Flatten = &flatten.Bool
	}
	return &e, nil
}

// ListSafetyAlertsByStrategyPage is the OS-16/OS-17 per-strategy feed:
// strategy_id = ? rows ONLY (NULL-strategy rows are the global feed's),
// ORDER BY recorded_at DESC, alert_id DESC (the pinned tiebreak — alert_id
// is IN the payload; rowid is not exposed), LIMIT/OFFSET plus total.
func (s *Store) ListSafetyAlertsByStrategyPage(strategyID string, page, limit int) ([]SafetyAlert, int, error) {
	page, limit = normalizePage(page, limit)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM safety_alerts WHERE strategy_id = ?`,
		strategyID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT alert_id, kind, strategy_id, ref_id, details_json, recorded_at
		FROM safety_alerts WHERE strategy_id = ?
		ORDER BY recorded_at DESC, alert_id DESC LIMIT ? OFFSET ?`,
		strategyID, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanSafetyAlerts(rows)
	return out, total, err
}

// ListSafetyAlertsGlobalPage is the OS-21 global feed: ALL rows including
// NULL strategy_id, optional exact kind filter (open set — an unknown kind
// is an empty page, never an error), same pinned ordering.
func (s *Store) ListSafetyAlertsGlobalPage(kind string, page, limit int) ([]SafetyAlert, int, error) {
	page, limit = normalizePage(page, limit)
	where, args := "", []any{}
	if kind != "" {
		where, args = ` WHERE kind = ?`, []any{kind}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM safety_alerts`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT alert_id, kind, strategy_id, ref_id, details_json, recorded_at
		FROM safety_alerts`+where+`
		ORDER BY recorded_at DESC, alert_id DESC LIMIT ? OFFSET ?`,
		append(args, limit, (page-1)*limit)...)
	if err != nil {
		return nil, 0, err
	}
	out, err := scanSafetyAlerts(rows)
	return out, total, err
}

func scanSafetyAlerts(rows *sql.Rows) ([]SafetyAlert, error) {
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
