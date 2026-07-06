package store

import (
	"database/sql"
	"fmt"
)

// Password-auth users and their web sessions (multi-tenant-rbac.md
// §Password auth and web sessions). users/web_sessions follow the
// api_tokens pattern: password_hash and token_hash never cross the read
// boundary (no-read-back invariant); every create, login, and logout also
// appends a user_events row (invariant 7).

// insertUser persists a user (bcrypt hash, never plaintext) and its
// user_events 'created' row. ErrEmailExists on a taken email.
func insertUser(q dbtx, u User, passwordHash, eventID string) error {
	_, err := q.Exec(`INSERT INTO users
		(user_id, tenant_id, email, password_hash, role, created_at, disabled_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		u.UserID, u.TenantID, u.Email, passwordHash, u.Role, u.CreatedAt)
	if isUniqueConstraint(err) {
		return fmt.Errorf("user %s: %w", u.UserID, ErrEmailExists)
	}
	if err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO user_events (event_id, user_id, event, actor_id, recorded_at)
		VALUES (?, ?, 'created', ?, ?)`, eventID, u.UserID, u.UserID, u.CreatedAt)
	return err
}

// CreatePlatformAdmin performs the one-time bootstrap: the zero-admin check
// and the insert run in ONE transaction, so two concurrent bootstraps
// cannot both pass the gate. ErrPlatformAdminExists when any platform_admin
// user exists (disabled or not); ErrEmailExists on a taken email.
func (s *Store) CreatePlatformAdmin(u User, passwordHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'platform_admin'`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("bootstrap: %w", ErrPlatformAdminExists)
	}
	if err := insertUser(tx, u, passwordHash, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateTenantWithOwnerUser atomically creates a tenant AND its first owner
// user (multi-tenant-rbac.md §Password auth: signup). ErrTenantExists on a
// taken tenant_id; ErrEmailExists rolls the tenant back too.
func (s *Store) CreateTenantWithOwnerUser(t Tenant, u User, passwordHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := createTenant(tx, t); err != nil {
		return err
	}
	if err := insertUser(tx, u, passwordHash, eventID); err != nil {
		return err
	}
	return tx.Commit()
}

// UserByEmail resolves the login lookup, returning the user plus the bcrypt
// password_hash — handed to the login comparison ONLY, never to any read
// endpoint. ErrNotFound for an unknown email.
func (s *Store) UserByEmail(email string) (User, string, error) {
	var u User
	var passwordHash string
	var tenantID, disabledAt sql.NullString
	err := s.db.QueryRow(`SELECT user_id, tenant_id, email, password_hash, role, created_at, disabled_at
		FROM users WHERE email = ?`, email).
		Scan(&u.UserID, &tenantID, &u.Email, &passwordHash, &u.Role, &u.CreatedAt, &disabledAt)
	if err == sql.ErrNoRows {
		return User{}, "", fmt.Errorf("user email: %w", ErrNotFound)
	}
	u.TenantID = nullable(tenantID)
	u.DisabledAt = nullable(disabledAt)
	return u, passwordHash, err
}

// ListUsers returns every user's metadata — NEVER password_hash (the
// no-read-back invariant; the User struct has no hash field) — ordered
// created_at then user_id (the admin-console listing; env-admin ONLY at
// the API layer).
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT user_id, tenant_id, email, role, created_at, disabled_at
		FROM users ORDER BY created_at, user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var tenantID, disabledAt sql.NullString
		if err := rows.Scan(&u.UserID, &tenantID, &u.Email, &u.Role, &u.CreatedAt, &disabledAt); err != nil {
			return nil, err
		}
		u.TenantID = nullable(tenantID)
		u.DisabledAt = nullable(disabledAt)
		out = append(out, u)
	}
	return out, rows.Err()
}

// InsertWebSession persists a minted session (hash only, never plaintext)
// and its user_events 'login' row in one transaction. ErrDuplicateTokenHash
// on a UNIQUE token_hash collision (caller retries, never surfaces it).
func (s *Store) InsertWebSession(ws WebSession, tokenHash, eventID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	_, err = tx.Exec(`INSERT INTO web_sessions
		(session_id, user_id, token_hash, created_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, NULL)`,
		ws.SessionID, ws.UserID, tokenHash, ws.CreatedAt, ws.ExpiresAt)
	if isUniqueConstraint(err) {
		return ErrDuplicateTokenHash
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO user_events (event_id, user_id, event, actor_id, recorded_at)
		VALUES (?, ?, 'login', ?, ?)`, eventID, ws.UserID, ws.UserID, ws.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// WebSessionByHash resolves the auth lookup: exact match on the
// fixed-length SHA-256 digest via the UNIQUE index, joined to the session's
// user. The row is returned revoked/expired/disabled or not — the CALLER
// must observe RevokedAt, ExpiresAt, and DisabledAt and answer 401 (no auth
// result may outlive its request).
func (s *Store) WebSessionByHash(tokenHash string) (WebSession, User, error) {
	var ws WebSession
	var u User
	var revokedAt, tenantID, disabledAt sql.NullString
	err := s.db.QueryRow(`SELECT ws.session_id, ws.user_id, ws.created_at, ws.expires_at, ws.revoked_at,
		u.tenant_id, u.email, u.role, u.created_at, u.disabled_at
		FROM web_sessions ws JOIN users u ON u.user_id = ws.user_id
		WHERE ws.token_hash = ?`, tokenHash).
		Scan(&ws.SessionID, &ws.UserID, &ws.CreatedAt, &ws.ExpiresAt, &revokedAt,
			&tenantID, &u.Email, &u.Role, &u.CreatedAt, &disabledAt)
	if err == sql.ErrNoRows {
		return WebSession{}, User{}, fmt.Errorf("session hash: %w", ErrNotFound)
	}
	if err != nil {
		return WebSession{}, User{}, err
	}
	ws.RevokedAt = nullable(revokedAt)
	u.UserID = ws.UserID
	u.TenantID = nullable(tenantID)
	u.DisabledAt = nullable(disabledAt)
	return ws, u, nil
}

// RevokeWebSession sets revoked_at (the session's single legal mutation,
// once) and appends the user_events 'logout' row in one transaction. A
// session already revoked is left untouched — the first revocation stands —
// and reports revoked=false with no second event. ErrNotFound for an absent
// session_id.
func (s *Store) RevokeWebSession(sessionID, revokedAt, eventID string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	res, err := tx.Exec(`UPDATE web_sessions SET revoked_at = ?
		WHERE session_id = ? AND revoked_at IS NULL`, revokedAt, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM web_sessions WHERE session_id = ?`,
			sessionID).Scan(&exists); err != nil {
			return false, err
		}
		if exists == 0 {
			return false, fmt.Errorf("session %s: %w", sessionID, ErrNotFound)
		}
		return false, tx.Commit() // already revoked: idempotent no-op
	}
	if _, err := tx.Exec(`INSERT INTO user_events (event_id, user_id, event, actor_id, recorded_at)
		SELECT ?, user_id, 'logout', user_id, ? FROM web_sessions WHERE session_id = ?`,
		eventID, revokedAt, sessionID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// DisableUser sets disabled_at once (idempotent: the first disable stands).
// A disabled user's sessions stop resolving on the NEXT request — the
// session lookup observes disabled_at. ErrNotFound for an absent user_id.
func (s *Store) DisableUser(userID, disabledAt string) error {
	res, err := s.db.Exec(`UPDATE users SET disabled_at = ?
		WHERE user_id = ? AND disabled_at IS NULL`, disabledAt, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE user_id = ?`,
			userID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("user %s: %w", userID, ErrNotFound)
		}
	}
	return nil
}
