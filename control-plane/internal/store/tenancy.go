package store

import (
	"database/sql"
	"fmt"
)

// Tenants and DB-issued API tokens (docs/specs/multi-tenant-rbac.md).
// api_tokens follows the pending_approvals/positions pattern: a mutable
// snapshot whose ONLY legal mutation sets revoked_at once; every create and
// revoke also appends a token_events row (invariant 7). token_hash never
// crosses the read boundary (no-read-back invariant).

// CreateTenant inserts a tenants row; ErrTenantExists when taken.
func (s *Store) CreateTenant(t Tenant) error {
	return createTenant(s.db, t)
}

func createTenant(q dbtx, t Tenant) error {
	_, err := q.Exec(`INSERT INTO tenants (tenant_id, name, created_at) VALUES (?, ?, ?)`,
		t.TenantID, t.Name, t.CreatedAt)
	if isUniqueConstraint(err) {
		return fmt.Errorf("tenant %s: %w", t.TenantID, ErrTenantExists)
	}
	return err
}

// GetTenant returns one tenant or ErrNotFound.
func (s *Store) GetTenant(tenantID string) (Tenant, error) {
	var t Tenant
	err := s.db.QueryRow(`SELECT tenant_id, name, created_at FROM tenants WHERE tenant_id = ?`,
		tenantID).Scan(&t.TenantID, &t.Name, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return Tenant{}, fmt.Errorf("tenant %s: %w", tenantID, ErrNotFound)
	}
	return t, err
}

// ListTenants returns every tenant ordered created_at then tenant_id (the
// admin-console listing; env-admin ONLY at the API layer).
func (s *Store) ListTenants() ([]Tenant, error) {
	rows, err := s.db.Query(`SELECT tenant_id, name, created_at FROM tenants
		ORDER BY created_at, tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.TenantID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateTenantWithOwnerToken atomically creates a tenant AND mints its
// first owner token + created audit event (multi-tenant-rbac.md §Tenancy
// rules: the tenant-creation response mints the initial owner token).
// ErrTenantExists on a taken tenant_id; ErrDuplicateTokenHash tells the
// caller to retry with a fresh CSPRNG plaintext.
func (s *Store) CreateTenantWithOwnerToken(t Tenant, tok APIToken, tokenHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := createTenant(tx, t); err != nil {
		return err
	}
	if err := insertAPIToken(tx, tok, tokenHash, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertAPIToken persists a minted token (hash only, never plaintext) and
// its token_events 'created' row in one transaction. ErrDuplicateTokenHash
// on a UNIQUE token_hash collision (caller retries, never surfaces it).
func (s *Store) InsertAPIToken(tok APIToken, tokenHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := insertAPIToken(tx, tok, tokenHash, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAPIToken(q dbtx, tok APIToken, tokenHash, eventID string) error {
	_, err := q.Exec(`INSERT INTO api_tokens
		(token_id, tenant_id, principal, role, strategy_id, token_hash, label, created_by, created_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		tok.TokenID, tok.TenantID, tok.Principal, tok.Role, tok.StrategyID,
		tokenHash, tok.Label, tok.CreatedBy, tok.CreatedAt)
	if isUniqueConstraint(err) {
		return ErrDuplicateTokenHash
	}
	if err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO token_events (event_id, token_id, event, actor_id, recorded_at)
		VALUES (?, ?, 'created', ?, ?)`,
		eventID, tok.TokenID, tok.CreatedBy, tok.CreatedAt)
	return err
}

const apiTokenSelect = `SELECT token_id, tenant_id, principal, role, strategy_id,
	label, created_by, created_at, revoked_at FROM api_tokens`

// scanAPIToken scans one api_tokens metadata row (apiTokenSelect order).
func scanAPIToken(row rowScanner) (APIToken, error) {
	var t APIToken
	var role, strategyID, revokedAt sql.NullString
	if err := row.Scan(&t.TokenID, &t.TenantID, &t.Principal, &role, &strategyID,
		&t.Label, &t.CreatedBy, &t.CreatedAt, &revokedAt); err != nil {
		return APIToken{}, err
	}
	t.Role = nullable(role)
	t.StrategyID = nullable(strategyID)
	t.RevokedAt = nullable(revokedAt)
	return t, nil
}

// GetAPIToken returns one token's metadata (never the hash) or ErrNotFound.
func (s *Store) GetAPIToken(tokenID string) (APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRow(apiTokenSelect+` WHERE token_id = ?`, tokenID))
	if err == sql.ErrNoRows {
		return APIToken{}, fmt.Errorf("token %s: %w", tokenID, ErrNotFound)
	}
	return t, err
}

// TokenByHash resolves the auth lookup: exact match on the fixed-length
// SHA-256 digest via the UNIQUE index (constant-time-compatible: whole-value
// matches only, no prefix oracle). The row is returned revoked or not — the
// CALLER must observe RevokedAt and answer 401 (no auth result may outlive
// its request).
func (s *Store) TokenByHash(tokenHash string) (APIToken, error) {
	t, err := scanAPIToken(s.db.QueryRow(apiTokenSelect+` WHERE token_hash = ?`, tokenHash))
	if err == sql.ErrNoRows {
		return APIToken{}, fmt.Errorf("token hash: %w", ErrNotFound)
	}
	return t, err
}

// ListAPITokens returns one metadata page plus the tenant-scoped total.
// tenantID "" lists every tenant (env-admin, platform-scoped); tenant
// principals always pass their own tenant (§Lists: no foreign rows, ever).
func (s *Store) ListAPITokens(tenantID string, page, limit int) ([]APIToken, int, error) {
	page, limit = normalizePage(page, limit)
	where, args := "", []any{}
	if tenantID != "" {
		where, args = " WHERE tenant_id = ?", []any{tenantID}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_tokens`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(apiTokenSelect+where+` ORDER BY created_at, token_id LIMIT ? OFFSET ?`,
		append(args, limit, (page-1)*limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// RevokeAPIToken sets revoked_at (the token's single legal mutation, once)
// and appends the token_events 'revoked' row in one transaction. A token
// already revoked is left untouched — the first revocation stands — and
// reports revoked=false with no second event. ErrNotFound for an absent
// token_id.
func (s *Store) RevokeAPIToken(tokenID, revokedAt, eventID, actorID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	res, err := tx.Exec(`UPDATE api_tokens SET revoked_at = ?
		WHERE token_id = ? AND revoked_at IS NULL`, revokedAt, tokenID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE token_id = ?`,
			tokenID).Scan(&exists); err != nil {
			return false, err
		}
		if exists == 0 {
			return false, fmt.Errorf("token %s: %w", tokenID, ErrNotFound)
		}
		return false, tx.Commit() // already revoked: idempotent no-op
	}
	if _, err := tx.Exec(`INSERT INTO token_events (event_id, token_id, event, actor_id, recorded_at)
		VALUES (?, ?, 'revoked', ?, ?)`, eventID, tokenID, actorID, revokedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// CountUnrevokedOwnerTokens backs the env-admin owner-recovery mint gate
// (multi-tenant-rbac.md §Token lifecycle: recovery only at zero unrevoked
// owner tokens).
func (s *Store) CountUnrevokedOwnerTokens(tenantID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM api_tokens
		WHERE tenant_id = ? AND role = 'owner' AND revoked_at IS NULL`, tenantID).Scan(&n)
	return n, err
}

// InsertOwnerRecoveryToken performs the env-admin owner-recovery mint: the
// zero-unrevoked-owner check and the insert run in ONE transaction, so two
// concurrent recovery mints cannot both pass the gate (multi-tenant-rbac.md
// §Token lifecycle). ErrOwnerExists when an unrevoked owner token exists;
// ErrDuplicateTokenHash tells the caller to retry with a fresh plaintext.
func (s *Store) InsertOwnerRecoveryToken(tok APIToken, tokenHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM api_tokens
		WHERE tenant_id = ? AND role = 'owner' AND revoked_at IS NULL`,
		tok.TenantID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("tenant %s: %w", tok.TenantID, ErrOwnerExists)
	}
	if err := insertAPIToken(tx, tok, tokenHash, eventID); err != nil {
		return err
	}
	return tx.Commit()
}
