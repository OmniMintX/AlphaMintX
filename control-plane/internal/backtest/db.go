package backtest

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Backtest run statuses (backtest_runs.status CHECK constraint).
const (
	StatusRunning  = "running"
	StatusComplete = "complete"
	StatusFailed   = "failed"
)

// schemaDDL is the normative shape of docs/specs/backtest-engine.md
// §Persistence: the klines fetch cache and the backtest run/record tables
// live in a SEPARATE backtest.db, never in control.db (persistence
// isolation). All prices/volumes are TEXT decimal strings (ADR-0003).
const schemaDDL = `
CREATE TABLE IF NOT EXISTS klines (symbol TEXT NOT NULL, interval TEXT NOT NULL,  -- append-only fetch cache
  open_time INTEGER NOT NULL, open TEXT NOT NULL, high TEXT NOT NULL, low TEXT NOT NULL,
  close TEXT NOT NULL, volume TEXT NOT NULL, PRIMARY KEY (symbol, interval, open_time));
CREATE TABLE IF NOT EXISTS backtest_runs (backtest_id TEXT PRIMARY KEY, strategy_id TEXT NOT NULL,
  config_hash TEXT NOT NULL, dataset_sha256 TEXT NOT NULL, code_version TEXT NOT NULL,
  seed INTEGER NOT NULL, mask_level TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('running','complete','failed')), created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS backtest_records (backtest_id TEXT NOT NULL REFERENCES backtest_runs,
  seq INTEGER NOT NULL, kind TEXT NOT NULL, payload_json TEXT NOT NULL,  -- shaped like e2e records.jsonl rows
  PRIMARY KEY (backtest_id, seq));                                       -- append-only
`

// DB wraps the separate backtest SQLite file.
type DB struct {
	db *sql.DB
}

// OpenDB opens (creating if absent) the backtest DB at path with the same
// connection pragmas as the control store (WAL, busy_timeout, foreign keys)
// and applies the schema idempotently.
func OpenDB(path string) (*DB, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open backtest db %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply backtest schema %s: %w", path, err)
	}
	return &DB{db: db}, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.db.Close() }

// InsertKlines appends candles to the fetch cache in one transaction.
// INSERT OR IGNORE: refetching a window is a no-op, never a mutation
// (append-only cache).
func (d *DB) InsertKlines(klines []Kline) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, k := range klines {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO klines
			(symbol, interval, open_time, open, high, low, close, volume)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			k.Symbol, k.Interval, k.OpenTime, k.Open, k.High, k.Low, k.Close, k.Volume); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Klines returns the cached candles for (symbol, interval) with open_time
// in [startMS, endMS], ascending.
func (d *DB) Klines(symbol, interval string, startMS, endMS int64) ([]Kline, error) {
	rows, err := d.db.Query(`SELECT symbol, interval, open_time, open, high, low, close, volume
		FROM klines WHERE symbol = ? AND interval = ? AND open_time BETWEEN ? AND ?
		ORDER BY open_time`, symbol, interval, startMS, endMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Kline
	for rows.Next() {
		var k Kline
		if err := rows.Scan(&k.Symbol, &k.Interval, &k.OpenTime, &k.Open, &k.High, &k.Low, &k.Close, &k.Volume); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RunRow mirrors the backtest_runs table.
type RunRow struct {
	BacktestID    string
	StrategyID    string
	ConfigHash    string
	DatasetSHA256 string
	CodeVersion   string
	Seed          int64
	MaskLevel     string
	Status        string
	CreatedAt     string
}

// InsertRun inserts the run row (status usually 'running').
func (d *DB) InsertRun(r RunRow) error {
	_, err := d.db.Exec(`INSERT INTO backtest_runs
		(backtest_id, strategy_id, config_hash, dataset_sha256, code_version, seed, mask_level, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.BacktestID, r.StrategyID, r.ConfigHash, r.DatasetSHA256, r.CodeVersion, r.Seed, r.MaskLevel, r.Status, r.CreatedAt)
	return err
}

// GetRun reads one run row; ok=false when absent.
func (d *DB) GetRun(backtestID string) (RunRow, bool, error) {
	var r RunRow
	err := d.db.QueryRow(`SELECT backtest_id, strategy_id, config_hash, dataset_sha256, code_version,
		seed, mask_level, status, created_at FROM backtest_runs WHERE backtest_id = ?`, backtestID).
		Scan(&r.BacktestID, &r.StrategyID, &r.ConfigHash, &r.DatasetSHA256, &r.CodeVersion,
			&r.Seed, &r.MaskLevel, &r.Status, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return RunRow{}, false, nil
	}
	if err != nil {
		return RunRow{}, false, err
	}
	return r, true, nil
}

// FinishRun moves a run from 'running' to 'complete' or 'failed'; any other
// transition is an error (terminal statuses are immutable).
func (d *DB) FinishRun(backtestID, status string) error {
	if status != StatusComplete && status != StatusFailed {
		return fmt.Errorf("illegal terminal status %q", status)
	}
	res, err := d.db.Exec(`UPDATE backtest_runs SET status = ? WHERE backtest_id = ? AND status = ?`,
		status, backtestID, StatusRunning)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("backtest %s is not running (terminal statuses are immutable)", backtestID)
	}
	return nil
}

// AppendRecord appends one record line to backtest_records; payload is the
// EXACT record line written to records.jsonl minus the trailing LF
// (byte-identical tee).
func (d *DB) AppendRecord(backtestID string, seq int, kind string, payload []byte) error {
	_, err := d.db.Exec(`INSERT INTO backtest_records (backtest_id, seq, kind, payload_json)
		VALUES (?, ?, ?, ?)`, backtestID, seq, kind, string(payload))
	return err
}

// Records returns the ordered record payloads for one backtest.
func (d *DB) Records(backtestID string) ([]string, error) {
	rows, err := d.db.Query(`SELECT payload_json FROM backtest_records
		WHERE backtest_id = ? ORDER BY seq`, backtestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
