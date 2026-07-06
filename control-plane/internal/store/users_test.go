package store

import (
	"errors"
	"testing"
)

// TestUserStoreLifecycle covers the store half of the password-auth rules
// (multi-tenant-rbac.md §Password auth and web sessions): the transactional
// bootstrap gate, atomic tenant+owner signup with email rollback,
// hash-keyed session lookup, the single revoked_at mutation with its audit
// events, and the sentinel errors the API layer routes on.
func TestUserStoreLifecycle(t *testing.T) {
	s := openStore(t)
	admin := User{UserID: uid(1), Email: "root@example.com", Role: "platform_admin", CreatedAt: formatTime(testNow)}
	if err := s.CreatePlatformAdmin(admin, "bcrypt-hash-a", uid(10)); err != nil {
		t.Fatalf("CreatePlatformAdmin: %v", err)
	}
	second := User{UserID: uid(2), Email: "root2@example.com", Role: "platform_admin", CreatedAt: formatTime(testNow)}
	if err := s.CreatePlatformAdmin(second, "bcrypt-hash-b", uid(11)); !errors.Is(err, ErrPlatformAdminExists) {
		t.Fatalf("second bootstrap err = %v, want ErrPlatformAdminExists", err)
	}

	tenantA := "tenant-a"
	owner := User{UserID: uid(3), TenantID: &tenantA, Email: "owner@a.com", Role: "owner", CreatedAt: formatTime(testNow)}
	if err := s.CreateTenantWithOwnerUser(Tenant{TenantID: tenantA, Name: "A", CreatedAt: formatTime(testNow)},
		owner, "bcrypt-hash-c", uid(12)); err != nil {
		t.Fatalf("CreateTenantWithOwnerUser: %v", err)
	}
	// A taken email rolls the WHOLE signup back, tenant included.
	tenantB := "tenant-b"
	dup := User{UserID: uid(4), TenantID: &tenantB, Email: "owner@a.com", Role: "owner", CreatedAt: formatTime(testNow)}
	if err := s.CreateTenantWithOwnerUser(Tenant{TenantID: tenantB, Name: "B", CreatedAt: formatTime(testNow)},
		dup, "bcrypt-hash-d", uid(13)); !errors.Is(err, ErrEmailExists) {
		t.Fatalf("duplicate email err = %v, want ErrEmailExists", err)
	}
	if _, err := s.GetTenant(tenantB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant-b after rollback err = %v, want ErrNotFound", err)
	}

	u, hash, err := s.UserByEmail("owner@a.com")
	if err != nil || hash != "bcrypt-hash-c" || u.UserID != uid(3) || u.TenantID == nil || *u.TenantID != tenantA {
		t.Fatalf("UserByEmail = %+v hash %q err %v", u, hash, err)
	}
	if _, _, err := s.UserByEmail("nobody@a.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown email err = %v, want ErrNotFound", err)
	}

	ws := WebSession{SessionID: uid(5), UserID: uid(3), CreatedAt: formatTime(testNow), ExpiresAt: "2026-07-11T12:00:00Z"}
	if err := s.InsertWebSession(ws, "session-hash-1", uid(14)); err != nil {
		t.Fatalf("InsertWebSession: %v", err)
	}
	collide := WebSession{SessionID: uid(6), UserID: uid(3), CreatedAt: formatTime(testNow), ExpiresAt: "2026-07-11T12:00:00Z"}
	if err := s.InsertWebSession(collide, "session-hash-1", uid(15)); !errors.Is(err, ErrDuplicateTokenHash) {
		t.Fatalf("hash collision err = %v, want ErrDuplicateTokenHash", err)
	}
	got, gu, err := s.WebSessionByHash("session-hash-1")
	if err != nil || got.SessionID != uid(5) || got.RevokedAt != nil || gu.Email != "owner@a.com" {
		t.Fatalf("WebSessionByHash = %+v / %+v err %v", got, gu, err)
	}
	if _, _, err := s.WebSessionByHash("unknown-hash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session err = %v, want ErrNotFound", err)
	}

	if revoked, err := s.RevokeWebSession(uid(5), formatTime(testNow), uid(16)); err != nil || !revoked {
		t.Fatalf("RevokeWebSession = %v, %v", revoked, err)
	}
	if revoked, err := s.RevokeWebSession(uid(5), formatTime(testNow), uid(17)); err != nil || revoked {
		t.Fatalf("second revoke = %v, %v (want idempotent no-op)", revoked, err)
	}
	if _, err := s.RevokeWebSession(uid(99), formatTime(testNow), uid(18)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent session revoke err = %v, want ErrNotFound", err)
	}
	if got, _, err = s.WebSessionByHash("session-hash-1"); err != nil || got.RevokedAt == nil {
		t.Fatalf("revoked session = %+v err %v, want revoked_at set", got, err)
	}

	if err := s.DisableUser(uid(3), formatTime(testNow)); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if u, _, _ = s.UserByEmail("owner@a.com"); u.DisabledAt == nil {
		t.Fatalf("user = %+v, want disabled_at set", u)
	}
	if err := s.DisableUser(uid(99), formatTime(testNow)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent user disable err = %v, want ErrNotFound", err)
	}

	// Audit trail (invariant 7): created + login + logout for the owner.
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM user_events WHERE user_id = ?`, uid(3)).Scan(&events); err != nil {
		t.Fatalf("count user_events: %v", err)
	}
	if events != 3 {
		t.Fatalf("user_events rows = %d, want 3 (created, login, logout)", events)
	}
}
