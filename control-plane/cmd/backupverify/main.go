// Command backupverify is the offline artifact/restore verifier
// (docs/specs/ops-backup.md OB-11): `backupverify -db <path>` opens the
// file read-only with immutable=1 (no schema apply, no sidecars, no locks
// — works on read-only media; NEVER point it at a live, attached DB) and
// prints a report. Exit 0 iff integrity_check is "ok" AND
// foreign_key_check finds zero violations. The RUNBOOK forbids
// substituting a system sqlite3 binary in the normative restore check:
// this tool shares the exact driver version with the server via go.mod.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry: parses flags, verifies, prints the OB-11
// report to out, and returns the process exit code.
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("backupverify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	dbPath := fs.String("db", "", "path to the backup artifact (REQUIRED)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		fmt.Fprintln(errOut, "backupverify: -db <path> is required")
		fs.Usage()
		return 2
	}
	ok, err := verify(*dbPath, out)
	if err != nil {
		fmt.Fprintf(errOut, "backupverify: %v\n", err)
		return 1
	}
	if !ok {
		return 1
	}
	return 0
}

// verify runs the OB-11 report over a read-only immutable handle; ok
// reports whether checks 1-2 (integrity, foreign keys) both passed.
func verify(path string, out io.Writer) (ok bool, err error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro&immutable=1")
	if err != nil {
		return false, fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// 1. PRAGMA integrity_check: exactly one row "ok".
	lines, err := queryStrings(db, "PRAGMA integrity_check")
	if err != nil {
		return false, fmt.Errorf("integrity_check: %w", err)
	}
	integrityOK := len(lines) == 1 && lines[0] == "ok"
	fmt.Fprintf(out, "integrity_check: %s\n", strings.Join(lines, "; "))

	// 2. PRAGMA foreign_key_check: zero violation rows.
	violations, err := queryStrings(db, "SELECT \"table\" FROM pragma_foreign_key_check")
	if err != nil {
		return false, fmt.Errorf("foreign_key_check: %w", err)
	}
	fmt.Fprintf(out, "foreign_key_check violations: %d\n", len(violations))

	// 3. PRAGMA journal_mode (informational; the artifact is checkpointed
	// so any value opens cleanly under the store's WAL DSN later).
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return false, fmt.Errorf("journal_mode: %w", err)
	}
	fmt.Fprintf(out, "journal_mode: %s\n", mode)

	// 3a. PRAGMA user_version (informational, deploy-and-survive.md
	// DS-1a): 1 on engine-stamped artifacts — restoring this file engages
	// the restore gate.
	var userVersion int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return false, fmt.Errorf("user_version: %w", err)
	}
	fmt.Fprintf(out, "user_version: %d\n", userVersion)

	// 4. Per-table row counts: every user table under the OB-2 step 3
	// pinned predicate, ordered by name.
	names, err := queryStrings(db,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return false, fmt.Errorf("list tables: %w", err)
	}
	fmt.Fprintf(out, "tables: %d\n", len(names))
	tables := make(map[string]bool, len(names))
	for _, name := range names {
		tables[name] = true
		var count int64
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + quoted).Scan(&count); err != nil {
			return false, fmt.Errorf("count %s: %w", name, err)
		}
		fmt.Fprintf(out, "  %s: %d rows\n", name, count)
	}

	// 5. Newest recorded_at/created_at across the operational tables (when
	// present) — the operator's data-loss bound for a restore. Each
	// table's own timestamp column (schema.go): orders has submitted_at.
	for _, op := range []struct{ table, column string }{
		{"runs", "created_at"},
		{"model_costs", "recorded_at"},
		{"safety_alerts", "recorded_at"},
		{"orders", "submitted_at"},
	} {
		if !tables[op.table] {
			continue
		}
		var newest sql.NullString
		if err := db.QueryRow(`SELECT MAX(` + op.column + `) FROM ` + op.table).Scan(&newest); err != nil {
			return false, fmt.Errorf("newest %s.%s: %w", op.table, op.column, err)
		}
		v := "(no rows)"
		if newest.Valid {
			v = newest.String
		}
		fmt.Fprintf(out, "newest %s.%s: %s\n", op.table, op.column, v)
	}

	return integrityOK && len(violations) == 0, nil
}

// queryStrings collects a single-string-column result set.
func queryStrings(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
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
