// Command betaaudit runs the BP-8 V1-V9 audit queries
// (docs/specs/beta-ops-tooling.md BA-1..BA-4) against a backup artifact
// (recommended, mode=ro&immutable=1) or the live DB (spot checks only,
// mode=ro). It performs ZERO writes, never runs VACUUM or any mutating
// PRAGMA, and prints every executed query (BP-8: audits "logged with the
// queries used"). Exit 0 iff no FAIL; MANUAL never affects the exit code.
package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// appendOnlyTables is the NORMATIVE V7b set (spec BA-3): tables whose
// rows never legally mutate. orders/runs/protective_obligations/
// order_intents etc. are excluded because their columns legally mutate.
var appendOnlyTables = []string{
	"proposals", "verdicts", "approvals", "fills",
	"lifecycle_transitions", "kill_breaker_events", "kill_clear_events",
	"safety_alerts", "safety_effects", "oms_recon_events",
	"risk_limit_changes", "rejected_submissions", "token_events",
	"venue_epochs", "agent_traces", "model_costs",
}

// alertSources is the AN-1 whitelist: source wire name -> source table
// (store/alertdispatch.go; names coincide by design).
var alertSources = []string{"kill_breaker_events", "kill_clear_events", "safety_alerts"}

type check struct {
	ID       string   `json:"id"`
	Verdict  string   `json:"verdict"` // PASS | FAIL | MANUAL
	Status   string   `json:"status,omitempty"`
	Count    int      `json:"finding_count"`
	Findings []string `json:"findings,omitempty"` // first <= 20 ids
	Queries  []string `json:"queries,omitempty"`
	Notes    []string `json:"notes,omitempty"`
	Manual   string   `json:"manual_procedure,omitempty"`
}

type header struct {
	InputPath   string           `json:"input_path"`
	InputMode   string           `json:"input_mode"` // artifact | db
	InputSHA256 string           `json:"input_sha256,omitempty"`
	SHANote     string           `json:"sha_note,omitempty"`
	RefPath     string           `json:"ref_artifact_path,omitempty"`
	RefSHA256   string           `json:"ref_artifact_sha256,omitempty"`
	UserVersion int64            `json:"user_version"`
	MaxRowids   map[string]int64 `json:"max_rowids"`
	StartedAt   string           `json:"audit_started_at"`
}

type report struct {
	Header header  `json:"header"`
	Checks []check `json:"checks"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("betaaudit", flag.ContinueOnError)
	fs.SetOutput(errOut)
	artifact := fs.String("artifact", "", "backup artifact path (RECOMMENDED; mode=ro&immutable=1)")
	dbPath := fs.String("db", "", "live DB path (spot checks only; mode=ro)")
	refArtifact := fs.String("ref-artifact", "", "older reference artifact for V7b (requires -artifact)")
	strategy := fs.String("strategy", "", "certified strategy id for V9a")
	logPath := fs.String("log", "", "betalog file for V9a limit_change coverage")
	stallSeconds := fs.Int("stall-seconds", 600, "V4a safety-effect stall threshold (safety_effect_stall_seconds)")
	jsonOut := fs.Bool("json", false, "emit the report as a single JSON object")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*artifact == "") == (*dbPath == "") {
		fmt.Fprintln(errOut, "betaaudit: exactly one of -artifact or -db is required")
		fs.Usage()
		return 2
	}
	a := &audit{startedAt: time.Now().UTC()}
	var path, dsn string
	if *artifact != "" {
		path, a.mode = *artifact, "artifact"
		dsn = "file:" + url.PathEscape(path) + "?mode=ro&immutable=1"
	} else {
		path, a.mode = *dbPath, "db"
		dsn = "file:" + url.PathEscape(path) + "?mode=ro&_pragma=busy_timeout(5000)"
		fmt.Fprintln(errOut, "betaaudit: WARNING: -db reads the live WAL file; a long read blocks the"+
			" backup's wal_checkpoint(TRUNCATE) (store/backup.go) and can manufacture backup_failed"+
			" incidents. Prefer -artifact (BA-1).")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		fmt.Fprintf(errOut, "betaaudit: open %s: %v\n", path, err)
		return 1
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a.db = db

	if err := a.buildHeader(path, *refArtifact); err != nil {
		fmt.Fprintf(errOut, "betaaudit: %v\n", err)
		return 1
	}
	if err := a.runChecks(*refArtifact, *strategy, *logPath, *stallSeconds); err != nil {
		fmt.Fprintf(errOut, "betaaudit: %v\n", err)
		return 1
	}
	rep := report{Header: a.hdr, Checks: a.checks}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(errOut, "betaaudit: encode: %v\n", err)
			return 1
		}
	} else {
		printText(out, rep)
	}
	for _, c := range a.checks {
		if c.Verdict == "FAIL" {
			return 1
		}
	}
	return 0
}

type audit struct {
	db        *sql.DB
	mode      string
	startedAt time.Time
	hdr       header
	checks    []check
}

// buildHeader binds the report to its inputs (BA-2): path, mode, artifact
// SHA-256 (the live DB is not stably hashable), user_version, per-table
// max(rowid) for the V7b set, and the audit start time.
func (a *audit) buildHeader(path, refArtifact string) error {
	a.hdr = header{
		InputPath: path, InputMode: a.mode,
		MaxRowids: map[string]int64{},
		StartedAt: a.startedAt.Format(time.RFC3339),
	}
	if a.mode == "artifact" {
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		a.hdr.InputSHA256 = sum
	} else {
		a.hdr.SHANote = "live DB: not stably hashable (WAL); no input hash"
	}
	if refArtifact != "" {
		sum, err := fileSHA256(refArtifact)
		if err != nil {
			return err
		}
		a.hdr.RefPath, a.hdr.RefSHA256 = refArtifact, sum
	}
	if err := a.db.QueryRow("PRAGMA user_version").Scan(&a.hdr.UserVersion); err != nil {
		return fmt.Errorf("user_version: %w", err)
	}
	for _, t := range appendOnlyTables {
		var max sql.NullInt64
		if err := a.db.QueryRow("SELECT MAX(rowid) FROM " + t).Scan(&max); err != nil {
			return fmt.Errorf("max rowid %s: %w", t, err)
		}
		a.hdr.MaxRowids[t] = max.Int64
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// queryCheck runs one FAIL-check query returning a single id column and
// records the verdict.
func (a *audit) queryCheck(id, query string, notes []string, args ...any) error {
	ids, err := queryStrings(a.db, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", id, err)
	}
	c := check{ID: id, Verdict: "PASS", Count: len(ids), Queries: []string{query}, Notes: notes}
	if len(ids) > 0 {
		c.Verdict = "FAIL"
		if len(ids) > 20 {
			ids = ids[:20]
		}
		c.Findings = ids
	}
	a.checks = append(a.checks, c)
	return nil
}

func (a *audit) manual(id, procedure string, notes ...string) {
	a.checks = append(a.checks, check{ID: id, Verdict: "MANUAL", Status: "OPEN",
		Manual: procedure, Notes: notes})
}

func queryStrings(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// runChecks executes BA-3 in order. now is the audit start time (V2a/V4a
// compare against it, never against wall-clock re-reads).
func (a *audit) runChecks(refArtifact, strategy, logPath string, stallSeconds int) error {
	now := a.startedAt.Format(time.RFC3339)

	// V1a: proposal-originated orders without a verdict chain.
	if err := a.queryCheck("V1a", `SELECT o.order_id FROM orders o
  WHERE o.origin = 'proposal' AND (o.proposal_id IS NULL
    OR NOT EXISTS (SELECT 1 FROM verdicts v WHERE v.proposal_id = o.proposal_id))
  ORDER BY o.order_id`, nil); err != nil {
		return err
	}
	// V1b: order paired with reject, or escalate without outcome exactly
	// 'approved' (approved_but_blocked means NO order exists).
	if err := a.queryCheck("V1b", `SELECT o.order_id FROM orders o
  JOIN verdicts v ON v.proposal_id = o.proposal_id
  WHERE o.origin = 'proposal' AND (v.decision = 'reject'
    OR (v.decision = 'escalate' AND NOT EXISTS (SELECT 1 FROM approvals ap
        WHERE ap.verdict_id = v.verdict_id AND ap.outcome = 'approved')))
  ORDER BY o.order_id`,
		[]string{"approved_but_blocked/rejected/timeout paired with an order is the unexplained order V1 exists to catch"}); err != nil {
		return err
	}
	// V1c: origin outside the legal set (defense-in-depth behind the CHECK).
	if err := a.queryCheck("V1c", `SELECT order_id FROM orders
  WHERE origin NOT IN ('proposal','breaker','kill','watchdog','sl_contingency')
  ORDER BY order_id`, nil); err != nil {
		return err
	}
	// V2a: overdue unsatisfied protective obligations without the
	// sl_deadline_contingency event, matched per (strategy_id, symbol).
	if err := a.queryCheck("V2a", `SELECT po.obligation_id FROM protective_obligations po
  JOIN orders o ON o.order_id = po.entry_order_id
  WHERE po.satisfied_at IS NULL AND po.due_at < ?1
    AND NOT EXISTS (SELECT 1 FROM oms_recon_events e
      WHERE e.kind = 'sl_deadline_contingency' AND e.strategy_id = po.strategy_id
        AND e.symbol = o.symbol AND e.recorded_at >= po.due_at)
  ORDER BY po.obligation_id`,
		[]string{"match is per (strategy_id, symbol) — AMBIGUOUS when multiple entries share a symbol (the contingency event carries no order id)"},
		now); err != nil {
		return err
	}
	a.manual("V2-residency", "Verify exchange-side SL/TP residency for open positions"+
		" (venue UI/API: stops rest ON the venue) and that no agent-plane path mutates them (BP-8 V2).")
	// V4a: kill/breaker rows older than the stall threshold with no
	// safety_effects completion row.
	stallCutoff := a.startedAt.Add(-time.Duration(stallSeconds) * time.Second).Format(time.RFC3339)
	if err := a.queryCheck("V4a", `SELECT e.event_id FROM kill_breaker_events e
  WHERE e.recorded_at < ?1
    AND NOT EXISTS (SELECT 1 FROM safety_effects se WHERE se.event_id = e.event_id)
  ORDER BY e.event_id`,
		[]string{fmt.Sprintf("stall threshold %ds (safety_effect_stall_seconds); valid only against live-OMS deployments (LC-38)", stallSeconds)},
		stallCutoff); err != nil {
		return err
	}
	// V4b: exit from 'killed' without a scope-covering clear at-or-before
	// the transition (LC-28 three-clause cover, store/killclear.go).
	if err := a.queryCheck("V4b", `SELECT t.transition_id FROM lifecycle_transitions t
  WHERE t.from_state = 'killed'
    AND NOT EXISTS (SELECT 1 FROM kill_clear_events c
      WHERE c.recorded_at <= t.recorded_at AND (
        (c.scope = 'strategy' AND c.strategy_id = t.strategy_id)
        OR (c.scope = 'tenant' AND c.tenant_id =
            (SELECT tenant_id FROM strategies s WHERE s.strategy_id = t.strategy_id))
        OR c.scope = 'platform'))
  ORDER BY t.transition_id`, nil); err != nil {
		return err
	}
	// V4c: kill row covering a strategy that was live_* at kill time must
	// pair with a system-actor transition to 'killed' at-or-after the kill.
	if err := a.queryCheck("V4c", `SELECT e.event_id || '/' || s.strategy_id
  FROM kill_breaker_events e
  JOIN strategies s ON ((e.strategy_id = s.strategy_id)
    OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL AND e.tenant_id = s.tenant_id)
    OR (e.strategy_id IS NULL AND e.tenant_id IS NULL))
  WHERE e.kind = 'kill'
    AND (SELECT lt.to_state FROM lifecycle_transitions lt
         WHERE lt.strategy_id = s.strategy_id AND lt.recorded_at <= e.recorded_at
         ORDER BY lt.recorded_at DESC, lt.rowid DESC LIMIT 1) LIKE 'live_%'
    AND NOT EXISTS (SELECT 1 FROM lifecycle_transitions k
      WHERE k.strategy_id = s.strategy_id AND k.to_state = 'killed'
        AND k.actor_role = 'system' AND k.recorded_at >= e.recorded_at)
  ORDER BY e.event_id`, nil); err != nil {
		return err
	}
	// V5a: watchdog escalation alerts must pair with their kill row
	// (ref_id = kill event_id, actor 'watchdog' — safety/watchdog.go).
	if err := a.queryCheck("V5a", `SELECT a.alert_id FROM safety_alerts a
  WHERE a.kind = 'watchdog_kill_escalation'
    AND NOT EXISTS (SELECT 1 FROM kill_breaker_events e
      WHERE e.event_id = a.ref_id AND e.kind = 'kill' AND e.actor_id = 'watchdog')
  ORDER BY a.alert_id`, nil); err != nil {
		return err
	}
	a.manual("V5-timing", "Reconstruct watchdog silence timing from the journal for one"+
		" escalation: silence onset -> entry sweep -> kill escalation within configured rungs (BP-8 V5).")
	// V7a: linkage integrity.
	if err := a.queryCheck("V7a", `SELECT 'fill:' || f.fill_id FROM fills f
  WHERE NOT EXISTS (SELECT 1 FROM orders o WHERE o.order_id = f.order_id)
UNION ALL SELECT 'verdict:' || v.verdict_id FROM verdicts v
  WHERE NOT EXISTS (SELECT 1 FROM proposals p WHERE p.proposal_id = v.proposal_id)
UNION ALL SELECT 'order:' || o.order_id FROM orders o
  WHERE o.origin = 'proposal' AND o.proposal_id IS NOT NULL
    AND (NOT EXISTS (SELECT 1 FROM proposals p WHERE p.proposal_id = o.proposal_id)
      OR NOT EXISTS (SELECT 1 FROM verdicts v WHERE v.proposal_id = o.proposal_id))
  ORDER BY 1`, nil); err != nil {
		return err
	}
	if err := a.checkV7b(refArtifact); err != nil {
		return err
	}
	if strategy != "" {
		if err := a.checkV9a(strategy, logPath); err != nil {
			return err
		}
	}
	if err := a.checkM6a(); err != nil {
		return err
	}
	a.manual("V3", "Kill-switch reachability: exercise all three kill tiers on the beta host"+
		" (API, operator surface, host-local) and verify effects (BP-8 V3).")
	a.manual("V6", "Config/host inspection: decimal-string money handling, testnet/prod ack"+
		" env, plane boundary — no venue keys or control tokens on the agent plane (BP-8 V6).")
	a.manual("V8", "Backup/restore evidence: OB-10 loop green, latest artifact passes"+
		" backupverify, restore drill per RUNBOOK with the restore gate engaging (BP-8 V8).")
	return nil
}

// checkV7b compares the reference artifact against the current input:
// every reference rowid must exist unchanged (per-row hash) and
// max(rowid) must not shrink. Both sides must be artifacts (BA-1).
func (a *audit) checkV7b(refArtifact string) error {
	if refArtifact == "" {
		a.checks = append(a.checks, check{ID: "V7b", Verdict: "MANUAL", Status: "OPEN",
			Manual: "Provide -ref-artifact (older daily artifact) to run the append-only growth check."})
		return nil
	}
	if a.mode != "artifact" {
		a.checks = append(a.checks, check{ID: "V7b", Verdict: "MANUAL", Status: "OPEN",
			Manual: "V7b requires BOTH sides to be artifacts (BA-1); rerun with -artifact instead of -db.",
			Notes:  []string{"skipped: -db input"}})
		return nil
	}
	ref, err := sql.Open("sqlite", "file:"+url.PathEscape(refArtifact)+"?mode=ro&immutable=1")
	if err != nil {
		return fmt.Errorf("V7b open ref: %w", err)
	}
	defer ref.Close()
	ref.SetMaxOpenConns(1)
	c := check{ID: "V7b", Verdict: "PASS",
		Notes: []string{"append-only set: per-row hash of every reference rowid + max(rowid) growth"}}
	var findings []string
	for _, table := range appendOnlyTables {
		q := "SELECT rowid, * FROM " + table + " ORDER BY rowid"
		c.Queries = append(c.Queries, q)
		refRows, err := tableRowHashes(ref, q)
		if err != nil {
			return fmt.Errorf("V7b ref %s: %w", table, err)
		}
		curRows, err := tableRowHashes(a.db, q)
		if err != nil {
			return fmt.Errorf("V7b cur %s: %w", table, err)
		}
		var refMax, curMax int64
		for id := range refRows {
			if id > refMax {
				refMax = id
			}
		}
		for id := range curRows {
			if id > curMax {
				curMax = id
			}
		}
		if curMax < refMax {
			findings = append(findings, fmt.Sprintf("%s:max(rowid) shrank %d->%d", table, refMax, curMax))
		}
		for id, h := range refRows {
			cur, ok := curRows[id]
			if !ok {
				findings = append(findings, fmt.Sprintf("%s:rowid %d missing", table, id))
			} else if cur != h {
				findings = append(findings, fmt.Sprintf("%s:rowid %d mutated", table, id))
			}
		}
	}
	if len(findings) > 0 {
		c.Verdict, c.Count = "FAIL", len(findings)
		if len(findings) > 20 {
			findings = findings[:20]
		}
		c.Findings = findings
	}
	a.checks = append(a.checks, c)
	return nil
}

// tableRowHashes hashes every row of q (rowid first column) with a
// length-prefixed, type-tagged serialization — deterministic and
// injective, so byte-identical is exactly hash-identical.
func tableRowHashes(db *sql.DB, q string) (map[int64]string, error) {
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rowid, ok := vals[0].(int64)
		if !ok {
			return nil, fmt.Errorf("rowid column is %T", vals[0])
		}
		h := sha256.New()
		for _, v := range vals[1:] {
			writeValue(h, v)
		}
		out[rowid] = hex.EncodeToString(h.Sum(nil))
	}
	return out, rows.Err()
}

func writeValue(h io.Writer, v any) {
	var tag byte
	var b []byte
	switch x := v.(type) {
	case nil:
		tag = 0
	case int64:
		tag, b = 1, binary.BigEndian.AppendUint64(nil, uint64(x))
	case float64:
		tag, b = 2, binary.BigEndian.AppendUint64(nil, math.Float64bits(x))
	case string:
		tag, b = 3, []byte(x)
	case []byte:
		tag, b = 4, x
	default:
		tag, b = 5, []byte(fmt.Sprintf("%v", x))
	}
	h.Write([]byte{tag})
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(b))))
	h.Write(b)
}

// checkV9a replays risk_limit_changes for the certified strategy
// (who/when/old->new, numeric classification) and, with -log, FAILs any
// change_id lacking a limit_change beta-log entry (coverage only — the
// chain proves order, not time; timeliness is BL-6's prefix custody).
func (a *audit) checkV9a(strategy, logPath string) error {
	const q = `SELECT change_id, field, COALESCE(old_value, ''), new_value, actor_id, changed_at
  FROM risk_limit_changes WHERE strategy_id = ?1 ORDER BY changed_at, rowid`
	rows, err := a.db.Query(q, strategy)
	if err != nil {
		return fmt.Errorf("V9a: %w", err)
	}
	defer rows.Close()
	c := check{ID: "V9a", Verdict: "PASS", Queries: []string{q},
		Notes: []string{"classification is a report; pairing loosenings with a clock restart is the reviewer's judgment",
			"symbol_whitelist is not runtime-changeable (api/limits.go); whitelist diff vs Day-0 = MANUAL"}}
	type change struct{ id, field, oldV, newV, actor, at string }
	var changes []change
	for rows.Next() {
		var ch change
		if err := rows.Scan(&ch.id, &ch.field, &ch.oldV, &ch.newV, &ch.actor, &ch.at); err != nil {
			return fmt.Errorf("V9a scan: %w", err)
		}
		changes = append(changes, ch)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("V9a rows: %w", err)
	}
	logged := map[string]bool{}
	if logPath != "" {
		var err error
		logged, err = betalogChangeIDs(logPath)
		if err != nil {
			return fmt.Errorf("V9a log: %w", err)
		}
	}
	var findings []string
	for _, ch := range changes {
		c.Notes = append(c.Notes, fmt.Sprintf("%s %s: %s %q -> %q by %s (%s)",
			ch.at, ch.field, classify(ch.oldV, ch.newV), ch.oldV, ch.newV, ch.actor, ch.id))
		if logPath != "" && !logged[ch.id] {
			findings = append(findings, ch.id)
		}
	}
	if len(findings) > 0 {
		c.Verdict, c.Count = "FAIL", len(findings)
		if len(findings) > 20 {
			findings = findings[:20]
		}
		c.Findings = findings
	}
	a.checks = append(a.checks, c)
	return nil
}

// classify labels a numeric-cap change: new > old = loosening (BA-3).
// Values are decimal strings (money/size are never floats); unparseable
// pairs are reported unclassified.
func classify(oldV, newV string) string {
	o, ok1 := new(big.Rat).SetString(strings.TrimSpace(oldV))
	n, ok2 := new(big.Rat).SetString(strings.TrimSpace(newV))
	if !ok1 || !ok2 {
		return "unclassified"
	}
	switch n.Cmp(o) {
	case 1:
		return "LOOSENING"
	case -1:
		return "tightening"
	default:
		return "unchanged"
	}
}

// betalogChangeIDs reads a betalog JSONL file (fields per beta-ops-tooling
// BL-1) and collects refs.change_id of every limit_change entry. The hash
// chain is NOT verified here — that is betalog verify's job.
func betalogChangeIDs(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e struct {
			Type string            `json:"type"`
			Refs map[string]string `json:"refs"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // chain integrity is betalog verify's concern
		}
		if e.Type == "limit_change" && e.Refs["change_id"] != "" {
			out[e.Refs["change_id"]] = true
		}
	}
	return out, sc.Err()
}

// checkM6a reports per-source watermark lag (MANUAL: the M6 bar is the
// reviewer's call; poison-row skips advance the watermark and are only
// visible in the journal).
func (a *audit) checkM6a() error {
	const q = `SELECT source, last_rowid FROM alert_dispatch_state ORDER BY source`
	rows, err := a.db.Query(q)
	if err != nil {
		return fmt.Errorf("M6a: %w", err)
	}
	defer rows.Close()
	watermarks := map[string]int64{}
	for rows.Next() {
		var src string
		var last int64
		if err := rows.Scan(&src, &last); err != nil {
			return fmt.Errorf("M6a scan: %w", err)
		}
		watermarks[src] = last
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("M6a rows: %w", err)
	}
	c := check{ID: "M6a", Verdict: "MANUAL", Status: "OPEN", Queries: []string{q},
		Manual: "Judge lag against the M6 bar; then grep the journal for 'ALERT DISPATCH SKIPPED'" +
			" — poison-row skips ADVANCE the watermark (AN-4a) and are invisible to this query."}
	for _, src := range alertSources {
		var max sql.NullInt64
		mq := "SELECT MAX(rowid) FROM " + src
		c.Queries = append(c.Queries, mq)
		if err := a.db.QueryRow(mq).Scan(&max); err != nil {
			return fmt.Errorf("M6a max %s: %w", src, err)
		}
		wm, seeded := watermarks[src]
		if !seeded {
			c.Notes = append(c.Notes, fmt.Sprintf("%s: watermark not seeded (notifier disabled?), max(rowid)=%d", src, max.Int64))
			continue
		}
		c.Notes = append(c.Notes, fmt.Sprintf("%s: watermark=%d max(rowid)=%d lag=%d", src, wm, max.Int64, max.Int64-wm))
	}
	a.checks = append(a.checks, c)
	return nil
}

// printText renders the report block-per-check (BA-2).
func printText(out io.Writer, rep report) {
	h := rep.Header
	fmt.Fprintf(out, "betaaudit report\n  input: %s (%s)\n", h.InputPath, h.InputMode)
	if h.InputSHA256 != "" {
		fmt.Fprintf(out, "  input sha256: %s\n", h.InputSHA256)
	}
	if h.SHANote != "" {
		fmt.Fprintf(out, "  input sha256: %s\n", h.SHANote)
	}
	if h.RefPath != "" {
		fmt.Fprintf(out, "  ref-artifact: %s\n  ref sha256: %s\n", h.RefPath, h.RefSHA256)
	}
	fmt.Fprintf(out, "  user_version: %d\n  started: %s\n  max(rowid):\n", h.UserVersion, h.StartedAt)
	for _, t := range appendOnlyTables {
		fmt.Fprintf(out, "    %s: %d\n", t, h.MaxRowids[t])
	}
	for _, c := range rep.Checks {
		fmt.Fprintf(out, "\n[%s] %s (findings: %d)\n", c.Verdict, c.ID, c.Count)
		for _, q := range c.Queries {
			fmt.Fprintf(out, "  query: %s\n", strings.ReplaceAll(q, "\n", "\n         "))
		}
		for _, n := range c.Notes {
			fmt.Fprintf(out, "  note: %s\n", n)
		}
		if c.Manual != "" {
			fmt.Fprintf(out, "  manual: %s\n", c.Manual)
		}
		for _, f := range c.Findings {
			fmt.Fprintf(out, "  finding: %s\n", f)
		}
	}
}
