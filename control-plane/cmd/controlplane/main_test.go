package main

// Serve-wiring alert shapes (deploy-and-survive.md DS-4/DS-9): the
// periodic-backup failure-category mapping and the exact details_json
// bytes of the two safety-alert rows main appends — category/user_version
// only, never raw error text.

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestBackupFailureCategory pins the DS-9 mapping: the two sentinels map
// to their categories, bare or wrapped; everything else — including a
// retention failure after a good artifact — is "io".
func TestBackupFailureCategory(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "verify failed", err: store.ErrBackupVerifyFailed, want: "verify_failed"},
		{name: "verify failed wrapped", want: "verify_failed",
			err: fmt.Errorf("%w: control-20260704T120000Z.db: digest mismatch", store.ErrBackupVerifyFailed)},
		{name: "artifact exists", err: store.ErrBackupExists, want: "artifact_exists"},
		{name: "artifact exists wrapped", want: "artifact_exists",
			err: fmt.Errorf("periodic backup: %w", store.ErrBackupExists)},
		{name: "plain error is io", err: errors.New("open /backups: permission denied"), want: "io"},
		{name: "wrapped plain error is io", err: fmt.Errorf("retention: %w", errors.New("disk full")), want: "io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backupFailureCategory(tc.err); got != tc.want {
				t.Errorf("backupFailureCategory(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestRestoreGateEngagedAlertShape pins the DS-4 row: kind, NULL scope,
// and the exact details bytes — the stamped user_version only.
func TestRestoreGateEngagedAlertShape(t *testing.T) {
	now := formatTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	a := restoreGateEngagedAlert(1, now)
	if a.Kind != "restore_gate_engaged" {
		t.Errorf("kind = %q, want restore_gate_engaged", a.Kind)
	}
	if a.StrategyID != nil || a.RefID != nil {
		t.Errorf("scope = %v/%v, want NULL strategy_id and ref_id", a.StrategyID, a.RefID)
	}
	if a.DetailsJSON != `{"user_version": 1}` {
		t.Errorf("details = %q, want %q", a.DetailsJSON, `{"user_version": 1}`)
	}
	if a.RecordedAt != now {
		t.Errorf("recorded_at = %q, want %q", a.RecordedAt, now)
	}
	if a.AlertID == "" {
		t.Error("alert_id is empty")
	}
	assertDetailKeys(t, a.DetailsJSON, "user_version")
}

// TestBackupFailedAlertShape pins the DS-9 row: kind, NULL scope, and the
// exact details bytes — trigger and category only, no raw error text.
func TestBackupFailedAlertShape(t *testing.T) {
	now := formatTime(time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC))
	a := backupFailedAlert(backupFailureCategory(errors.New("open /backups: permission denied")), now)
	if a.Kind != "backup_failed" {
		t.Errorf("kind = %q, want backup_failed", a.Kind)
	}
	if a.StrategyID != nil || a.RefID != nil {
		t.Errorf("scope = %v/%v, want NULL strategy_id and ref_id", a.StrategyID, a.RefID)
	}
	if a.DetailsJSON != `{"trigger": "periodic", "category": "io"}` {
		t.Errorf("details = %q, want %q", a.DetailsJSON, `{"trigger": "periodic", "category": "io"}`)
	}
	if a.RecordedAt != now {
		t.Errorf("recorded_at = %q, want %q", a.RecordedAt, now)
	}
	if a.AlertID == "" {
		t.Error("alert_id is empty")
	}
	assertDetailKeys(t, a.DetailsJSON, "trigger", "category")
}

// assertDetailKeys requires details_json to carry EXACTLY the given keys —
// in particular no field carrying raw error text (DS-9 hygiene).
func assertDetailKeys(t *testing.T, details string, keys ...string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(details), &m); err != nil {
		t.Fatalf("details %q: %v", details, err)
	}
	if len(m) != len(keys) {
		t.Fatalf("details %q carries %d fields, want exactly %v", details, len(m), keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("details %q missing key %q", details, k)
		}
	}
}
