package store

// Restore-gate tests (docs/specs/deploy-and-survive.md §Test obligations,
// store side): the DS-1 artifact stamp and its digest, the untouched live
// DB, gate engagement on opening a restored artifact, the one-transaction
// clear persisting across reopen, and the DS-4 boot-alert dedupe.

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// restoredCopy backs up s and "restores" the artifact to a fresh path.
func restoredCopy(t *testing.T, s *Store) string {
	t.Helper()
	dir := t.TempDir()
	pinBackupClock(s, testNow)
	res, err := s.Backup(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, res.Artifact))
	if err != nil {
		t.Fatalf("ReadFile artifact: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, b, 0o600); err != nil {
		t.Fatalf("WriteFile restored: %v", err)
	}
	return restored
}

// TestBackupArtifactStamped pins DS-1/DS-1a: the artifact carries
// user_version 1 at header bytes 60-63, the recorded SHA-256 matches the
// stamped bytes, OB-5 verification passes, and the live DB's own
// user_version stays 0 — the stamp never touches the source.
func TestBackupArtifactStamped(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	dir := t.TempDir()
	pinBackupClock(s, testNow)
	res, err := s.Backup(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !res.Verified {
		t.Error("result.Verified = false, want true (DS-1a: OB-5 passes on stamped artifacts)")
	}
	artifact := filepath.Join(dir, res.Artifact)
	b, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("ReadFile artifact: %v", err)
	}
	if got := binary.BigEndian.Uint32(b[userVersionOffset : userVersionOffset+4]); got != stampedUserVersion {
		t.Errorf("artifact user_version = %d, want %d (DS-1 stamp)", got, stampedUserVersion)
	}
	sha, err := sha256File(artifact)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	if sha != res.SHA256 {
		t.Errorf("recorded SHA-256 %s != stamped artifact digest %s (DS-1: one write pass)", res.SHA256, sha)
	}
	var live int64
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&live); err != nil {
		t.Fatalf("live user_version: %v", err)
	}
	if live != 0 || s.RestoreGateEngaged() {
		t.Errorf("live DB user_version = %d, gate engaged = %v, want 0 and false", live, s.RestoreGateEngaged())
	}
}

// TestRestoreGateEngageClearReopen pins DS-2/DS-5: opening a restored
// artifact engages the gate; ClearRestoreGate resets user_version and
// appends the cleared alert in one transaction; a second clear is
// ErrRestoreGateNotEngaged; and the clear persists across reopen.
func TestRestoreGateEngageClearReopen(t *testing.T) {
	src := openStore(t)
	createStrategy(t, src, uid(1))
	restored := restoredCopy(t, src)

	s, err := Open(restored)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	if !s.RestoreGateEngaged() || s.RestoreGateUserVersion() != 1 {
		t.Fatalf("gate engaged = %v, user_version = %d, want true and 1 (DS-2)",
			s.RestoreGateEngaged(), s.RestoreGateUserVersion())
	}
	alert := SafetyAlert{AlertID: uid(50), Kind: "restore_gate_cleared",
		DetailsJSON: `{"actor_id":"env-admin"}`, RecordedAt: formatTime(testNow)}
	if err := s.ClearRestoreGate(alert); err != nil {
		t.Fatalf("ClearRestoreGate: %v", err)
	}
	if s.RestoreGateEngaged() {
		t.Error("gate still engaged after clear")
	}
	if err := s.ClearRestoreGate(alert); !errors.Is(err, ErrRestoreGateNotEngaged) {
		t.Errorf("second clear: err = %v, want ErrRestoreGateNotEngaged", err)
	}
	rows, err := s.ListSafetyAlertsAfter(0, 10)
	if err != nil {
		t.Fatalf("ListSafetyAlertsAfter: %v", err)
	}
	var cleared []SafetyAlert
	for _, r := range rows {
		if r.Kind == "restore_gate_cleared" {
			cleared = append(cleared, r.SafetyAlert)
		}
	}
	if len(cleared) != 1 || cleared[0].StrategyID != nil || cleared[0].DetailsJSON != alert.DetailsJSON {
		t.Errorf("cleared alert rows = %+v, want exactly one NULL-strategy row with the actor details", cleared)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(restored)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if s2.RestoreGateEngaged() {
		t.Error("gate re-engaged after reopen: the clear did not persist (DS-3 obligation)")
	}
}

// TestClearRestoreGateFailedTxRearms pins the DS-5 failure path: a failed
// clear transaction re-arms the in-memory flag and rolls back the
// user_version reset, so a retry after the failure condition lifts still
// clears and persists. The failure is injected by renaming safety_alerts
// out from under the insert.
func TestClearRestoreGateFailedTxRearms(t *testing.T) {
	src := openStore(t)
	restored := restoredCopy(t, src)

	s, err := Open(restored)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`ALTER TABLE safety_alerts RENAME TO safety_alerts_gone`); err != nil {
		t.Fatalf("rename safety_alerts: %v", err)
	}
	alert := SafetyAlert{AlertID: uid(70), Kind: "restore_gate_cleared",
		DetailsJSON: `{"actor_id":"env-admin"}`, RecordedAt: formatTime(testNow)}
	if err := s.ClearRestoreGate(alert); err == nil || errors.Is(err, ErrRestoreGateNotEngaged) {
		t.Fatalf("clear with a broken tx: err = %v, want a transaction failure", err)
	}
	if !s.RestoreGateEngaged() {
		t.Fatal("gate disengaged after a FAILED clear: the flag was not re-armed")
	}
	var v int64
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("user_version = %d (%v) after failed clear, want 1 (tx rolled back)", v, err)
	}
	if _, err := s.db.Exec(`ALTER TABLE safety_alerts_gone RENAME TO safety_alerts`); err != nil {
		t.Fatalf("restore safety_alerts: %v", err)
	}
	if err := s.ClearRestoreGate(alert); err != nil {
		t.Fatalf("retry after the failure lifted: %v", err)
	}
	if s.RestoreGateEngaged() {
		t.Error("gate still engaged after a successful retry")
	}
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 0 {
		t.Errorf("user_version = %d (%v) after retry, want 0", v, err)
	}
	rows, err := s.ListSafetyAlertsAfter(0, 10)
	if err != nil {
		t.Fatalf("ListSafetyAlertsAfter: %v", err)
	}
	var cleared []SafetyAlert
	for _, r := range rows {
		if r.Kind == "restore_gate_cleared" {
			cleared = append(cleared, r.SafetyAlert)
		}
	}
	if len(cleared) != 1 || cleared[0].AlertID != alert.AlertID {
		t.Errorf("cleared alert rows = %+v, want exactly the retried row", cleared)
	}
}

// TestRestoreGateAlertPendingDedupe pins DS-4: the boot alert is due on
// the first gated boot, NOT due on a second gated boot without an
// intervening clear (one row per ENGAGEMENT), and due again once a
// cleared row is newer than the last engaged row.
func TestRestoreGateAlertPendingDedupe(t *testing.T) {
	src := openStore(t)
	restored := restoredCopy(t, src)

	s, err := Open(restored)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	if pending, err := s.RestoreGateAlertPending(); err != nil || !pending {
		t.Fatalf("first gated boot: pending = %v, %v, want true", pending, err)
	}
	if err := s.AppendSafetyAlert(SafetyAlert{AlertID: uid(60), Kind: "restore_gate_engaged",
		DetailsJSON: `{"user_version": 1}`, RecordedAt: formatTime(testNow)}); err != nil {
		t.Fatalf("AppendSafetyAlert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(restored)
	if err != nil {
		t.Fatalf("second gated boot: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if !s2.RestoreGateEngaged() {
		t.Fatal("second boot: gate not engaged (no clear happened)")
	}
	if pending, err := s2.RestoreGateAlertPending(); err != nil || pending {
		t.Errorf("second gated boot: pending = %v, %v, want false (no crash-loop flooding)", pending, err)
	}
	if err := s2.ClearRestoreGate(SafetyAlert{AlertID: uid(61), Kind: "restore_gate_cleared",
		DetailsJSON: `{"actor_id":"env-admin"}`, RecordedAt: formatTime(testNow)}); err != nil {
		t.Fatalf("ClearRestoreGate: %v", err)
	}
	if pending, err := s2.RestoreGateAlertPending(); err != nil || !pending {
		t.Errorf("after clear: pending = %v, %v, want true (a NEW engagement alerts again)", pending, err)
	}
}
