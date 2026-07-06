package store

// Alert-notifier persistence surface (docs/specs/alert-notifier.md): the
// AN-7 alert_dispatch_state watermark table (mutable in place, exempt from
// the append-only invariant exactly like strategy_state), the AN-8 atomic
// seed statement, the AN-2 rowid-ordered source reads, and the AN-1a
// combined recon-event+alert mutator. rowid is a safe watermark ONLY under
// the single-connection pool (store.go SetMaxOpenConns(1)) and the
// no-VACUUM regime (ops-backup.md OB-2); the pool size carries a tripwire
// test.

import (
	"database/sql"
	"fmt"
)

// Alert-notifier source wire names (AN-1). Each maps to its table through
// the fixed whitelist below — table names never arrive from a caller.
const (
	AlertSourceKillBreaker = "kill_breaker_events"
	AlertSourceKillClear   = "kill_clear_events"
	AlertSourceSafetyAlert = "safety_alerts"
)

var alertSourceTables = map[string]string{
	AlertSourceKillBreaker: "kill_breaker_events",
	AlertSourceKillClear:   "kill_clear_events",
	AlertSourceSafetyAlert: "safety_alerts",
}

func alertSourceTable(source string) (string, error) {
	table, ok := alertSourceTables[source]
	if !ok {
		return "", fmt.Errorf("unknown alert source %q", source)
	}
	return table, nil
}

// migrateAlertDispatch creates alert_dispatch_state UNCONDITIONALLY at
// Open (AN-7: config never reaches the store layer; only seeding and the
// dispatcher goroutine are config-gated — an empty table on a deployment
// that never enables the notifier is the intended state).
func migrateAlertDispatch(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS alert_dispatch_state (
		source TEXT PRIMARY KEY,
		last_rowid INTEGER NOT NULL,
		updated_at TEXT NOT NULL)`)
	return err
}

// KillClearEvent mirrors the append-only kill_clear_events table — the
// full row, because the AN-13 wire carries facts, never the OS-7 joined
// view shapes.
type KillClearEvent struct {
	ClearID      string  `json:"clear_id"`
	Scope        string  `json:"scope"`
	StrategyID   *string `json:"strategy_id"`
	TenantID     *string `json:"tenant_id"`
	ClearedEpoch int64   `json:"cleared_epoch"`
	ActorID      string  `json:"actor_id"`
	Reason       string  `json:"reason"`
	RecordedAt   string  `json:"recorded_at"`
}

// NotifyKillBreakerEvent pairs a source row with its rowid — the AN-13
// seq and the AN-2 watermark position. rowid never crosses the API layer.
type NotifyKillBreakerEvent struct {
	Rowid int64
	KillBreakerEvent
}

// NotifyKillClearEvent pairs a kill_clear_events row with its rowid.
type NotifyKillClearEvent struct {
	Rowid int64
	KillClearEvent
}

// NotifySafetyAlert pairs a safety_alerts row with its rowid.
type NotifySafetyAlert struct {
	Rowid int64
	SafetyAlert
}

// AlertDispatchWatermark reads one source's last delivered rowid;
// ok=false when the source was never seeded (notifier never enabled).
func (s *Store) AlertDispatchWatermark(source string) (int64, bool, error) {
	var w int64
	err := s.db.QueryRow(`SELECT last_rowid FROM alert_dispatch_state
		WHERE source = ?`, source).Scan(&w)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return w, true, nil
}

// UpsertAlertDispatchWatermark advances (or rewinds, for the AN-8a clamp)
// one source's watermark in place (AN-9): a single short-lived statement,
// never part of a longer transaction — the dispatcher must not hold the
// pool-of-one connection across network I/O (AN-2a).
func (s *Store) UpsertAlertDispatchWatermark(source string, lastRowid int64, updatedAt string) error {
	_, err := s.db.Exec(`INSERT INTO alert_dispatch_state (source, last_rowid, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET
			last_rowid = excluded.last_rowid, updated_at = excluded.updated_at`,
		source, lastRowid, updatedAt)
	return err
}

// SeedAlertDispatchWatermark seeds last_rowid = COALESCE(MAX(rowid), 0)
// iff the source has no watermark row (AN-8 seed-at-enable) in ONE
// statement, so no row can commit between the MAX read and the insert
// (the AN-8 seed-race case). An existing watermark is never moved.
func (s *Store) SeedAlertDispatchWatermark(source, updatedAt string) error {
	table, err := alertSourceTable(source)
	if err != nil {
		return err
	}
	// NOTE: the SELECT is an aggregate, so it yields exactly one row no
	// matter what its WHERE clause says — existence must be handled by
	// ON CONFLICT DO NOTHING, not by a WHERE NOT EXISTS (which would be
	// vacuous here). WHERE true disambiguates the ON CONFLICT parse.
	_, err = s.db.Exec(`INSERT INTO alert_dispatch_state (source, last_rowid, updated_at)
		SELECT ?1, COALESCE(MAX(rowid), 0), ?2 FROM `+table+` WHERE true
		ON CONFLICT (source) DO NOTHING`,
		source, updatedAt)
	return err
}

// MaxAlertSourceRowid returns MAX(rowid) of a source table (0 when
// empty) — the AN-8a clamp and backlog input.
func (s *Store) MaxAlertSourceRowid(source string) (int64, error) {
	table, err := alertSourceTable(source)
	if err != nil {
		return 0, err
	}
	var max int64
	err = s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM ` + table).Scan(&max)
	return max, err
}

// ListKillBreakerEventsAfter returns kill_breaker_events rows with
// rowid > after in rowid ASC order, LIMIT limit (AN-2: insert order,
// never timestamp order). The batch is fully materialized — no store
// resource outlives the call (AN-2a).
func (s *Store) ListKillBreakerEventsAfter(after int64, limit int) ([]NotifyKillBreakerEvent, error) {
	rows, err := s.db.Query(`SELECT rowid, event_id, kind, scope, strategy_id, tenant_id,
		kill_epoch, flatten, trigger_ref, actor_id, recorded_at
		FROM kill_breaker_events WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyKillBreakerEvent
	for rows.Next() {
		var e NotifyKillBreakerEvent
		var strategyID, tenantID, triggerRef sql.NullString
		var killEpoch sql.NullInt64
		var flatten sql.NullBool
		if err := rows.Scan(&e.Rowid, &e.EventID, &e.Kind, &e.Scope, &strategyID, &tenantID,
			&killEpoch, &flatten, &triggerRef, &e.ActorID, &e.RecordedAt); err != nil {
			return nil, err
		}
		e.StrategyID, e.TenantID, e.TriggerRef = nullable(strategyID), nullable(tenantID), nullable(triggerRef)
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

// ListKillClearEventsAfter returns kill_clear_events rows with
// rowid > after in rowid ASC order, LIMIT limit (AN-2).
func (s *Store) ListKillClearEventsAfter(after int64, limit int) ([]NotifyKillClearEvent, error) {
	rows, err := s.db.Query(`SELECT rowid, clear_id, scope, strategy_id, tenant_id,
		cleared_epoch, actor_id, reason, recorded_at
		FROM kill_clear_events WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifyKillClearEvent
	for rows.Next() {
		var e NotifyKillClearEvent
		var strategyID, tenantID sql.NullString
		if err := rows.Scan(&e.Rowid, &e.ClearID, &e.Scope, &strategyID, &tenantID,
			&e.ClearedEpoch, &e.ActorID, &e.Reason, &e.RecordedAt); err != nil {
			return nil, err
		}
		e.StrategyID, e.TenantID = nullable(strategyID), nullable(tenantID)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListSafetyAlertsAfter returns safety_alerts rows with rowid > after in
// rowid ASC order, LIMIT limit (AN-2).
func (s *Store) ListSafetyAlertsAfter(after int64, limit int) ([]NotifySafetyAlert, error) {
	rows, err := s.db.Query(`SELECT rowid, alert_id, kind, strategy_id, ref_id, details_json,
		recorded_at FROM safety_alerts WHERE rowid > ? ORDER BY rowid ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotifySafetyAlert
	for rows.Next() {
		var a NotifySafetyAlert
		var strategyID, refID sql.NullString
		if err := rows.Scan(&a.Rowid, &a.AlertID, &a.Kind, &strategyID, &refID,
			&a.DetailsJSON, &a.RecordedAt); err != nil {
			return nil, err
		}
		a.StrategyID, a.RefID = nullable(strategyID), nullable(refID)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AppendOMSReconEventWithAlert appends one recon audit row AND its
// companion safety_alerts row in ONE transaction (AN-1a: the venue_reset
// and sl_deadline_contingency writers must commit the alert with the
// recon event — two sequential appends leave a crash window that loses
// the alert).
func (s *Store) AppendOMSReconEventWithAlert(e OMSReconEvent, a SafetyAlert) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := appendOMSReconEvent(tx, e); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO safety_alerts
		(alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.AlertID, a.Kind, a.StrategyID, a.RefID, a.DetailsJSON, a.RecordedAt); err != nil {
		return err
	}
	return tx.Commit()
}
