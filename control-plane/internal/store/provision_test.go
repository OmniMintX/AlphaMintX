package store

import (
	"errors"
	"testing"
)

func provisioned(tenantID, strategyID, name, state string) Strategy {
	return Strategy{
		StrategyID: strategyID, TenantID: tenantID, Name: name,
		LifecycleState: state, CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	}
}

// TestCreateStrategyProvisionedStateRestriction pins the SP-4 defense in
// depth: the provisioning entry point refuses every non-draft/paper
// initial state at the store layer (no row), even though CreateStrategy
// (tests/replay) keeps its unrestricted LC-16a semantics.
func TestCreateStrategyProvisionedStateRestriction(t *testing.T) {
	s := openStore(t)
	for _, state := range []string{"live_l1", "live_l2", "live_l3", "killed", "paused", ""} {
		err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(1), "n", state), "env-admin", 100)
		if !errors.Is(err, ErrInvalidInitialState) {
			t.Fatalf("state %q: err = %v, want ErrInvalidInitialState", state, err)
		}
	}
	if _, total, err := s.ListStrategies(1, 10); err != nil || total != 0 {
		t.Fatalf("rows after rejections = %d (err %v), want 0", total, err)
	}
}

// TestCreateStrategyProvisionedPersistsCreatedBy pins SP-4a: the creator
// is persisted; a paper birth writes exactly one LC-16a bootstrap row and
// a draft birth writes none; legacy CreateStrategy rows read created_by ”.
func TestCreateStrategyProvisionedPersistsCreatedBy(t *testing.T) {
	s := openStore(t)
	if err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(1), "alpha", "paper"), "token:t-1", 100); err != nil {
		t.Fatalf("paper create: %v", err)
	}
	if err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(2), "beta", "draft"), "env-admin", 100); err != nil {
		t.Fatalf("draft create: %v", err)
	}
	createStrategy(t, s, uid(3)) // legacy path, no created_by
	for _, c := range []struct{ id, wantBy string }{
		{uid(1), "token:t-1"}, {uid(2), "env-admin"}, {uid(3), ""},
	} {
		var by string
		if err := s.db.QueryRow(`SELECT created_by FROM strategies WHERE strategy_id = ?`, c.id).Scan(&by); err != nil {
			t.Fatalf("read created_by(%s): %v", c.id, err)
		}
		if by != c.wantBy {
			t.Errorf("created_by(%s) = %q, want %q", c.id, by, c.wantBy)
		}
	}
	for _, c := range []struct {
		id   string
		want int
	}{{uid(1), 1}, {uid(2), 0}} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_transitions
			WHERE strategy_id = ? AND from_state = 'draft' AND actor_id = 'bootstrap'
			AND actor_role = 'system'`, c.id).Scan(&n); err != nil {
			t.Fatalf("count transitions(%s): %v", c.id, err)
		}
		if n != c.want {
			t.Errorf("bootstrap rows(%s) = %d, want %d", c.id, n, c.want)
		}
	}
}

// TestCreateStrategyProvisionedNameTaken pins SP-4 uniqueness: same
// trimmed name in the same tenant conflicts — including against a LEGACY
// row with stray whitespace (TRIM on the stored side) — while the same
// name in a different tenant is fine.
func TestCreateStrategyProvisionedNameTaken(t *testing.T) {
	s := openStore(t)
	if err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(1), "alpha", "draft"), "e", 100); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(2), "alpha", "draft"), "e", 100)
	if !errors.Is(err, ErrStrategyNameTaken) {
		t.Fatalf("duplicate: err = %v, want ErrStrategyNameTaken", err)
	}
	// Legacy rows with stray whitespace collide — the stored side is
	// compared Go-trimmed, so tabs/NBSP pad the same as spaces (SQLite's
	// TRIM would strip 0x20 only).
	for i, legacy := range []string{"  gamma  ", "\tdelta", "\u00A0eps\u00A0"} {
		if err := s.CreateStrategy(provisioned("tenant-1", uid(10+i), legacy, "draft")); err != nil {
			t.Fatalf("legacy insert %q: %v", legacy, err)
		}
	}
	for _, name := range []string{"gamma", "delta", "eps"} {
		err = s.CreateStrategyProvisioned(provisioned("tenant-1", uid(20), name, "draft"), "e", 100)
		if !errors.Is(err, ErrStrategyNameTaken) {
			t.Fatalf("legacy trim collision %q: err = %v, want ErrStrategyNameTaken", name, err)
		}
	}
	if err := s.CreateStrategyProvisioned(provisioned("tenant-2", uid(5), "alpha", "draft"), "e", 100); err != nil {
		t.Fatalf("other tenant, same name: %v", err)
	}
}

// TestCreateStrategyProvisionedQuota pins SP-4b: the cap counts ALL rows
// of the tenant regardless of lifecycle state; at the cap the create is
// refused with no row; other tenants are unaffected.
func TestCreateStrategyProvisionedQuota(t *testing.T) {
	s := openStore(t)
	for i, name := range []string{"a", "b"} {
		if err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(i+1), name, "draft"), "e", 2); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	err := s.CreateStrategyProvisioned(provisioned("tenant-1", uid(3), "c", "draft"), "e", 2)
	if !errors.Is(err, ErrStrategyLimitReached) {
		t.Fatalf("at cap: err = %v, want ErrStrategyLimitReached", err)
	}
	// At the cap a DUPLICATE name still answers name-taken, not limit:
	// the timed-out-retry contract (SP-4) survives the cap boundary.
	err = s.CreateStrategyProvisioned(provisioned("tenant-1", uid(3), "a", "draft"), "e", 2)
	if !errors.Is(err, ErrStrategyNameTaken) {
		t.Fatalf("dup at cap: err = %v, want ErrStrategyNameTaken", err)
	}
	if _, total, err := s.ListStrategies(1, 10); err != nil || total != 2 {
		t.Fatalf("rows = %d (err %v), want 2", total, err)
	}
	if err := s.CreateStrategyProvisioned(provisioned("tenant-2", uid(4), "c", "draft"), "e", 2); err != nil {
		t.Fatalf("other tenant under cap: %v", err)
	}
}
