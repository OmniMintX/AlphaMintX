package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestAdminListings pins the two admin-console listings
// (platform-secrets.md §Admin listings): env-admin sees every tenant and
// every user (with a disabled flag, NEVER password_hash); tenant
// principals are 403 — these are platform surfaces.
func TestAdminListings(t *testing.T) {
	e := newEnv(t, nil)
	admin := store.User{UserID: uid(41), Email: "root@example.com", Role: RolePlatformAdmin,
		CreatedAt: formatTime(testNow)}
	if err := e.store.CreatePlatformAdmin(admin, "bcrypt-hash-root", uid(51)); err != nil {
		t.Fatalf("CreatePlatformAdmin: %v", err)
	}
	tenantA := "tenant-a"
	owner := store.User{UserID: uid(42), TenantID: &tenantA, Email: "owner@a.com", Role: RoleOwner,
		CreatedAt: formatTime(testNow)}
	if err := e.store.CreateTenantWithOwnerUser(
		store.Tenant{TenantID: tenantA, Name: "Tenant A", CreatedAt: formatTime(testNow)},
		owner, "bcrypt-hash-owner", uid(52)); err != nil {
		t.Fatalf("CreateTenantWithOwnerUser: %v", err)
	}
	if err := e.store.DisableUser(uid(42), formatTime(testNow)); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	viewer := seedUserToken(t, e.store, tenantA, RoleViewer, "db-viewer-token")

	rec := e.do(t, "GET", "/api/v1/tenants", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list tenants = %d (body %q)", rec.Code, rec.Body.String())
	}
	var tenants struct {
		Items []store.Tenant `json:"items"`
	}
	decodeJSON(t, rec, &tenants)
	// 'default' is seeded by the tenancy migration at Open, so tenant-a
	// joins it; each row is exactly {tenant_id, name, created_at}.
	found := false
	for _, tn := range tenants.Items {
		if tn.TenantID == tenantA && tn.Name == "Tenant A" && tn.CreatedAt == formatTime(testNow) {
			found = true
		}
	}
	if len(tenants.Items) != 2 || !found {
		t.Fatalf("tenants = %+v, want default plus tenant-a", tenants.Items)
	}

	rec = e.do(t, "GET", "/api/v1/users", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users = %d (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "password_hash") ||
		strings.Contains(rec.Body.String(), "bcrypt-hash") {
		t.Fatalf("users listing leaks password material: %q", rec.Body.String())
	}
	var users struct {
		Items []userView `json:"items"`
	}
	decodeJSON(t, rec, &users)
	if len(users.Items) != 2 {
		t.Fatalf("users = %+v, want 2 rows", users.Items)
	}
	byID := map[string]userView{}
	for _, u := range users.Items {
		byID[u.UserID] = u
	}
	root := byID[uid(41)]
	if root.Email != "root@example.com" || root.Role != RolePlatformAdmin ||
		root.TenantID != nil || root.Disabled {
		t.Errorf("platform admin row = %+v, want enabled with null tenant_id", root)
	}
	ownerRow := byID[uid(42)]
	if ownerRow.Email != "owner@a.com" || ownerRow.Role != RoleOwner ||
		ownerRow.TenantID == nil || *ownerRow.TenantID != tenantA || !ownerRow.Disabled {
		t.Errorf("owner row = %+v, want disabled in tenant-a", ownerRow)
	}

	// Tenant principals never see the platform listings (matrix rows are
	// env-admin ONLY; TestRBACMatrix covers every principal).
	wantError(t, e.do(t, "GET", "/api/v1/tenants", viewer, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/users", viewer, nil), 403, codeForbidden)
}
