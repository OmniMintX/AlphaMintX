package api

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rbacEnv is the FULLY-WIRED server of the RBAC matrix test: every optional
// dependency set (gatedEnv wires limits + runtime state), so every route in
// the permission table is registered, plus tenant-1 with one strategy and a
// DB token per role and a DB agent token.
type rbacPrincipals struct {
	viewer, trader, admin, owner, agent string
}

func rbacEnv(t *testing.T) (*testEnv, rbacPrincipals) {
	t.Helper()
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	p := rbacPrincipals{
		viewer: seedUserToken(t, e.store, "tenant-1", RoleViewer, "db-viewer-token"),
		trader: seedUserToken(t, e.store, "tenant-1", RoleTrader, "db-trader-token"),
		admin:  seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin-token"),
		owner:  seedUserToken(t, e.store, "tenant-1", RoleOwner, "db-owner-token"),
		agent:  seedAgentToken(t, e.store, "tenant-1", strat1, "db-agent-token"),
	}
	return e, p
}

// matrixPath substitutes concrete in-tenant identifiers into a route
// pattern so allowed principals clear the authorization tier.
func matrixPath(pattern string) string {
	r := strings.NewReplacer("{id}", strat1, "{run_id}", uid(99),
		"{tenant_id}", "tenant-1", "{token_id}", uid(98))
	return r.Replace(pattern)
}

// TestRBACMatrix iterates (endpoint x principal) over the exported
// permission table against the fully-wired server, asserting the expected
// status tier per the spec's status semantics: 401 for missing/unknown
// tokens, 403 FORBIDDEN for a known principal outside the row, and never
// 401/403 for a principal the row allows. The registered-route enumeration
// must equal the table exactly — a route cannot exist without a matrix
// entry.
func TestRBACMatrix(t *testing.T) {
	e, dbToks := rbacEnv(t)

	if got, want := e.srv.routes, Permissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered routes = %+v, want the full permission table %+v", got, want)
	}

	principals := []struct {
		name  string
		token string
		class string // matched against RoutePermission.Classes
		role  string // matched against RoutePermission.Roles
	}{
		{"env-read", readTok, classRead, ""},
		{"env-operator", opTok, classOperator, ""},
		{"env-agent", agent1Tok, classAgent, ""},
		{"env-admin", adminTok, classEnvAdmin, ""},
		{"db-viewer", dbToks.viewer, "", RoleViewer},
		{"db-trader", dbToks.trader, "", RoleTrader},
		{"db-admin", dbToks.admin, "", RoleAdmin},
		{"db-owner", dbToks.owner, "", RoleOwner},
		{"db-agent", dbToks.agent, classAgent, ""},
	}
	for _, perm := range Permissions() {
		path := matrixPath(perm.Path)
		t.Run(perm.Method+" "+perm.Path, func(t *testing.T) {
			if perm.Public {
				if rec := e.do(t, perm.Method, path, "", nil); rec.Code != http.StatusOK {
					t.Errorf("public route: status = %d, want 200", rec.Code)
				}
				return
			}
			wantError(t, e.do(t, perm.Method, path, "", nil), 401, codeUnauthorized)
			wantError(t, e.do(t, perm.Method, path, "not-a-known-token", nil), 401, codeUnauthorized)
			for _, pr := range principals {
				allowed := slices.Contains(perm.Classes, pr.class) ||
					(pr.role != "" && slices.Contains(perm.Roles, pr.role))
				rec := e.do(t, perm.Method, path, pr.token, nil)
				if allowed {
					if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
						t.Errorf("%s: status = %d (body %q), want authorization to pass", pr.name, rec.Code, rec.Body.String())
					}
					continue
				}
				wantError(t, rec, 403, codeForbidden)
			}
		})
	}
}

// TestRBACMatrixPins spot-checks the PLAN.md exit-criterion rows so a table
// edit cannot silently weaken them: Trader cannot change limits; agent
// tokens are rejected by every endpoint outside their two ingestion routes;
// env-admin has no read surface.
func TestRBACMatrixPins(t *testing.T) {
	e, dbToks := rbacEnv(t)
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/limits", dbToks.trader,
		map[string]any{"changes": map[string]any{"max_open_positions": 1}}), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/tokens", dbToks.agent, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/strategies", dbToks.agent, nil), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/tenants/tenant-1/kill", dbToks.agent, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/strategies", adminTok, nil), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", adminTok, nil), 403, codeForbidden)
}
