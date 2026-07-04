package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// seedTwoTenants builds tenant-a (strat1) and tenant-b (strat2) with DB
// tokens for the isolation tests. Bearer plaintexts are deterministic.
type twoTenants struct {
	aViewer, aTrader, aAdmin, aAgent, bViewer, bTrader string
}

func seedTwoTenants(t *testing.T, e *testEnv, state string) twoTenants {
	t.Helper()
	createTenant(t, e.store, "tenant-a")
	createTenant(t, e.store, "tenant-b")
	createTenantStrategy(t, e.store, strat1, "tenant-a", state)
	createTenantStrategy(t, e.store, strat2, "tenant-b", state)
	return twoTenants{
		aViewer: seedUserToken(t, e.store, "tenant-a", RoleViewer, "db-a-viewer"),
		aTrader: seedUserToken(t, e.store, "tenant-a", RoleTrader, "db-a-trader"),
		aAdmin:  seedUserToken(t, e.store, "tenant-a", RoleAdmin, "db-a-admin"),
		aAgent:  seedAgentToken(t, e.store, "tenant-a", strat1, "db-a-agent"),
		bViewer: seedUserToken(t, e.store, "tenant-b", RoleViewer, "db-b-viewer"),
		bTrader: seedUserToken(t, e.store, "tenant-b", RoleTrader, "db-b-trader"),
	}
}

func TestCreateTenant(t *testing.T) {
	e := newEnv(t, nil)
	for _, bad := range []string{"", "default", "UPPER", "-lead", "has space",
		"way-too-long-tenant-id-over-32-chars-x"} {
		wantError(t, e.do(t, "POST", "/api/v1/tenants", adminTok,
			createTenantRequest{TenantID: bad, Name: "n"}), 400, codeInvalidTenantID)
	}

	rec := e.do(t, "POST", "/api/v1/tenants", adminTok,
		createTenantRequest{TenantID: "acme", Name: "Acme"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp createTenantResponse
	decodeJSON(t, rec, &resp)
	if resp.TenantID != "acme" || resp.OwnerToken.Role == nil || *resp.OwnerToken.Role != RoleOwner ||
		resp.OwnerToken.CreatedBy != "env-admin" || !plaintextPattern.MatchString(resp.OwnerToken.Token) {
		t.Fatalf("create response = %+v", resp)
	}
	// The initial owner token is live: the tenant is self-service from here.
	if rec := e.do(t, "GET", "/api/v1/tokens", resp.OwnerToken.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner list status = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "POST", "/api/v1/tenants", adminTok,
		createTenantRequest{TenantID: "acme", Name: "again"}), 409, codeTenantExists)
}

// TestTenantIsolation_CrossRead404: every cross-tenant GET answers the SAME
// 404 as a nonexistent object — no existence oracle.
func TestTenantIsolation_CrossRead404(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")
	_, _, runB := insertChain(t, e.store, 20, strat2, 0)

	foreign := e.do(t, "GET", "/api/v1/strategies/"+strat2, toks.aViewer, nil)
	absent := e.do(t, "GET", "/api/v1/strategies/"+uid(99), toks.aViewer, nil)
	wantError(t, foreign, 404, codeUnknownStrategy)
	if foreign.Body.String() != absent.Body.String() {
		t.Errorf("foreign body %q != absent body %q", foreign.Body.String(), absent.Body.String())
	}
	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+strat2+"/runs", toks.aViewer, nil), 404, codeUnknownStrategy)

	foreignRun := e.do(t, "GET", "/api/v1/strategies/"+strat2+"/runs/"+runB, toks.aViewer, nil)
	absentRun := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs/"+uid(99), toks.aViewer, nil)
	wantError(t, foreignRun, 404, codeUnknownRun)
	if foreignRun.Body.String() != absentRun.Body.String() {
		t.Errorf("foreign run body %q != absent run body %q", foreignRun.Body.String(), absentRun.Body.String())
	}
	// The platform env READ token still reads every tenant (deployer).
	if rec := e.do(t, "GET", "/api/v1/strategies/"+strat2, readTok, nil); rec.Code != http.StatusOK {
		t.Errorf("env read cross-tenant status = %d", rec.Code)
	}
}

// TestTenantIsolation_CrossApproval404 covers BOTH attack shapes: a foreign
// strategy path (404 UNKNOWN_STRATEGY, resolved FIRST) and an own path
// carrying a foreign verdict_id (404 UNKNOWN_VERDICT).
func TestTenantIsolation_CrossApproval404(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "live_l1")
	_, verdictB, _ := insertChain(t, e.store, 20, strat2, 0)
	if err := e.store.CreatePendingApproval(verdictB, strat2, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}

	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat2+"/approvals", toks.aTrader,
		approvalRequest{VerdictID: verdictB, Approved: true}), 404, codeUnknownStrategy)
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", toks.aTrader,
		approvalRequest{VerdictID: verdictB, Approved: true}), 404, codeUnknownVerdict)

	// The owning tenant's trader decides, and decided_by records its
	// token_id — stable and non-secret (§Audit identity).
	rec := e.do(t, "POST", "/api/v1/strategies/"+strat2+"/approvals", toks.bTrader,
		approvalRequest{VerdictID: verdictB, Approved: false})
	if rec.Code != http.StatusOK {
		t.Fatalf("own-tenant approval status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var a store.Approval
	decodeJSON(t, rec, &a)
	items, _, err := e.store.ListAPITokens("tenant-b", 1, 100)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	i := slices.IndexFunc(items, func(tok store.APIToken) bool {
		return tok.Role != nil && *tok.Role == RoleTrader
	})
	if i < 0 || a.DecidedBy != items[i].TokenID {
		t.Errorf("decided_by = %q, want the trader token_id", a.DecidedBy)
	}
}

// TestTenantIsolation_CrossKill404: the kill path tenant must equal the
// principal's tenant; env-admin may kill any EXISTING tenant.
func TestTenantIsolation_CrossKill404(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	wantError(t, e.do(t, "POST", "/api/v1/tenants/tenant-b/kill", toks.aAdmin, struct{}{}), 404, codeUnknownTenant)
	wantError(t, e.do(t, "POST", "/api/v1/tenants/no-such/kill", toks.aAdmin, struct{}{}), 404, codeUnknownTenant)
	wantError(t, e.do(t, "POST", "/api/v1/tenants/no-such/kill", adminTok, struct{}{}), 404, codeUnknownTenant)

	rec := e.do(t, "POST", "/api/v1/tenants/tenant-b/kill", adminTok, struct{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("env-admin kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp tenantKillResponse
	decodeJSON(t, rec, &resp)
	if resp.TenantID != "tenant-b" || resp.KillEpoch < 1 {
		t.Fatalf("kill response = %+v", resp)
	}
}

// TestTenantIsolation_KillDoesNotBleedAcrossTenants: after a tenant A kill,
// tenant B proposals still pass the gate AND tenant B approvals still pass
// the preflight, while tenant A is fully blocked (most restrictive wins,
// never wider).
func TestTenantIsolation_KillDoesNotBleedAcrossTenants(t *testing.T) {
	e := gatedEnv(t)
	toks := seedTwoTenants(t, e, "paper")
	createTenantStrategy(t, e.store, uid(3), "tenant-a", "live_l1")
	createTenantStrategy(t, e.store, uid(4), "tenant-b", "live_l1")
	_, verdictA, _ := insertChain(t, e.store, 30, uid(3), 0)
	_, verdictB, _ := insertChain(t, e.store, 40, uid(4), 0)
	if err := e.store.CreatePendingApproval(verdictA, uid(3), testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	if err := e.store.CreatePendingApproval(verdictB, uid(4), testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	putMark(e, "BTC/USDT", "64000")

	if rec := e.do(t, "POST", "/api/v1/tenants/tenant-a/kill", toks.aAdmin, struct{}{}); rec.Code != http.StatusOK {
		t.Fatalf("tenant-a kill status = %d (body %q)", rec.Code, rec.Body.String())
	}

	// Tenant A proposals are rejected KILL_SWITCH_ACTIVE ...
	vA, _ := postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if vA.Decision != "reject" || len(vA.Reasons) == 0 || vA.Reasons[0].Code != "KILL_SWITCH_ACTIVE" {
		t.Fatalf("tenant-a verdict = %s (%v), want reject KILL_SWITCH_ACTIVE", vA.Decision, vA.Reasons)
	}
	// ... while tenant B proposals pass the gate ...
	vB, _ := postProposal(e, t, strat2, agent2Tok, 0, openProposal(t, uid(50), strat2, uid(52)))
	if vB.Decision != "approve" {
		t.Fatalf("tenant-b verdict = %s (%v), want approve", vB.Decision, vB.Reasons)
	}

	// ... and tenant B approvals pass the preflight (no kill reason), while
	// tenant A approvals block on it.
	rec := e.do(t, "POST", "/api/v1/strategies/"+uid(4)+"/approvals", opTok,
		approvalRequest{VerdictID: verdictB, Approved: true})
	var aB store.Approval
	decodeJSON(t, rec, &aB)
	if aB.Outcome != store.OutcomeApproved {
		t.Fatalf("tenant-b approval outcome = %q (%v), want approved", aB.Outcome, aB.PreflightReasons)
	}
	rec = e.do(t, "POST", "/api/v1/strategies/"+uid(3)+"/approvals", opTok,
		approvalRequest{VerdictID: verdictA, Approved: true})
	var aA store.Approval
	decodeJSON(t, rec, &aA)
	if aA.Outcome != store.OutcomeApprovedButBlocked || !slices.Contains(aA.PreflightReasons, reasonKillSwitchActive) {
		t.Fatalf("tenant-a approval = %q (%v), want approved_but_blocked KILL_SWITCH_ACTIVE", aA.Outcome, aA.PreflightReasons)
	}
}

// TestTenantIsolation_AgentCrossStrategy403: a DB agent token outside its
// strategy answers the existing 403 STRATEGY_SCOPE_MISMATCH.
func TestTenantIsolation_AgentCrossStrategy403(t *testing.T) {
	e := gatedEnv(t)
	toks := seedTwoTenants(t, e, "paper")
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat2+"/traces", toks.aAgent, nil),
		403, codeStrategyScopeMismatch)
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat2+"/proposals", toks.aAgent, nil),
		403, codeStrategyScopeMismatch)
}

// TestTenantIsolation_ListsExcludeForeignRows: list items AND totals are
// tenant-scoped for tenant principals; the env read class stays platform.
func TestTenantIsolation_ListsExcludeForeignRows(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	var pg struct {
		Items []store.Strategy `json:"items"`
		Total int              `json:"total"`
	}
	decodeJSON(t, e.do(t, "GET", "/api/v1/strategies", toks.aViewer, nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0].StrategyID != strat1 {
		t.Fatalf("tenant-a strategy list = %+v, want only %s", pg, strat1)
	}
	decodeJSON(t, e.do(t, "GET", "/api/v1/strategies", toks.bViewer, nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 || pg.Items[0].StrategyID != strat2 {
		t.Fatalf("tenant-b strategy list = %+v, want only %s", pg, strat2)
	}
	decodeJSON(t, e.do(t, "GET", "/api/v1/strategies", readTok, nil), &pg)
	if pg.Total != 2 {
		t.Fatalf("env read total = %d, want 2 (platform-scoped)", pg.Total)
	}

	var toksPg struct {
		Items []store.APIToken `json:"items"`
		Total int              `json:"total"`
	}
	decodeJSON(t, e.do(t, "GET", "/api/v1/tokens", toks.aAdmin, nil), &toksPg)
	if toksPg.Total != 4 || len(toksPg.Items) != 4 {
		t.Fatalf("tenant-a token list = %+v, want the 4 tenant-a tokens", toksPg)
	}
	for _, tok := range toksPg.Items {
		if tok.TenantID != "tenant-a" {
			t.Errorf("token list leaked foreign row %+v", tok)
		}
	}
}
