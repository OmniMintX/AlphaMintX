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
	// registered, requiresIngestion (limits + runtime state),
	// requiresLimits (a limits provider), or requiresLiveOMS (a
	// ReconStatusProvider — live deployments only).
	Requires string
}

const (
	requiresIngestion = "ingestion"
	requiresLimits    = "limits"
	requiresLiveOMS   = "live-oms"
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
		// Heartbeat receiver (watchdog.md WD-2/WD-3): agent tokens only,
		// own strategy (guard-enforced), ALWAYS registered — receipt is
		// mode-independent and paper-mode agents must not 404 per 30 s.
		{Method: "POST", Path: "/api/v1/strategies/{id}/heartbeat", Classes: []string{classAgent}},
		{Method: "POST", Path: "/api/v1/strategies/{id}/limits", Roles: admins, Classes: []string{classEnvAdmin}, Requires: requiresLimits},
		{Method: "POST", Path: "/api/v1/tenants", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tenants/{tenant_id}/kill", Roles: admins, Classes: []string{classEnvAdmin}},
		// Kill tiers (safety-wiring.md §Kill endpoints): the strategy tier
		// is trader+ own tenant plus env-admin (any strategy); the platform
		// tier is env-admin ONLY — no tenant role may kill the platform.
		// Both are always registered: the gate-block half of a kill is
		// mode-independent.
		{Method: "POST", Path: "/api/v1/strategies/{id}/kill", Roles: approvers, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/platform/kill", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens/{token_id}/revoke", Roles: admins, Classes: []string{classEnvAdmin}},
		// Billing (billing-and-metering.md §Permission matrix additions):
		// the three POSTs are deployer acts (env-admin ONLY); invoice and
		// reconciliation reads are financial records — admin/owner own
		// tenant plus the platform read and env-admin classes, never
		// viewer/trader, never agents.
		{Method: "POST", Path: "/api/v1/billing/metering", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/billing/periods/close", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/billing/reconcile", Classes: []string{classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/billing/invoices", Roles: admins, Classes: []string{classRead, classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/billing/invoices/{invoice_id}", Roles: admins, Classes: []string{classRead, classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/billing/reconciliations", Roles: admins, Classes: []string{classRead, classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/billing/reconciliations/{recon_id}", Roles: admins, Classes: []string{classRead, classEnvAdmin}},
		// Live-OMS reconciliation (live-oms-and-reconciler.md §API
		// surface): status is tenant-filtered for tenant principals; the
		// run trigger is a deployer act (env-admin ONLY, like the billing
		// POSTs). Both exist only when the live OMS is wired.
		{Method: "GET", Path: "/api/v1/oms/recon/status", Roles: readers, Classes: []string{classRead, classEnvAdmin}, Requires: requiresLiveOMS},
		{Method: "POST", Path: "/api/v1/oms/recon/run", Classes: []string{classEnvAdmin}, Requires: requiresLiveOMS},
	}
}
