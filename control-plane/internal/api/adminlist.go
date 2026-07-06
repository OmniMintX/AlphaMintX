package api

import (
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Admin-console listings (docs/specs/platform-secrets.md §Admin listings):
// both routes are env-admin ONLY — platform-wide views for the admin UI,
// never a tenant surface.

// handleListTenants is GET /api/v1/tenants: every tenant, ordered
// created_at then tenant_id.
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := s.cfg.Store.ListTenants()
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if tenants == nil {
		tenants = []store.Tenant{}
	}
	writeJSON(w, http.StatusOK, map[string][]store.Tenant{"items": tenants})
}

// userView is the admin listing row: identity, role, and a disabled flag —
// NEVER password_hash (the store never reads it back).
type userView struct {
	UserID    string  `json:"user_id"`
	Email     string  `json:"email"`
	TenantID  *string `json:"tenant_id"`
	Role      string  `json:"role"`
	CreatedAt string  `json:"created_at"`
	Disabled  bool    `json:"disabled"`
}

// handleListUsers is GET /api/v1/users: every user, ordered created_at
// then user_id.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.cfg.Store.ListUsers()
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	items := make([]userView, 0, len(users))
	for _, u := range users {
		items = append(items, userView{
			UserID: u.UserID, Email: u.Email, TenantID: u.TenantID,
			Role: u.Role, CreatedAt: u.CreatedAt, Disabled: u.DisabledAt != nil,
		})
	}
	writeJSON(w, http.StatusOK, map[string][]userView{"items": items})
}
