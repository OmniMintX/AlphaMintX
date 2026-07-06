package store

import (
	"database/sql"
	"fmt"
)

// Platform secrets (docs/specs/platform-secrets.md): platform_secrets is a
// mutable snapshot like strategy_state — vault-sealed ciphertext plus
// non-secret display metadata; every upsert appends a secret_events audit
// row in the SAME transaction ('set' the first time, 'rotated' after),
// invariant 7. The ciphertext crosses the read boundary ONLY through
// GetPlatformSecret; the listing is metadata-only.

// UpsertPlatformSecret writes the snapshot and its audit row in one
// transaction; the action recorded is 'set' when the kind is new and
// 'rotated' when it replaces an existing row.
func (s *Store) UpsertPlatformSecret(kind, ciphertext, metaJSON, actorID, eventID, recordedAt string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM platform_secrets WHERE kind = ?`, kind).Scan(&n); err != nil {
		return err
	}
	action := "set"
	if n > 0 {
		action = "rotated"
	}
	if _, err := tx.Exec(`INSERT INTO platform_secrets (kind, payload_ciphertext, meta_json, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (kind) DO UPDATE SET payload_ciphertext = excluded.payload_ciphertext,
			meta_json = excluded.meta_json, updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		kind, ciphertext, metaJSON, recordedAt, actorID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO secret_events (event_id, kind, action, actor_id, recorded_at)
		VALUES (?, ?, ?, ?, ?)`, eventID, kind, action, actorID, recordedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// GetPlatformSecret returns one kind's sealed ciphertext plus metadata, or
// ErrNotFound. The ciphertext is opened by the vault holder only — it never
// crosses an API response boundary.
func (s *Store) GetPlatformSecret(kind string) (ciphertext, metaJSON, updatedAt, updatedBy string, err error) {
	err = s.db.QueryRow(`SELECT payload_ciphertext, meta_json, updated_at, updated_by
		FROM platform_secrets WHERE kind = ?`, kind).
		Scan(&ciphertext, &metaJSON, &updatedAt, &updatedBy)
	if err == sql.ErrNoRows {
		return "", "", "", "", fmt.Errorf("platform secret %s: %w", kind, ErrNotFound)
	}
	return ciphertext, metaJSON, updatedAt, updatedBy, err
}

// PlatformSecretMeta is the metadata-only listing row: NO ciphertext.
type PlatformSecretMeta struct {
	Kind      string
	MetaJSON  string
	UpdatedAt string
	UpdatedBy string
}

// ListPlatformSecretMeta lists every stored secret's non-secret metadata,
// sorted by kind. The ciphertext column is never selected.
func (s *Store) ListPlatformSecretMeta() ([]PlatformSecretMeta, error) {
	rows, err := s.db.Query(`SELECT kind, meta_json, updated_at, updated_by
		FROM platform_secrets ORDER BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlatformSecretMeta
	for rows.Next() {
		var m PlatformSecretMeta
		if err := rows.Scan(&m.Kind, &m.MetaJSON, &m.UpdatedAt, &m.UpdatedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
