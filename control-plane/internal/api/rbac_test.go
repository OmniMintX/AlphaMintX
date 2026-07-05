package api

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/live"
)

// fakeReconStatus is the test ReconStatusProvider (spec §API surface: the
// RBAC test env MUST wire a fake so the requiresLiveOMS routes register and
// the matrix pin holds). It mirrors the tenant-filtering contract: a tenant
// scope gets the restricted subset plus its own counts; "" the full view.
type fakeReconStatus struct {
	runErr error // TriggerRun result (tests inject RECON_RUNNING)
}

func (f *fakeReconStatus) Status(tenantID string) (live.ReconStatus, error) {
	st := live.ReconStatus{
		Mode: "live", VenueEnv: "testnet", Reconciled: true,
		LastRun: &live.ReconRun{Status: "completed", CompletedAt: formatTime(testNow)},
	}
	if tenantID != "" {
		orphans := 0
		st.Orphans = &orphans
		return st, nil
	}
	epoch := int64(0)
	st.VenueEpoch = &epoch
	st.LastRun.RunID = uid(96)
	st.LastRun.StartedAt = formatTime(testNow)
	st.LastRun.Counters = &live.RunCounters{FillsBackfilled: 1}
	st.PendingIntents = 2
	st.Watermarks = []live.Watermark{{Symbol: "BTC/USDT", VenueEpoch: 0, ExchangeTradeID: 42}}
	return st, nil
}

func (f *fakeReconStatus) TriggerRun(context.Context, bool) error { return f.runErr }

// rbacEnv is the FULLY-WIRED server of the RBAC matrix test: every optional
// dependency set (gatedEnv wires limits + runtime state; the fake recon
// provider registers the live-OMS routes), so every route in the permission
// table is registered, plus tenant-1 with one strategy and a DB token per
// role and a DB agent token.
type rbacPrincipals struct {
	viewer, trader, admin, owner, agent string
}

func rbacEnv(t *testing.T) (*testEnv, rbacPrincipals) {
	t.Helper()
	e := gatedEnv(t, func(cfg *Config) { cfg.ReconStatus = &fakeReconStatus{} })
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
		"{tenant_id}", "tenant-1", "{token_id}", uid(98),
		"{invoice_id}", "inv-tenant-1-2026-06", "{recon_id}", uid(97))
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

// TestReconStatusTenantFiltered: tenant principals receive ONLY the
// restricted subset plus their own counts (multi-tenant-rbac.md isolation
// rule); account-level detail — watermarks, venue epoch, run_id/counters —
// is env-class only.
func TestReconStatusTenantFiltered(t *testing.T) {
	e, dbToks := rbacEnv(t)

	var tenant map[string]any
	rec := e.do(t, "GET", "/api/v1/oms/recon/status", dbToks.viewer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant status = %d (body %q)", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &tenant)
	for _, k := range []string{"watermarks", "venue_epoch"} {
		if _, ok := tenant[k]; ok {
			t.Errorf("tenant view leaks %q", k)
		}
	}
	if _, ok := tenant["orphans"]; !ok {
		t.Error("tenant view missing its own orphan count")
	}
	lastRun, _ := tenant["last_run"].(map[string]any)
	for _, k := range []string{"run_id", "started_at", "counters"} {
		if _, ok := lastRun[k]; ok {
			t.Errorf("tenant last_run leaks %q", k)
		}
	}

	var env map[string]any
	decodeJSON(t, e.do(t, "GET", "/api/v1/oms/recon/status", readTok, nil), &env)
	for _, k := range []string{"watermarks", "venue_epoch", "pending_intents"} {
		if _, ok := env[k]; !ok {
			t.Errorf("env view missing %q", k)
		}
	}
}

// TestReconRun: 200 with the run_completed counters; unknown body fields
// are rejected (DisallowUnknownFields); RECON_RUNNING maps to 409.
func TestReconRun(t *testing.T) {
	e, _ := rbacEnv(t)
	rec := e.do(t, "POST", "/api/v1/oms/recon/run", adminTok,
		map[string]any{"accept_venue_reset": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("run status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var run live.ReconRun
	decodeJSON(t, rec, &run)
	if run.Status != "completed" || run.Counters == nil || run.Counters.FillsBackfilled != 1 {
		t.Fatalf("run = %+v, want completed with counters", run)
	}
	wantError(t, e.do(t, "POST", "/api/v1/oms/recon/run", adminTok,
		map[string]any{"bogus": 1}), 400, codeSchemaInvalid)

	busy := gatedEnv(t, func(cfg *Config) {
		cfg.ReconStatus = &fakeReconStatus{runErr: live.ErrReconRunning}
	})
	wantError(t, busy.do(t, "POST", "/api/v1/oms/recon/run", adminTok, nil), 409, codeReconRunning)
}
