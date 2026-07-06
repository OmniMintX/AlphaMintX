package store

// Restore-gate persistence (docs/specs/deploy-and-survive.md DS-2/DS-4/
// DS-5): a DB opened with the DS-1 artifact stamp (user_version >= 1) IS a
// restored artifact — new trading intent stays blocked until an env-admin
// acknowledges the restore, which resets user_version to 0 and appends the
// cleared alert in ONE transaction. Clearing is one-way: the gate cannot
// be re-armed in-process.

import "errors"

// ErrRestoreGateNotEngaged: the ack lost the DS-5 CAS race, or the gate
// was never engaged — an ack aimed at the wrong deployment (409).
var ErrRestoreGateNotEngaged = errors.New("RESTORE_GATE_NOT_ENGAGED")

// RestoreGateEngaged reports the DS-2 gate state read at Open.
func (s *Store) RestoreGateEngaged() bool { return s.restoreGate.Load() >= 1 }

// RestoreGateUserVersion returns the user_version read at Open — the DS-4
// engaged-alert details value (0 once the gate is cleared).
func (s *Store) RestoreGateUserVersion() int64 { return s.restoreGate.Load() }

// RestoreGateAlertPending reports whether the DS-4 boot alert is due: no
// restore_gate_engaged row newer than the newest restore_gate_cleared row
// — once per ENGAGEMENT, not per boot, so a crash-looping gated server
// never floods the append-only table or the webhook. rowid is insert
// order, safe under the pool-of-one + no-VACUUM regime (the AN-2
// watermark precedent).
func (s *Store) RestoreGateAlertPending() (bool, error) {
	var engaged, cleared int64
	if err := s.db.QueryRow(`SELECT
		COALESCE((SELECT MAX(rowid) FROM safety_alerts WHERE kind = 'restore_gate_engaged'), 0),
		COALESCE((SELECT MAX(rowid) FROM safety_alerts WHERE kind = 'restore_gate_cleared'), 0)`).
		Scan(&engaged, &cleared); err != nil {
		return false, err
	}
	return engaged <= cleared, nil
}

// ClearRestoreGate acknowledges the restore (DS-5): the in-memory flag is
// compare-and-swapped so exactly one concurrent caller wins, then
// user_version = 0 and the cleared safety_alerts row commit in ONE
// transaction (the AN-1a precedent: two sequential writes leave a crash
// window that loses the alert). A failed transaction re-arms the flag;
// the loser gets ErrRestoreGateNotEngaged.
func (s *Store) ClearRestoreGate(a SafetyAlert) error {
	v := s.restoreGate.Load()
	if v < 1 || !s.restoreGate.CompareAndSwap(v, 0) {
		return ErrRestoreGateNotEngaged
	}
	if err := s.clearRestoreGateTx(a); err != nil {
		s.restoreGate.Store(v)
		return err
	}
	return nil
}

func (s *Store) clearRestoreGateTx(a SafetyAlert) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(`PRAGMA user_version = 0`); err != nil {
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
