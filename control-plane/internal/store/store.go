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
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

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

// ErrEmailExists: the email is already taken by another user
// (multi-tenant-rbac.md §Password auth, 409 EMAIL_EXISTS).
var ErrEmailExists = errors.New("EMAIL_EXISTS")

// ErrPlatformAdminExists: bootstrap found an existing platform_admin user
// (multi-tenant-rbac.md §Password auth: bootstrap runs exactly once,
// checked in the SAME transaction as the insert; 409 BOOTSTRAP_COMPLETE).
var ErrPlatformAdminExists = errors.New("PLATFORM_ADMIN_EXISTS")

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
	// path is the RAW configured DB file path recorded at Open (the DSN is
	// URL-escaped); the backup copy reads this file directly
	// (docs/specs/ops-backup.md OB-2 step 4).
	path string

	// backupMu is the OB-6a engine mutex: at most one backup at a time,
	// covering retention and the `.tmp` cleanup too; a concurrent call
	// fails immediately with ErrBackupInProgress, never queues.
	backupMu sync.Mutex
	// backupNow overrides the artifact-name clock in tests; nil = time.Now.
	backupNow func() time.Time
	// backupVerify overrides the OB-5 artifact verification in tests;
	// nil = verifyBackupArtifact.
	backupVerify func(artifactPath string, fingerprint []tableCount) error

	// restoreGate holds the user_version read at Open (deploy-and-
	// survive.md DS-2): >= 1 marks a restored artifact and engages the
	// restore gate; ClearRestoreGate compare-and-swaps it to 0 exactly
	// once (DS-5).
	restoreGate atomic.Int64
}

// Open opens (creating if absent) the DB at path, applies the connection
// pragmas required by the spec (journal_mode=WAL, busy_timeout >= 5000 ms,
// foreign_keys ON, synchronous=FULL — the WAL durability level made
// explicit rather than an implicit driver default) and executes the
// embedded schema idempotently, followed by the guarded tenancy migration
// (multi-tenant-rbac.md §Migration note).
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)"
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
	if err := migrateLifecycleBootstrap(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("lifecycle bootstrap migration %s: %w", path, err)
	}
	if err := migrateAlertDispatch(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("alert dispatch migration %s: %w", path, err)
	}
	if err := migrateStrategyProvisioning(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("strategy provisioning migration %s: %w", path, err)
	}
	// DS-2: user_version is read AFTER migrations (they never touch it);
	// a value >= 1 is the DS-1 artifact stamp — this DB is a restored
	// artifact and the restore gate engages.
	var userVersion int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("read user_version %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	s.restoreGate.Store(userVersion)
	return s, nil
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

// migrateLifecycleBootstrap is the additive lifecycle-api.md LC-16a
// migration: every legacy `paper` OR `paused` strategy with no
// to_state='paper' lifecycle_transitions row gets ONE synthetic
// draft→paper bootstrap row (recorded_at = created_at) so the LC-16
// paper window never fails closed merely because the strategy predates
// CreateStrategy's atomic bootstrap. Idempotent: guarded by the
// NOT EXISTS condition; for a paused strategy the synthetic row is a
// WINDOW-START record only (PausedProvenance reads to_state='paused'
// rows, untouched).
func migrateLifecycleBootstrap(db *sql.DB) error {
	rows, err := db.Query(`SELECT strategy_id, created_at FROM strategies s
		WHERE s.lifecycle_state IN ('paper', 'paused')
		AND NOT EXISTS (SELECT 1 FROM lifecycle_transitions t
			WHERE t.strategy_id = s.strategy_id AND t.to_state = 'paper')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type legacy struct{ strategyID, createdAt string }
	var missing []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.strategyID, &l.createdAt); err != nil {
			return err
		}
		missing = append(missing, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	for _, l := range missing {
		if _, err := tx.Exec(`INSERT INTO lifecycle_transitions
			(transition_id, strategy_id, from_state, to_state, actor_id, actor_role, reason, recorded_at)
			VALUES (?, ?, 'draft', 'paper', 'bootstrap', 'system', 'bootstrap', ?)`,
			uuid.NewString(), l.strategyID, l.createdAt); err != nil {
			return err
		}
	}
	return tx.Commit()
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
