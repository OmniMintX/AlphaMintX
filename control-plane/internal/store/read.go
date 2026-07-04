package store

import (
	"database/sql"
	"fmt"
)

// Pagination bounds (persistence-and-api.md HTTP API): page is 1-based,
// limit defaults to 20 and caps at 100.
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

func normalizePage(page, limit int) (int, int) {
	if page < 1 {
		page = 1
	}
	if limit < 1 {
		limit = DefaultPageLimit
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	return page, limit
}

// ListStrategies returns one page of strategies plus the total count.
func (s *Store) ListStrategies(page, limit int) ([]Strategy, int, error) {
	page, limit = normalizePage(page, limit)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM strategies`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at
		FROM strategies ORDER BY created_at, strategy_id LIMIT ? OFFSET ?`, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		var st Strategy
		if err := rows.Scan(&st.StrategyID, &st.TenantID, &st.Name, &st.LifecycleState,
			&st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, st)
	}
	return out, total, rows.Err()
}

// GetStrategy returns one strategy or ErrNotFound.
func (s *Store) GetStrategy(strategyID string) (Strategy, error) {
	var st Strategy
	err := s.db.QueryRow(`SELECT strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at
		FROM strategies WHERE strategy_id = ?`, strategyID).
		Scan(&st.StrategyID, &st.TenantID, &st.Name, &st.LifecycleState, &st.CreatedAt, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return Strategy{}, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
	}
	return st, err
}

// GetStrategyInTenant is the tenant-scoped root resolution
// (multi-tenant-rbac.md §Tenancy rules): tenantID is threaded from the
// authenticated principal, never from request input, and a foreign-tenant
// strategy is indistinguishable from absence (ErrNotFound).
func (s *Store) GetStrategyInTenant(strategyID, tenantID string) (Strategy, error) {
	var st Strategy
	err := s.db.QueryRow(`SELECT strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at
		FROM strategies WHERE strategy_id = ? AND tenant_id = ?`, strategyID, tenantID).
		Scan(&st.StrategyID, &st.TenantID, &st.Name, &st.LifecycleState, &st.CreatedAt, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return Strategy{}, fmt.Errorf("strategy %s: %w", strategyID, ErrNotFound)
	}
	return st, err
}

// ListStrategiesByTenant is the tenant-scoped ListStrategies: items and
// total never contain foreign rows (multi-tenant-rbac.md §Lists).
func (s *Store) ListStrategiesByTenant(tenantID string, page, limit int) ([]Strategy, int, error) {
	page, limit = normalizePage(page, limit)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM strategies WHERE tenant_id = ?`, tenantID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at
		FROM strategies WHERE tenant_id = ? ORDER BY created_at, strategy_id LIMIT ? OFFSET ?`,
		tenantID, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Strategy
	for rows.Next() {
		var st Strategy
		if err := rows.Scan(&st.StrategyID, &st.TenantID, &st.Name, &st.LifecycleState,
			&st.CreatedAt, &st.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, st)
	}
	return out, total, rows.Err()
}

// ListRuns returns one page of a strategy's runs, tick_number DESC, plus the
// total count.
func (s *Store) ListRuns(strategyID string, page, limit int) ([]Run, int, error) {
	page, limit = normalizePage(page, limit)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs WHERE strategy_id = ?`, strategyID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT run_id, strategy_id, tick_number, created_at, completed_at
		FROM runs WHERE strategy_id = ? ORDER BY tick_number DESC LIMIT ? OFFSET ?`,
		strategyID, limit, (page-1)*limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

func scanRun(row rowScanner) (Run, error) {
	var r Run
	var completed sql.NullString
	if err := row.Scan(&r.RunID, &r.StrategyID, &r.TickNumber, &r.CreatedAt, &completed); err != nil {
		return Run{}, err
	}
	if completed.Valid {
		r.CompletedAt = &completed.String
	}
	return r, nil
}
