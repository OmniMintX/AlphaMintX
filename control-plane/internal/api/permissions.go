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
	// requiresLimits (a limits provider), requiresLiveOMS (a
	// ReconStatusProvider — live deployments only), or requiresBackup (a
	// BackupEngine — CONTROLPLANE_BACKUP_DIR configured).
	Requires string
}

const (
	requiresIngestion = "ingestion"
	requiresLimits    = "limits"
	requiresLiveOMS   = "live-oms"
	requiresBackup    = "backup"
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
		// Effective-limits read (multi-tenant-rbac.md §Runtime limit
		// changes): the standard reader tier (the GET .../safety row);
		// registered only with a limits provider, like the POST.
		{Method: "GET", Path: "/api/v1/strategies/{id}/limits", Roles: readers, Classes: []string{classRead}, Requires: requiresLimits},
		{Method: "POST", Path: "/api/v1/tenants", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tenants/{tenant_id}/kill", Roles: admins, Classes: []string{classEnvAdmin}},
		// Kill tiers (safety-wiring.md §Kill endpoints): the strategy tier
		// is trader+ own tenant plus env-admin (any strategy); the platform
		// tier is env-admin ONLY — no tenant role may kill the platform.
		// Both are always registered: the gate-block half of a kill is
		// mode-independent.
		{Method: "POST", Path: "/api/v1/strategies/{id}/kill", Roles: approvers, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/platform/kill", Classes: []string{classEnvAdmin}},
		// Lifecycle transitions (lifecycle-api.md LC-2): trader+ own
		// tenant plus env-admin; the read/operator/agent classes can never
		// transition. The paper-gate read is promotion visibility for
		// every reader (LC-24). Always registered, both modes (LC-1).
		{Method: "POST", Path: "/api/v1/strategies/{id}/lifecycle", Roles: approvers, Classes: []string{classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/strategies/{id}/paper-gate", Roles: readers, Classes: []string{classRead}},
		// Operator surface (operator-surface.md OS-5/OS-15/OS-19): the
		// two strategy-scoped safety reads are standard reader rows; the
		// global alert feed is env-class only — no DB role, so every
		// tenant principal is 403 (OS-20: safety_alerts has no tenant
		// column and NULL-strategy rows are platform operational data).
		// Always registered, both modes.
		{Method: "GET", Path: "/api/v1/strategies/{id}/safety", Roles: readers, Classes: []string{classRead}},
		{Method: "GET", Path: "/api/v1/strategies/{id}/alerts", Roles: readers, Classes: []string{classRead}},
		{Method: "GET", Path: "/api/v1/alerts", Classes: []string{classRead, classEnvAdmin}},
		// Kill-clear tiers (lifecycle-api.md LC-29): one level stricter
		// than kill on the strategy tier (unlock is Admin+); the platform
		// tier is env-admin ONLY. Always registered, mode-independent.
		{Method: "POST", Path: "/api/v1/strategies/{id}/kill/clear", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tenants/{tenant_id}/kill/clear", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/platform/kill/clear", Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "GET", Path: "/api/v1/tokens", Roles: admins, Classes: []string{classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/tokens/{token_id}/revoke", Roles: admins, Classes: []string{classEnvAdmin}},
		// Strategy provisioning (strategy-provisioning.md SP-1): the
		// exact tier of POST /api/v1/tokens — tenant owner/admin create
		// in their own tenant, env-admin in any existing tenant. Always
		// registered.
		{Method: "POST", Path: "/api/v1/strategies", Roles: admins, Classes: []string{classEnvAdmin}},
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
		// Backup (ops-backup.md OB-6/OB-7): both routes are deployer acts
		// — env-admin class ONLY, no DB role (tenants never see platform
		// backups) — and exist only when the backup engine is configured
		// (CONTROLPLANE_BACKUP_DIR); unconfigured deployments 404.
		{Method: "POST", Path: "/api/v1/ops/backups/run", Classes: []string{classEnvAdmin}, Requires: requiresBackup},
		{Method: "GET", Path: "/api/v1/ops/backups", Classes: []string{classEnvAdmin}, Requires: requiresBackup},
		// Restore gate (deploy-and-survive.md DS-5/DS-6): the ack is a
		// deployer act (env-admin ONLY); status is platform operational
		// data (the GET /api/v1/alerts OS-19 precedent — the ops panel's
		// READ token must render WHY approvals 503). Always registered:
		// unlike the backup routes, no CONTROLPLANE_BACKUP_DIR required.
		{Method: "GET", Path: "/api/v1/ops/restore", Classes: []string{classRead, classEnvAdmin}},
		{Method: "POST", Path: "/api/v1/ops/restore/ack", Classes: []string{classEnvAdmin}},
	}
}
