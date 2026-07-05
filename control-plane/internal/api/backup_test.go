package api

// ops-backup API tests (docs/specs/ops-backup.md §Test obligations, api
// side): 404 when unconfigured, the OB-6 response shape, the specific 409
// and 500 codes (never the uniform INTERNAL for engine failures), the OB-7
// list shape with items never null, and no rate charge on the GET.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// fakeBackupEngine is the test BackupEngine (spec §Wiring seams: the RBAC
// env MUST wire one so both requiresBackup rows register and the
// DeepEqual(routes, Permissions()) pin holds).
type fakeBackupEngine struct {
	runRes  store.BackupResult
	runErr  error
	items   []store.BackupInfo
	listErr error
}

func (f *fakeBackupEngine) Run(context.Context) (store.BackupResult, error) {
	return f.runRes, f.runErr
}

func (f *fakeBackupEngine) List() ([]store.BackupInfo, error) { return f.items, f.listErr }

// backupEnv is newEnv with only the backup engine wired.
func backupEnv(t *testing.T, f *fakeBackupEngine) *testEnv {
	t.Helper()
	return newEnv(t, func(cfg *Config) { cfg.Backup = f })
}

// TestBackupRoutesUnconfigured: without a BackupEngine both routes are
// UNREGISTERED — 404, not 403 (invariant 6: the surface is invisible
// unless explicitly configured).
func TestBackupRoutesUnconfigured(t *testing.T) {
	e := newEnv(t, nil)
	if rec := e.do(t, "POST", "/api/v1/ops/backups/run", adminTok, nil); rec.Code != http.StatusNotFound {
		t.Errorf("POST run: status = %d, want 404", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/v1/ops/backups", adminTok, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET list: status = %d, want 404", rec.Code)
	}
}

// TestBackupRunResponseShape: the OB-6 success body, field for field.
func TestBackupRunResponseShape(t *testing.T) {
	f := &fakeBackupEngine{runRes: store.BackupResult{
		Artifact: "control-20260704T120000Z.db", Bytes: 12345, SHA256: "ab12",
		Tables: 7, RowsTotal: 42,
		StartedAt: testNow, FinishedAt: testNow.Add(2 * time.Second), Verified: true,
	}}
	e := backupEnv(t, f)
	rec := e.do(t, "POST", "/api/v1/ops/backups/run", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var got map[string]any
	decodeJSON(t, rec, &got)
	want := map[string]any{
		"artifact": "control-20260704T120000Z.db", "bytes": float64(12345),
		"sha256": "ab12", "tables": float64(7), "rows_total": float64(42),
		"started_at":  testNow.UTC().Format(time.RFC3339),
		"finished_at": testNow.Add(2 * time.Second).UTC().Format(time.RFC3339),
		"verified":    true,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("response has %d fields %v, want exactly %d", len(got), got, len(want))
	}
}

// TestBackupRunErrorMapping: the engine sentinels map to their specific
// codes — the 500s deliberately bypass the uniform INTERNAL envelope.
func TestBackupRunErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"in progress", store.ErrBackupInProgress, 409, codeBackupInProgress},
		{"exists", store.ErrBackupExists, 409, codeBackupExists},
		{"verify failed", fmt.Errorf("%w: detail", store.ErrBackupVerifyFailed), 500, codeBackupVerifyFailed},
		{"plain failure", errors.New("disk full"), 500, codeBackupFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := backupEnv(t, &fakeBackupEngine{
				runErr: tc.err,
				runRes: store.BackupResult{Artifact: "control-20260704T120000Z.db"},
			})
			wantError(t, e.do(t, "POST", "/api/v1/ops/backups/run", adminTok, nil), tc.status, tc.code)
		})
	}
}

// TestBackupListShape: newest-first order preserved from the engine, exact
// item fields, items NEVER null when empty (OB-7), engine errors 500.
func TestBackupListShape(t *testing.T) {
	f := &fakeBackupEngine{items: []store.BackupInfo{
		{Artifact: "control-20260704T120000Z.db", Bytes: 9, ModifiedAt: testNow},
		{Artifact: "control-20260703T120000Z.db", Bytes: 5, ModifiedAt: testNow.Add(-24 * time.Hour)},
	}}
	e := backupEnv(t, f)
	rec := e.do(t, "GET", "/api/v1/ops/backups", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	decodeJSON(t, rec, &got)
	if len(got.Items) != 2 || got.Items[0]["artifact"] != "control-20260704T120000Z.db" ||
		got.Items[0]["bytes"] != float64(9) ||
		got.Items[0]["modified_at"] != testNow.UTC().Format(time.RFC3339) {
		t.Errorf("items = %v, want the engine order with artifact/bytes/modified_at", got.Items)
	}

	empty := backupEnv(t, &fakeBackupEngine{})
	rec = empty.do(t, "GET", "/api/v1/ops/backups", adminTok, nil)
	var emptyGot struct {
		Items []any `json:"items"`
	}
	decodeJSON(t, rec, &emptyGot)
	if emptyGot.Items == nil {
		t.Errorf("empty list body = %q, want items [] never null", rec.Body.String())
	}

	broken := backupEnv(t, &fakeBackupEngine{listErr: errors.New("readdir")})
	wantError(t, broken.do(t, "GET", "/api/v1/ops/backups", adminTok, nil), 500, codeInternal)
}

// TestBackupListNoRateCharge: the read-only GET never charges the
// per-token bucket (the guard charges non-GETs only) — under the FIXED
// test clock the bucket never refills, so a charging GET would 429 at
// request 61.
func TestBackupListNoRateCharge(t *testing.T) {
	e := backupEnv(t, &fakeBackupEngine{})
	for i := 0; i < rateLimitBurst+5; i++ {
		if rec := e.do(t, "GET", "/api/v1/ops/backups", adminTok, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET #%d: status = %d (body %q), want 200", i+1, rec.Code, rec.Body.String())
		}
	}
}
