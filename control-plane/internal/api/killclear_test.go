package api

import (
	"net/http"
	"testing"
)

func clearBody(reason string, epoch int64) map[string]any {
	return map[string]any{"reason": reason, "observed_epoch": epoch}
}

// activeKill asserts the store's LC-28 predicate for a strategy.
func activeKill(t *testing.T, e *testEnv, strategyID string, want bool) {
	t.Helper()
	got, err := e.store.ActiveKill(strategyID)
	if err != nil {
		t.Fatalf("ActiveKill(%s): %v", strategyID, err)
	}
	if got != want {
		t.Fatalf("ActiveKill(%s) = %v, want %v", strategyID, got, want)
	}
}

// TestStrategyKillClearFlow drives kill -> clear at the strategy tier:
// LC-30 body validation, the LC-27 CAS on observed_epoch, the LC-33
// response, and LC-31's NO_ACTIVE_KILL on a second clear.
func TestStrategyKillClearFlow(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")
	clearPath := "/api/v1/strategies/" + strat1 + "/kill/clear"

	rec := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/kill", toks.aTrader, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var kill strategyKillResponse
	decodeJSON(t, rec, &kill)
	activeKill(t, e, strat1, true)

	// Body semantics after resolution: unknown strategy 404 with no body.
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+uid(99)+"/kill/clear", adminTok, nil),
		404, codeUnknownStrategy)
	wantError(t, e.do(t, "POST", clearPath, adminTok, nil), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", clearPath, adminTok, map[string]any{"reason": "r"}),
		400, codeSchemaInvalid) // observed_epoch REQUIRED
	wantError(t, e.do(t, "POST", clearPath, adminTok, map[string]any{"observed_epoch": kill.KillEpoch}),
		400, codeSchemaInvalid) // reason REQUIRED
	// Unlock is Admin+ (one level stricter than kill): trader is 403.
	wantError(t, e.do(t, "POST", clearPath, toks.aTrader, clearBody("r", kill.KillEpoch)),
		403, codeForbidden)
	// LC-27: a stale observed_epoch is 409 and writes NOTHING.
	wantError(t, e.do(t, "POST", clearPath, toks.aAdmin, clearBody("r", kill.KillEpoch-1)),
		409, codeClearConflict)
	activeKill(t, e, strat1, true)

	rec = e.do(t, "POST", clearPath, toks.aAdmin, clearBody("root-caused", kill.KillEpoch))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var clear clearResponse
	decodeJSON(t, rec, &clear)
	if clear.ClearID == "" || clear.Scope != "strategy" || clear.StrategyID != strat1 ||
		clear.TenantID != "tenant-a" || clear.ClearedEpoch != kill.KillEpoch ||
		clear.RecordedAt != formatTime(testNow) || clear.SupersededEventIDs == nil {
		t.Fatalf("clear response = %+v", clear)
	}
	activeKill(t, e, strat1, false)

	// Nothing left at this scope: a second clear is 422 (before any 409).
	wantError(t, e.do(t, "POST", clearPath, toks.aAdmin, clearBody("r", kill.KillEpoch)),
		422, codeNoActiveKill)
	// A foreign-tenant strategy stays an indistinguishable 404.
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat2+"/kill/clear", toks.aAdmin,
		clearBody("r", 1)), 404, codeUnknownStrategy)
}

// TestTenantKillClear pins scope isolation (LC-32): a tenant kill is not
// clearable at the strategy scope, foreign tenants stay 404, and the own
// tenant's admin clear flips ActiveKill for the tenant's strategies.
func TestTenantKillClear(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	rec := e.do(t, "POST", "/api/v1/tenants/tenant-a/kill", toks.aAdmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var kill tenantKillResponse
	decodeJSON(t, rec, &kill)
	activeKill(t, e, strat1, true)

	// The strategy-scope clear finds no strategy-scope kill: 422 (LC-32).
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/kill/clear", toks.aAdmin,
		clearBody("r", kill.KillEpoch)), 422, codeNoActiveKill)
	// A foreign tenant path is 404, no existence oracle.
	wantError(t, e.do(t, "POST", "/api/v1/tenants/tenant-b/kill/clear", toks.aAdmin,
		clearBody("r", kill.KillEpoch)), 404, codeUnknownTenant)

	rec = e.do(t, "POST", "/api/v1/tenants/tenant-a/kill/clear", toks.aAdmin,
		clearBody("resolved", kill.KillEpoch))
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant clear status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var clear clearResponse
	decodeJSON(t, rec, &clear)
	if clear.Scope != "tenant" || clear.TenantID != "tenant-a" || clear.StrategyID != "" ||
		clear.ClearedEpoch != kill.KillEpoch {
		t.Fatalf("tenant clear response = %+v", clear)
	}
	activeKill(t, e, strat1, false)
}

// TestPlatformKillClear pins the CLEAR-PLATFORM ack (LC-30): missing or
// wrong literals write nothing; the acked clear flips every strategy.
func TestPlatformKillClear(t *testing.T) {
	e := newEnv(t, nil)
	seedTwoTenants(t, e, "paper")

	rec := e.do(t, "POST", "/api/v1/platform/kill", adminTok, map[string]any{"ack": platformKillAck})
	if rec.Code != http.StatusOK {
		t.Fatalf("platform kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var kill platformKillResponse
	decodeJSON(t, rec, &kill)
	activeKill(t, e, strat1, true)
	activeKill(t, e, strat2, true)

	body := clearBody("resolved", kill.KillEpoch)
	wantError(t, e.do(t, "POST", "/api/v1/platform/kill/clear", adminTok, body),
		400, codePlatformClearAckRequired)
	body["ack"] = "clear-platform" // case-sensitive
	wantError(t, e.do(t, "POST", "/api/v1/platform/kill/clear", adminTok, body),
		400, codePlatformClearAckRequired)
	activeKill(t, e, strat1, true)

	body["ack"] = platformClearAck
	rec = e.do(t, "POST", "/api/v1/platform/kill/clear", adminTok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform clear status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var clear clearResponse
	decodeJSON(t, rec, &clear)
	if clear.Scope != "platform" || clear.StrategyID != "" || clear.TenantID != "" ||
		clear.ClearedEpoch != kill.KillEpoch {
		t.Fatalf("platform clear response = %+v", clear)
	}
	activeKill(t, e, strat1, false)
	activeKill(t, e, strat2, false)
}
