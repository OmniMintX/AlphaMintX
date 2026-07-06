package store

import (
	"errors"
	"testing"
)

// TestPlatformSecretLifecycle covers the store half of platform-secrets.md:
// the snapshot upsert with its same-transaction audit trail ('set' first,
// 'rotated' after), the ErrNotFound read, and the metadata-only listing
// (sorted by kind, NO ciphertext).
func TestPlatformSecretLifecycle(t *testing.T) {
	s := openStore(t)
	if _, _, _, _, err := s.GetPlatformSecret("binance"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent secret err = %v, want ErrNotFound", err)
	}

	if err := s.UpsertPlatformSecret("llm", "sealed-llm-1", `{"base_url":"https://llm.example"}`,
		"admin-1", uid(1), formatTime(testNow)); err != nil {
		t.Fatalf("first llm upsert: %v", err)
	}
	if err := s.UpsertPlatformSecret("binance", "sealed-bnc-1", `{"env":"testnet","api_key_last4":"key1"}`,
		"admin-1", uid(2), formatTime(testNow)); err != nil {
		t.Fatalf("first binance upsert: %v", err)
	}
	// Rotation replaces the snapshot in place and appends a 'rotated' row.
	if err := s.UpsertPlatformSecret("binance", "sealed-bnc-2", `{"env":"prod","api_key_last4":"key2"}`,
		"admin-2", uid(3), "2026-07-04T13:00:00Z"); err != nil {
		t.Fatalf("binance rotation: %v", err)
	}

	ct, meta, updatedAt, updatedBy, err := s.GetPlatformSecret("binance")
	if err != nil || ct != "sealed-bnc-2" || meta != `{"env":"prod","api_key_last4":"key2"}` ||
		updatedAt != "2026-07-04T13:00:00Z" || updatedBy != "admin-2" {
		t.Fatalf("GetPlatformSecret = %q %q %q %q err %v, want the rotated snapshot", ct, meta, updatedAt, updatedBy, err)
	}

	list, err := s.ListPlatformSecretMeta()
	if err != nil || len(list) != 2 {
		t.Fatalf("ListPlatformSecretMeta = %+v err %v, want 2 rows", list, err)
	}
	if list[0].Kind != "binance" || list[1].Kind != "llm" {
		t.Errorf("listing order = [%s %s], want sorted by kind", list[0].Kind, list[1].Kind)
	}
	if list[0].MetaJSON != `{"env":"prod","api_key_last4":"key2"}` || list[0].UpdatedBy != "admin-2" {
		t.Errorf("binance meta row = %+v, want the rotated metadata", list[0])
	}

	// Audit trail (invariant 7): one 'set' per kind plus one 'rotated'.
	type event struct{ kind, action, actor string }
	rows, err := s.db.Query(`SELECT kind, action, actor_id FROM secret_events ORDER BY recorded_at, event_id`)
	if err != nil {
		t.Fatalf("query secret_events: %v", err)
	}
	defer rows.Close()
	var events []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.kind, &e.action, &e.actor); err != nil {
			t.Fatalf("scan secret_events: %v", err)
		}
		events = append(events, e)
	}
	want := []event{
		{"llm", "set", "admin-1"},
		{"binance", "set", "admin-1"},
		{"binance", "rotated", "admin-2"},
	}
	if len(events) != len(want) {
		t.Fatalf("secret_events = %+v, want %+v", events, want)
	}
	for i, e := range events {
		if e != want[i] {
			t.Errorf("secret_events[%d] = %+v, want %+v", i, e, want[i])
		}
	}
}

// TestListTenantsAndUsers pins the two admin-console listings: ordering by
// created_at with a deterministic tiebreak, and the User rows carrying
// disabled_at while NEVER exposing password_hash (no struct field exists).
func TestListTenantsAndUsers(t *testing.T) {
	s := openStore(t)
	admin := User{UserID: uid(1), Email: "root@example.com", Role: "platform_admin", CreatedAt: "2026-07-04T11:00:00Z"}
	if err := s.CreatePlatformAdmin(admin, "bcrypt-hash-a", uid(10)); err != nil {
		t.Fatalf("CreatePlatformAdmin: %v", err)
	}
	tenantA := "tenant-a"
	owner := User{UserID: uid(2), TenantID: &tenantA, Email: "owner@a.com", Role: "owner", CreatedAt: "2026-07-04T12:00:00Z"}
	if err := s.CreateTenantWithOwnerUser(Tenant{TenantID: tenantA, Name: "A", CreatedAt: "2026-07-04T12:00:00Z"},
		owner, "bcrypt-hash-b", uid(11)); err != nil {
		t.Fatalf("CreateTenantWithOwnerUser: %v", err)
	}
	if err := s.CreateTenant(Tenant{TenantID: "tenant-b", Name: "B", CreatedAt: "2026-07-05T12:00:00Z"}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.DisableUser(uid(2), "2026-07-04T12:30:00Z"); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}

	tenants, err := s.ListTenants()
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	// 'default' is seeded by the tenancy migration at Open time (wall
	// clock), so only the relative order of the fixed rows is asserted.
	posA, posB := -1, -1
	for i, tn := range tenants {
		switch tn.TenantID {
		case tenantA:
			posA = i
		case "tenant-b":
			posB = i
		}
	}
	if len(tenants) != 3 || posA == -1 || posB == -1 || posA >= posB {
		t.Fatalf("ListTenants = %+v, want 3 rows with tenant-a before tenant-b", tenants)
	}

	users, err := s.ListUsers()
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers = %+v err %v, want 2 rows", users, err)
	}
	if users[0].UserID != uid(1) || users[0].TenantID != nil || users[0].DisabledAt != nil {
		t.Errorf("users[0] = %+v, want the enabled platform admin first", users[0])
	}
	if users[1].UserID != uid(2) || users[1].TenantID == nil || *users[1].TenantID != tenantA ||
		users[1].DisabledAt == nil {
		t.Errorf("users[1] = %+v, want the disabled tenant owner", users[1])
	}
}
