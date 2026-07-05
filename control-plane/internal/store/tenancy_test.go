package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// createTenantStrategy inserts a strategy under an explicit tenant.
func createTenantStrategy(t *testing.T, s *Store, strategyID, tenantID string) {
	t.Helper()
	err := s.CreateStrategy(Strategy{
		StrategyID: strategyID, TenantID: tenantID, Name: "strategy-" + strategyID,
		LifecycleState: "paper", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("CreateStrategy(%s): %v", strategyID, err)
	}
}

func strptr(v string) *string { return &v }

// TestOpenMigratesTenancy: reopening an existing DB is idempotent — the
// guarded tenant_id ALTER runs once, and every pre-existing
// strategies.tenant_id value plus 'default' gets a tenants row.
func TestOpenMigratesTenancy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	createTenantStrategy(t, s, uid(1), "grandfathered")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < 2; i++ { // reopen twice: ALTER and seeds stay idempotent
		if s, err = Open(path); err != nil {
			t.Fatalf("reopen #%d: %v", i+1, err)
		}
		if i == 0 {
			s.Close()
		}
	}
	defer s.Close()
	for _, tenant := range []string{"default", "grandfathered"} {
		if _, err := s.GetTenant(tenant); err != nil {
			t.Errorf("GetTenant(%s) after reopen: %v", tenant, err)
		}
	}
	// The migrated tenant_id column is live: a tenant kill persists.
	if _, err := s.AppendTenantKill(uid(90), "grandfathered", "env-admin", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendTenantKill on migrated DB: %v", err)
	}
}

// TestTenantKillScoping pins the normative kill predicate: Phase 1 global
// rows (both columns NULL) bind every strategy, strategy rows bind their
// strategy, tenant rows bind ONLY their tenant — never globally.
func TestTenantKillScoping(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-b")
	cutoff := "2026-07-04T11:00:00Z"

	epoch, err := s.AppendTenantKill(uid(80), "tenant-a", "admin-1", formatTime(testNow), false)
	if err != nil || epoch != 1 {
		t.Fatalf("AppendTenantKill: epoch=%d err=%v, want 1, nil", epoch, err)
	}
	assertEpoch := func(name, strategyID string, want int64) {
		t.Helper()
		if got, err := s.GlobalMaxKillEpoch(strategyID); err != nil || got != want {
			t.Errorf("%s: GlobalMaxKillEpoch = %d err=%v, want %d", name, got, err, want)
		}
		if got, err := s.MaxKillEpoch(strategyID, cutoff); err != nil || got != want {
			t.Errorf("%s: MaxKillEpoch = %d err=%v, want %d", name, got, err, want)
		}
	}
	assertEpoch("tenant-a strategy sees the tenant kill", uid(1), 1)
	assertEpoch("tenant-b strategy is untouched", uid(2), 0)

	// A Phase 1 global row (both columns NULL) keeps binding everyone.
	five := int64(5)
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(81), Kind: "kill", Scope: "global", KillEpoch: &five,
		ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	assertEpoch("global row binds tenant-a", uid(1), 5)
	assertEpoch("global row binds tenant-b", uid(2), 5)

	// Epoch monotonicity across scopes: MAX over the WHOLE table + 1.
	epoch, err = s.AppendTenantKill(uid(82), "tenant-b", "admin-2", formatTime(testNow), false)
	if err != nil || epoch != 6 {
		t.Fatalf("second AppendTenantKill: epoch=%d err=%v, want 6 (strictly above every prior)", epoch, err)
	}
	assertEpoch("tenant-b now killed at 6", uid(2), 6)
	assertEpoch("tenant-a stays at the global 5", uid(1), 5)

	// A strategy-scope row still binds only its strategy.
	seven := int64(7)
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(83), Kind: "kill", Scope: "strategy", StrategyID: strptr(uid(2)),
		KillEpoch: &seven, ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	assertEpoch("strategy row binds uid(2)", uid(2), 7)
	assertEpoch("strategy row does not bind uid(1)", uid(1), 5)
}

// TestRiskLimitChangesReplayOrder: the overlay replay order is rowid
// ascending — last write wins even when changed_at ties.
func TestRiskLimitChangesReplayOrder(t *testing.T) {
	s := openStore(t)
	first := []RiskLimitChange{
		{ChangeID: uid(1), StrategyID: uid(10), Field: "max_open_positions",
			OldValue: strptr("3"), NewValue: "5", ActorID: "a", ChangedAt: formatTime(testNow)},
		{ChangeID: uid(2), StrategyID: uid(10), Field: "daily_loss_limit_quote",
			OldValue: strptr("500"), NewValue: "250", ActorID: "a", ChangedAt: formatTime(testNow)},
	}
	if err := s.AppendRiskLimitChanges(first); err != nil {
		t.Fatalf("AppendRiskLimitChanges: %v", err)
	}
	second := []RiskLimitChange{{
		ChangeID: uid(3), StrategyID: uid(10), Field: "max_open_positions",
		OldValue: strptr("5"), NewValue: "2", ActorID: "b", ChangedAt: formatTime(testNow),
	}}
	if err := s.AppendRiskLimitChanges(second); err != nil {
		t.Fatalf("AppendRiskLimitChanges: %v", err)
	}
	rows, err := s.RiskLimitChanges()
	if err != nil {
		t.Fatalf("RiskLimitChanges: %v", err)
	}
	if len(rows) != 3 || rows[0].ChangeID != uid(1) || rows[1].ChangeID != uid(2) || rows[2].ChangeID != uid(3) {
		t.Fatalf("replay order = %+v, want insert (rowid) order despite equal changed_at", rows)
	}
	if rows[2].NewValue != "2" || *rows[2].OldValue != "5" {
		t.Errorf("last row = %+v, want the winning 5 -> 2 change", rows[2])
	}
}

// TestAPITokenStoreLifecycle covers the store half of the token rules:
// hash-keyed lookup, single revoked_at mutation with its audit events,
// tenant-scoped lists, and the sentinel errors the API layer routes on.
func TestAPITokenStoreLifecycle(t *testing.T) {
	s := openStore(t)
	if err := s.CreateTenant(Tenant{TenantID: "tenant-a", Name: "A", CreatedAt: formatTime(testNow)}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.CreateTenant(Tenant{TenantID: "tenant-a", Name: "again", CreatedAt: formatTime(testNow)}); !errors.Is(err, ErrTenantExists) {
		t.Fatalf("duplicate tenant err = %v, want ErrTenantExists", err)
	}

	owner := "owner"
	tok := APIToken{
		TokenID: uid(1), TenantID: "tenant-a", Principal: "user", Role: &owner,
		Label: "first-owner", CreatedBy: "env-admin", CreatedAt: formatTime(testNow),
	}
	if err := s.InsertAPIToken(tok, "hash-1", uid(2)); err != nil {
		t.Fatalf("InsertAPIToken: %v", err)
	}
	dup := tok
	dup.TokenID = uid(3)
	if err := s.InsertAPIToken(dup, "hash-1", uid(4)); !errors.Is(err, ErrDuplicateTokenHash) {
		t.Fatalf("duplicate hash err = %v, want ErrDuplicateTokenHash", err)
	}

	if got, err := s.TokenByHash("hash-1"); err != nil || got.TokenID != uid(1) || got.RevokedAt != nil {
		t.Fatalf("TokenByHash = %+v err=%v", got, err)
	}
	if _, err := s.TokenByHash("hash-absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent hash err = %v, want ErrNotFound", err)
	}
	if n, err := s.CountUnrevokedOwnerTokens("tenant-a"); err != nil || n != 1 {
		t.Fatalf("CountUnrevokedOwnerTokens = %d err=%v, want 1", n, err)
	}

	if revoked, err := s.RevokeAPIToken(uid(1), formatTime(testNow), uid(5), "actor"); err != nil || !revoked {
		t.Fatalf("RevokeAPIToken: revoked=%v err=%v, want true, nil", revoked, err)
	}
	// Second revoke: no-op, the first revocation stands, no second event.
	if revoked, err := s.RevokeAPIToken(uid(1), "2027-01-01T00:00:00Z", uid(6), "actor"); err != nil || revoked {
		t.Fatalf("second revoke: revoked=%v err=%v, want false, nil", revoked, err)
	}
	if _, err := s.RevokeAPIToken(uid(9), formatTime(testNow), uid(7), "actor"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent token revoke err = %v, want ErrNotFound", err)
	}
	got, err := s.GetAPIToken(uid(1))
	if err != nil || got.RevokedAt == nil || *got.RevokedAt != formatTime(testNow) {
		t.Fatalf("GetAPIToken after revokes = %+v err=%v, want the FIRST revoked_at", got, err)
	}
	if n, err := s.CountUnrevokedOwnerTokens("tenant-a"); err != nil || n != 0 {
		t.Fatalf("CountUnrevokedOwnerTokens after revoke = %d err=%v, want 0", n, err)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM token_events WHERE token_id = ?`, uid(1)).Scan(&events); err != nil || events != 2 {
		t.Fatalf("token_events = %d err=%v, want created + revoked only", events, err)
	}

	// Atomic tenant + first-owner mint: a taken tenant_id rolls the token
	// back too (no orphan credential).
	fresh := APIToken{TokenID: uid(20), TenantID: "tenant-c", Principal: "user", Role: &owner,
		Label: "initial-owner", CreatedBy: "env-admin", CreatedAt: formatTime(testNow)}
	if err := s.CreateTenantWithOwnerToken(Tenant{TenantID: "tenant-c", Name: "C", CreatedAt: formatTime(testNow)},
		fresh, "hash-c", uid(21)); err != nil {
		t.Fatalf("CreateTenantWithOwnerToken: %v", err)
	}
	taken := fresh
	taken.TokenID, taken.TenantID = uid(22), "tenant-a"
	err = s.CreateTenantWithOwnerToken(Tenant{TenantID: "tenant-a", Name: "dup", CreatedAt: formatTime(testNow)},
		taken, "hash-d", uid(23))
	if !errors.Is(err, ErrTenantExists) {
		t.Fatalf("taken tenant err = %v, want ErrTenantExists", err)
	}
	if _, err := s.TokenByHash("hash-d"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled-back mint left a credential: err = %v, want ErrNotFound", err)
	}

	items, total, err := s.ListAPITokens("tenant-a", 1, 10)
	if err != nil || total != 1 || len(items) != 1 || items[0].TenantID != "tenant-a" {
		t.Fatalf("ListAPITokens(tenant-a) = %+v total=%d err=%v, want the single tenant-a row", items, total, err)
	}
	if _, total, err = s.ListAPITokens("", 1, 10); err != nil || total != 2 {
		t.Fatalf("ListAPITokens(all) total = %d err=%v, want 2", total, err)
	}
}
