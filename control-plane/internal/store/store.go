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

// ErrNotFound: the referenced row does not exist.
var ErrNotFound = errors.New("NOT_FOUND")

// ErrNotPending: the verdict has no pending_approvals row and no recorded
// decision (persistence-and-api.md L1 semantics, 422 NOT_PENDING).
var ErrNotPending = errors.New("NOT_PENDING")

// Store wraps the single control-plane SQLite file.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the DB at path, applies the connection
// pragmas required by the spec (journal_mode=WAL, busy_timeout >= 5000 ms,
// foreign_keys ON) and executes the embedded schema idempotently.
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
	return &Store{db: db}, nil
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
