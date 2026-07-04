// Package store is the Phase-1 control-plane persistence layer
// (docs/specs/persistence-and-api.md): SQLite via modernc.org/sqlite, one DB
// file, WAL + busy_timeout, decimal-as-string money columns, RFC 3339 UTC
// timestamps. Contract objects are stored as canonical JSON in payload_json
// columns (the source of truth); extracted columns index/filter only.
// Append-only tables (invariant 7) have INSERT methods and no mutators.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"

	_ "modernc.org/sqlite"
)

// ErrIdempotencyConflict: a duplicate key arrived with a different payload
// (canonical hash mismatch); mirrors riskgate.ErrIdempotencyConflict.
var ErrIdempotencyConflict = errors.New(contract.CodeIdempotencyConflict)

// ErrRunTickConflict: the submission's run/tick pairing contradicts the
// runs table — a different run already owns (strategy_id, tick_number), or
// the run exists at a different tick (UNIQUE (strategy_id, tick_number)).
var ErrRunTickConflict = errors.New("RUN_TICK_CONFLICT")

// ErrNotFound: the referenced row does not exist.
var ErrNotFound = errors.New("NOT_FOUND")

// ErrNotPending: the verdict has no pending_approvals row and no recorded
// decision (persistence-and-api.md L1 semantics, 422 NOT_PENDING).
var ErrNotPending = errors.New("NOT_PENDING")

// ErrTenantExists: the tenant_id is already taken (multi-tenant-rbac.md
// §Tenancy rules, 409 TENANT_EXISTS).
var ErrTenantExists = errors.New("TENANT_EXISTS")

// ErrDuplicateTokenHash: the minted token_hash collided with an existing
// row (UNIQUE token_hash); the caller retries with a fresh CSPRNG value,
// never surfacing the collision (multi-tenant-rbac.md §Token lifecycle).
var ErrDuplicateTokenHash = errors.New("DUPLICATE_TOKEN_HASH")

// ErrOwnerExists: an owner-recovery mint found an unrevoked owner token
// (multi-tenant-rbac.md §Token lifecycle: recovery only at zero unrevoked
// owner tokens; checked in the SAME transaction as the insert).
var ErrOwnerExists = errors.New("OWNER_TOKEN_EXISTS")

// Store wraps the single control-plane SQLite file.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the DB at path, applies the connection
// pragmas required by the spec (journal_mode=WAL, busy_timeout >= 5000 ms,
// foreign_keys ON) and executes the embedded schema idempotently, followed
// by the guarded tenancy migration (multi-tenant-rbac.md §Migration note).
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	// Single-node Phase 1: one connection serializes writers; the DSN pragmas
	// apply to every (re)opened connection either way.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema %s: %w", path, err)
	}
	if err := migrateTenancy(db, time.Now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("tenancy migration %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// migrateTenancy is the additive Phase-2 migration (multi-tenant-rbac.md
// §Migration note): the kill_breaker_events.tenant_id column is added iff
// absent (the table pre-exists, so CREATE IF NOT EXISTS cannot carry it),
// and every pre-existing strategies.tenant_id value — plus 'default' —
// gets a tenants row so DB tokens can be minted for grandfathered tenants.
// NO data backfill on kill_breaker_events: existing rows stay tenant_id
// NULL (global or strategy scope, exactly as before).
func migrateTenancy(db *sql.DB, now time.Time) error {
	var hasTenantID int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('kill_breaker_events')
		WHERE name = 'tenant_id'`).Scan(&hasTenantID); err != nil {
		return err
	}
	if hasTenantID == 0 {
		if _, err := db.Exec(`ALTER TABLE kill_breaker_events ADD COLUMN tenant_id TEXT`); err != nil {
			return err
		}
	}
	seededAt := formatTime(now)
	if _, err := db.Exec(`INSERT OR IGNORE INTO tenants (tenant_id, name, created_at)
		VALUES ('default', 'default', ?)`, seededAt); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO tenants (tenant_id, name, created_at)
		SELECT DISTINCT tenant_id, tenant_id, ? FROM strategies`, seededAt)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// formatTime renders an RFC 3339 UTC timestamp with Z suffix (spec: all
// timestamp columns), matching contract.NewUTCTime's second precision.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// canonicalJSON marshals a contract object once; the bytes are both the
// stored payload_json and the idempotency hash input (riskgate step 0b).
func canonicalJSON(v any) ([]byte, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

func rollback(tx *sql.Tx) { _ = tx.Rollback() }

// dbtx is the statement surface shared by *sql.DB and *sql.Tx: row helpers
// take it so the same SQL serves both the one-off Store methods and the
// ApplySweep / StrategySnapshot transactions.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}
