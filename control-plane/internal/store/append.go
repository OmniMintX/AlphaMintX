package store

import "database/sql"

// Append-only audit tables (invariant 7): INSERT-only methods, no UPDATE or
// DELETE, ever.

// AppendLifecycleTransition appends the transition audit row and, in the
// same transaction, advances the strategies.lifecycle_state snapshot (the
// transitions are the audit; the strategy row is the current state).
func (s *Store) AppendLifecycleTransition(t LifecycleTransition) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(`INSERT INTO lifecycle_transitions
		(transition_id, strategy_id, from_state, to_state, actor_id, actor_role, reason, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TransitionID, t.StrategyID, t.FromState, t.ToState, t.ActorID, t.ActorRole, t.Reason, t.RecordedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE strategies SET lifecycle_state = ?, updated_at = ? WHERE strategy_id = ?`,
		t.ToState, t.RecordedAt, t.StrategyID); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendRejectedSubmission records a malformed submission that never earned
// a verdict, or an approved decision whose OMS submission failed
// (SUBMIT_FAILED).
func (s *Store) AppendRejectedSubmission(r RejectedSubmission) error {
	_, err := s.db.Exec(`INSERT INTO rejected_submissions
		(rejection_id, strategy_id, received_at, reason, payload_json)
		VALUES (?, ?, ?, ?, ?)`,
		r.RejectionID, r.StrategyID, r.ReceivedAt, r.Reason, r.PayloadJSON)
	return err
}

// RejectedSubmissions returns a strategy's rejected_submissions audit rows,
// oldest first.
func (s *Store) RejectedSubmissions(strategyID string) ([]RejectedSubmission, error) {
	rows, err := s.db.Query(`SELECT rejection_id, strategy_id, received_at, reason, payload_json
		FROM rejected_submissions WHERE strategy_id = ? ORDER BY received_at, rejection_id`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RejectedSubmission
	for rows.Next() {
		var r RejectedSubmission
		if err := rows.Scan(&r.RejectionID, &r.StrategyID, &r.ReceivedAt, &r.Reason, &r.PayloadJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendKillBreakerEvent persists the kill/breaker intent; it MUST be
// acknowledged before any side effect executes (Row rules).
func (s *Store) AppendKillBreakerEvent(e KillBreakerEvent) error {
	_, err := s.db.Exec(`INSERT INTO kill_breaker_events
		(event_id, kind, scope, strategy_id, tenant_id, kill_epoch, flatten, trigger_ref, actor_id, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.EventID, e.Kind, e.Scope, e.StrategyID, e.TenantID, e.KillEpoch, e.Flatten, e.TriggerRef, e.ActorID, e.RecordedAt)
	return err
}

// AppendTenantKill persists a tenant-scope kill event with
// kill_epoch = MAX(kill_epoch) over the WHOLE table + 1, computed inside
// the insert transaction (multi-tenant-rbac.md §Tenant kill-switch: one
// global epoch counter, race-free under the single-connection invariant).
// strategy_id is NULL and flatten is 0 (v1 is gate-block only). Returns the
// recorded epoch.
func (s *Store) AppendTenantKill(eventID, tenantID, actorID, recordedAt string) (int64, error) {
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
		VALUES (?, 'kill', 'tenant', NULL, ?, ?, 0, NULL, ?, ?)`,
		eventID, tenantID, epoch, actorID, recordedAt); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

// AppendRiskLimitChanges appends one audit row per changed field in ONE
// transaction (multi-tenant-rbac.md §Runtime limit changes: all rows land
// atomically, BEFORE the in-memory effect).
func (s *Store) AppendRiskLimitChanges(changes []RiskLimitChange) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	for _, c := range changes {
		if _, err := tx.Exec(`INSERT INTO risk_limit_changes
			(change_id, strategy_id, field, old_value, new_value, actor_id, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ChangeID, c.StrategyID, c.Field, c.OldValue, c.NewValue, c.ActorID, c.ChangedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RiskLimitChanges returns every persisted limit change in rowid order —
// the normative overlay replay order (last write wins; changed_at has
// second precision and MUST NOT be the order key).
func (s *Store) RiskLimitChanges() ([]RiskLimitChange, error) {
	rows, err := s.db.Query(`SELECT change_id, strategy_id, field, old_value, new_value, actor_id, changed_at
		FROM risk_limit_changes ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RiskLimitChange
	for rows.Next() {
		var c RiskLimitChange
		var old sql.NullString
		if err := rows.Scan(&c.ChangeID, &c.StrategyID, &c.Field, &old, &c.NewValue, &c.ActorID, &c.ChangedAt); err != nil {
			return nil, err
		}
		c.OldValue = nullable(old)
		out = append(out, c)
	}
	return out, rows.Err()
}
