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

// ErrMeteringConflict: a metering re-import disagrees with the stored
// record for the same request_id (billing-and-metering.md §Metering
// ingest, 409 METERING_CONFLICT — the whole batch is rejected).
var ErrMeteringConflict = errors.New("METERING_CONFLICT")

// ErrPeriodClosed: the (tenant, period) billing period already has a
// billing_periods row (billing-and-metering.md §Billing, 409 PERIOD_CLOSED).
var ErrPeriodClosed = errors.New("PERIOD_CLOSED")

// ErrPeriodOpen: reconciliation requires a closed period — the invoice is
// the comparison target (billing-and-metering.md §Reconciliation, 409
// PERIOD_OPEN).
var ErrPeriodOpen = errors.New("PERIOD_OPEN")

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
	if err := migrateBilling(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("billing migration %s: %w", path, err)
	}
	if err := migrateLiveOMS(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("live OMS migration %s: %w", path, err)
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

// migrateBilling is the additive billing migration (billing-and-metering.md
// §Migration note): model_costs pre-exists, so its request_id and
// is_estimated columns are added iff absent, then the partial UNIQUE index
// enforces request_id uniqueness while leaving NULLs unconstrained. NO data
// backfill: existing rows read request_id NULL / is_estimated 0 (the
// "unattributed" reconciliation class).
func migrateBilling(db *sql.DB) error {
	for _, col := range []struct{ name, ddl string }{
		{"request_id", `ALTER TABLE model_costs ADD COLUMN request_id TEXT`},
		{"is_estimated", `ALTER TABLE model_costs ADD COLUMN is_estimated INTEGER NOT NULL DEFAULT 0`},
	} {
		var have int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('model_costs')
			WHERE name = ?`, col.name).Scan(&have); err != nil {
			return err
		}
		if have == 0 {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_model_costs_request_id
		ON model_costs (request_id) WHERE request_id IS NOT NULL`)
	return err
}

// migrateLiveOMS is the additive live-OMS migration (live-oms-and-
// reconciler.md §Migration note): orders and fills pre-exist, so their live
// columns are added iff absent, THEN the two partial unique indexes apply
// (they reference the ALTERed columns, so they run post-ALTER, not in
// schemaDDL). NO data backfill: existing paper rows keep NULL in every new
// column (fills.venue_epoch defaults to 0) and are excluded from the
// partial indexes.
func migrateLiveOMS(db *sql.DB) error {
	for _, col := range []struct{ table, name, ddl string }{
		{"orders", "client_order_id", `ALTER TABLE orders ADD COLUMN client_order_id TEXT`},
		{"orders", "exchange_order_id", `ALTER TABLE orders ADD COLUMN exchange_order_id TEXT`},
		{"fills", "venue_symbol", `ALTER TABLE fills ADD COLUMN venue_symbol TEXT`},
		{"fills", "exchange_trade_id", `ALTER TABLE fills ADD COLUMN exchange_trade_id INTEGER`},
		{"fills", "venue_epoch", `ALTER TABLE fills ADD COLUMN venue_epoch INTEGER NOT NULL DEFAULT 0`},
	} {
		var have int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?)
			WHERE name = ?`, col.table, col.name).Scan(&have); err != nil {
			return err
		}
		if have == 0 {
			if _, err := db.Exec(col.ddl); err != nil {
				return err
			}
		}
	}
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_client_order_id
			ON orders (client_order_id) WHERE client_order_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fills_venue_trade
			ON fills (venue_epoch, venue_symbol, exchange_trade_id)
			WHERE exchange_trade_id IS NOT NULL`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
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
