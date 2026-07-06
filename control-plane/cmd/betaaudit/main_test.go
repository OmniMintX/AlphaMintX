package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// newFixture creates a DB with the authoritative schema (store.Open runs
// the DDL plus every migration, so tenant_id and alert_dispatch_state
// exist exactly as in production).
func newFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return path
}

// seed executes raw statements against the fixture (checkpointed back to
// the main file on close so immutable=1 readers see everything).
func seed(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
}

// runAudit runs the tool and returns exit code, stdout, stderr.
func runAudit(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// auditJSON runs with -json and returns the parsed report.
func auditJSON(t *testing.T, args ...string) (int, report) {
	t.Helper()
	code, out, errOut := runAudit(append(args, "-json")...)
	var rep report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("parse report: %v\nstdout: %s\nstderr: %s", err, out, errOut)
	}
	return code, rep
}

func verdictOf(t *testing.T, rep report, id string) check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %s missing from report", id)
	return check{}
}

func TestUsageErrors(t *testing.T) {
	if code, _, _ := runAudit(); code != 2 {
		t.Fatalf("no input: code = %d, want 2", code)
	}
	if code, _, _ := runAudit("-artifact", "a", "-db", "b"); code != 2 {
		t.Fatalf("both inputs: code = %d, want 2", code)
	}
}

func TestCleanFixturePasses(t *testing.T) {
	path := newFixture(t)
	seed(t, path, "PRAGMA user_version = 7")
	code, rep := auditJSON(t, "-artifact", path)
	if code != 0 {
		t.Fatalf("clean fixture: code = %d, want 0", code)
	}
	for _, c := range rep.Checks {
		if c.Verdict == "FAIL" {
			t.Errorf("clean fixture: %s = FAIL (%v)", c.ID, c.Findings)
		}
	}
	if rep.Header.InputSHA256 == "" {
		t.Error("header missing artifact sha256")
	}
	if rep.Header.StartedAt == "" {
		t.Error("header missing started_at")
	}
	if rep.Header.UserVersion != 7 {
		t.Errorf("header user_version = %d, want 7 (must reflect the input's PRAGMA)", rep.Header.UserVersion)
	}
	if _, ok := rep.Header.MaxRowids["fills"]; !ok {
		t.Error("header missing max_rowids for fills")
	}
}

func TestV1aOrderWithoutVerdict(t *testing.T) {
	path := newFixture(t)
	seed(t, path, `INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol,
		class, side, type, reduce_only, qty_base, kill_epoch, status, submitted_at)
		VALUES ('o1', NULL, 'proposal', 's1', 'BTCUSDT', 'ENTRY', 'BUY', 'LIMIT', 0,
		'0.1', 0, 'submitted', '2026-07-01T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	c := verdictOf(t, rep, "V1a")
	if c.Verdict != "FAIL" || len(c.Findings) != 1 || c.Findings[0] != "o1" {
		t.Fatalf("V1a = %+v, want FAIL [o1]", c)
	}
}

func TestV1bOrderAgainstRejectAndUnapprovedEscalate(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO proposals (proposal_id, strategy_id, symbol, action, created_at, payload_json, payload_sha256)
		 VALUES ('p1', 's1', 'BTCUSDT', 'open_long', '2026-07-01T00:00:00Z', '{}', 'x')`,
		`INSERT INTO verdicts (verdict_id, proposal_id, decision, evaluated_at, payload_json)
		 VALUES ('v1', 'p1', 'reject', '2026-07-01T00:00:01Z', '{}')`,
		`INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		 reduce_only, qty_base, kill_epoch, status, submitted_at)
		 VALUES ('o1', 'p1', 'proposal', 's1', 'BTCUSDT', 'ENTRY', 'BUY', 'LIMIT', 0, '0.1', 0,
		 'submitted', '2026-07-01T00:00:02Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V1b"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V1b = %+v, want FAIL [o1]", c)
	}
}

// TestV1bEscalateBranch: an escalate-verdict order FAILs without an
// approvals row whose outcome is exactly 'approved'; a row with any
// other outcome does not heal it, and 'approved' does. Guards the
// one-character regression traps ('approve', 'escalated').
func TestV1bEscalateBranch(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO proposals (proposal_id, strategy_id, symbol, action, created_at, payload_json, payload_sha256)
		 VALUES ('p2', 's1', 'BTCUSDT', 'open_long', '2026-07-01T00:00:00Z', '{}', 'x')`,
		`INSERT INTO verdicts (verdict_id, proposal_id, decision, evaluated_at, payload_json)
		 VALUES ('v2', 'p2', 'escalate', '2026-07-01T00:00:01Z', '{}')`,
		`INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		 reduce_only, qty_base, kill_epoch, status, submitted_at)
		 VALUES ('o2', 'p2', 'proposal', 's1', 'BTCUSDT', 'ENTRY', 'BUY', 'LIMIT', 0, '0.1', 0,
		 'submitted', '2026-07-01T00:00:02Z')`,
		// Control pair: escalate WITH outcome='approved' must not appear.
		`INSERT INTO proposals (proposal_id, strategy_id, symbol, action, created_at, payload_json, payload_sha256)
		 VALUES ('p3', 's1', 'ETHUSDT', 'open_long', '2026-07-01T00:00:00Z', '{}', 'x')`,
		`INSERT INTO verdicts (verdict_id, proposal_id, decision, evaluated_at, payload_json)
		 VALUES ('v3', 'p3', 'escalate', '2026-07-01T00:00:01Z', '{}')`,
		`INSERT INTO approvals (approval_id, verdict_id, proposal_id, outcome, decided_by, decided_at, timeout_seconds)
		 VALUES ('ap3', 'v3', 'p3', 'approved', 'op', '2026-07-01T00:00:03Z', 300)`,
		`INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		 reduce_only, qty_base, kill_epoch, status, submitted_at)
		 VALUES ('o3', 'p3', 'proposal', 's1', 'ETHUSDT', 'ENTRY', 'BUY', 'LIMIT', 0, '0.1', 0,
		 'submitted', '2026-07-01T00:00:04Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V1b"); c.Verdict != "FAIL" || len(c.Findings) != 1 || c.Findings[0] != "o2" {
		t.Fatalf("V1b = %+v, want FAIL exactly [o2]", c)
	}
	// A non-'approved' outcome (timeout) does not heal.
	seed(t, path, `INSERT INTO approvals (approval_id, verdict_id, proposal_id, outcome, decided_by, decided_at, timeout_seconds)
		 VALUES ('ap2', 'v2', 'p2', 'timeout', 'op', '2026-07-01T00:00:05Z', 300)`)
	if _, rep := auditJSON(t, "-artifact", path); verdictOf(t, rep, "V1b").Verdict != "FAIL" {
		t.Fatal("V1b must still FAIL with a non-approved approvals outcome")
	}
}

func TestV1cIllegalOrigin(t *testing.T) {
	path := newFixture(t)
	// The live schema's CHECK blocks this insert; the audit query is
	// defense-in-depth behind it, so the fixture disables check
	// enforcement to plant the row.
	seed(t, path, `PRAGMA ignore_check_constraints = ON`,
		`INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		 reduce_only, qty_base, kill_epoch, status, submitted_at)
		 VALUES ('o1', NULL, 'manual', 's1', 'BTCUSDT', 'ENTRY', 'BUY', 'LIMIT', 0, '0.1', 0,
		 'submitted', '2026-07-01T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V1c"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V1c = %+v, want FAIL [o1]", c)
	}
}

func TestV2aOverdueObligationWithoutContingency(t *testing.T) {
	path := newFixture(t)
	entry := `INSERT INTO orders (order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		 reduce_only, qty_base, kill_epoch, status, submitted_at)
		 VALUES ('e1', NULL, 'kill', 's1', 'BTCUSDT', 'ENTRY', 'BUY', 'LIMIT', 0, '0.1', 0,
		 'filled', '2026-07-01T00:00:00Z')`
	obligation := `INSERT INTO protective_obligations (obligation_id, entry_order_id, strategy_id,
		 kind, due_at, created_at, satisfied_at)
		 VALUES ('ob1', 'e1', 's1', 'sl', '2026-07-01T00:05:00Z', '2026-07-01T00:00:00Z', NULL)`
	seed(t, path, entry, obligation)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V2a"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V2a = %+v, want FAIL [ob1]", c)
	}
	// The contingency event (same strategy+symbol, at/after due_at) heals it.
	seed(t, path, `INSERT INTO oms_recon_events (event_id, kind, strategy_id, symbol,
		 details_json, recorded_at)
		 VALUES ('ev1', 'sl_deadline_contingency', 's1', 'BTCUSDT', '{}', '2026-07-01T00:06:00Z')`)
	if _, rep := auditJSON(t, "-artifact", path); verdictOf(t, rep, "V2a").Verdict != "PASS" {
		t.Fatal("V2a should PASS once the contingency event exists")
	}
}

func TestV4aStalledSafetyEffect(t *testing.T) {
	path := newFixture(t)
	seed(t, path, `INSERT INTO kill_breaker_events (event_id, kind, scope, strategy_id,
		 kill_epoch, flatten, trigger_ref, actor_id, recorded_at)
		 VALUES ('k1', 'kill', 'strategy', 's1', 1, 1, NULL, 'operator', '2026-07-01T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V4a"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V4a = %+v, want FAIL [k1]", c)
	}
	seed(t, path, `INSERT INTO safety_effects (event_id, completed_at)
		 VALUES ('k1', '2026-07-01T00:00:05Z')`)
	if _, rep := auditJSON(t, "-artifact", path); verdictOf(t, rep, "V4a").Verdict != "PASS" {
		t.Fatal("V4a should PASS once the completion row exists")
	}
}

func TestV4bKilledExitWithoutClear(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO strategies (strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at)
		 VALUES ('s1', 't1', 'S1', 'paused', '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z')`,
		`INSERT INTO lifecycle_transitions (transition_id, strategy_id, from_state, to_state,
		 actor_id, actor_role, reason, recorded_at)
		 VALUES ('tr1', 's1', 'killed', 'paused', 'op', 'admin', 'resume', '2026-07-02T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V4b"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V4b = %+v, want FAIL [tr1]", c)
	}
	// A tenant-scope clear at-or-before the transition covers it (LC-28).
	seed(t, path, `INSERT INTO kill_clear_events (clear_id, scope, strategy_id, tenant_id,
		 cleared_epoch, actor_id, reason, recorded_at)
		 VALUES ('c1', 'tenant', NULL, 't1', 1, 'op', 'cleared', '2026-07-01T12:00:00Z')`)
	if _, rep := auditJSON(t, "-artifact", path); verdictOf(t, rep, "V4b").Verdict != "PASS" {
		t.Fatal("V4b should PASS once a covering clear exists")
	}
}

func TestV4cKillWithoutLifecycleLock(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO strategies (strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at)
		 VALUES ('s1', 't1', 'S1', 'live_testnet', '2026-07-01T00:00:00Z', '2026-07-01T00:00:00Z')`,
		`INSERT INTO lifecycle_transitions (transition_id, strategy_id, from_state, to_state,
		 actor_id, actor_role, reason, recorded_at)
		 VALUES ('tr1', 's1', 'paper', 'live_testnet', 'op', 'admin', 'go live', '2026-07-01T00:00:00Z')`,
		`INSERT INTO kill_breaker_events (event_id, kind, scope, strategy_id, kill_epoch, flatten,
		 trigger_ref, actor_id, recorded_at)
		 VALUES ('k1', 'kill', 'strategy', 's1', 1, 1, NULL, 'operator', '2026-07-05T00:00:00Z')`,
		`INSERT INTO safety_effects (event_id, completed_at) VALUES ('k1', '2026-07-05T00:00:05Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V4c"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V4c = %+v, want FAIL [k1/s1]", c)
	}
	seed(t, path, `INSERT INTO lifecycle_transitions (transition_id, strategy_id, from_state, to_state,
		 actor_id, actor_role, reason, recorded_at)
		 VALUES ('tr2', 's1', 'live_testnet', 'killed', 'operator', 'system',
		 'kill-switch event k1', '2026-07-05T00:00:01Z')`)
	if _, rep := auditJSON(t, "-artifact", path); verdictOf(t, rep, "V4c").Verdict != "PASS" {
		t.Fatal("V4c should PASS once the lifecycle lock row exists")
	}
}

func TestV5aEscalationAlertWithoutKillRow(t *testing.T) {
	path := newFixture(t)
	seed(t, path, `INSERT INTO safety_alerts (alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		 VALUES ('a1', 'watchdog_kill_escalation', 's1', 'missing-event', '{}', '2026-07-05T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V5a"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V5a = %+v, want FAIL [a1]", c)
	}
}

func TestV7aDanglingFill(t *testing.T) {
	path := newFixture(t)
	seed(t, path, `INSERT INTO fills (fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts)
		 VALUES ('f1', 'ghost', '0.1', '50000', '0.05', '2026-07-01T00:00:00Z')`)
	code, rep := auditJSON(t, "-artifact", path)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if c := verdictOf(t, rep, "V7a"); c.Verdict != "FAIL" || len(c.Findings) != 1 {
		t.Fatalf("V7a = %+v, want FAIL [fill:f1]", c)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func TestV7bShrinkMutationAppend(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO safety_alerts (alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		 VALUES ('a1', 'test_alert', 's1', NULL, '{}', '2026-07-01T00:00:00Z')`,
		`INSERT INTO safety_alerts (alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		 VALUES ('a2', 'test_alert', 's1', NULL, '{}', '2026-07-01T00:01:00Z')`)
	ref := filepath.Join(t.TempDir(), "ref.db")
	copyFile(t, path, ref)

	// Pure append: PASS.
	appended := filepath.Join(t.TempDir(), "appended.db")
	copyFile(t, path, appended)
	seed(t, appended, `INSERT INTO safety_alerts (alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		 VALUES ('a3', 'test_alert', 's1', NULL, '{}', '2026-07-01T00:02:00Z')`)
	code, rep := auditJSON(t, "-artifact", appended, "-ref-artifact", ref)
	if code != 0 {
		t.Fatalf("append-only: code = %d, want 0", code)
	}
	if c := verdictOf(t, rep, "V7b"); c.Verdict != "PASS" {
		t.Fatalf("V7b = %+v, want PASS on pure append", c)
	}

	// Row deletion (shrink): FAIL.
	shrunk := filepath.Join(t.TempDir(), "shrunk.db")
	copyFile(t, path, shrunk)
	seed(t, shrunk, `DELETE FROM safety_alerts WHERE alert_id = 'a2'`)
	if _, rep := auditJSON(t, "-artifact", shrunk, "-ref-artifact", ref); verdictOf(t, rep, "V7b").Verdict != "FAIL" {
		t.Fatal("V7b should FAIL on row deletion")
	}

	// Column mutation: FAIL.
	mutated := filepath.Join(t.TempDir(), "mutated.db")
	copyFile(t, path, mutated)
	seed(t, mutated, `UPDATE safety_alerts SET details_json = '{"doctored":true}' WHERE alert_id = 'a1'`)
	if _, rep := auditJSON(t, "-artifact", mutated, "-ref-artifact", ref); verdictOf(t, rep, "V7b").Verdict != "FAIL" {
		t.Fatal("V7b should FAIL on column mutation")
	}

	// -db input: V7b must be skipped as MANUAL, not computed.
	if _, rep := auditJSON(t, "-db", path, "-ref-artifact", ref); verdictOf(t, rep, "V7b").Verdict != "MANUAL" {
		t.Fatal("V7b with -db input should be MANUAL (both sides must be artifacts)")
	}
}

func TestV9aLogCoverage(t *testing.T) {
	path := newFixture(t)
	seed(t, path, `INSERT INTO risk_limit_changes (change_id, strategy_id, field, old_value, new_value,
		 actor_id, changed_at)
		 VALUES ('ch1', 's1', 'max_notional_usdt', '100', '200', 'op', '2026-07-02T00:00:00Z')`)
	// Without a matching limit_change entry: FAIL.
	emptyLog := filepath.Join(t.TempDir(), "beta.log")
	if err := os.WriteFile(emptyLog, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	code, rep := auditJSON(t, "-artifact", path, "-strategy", "s1", "-log", emptyLog)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	c := verdictOf(t, rep, "V9a")
	if c.Verdict != "FAIL" || len(c.Findings) != 1 || c.Findings[0] != "ch1" {
		t.Fatalf("V9a = %+v, want FAIL [ch1]", c)
	}
	// Classification note: 100 -> 200 is LOOSENING.
	if !strings.Contains(strings.Join(c.Notes, "\n"), "LOOSENING") {
		t.Errorf("V9a notes lack LOOSENING classification: %v", c.Notes)
	}
	// With the limit_change entry: PASS (coverage only).
	covered := filepath.Join(t.TempDir(), "beta2.log")
	entry := `{"n":1,"prev":"` + strings.Repeat("0", 64) + `","at":"2026-07-02T00:01:00Z","type":"limit_change","text":"raised cap","refs":{"change_id":"ch1"}}` + "\n"
	if err := os.WriteFile(covered, []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, rep := auditJSON(t, "-artifact", path, "-strategy", "s1", "-log", covered); verdictOf(t, rep, "V9a").Verdict != "PASS" {
		t.Fatal("V9a should PASS when every change_id is covered")
	}
	// Without -log: report-only, PASS.
	if _, rep := auditJSON(t, "-artifact", path, "-strategy", "s1"); verdictOf(t, rep, "V9a").Verdict != "PASS" {
		t.Fatal("V9a without -log should be report-only PASS")
	}
}

func TestM6aManualWithNumbers(t *testing.T) {
	path := newFixture(t)
	seed(t, path,
		`INSERT INTO safety_alerts (alert_id, kind, strategy_id, ref_id, details_json, recorded_at)
		 VALUES ('a1', 'test_alert', 's1', NULL, '{}', '2026-07-01T00:00:00Z')`,
		`INSERT INTO alert_dispatch_state (source, last_rowid, updated_at)
		 VALUES ('safety_alerts', 0, '2026-07-01T00:00:00Z')`)
	_, rep := auditJSON(t, "-artifact", path)
	c := verdictOf(t, rep, "M6a")
	if c.Verdict != "MANUAL" {
		t.Fatalf("M6a = %s, want MANUAL", c.Verdict)
	}
	notes := strings.Join(c.Notes, "\n")
	if !strings.Contains(notes, "safety_alerts: watermark=0 max(rowid)=1 lag=1") {
		t.Errorf("M6a notes lack lag numbers: %v", c.Notes)
	}
}

func TestManualChecksPresent(t *testing.T) {
	path := newFixture(t)
	_, rep := auditJSON(t, "-artifact", path)
	for _, id := range []string{"V2-residency", "V3", "V5-timing", "V6", "V8"} {
		c := verdictOf(t, rep, id)
		if c.Verdict != "MANUAL" || c.Status != "OPEN" || c.Manual == "" {
			t.Errorf("%s = %+v, want MANUAL/OPEN with procedure text", id, c)
		}
	}
}

func TestReadOnlyAndDBWarning(t *testing.T) {
	path := newFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runAudit("-db", path)
	if code != 0 {
		t.Fatalf("clean -db run: code = %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(errOut, "WARNING") || !strings.Contains(errOut, "wal_checkpoint") {
		t.Errorf("-db must print the live-WAL warning, got: %s", errOut)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("db file bytes changed across a read-only audit run")
	}
}
