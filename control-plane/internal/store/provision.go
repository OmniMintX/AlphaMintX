package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ErrStrategyNameTaken: another strategy in the SAME tenant already holds
// this trimmed name (strategy-provisioning.md SP-4 — retry safety: a
// re-POST after a timeout gets a deterministic conflict, never a silent
// duplicate).
var ErrStrategyNameTaken = errors.New("STRATEGY_NAME_TAKEN")

// ErrStrategyLimitReached: the tenant is at its strategy cap
// (strategy-provisioning.md SP-4b — the safety monitor and platform-kill
// driver iterate every strategies row, so tenant-driven growth is bounded).
var ErrStrategyLimitReached = errors.New("STRATEGY_LIMIT_REACHED")

// ErrInvalidInitialState: CreateStrategyProvisioned accepts only draft and
// paper at birth (strategy-provisioning.md SP-4 defense in depth under the
// handler's 400 — the store refuses live-at-birth rows even if a future
// handler regresses).
var ErrInvalidInitialState = errors.New("INVALID_INITIAL_STATE")

// CreateStrategyProvisioned is the POST /api/v1/strategies entry point
// (strategy-provisioning.md SP-4/SP-4a/SP-4b): inside ONE transaction it
// enforces the per-tenant trimmed-name uniqueness rule (the STORED side is
// compared Go-trimmed too, so legacy rows padded with ANY unicode
// whitespace still collide — SQLite's TRIM strips 0x20 only), THEN the
// per-tenant cap (name-taken wins at the cap boundary: a timed-out retry
// of the create that filled the cap must still get the deterministic
// STRATEGY_NAME_TAKEN), inserts the row WITH created_by, and writes the
// LC-16a bootstrap transition for an initial `paper` state. Race-freedom
// rests on the store's single-connection invariant (SetMaxOpenConns(1),
// the multi-tenant-rbac.md §Tenant kill-switch precedent); any relaxation
// of that invariant requires BEGIN IMMEDIATE or an equivalent here.
// CreateStrategy (tests/replay wiring) keeps its unrestricted semantics.
func (s *Store) CreateStrategyProvisioned(st Strategy, createdBy string, maxPerTenant int) error {
	if st.LifecycleState != "draft" && st.LifecycleState != "paper" {
		return fmt.Errorf("initial state %q: %w", st.LifecycleState, ErrInvalidInitialState)
	}
	if maxPerTenant < 1 {
		return fmt.Errorf("maxPerTenant %d: must be >= 1 (strategy-provisioning.md SP-4b)", maxPerTenant)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	// One pass serves both checks: the row count IS the quota input, and
	// the names are compared Go-trimmed (bounded by the cap, so the scan
	// is small by construction).
	rows, err := tx.Query(`SELECT name FROM strategies WHERE tenant_id = ?`, st.TenantID)
	if err != nil {
		return err
	}
	count, taken := 0, false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		count++
		if strings.TrimSpace(name) == st.Name {
			taken = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("name %q in tenant %s: %w", st.Name, st.TenantID, ErrStrategyNameTaken)
	}
	if count >= maxPerTenant {
		return fmt.Errorf("tenant %s at %d strategies: %w", st.TenantID, count, ErrStrategyLimitReached)
	}
	roleModels, err := marshalRoleModels(st.RoleModels)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO strategies
		(strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at, created_by, role_models)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		st.StrategyID, st.TenantID, st.Name, st.LifecycleState,
		st.CreatedAt, st.UpdatedAt, createdBy, roleModels); err != nil {
		return err
	}
	if st.LifecycleState == "paper" {
		if _, err := tx.Exec(`INSERT INTO lifecycle_transitions
			(transition_id, strategy_id, from_state, to_state, actor_id, actor_role, reason, recorded_at)
			VALUES (?, ?, 'draft', 'paper', 'bootstrap', 'system', 'bootstrap', ?)`,
			uuid.NewString(), st.StrategyID, st.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateStrategyProvisioning is the additive strategy-provisioning.md
// SP-4a migration: strategies pre-exists, so created_by is added iff
// absent (NOT NULL DEFAULT ” — legacy rows read ”). Audit-only: read
// paths never select it.
func migrateStrategyProvisioning(db *sql.DB) error {
	var have int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('strategies')
		WHERE name = 'created_by'`).Scan(&have); err != nil {
		return err
	}
	if have == 0 {
		if _, err := db.Exec(`ALTER TABLE strategies ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// migrateStrategyRoleModels is the additive Phase-29 migration: strategies
// pre-exists, so role_models is added iff absent (NOT NULL DEFAULT ” —
// legacy rows read ”, i.e. no per-role overrides). The column stores the
// role→model override map as raw JSON text.
func migrateStrategyRoleModels(db *sql.DB) error {
	var have int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('strategies')
		WHERE name = 'role_models'`).Scan(&have); err != nil {
		return err
	}
	if have == 0 {
		if _, err := db.Exec(`ALTER TABLE strategies ADD COLUMN role_models TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}
