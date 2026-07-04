package api

import "slices"

// RoutePermission is one row of the exported permission matrix
// (multi-tenant-rbac.md §Permission matrix). Routes are REGISTERED from
// this table — it is the total route set — and TestRBACMatrix iterates it.
type RoutePermission struct {
	Method string
	// Path is the net/http ServeMux pattern (method-less).
	Path string
	// Roles are the DB user roles allowed (viewer/trader/admin/owner).
	Roles []string
	// Classes are the allowed non-user classes: read, operator, agent
	// (env AND DB agent tokens — own strategy only, guard-enforced),
	// env-admin.
	Classes []string
	// Public marks the single unauthenticated route (GET /health).
	Public bool
	// Requires names optional wiring the route depends on: "" always
	// registered, requiresIngestion (limits + runtime state) or
	// requiresLimits (a limits provider).
	Requires string
}

const (
	requiresIngestion = "ingestion"
	requiresLimits    = "limits"
)

// allows reports whether the principal's role (user class) or class (env
// and agent classes) may call the route.
func (p RoutePermission) allows(pr principal) bool {
	if pr.class == classUser {
		return slices.Contains(p.Roles, pr.role)
	}
	return slices.Contains(p.Classes, pr.class)
}

// Permissions returns the declarative permission matrix. Reads are viewer+
// plus the env read class (Phase 1 semantics preserved); approvals are
// trader+ plus the env operator class; ingestion is agent tokens only; the
// admin surfaces (limits, tenant kill, token management) are admin/owner —
// own tenant, handler-enforced — plus the platform env-admin; tenant
// creation is env-admin ONLY in v1.
func Permissions() []RoutePermission {
	readers := []string{RoleViewer, RoleTrader, RoleAdmin, RoleOwner}
	approvers := []string{RoleTrader, RoleAdmin, RoleOwner}
	admins := []string{RoleAdmin, RoleOwner}
	return []RoutePermission{
		{Method: "GET", Path: "/health", Public: true},
		{Method: "GET", Path: "/api/v1/strategies", Roles: readers, Classes: []string{classRead}},
		{Method: "GET", Path: "/api/v1/strategies/{id}", Roles: readers, Classes: []string{classRead}},
		{Method: "GET", Path: "/api/v1/strategies/{id}/runs", Roles: readers, Classes: []string{classRead}},
		{Method: "GET", Path: "/api/v1/strategies/{id}/runs/{run_id}", Roles: readers, Classes: []string{classRead}},
		{Method: "POST", Path: "/api/v1/strategies/{id}/approvals", Roles: approvers, Classes: []string{classOperator}},
		{Method: "POST", Path: "/api/v1/strategies/{id}/traces", Classes: []string{classAgent}},
		{Method: "POST", Path: "/api/v1/strategies/{id}/proposals", Classes: []string{classAgent}, Requires: requiresIngestion},
		{Method: "POST", Path: "/api/v1/strategies/{id}/limits", Roles: admins, Classes: []string{classEnvAdmin}, Requires: requiresLimits},
		{Method: "POST", Path: "/api/v1/tenants", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tenants/{tenant_id}/kill", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens/{token_id}/revoke", Roles: admins, Classes: []string{classEnvAdmin}},
	}
}
