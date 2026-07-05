package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// killRows returns the persisted kill_breaker_events rows (nothing is
// served in these tests, so the unserved scan is the whole table).
func killRows(t *testing.T, s *store.Store) []store.KillBreakerEvent {
	t.Helper()
	rows, err := s.ListUnservedSafetyEvents()
	if err != nil {
		t.Fatalf("ListUnservedSafetyEvents: %v", err)
	}
	return rows
}

// TestStrategyKill_TraderOwnTenant: a trader kills an own-tenant strategy —
// 200 with epoch >= 1, and the persisted row carries scope 'strategy', the
// strategy id, the audit tenant_id, and the requested flatten.
func TestStrategyKill_TraderOwnTenant(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	rec := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/kill", toks.aTrader,
		map[string]any{"flatten": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp strategyKillResponse
	decodeJSON(t, rec, &resp)
	if resp.EventID == "" || resp.StrategyID != strat1 || resp.KillEpoch < 1 ||
		!resp.Flatten || resp.RecordedAt != formatTime(testNow) {
		t.Fatalf("kill response = %+v", resp)
	}

	rows := killRows(t, e.store)
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.EventID != resp.EventID || row.Kind != "kill" || row.Scope != "strategy" ||
		row.StrategyID == nil || *row.StrategyID != strat1 ||
		row.TenantID == nil || *row.TenantID != "tenant-a" ||
		row.KillEpoch == nil || *row.KillEpoch != resp.KillEpoch ||
		row.Flatten == nil || !*row.Flatten {
		t.Fatalf("persisted row = %+v", row)
	}
}

// TestStrategyKill_Foreign404NoOracle: a foreign-tenant strategy and an
// unknown id answer the IDENTICAL 404 (no existence oracle); no row lands.
func TestStrategyKill_Foreign404NoOracle(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	foreign := e.do(t, "POST", "/api/v1/strategies/"+strat2+"/kill", toks.aTrader, nil)
	absent := e.do(t, "POST", "/api/v1/strategies/"+uid(99)+"/kill", toks.aTrader, nil)
	wantError(t, foreign, 404, codeUnknownStrategy)
	wantError(t, absent, 404, codeUnknownStrategy)
	if foreign.Body.String() != absent.Body.String() {
		t.Errorf("foreign body %q != absent body %q", foreign.Body.String(), absent.Body.String())
	}
	// env-admin resolves every tenant, but an unknown id is still 404.
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+uid(99)+"/kill", adminTok, nil),
		404, codeUnknownStrategy)
	if rows := killRows(t, e.store); len(rows) != 0 {
		t.Fatalf("persisted rows = %d, want 0", len(rows))
	}
}

// TestStrategyKill_Viewer403: viewer is outside the row (403, no row).
func TestStrategyKill_Viewer403(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/kill", toks.aViewer, nil),
		403, codeForbidden)
	if rows := killRows(t, e.store); len(rows) != 0 {
		t.Fatalf("persisted rows = %d, want 0", len(rows))
	}
}

// TestStrategyKill_EnvAdminAnyStrategy: the env-admin class kills any
// tenant's strategy; an absent body never flattens (wire default false).
func TestStrategyKill_EnvAdminAnyStrategy(t *testing.T) {
	e := newEnv(t, nil)
	seedTwoTenants(t, e, "paper")

	rec := e.do(t, "POST", "/api/v1/strategies/"+strat2+"/kill", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp strategyKillResponse
	decodeJSON(t, rec, &resp)
	if resp.StrategyID != strat2 || resp.KillEpoch < 1 || resp.Flatten {
		t.Fatalf("kill response = %+v", resp)
	}
	rows := killRows(t, e.store)
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Scope != "strategy" || row.TenantID == nil || *row.TenantID != "tenant-b" ||
		row.ActorID != "env-admin" || row.Flatten == nil || *row.Flatten {
		t.Fatalf("persisted row = %+v", row)
	}
}

// TestPlatformKillRequiresAck pins the 400 on a missing/wrong ack literal
// (case-sensitive) — NO row is written (spec §Test obligations).
func TestPlatformKillRequiresAck(t *testing.T) {
	e := newEnv(t, nil)
	// Absent body, missing ack, wrong case, wrong literal — all 400.
	for _, body := range []any{
		nil,
		map[string]any{"flatten": true},
		map[string]any{"ack": "kill-platform"},
		map[string]any{"ack": "KILL PLATFORM"},
	} {
		wantError(t, e.do(t, "POST", "/api/v1/platform/kill", adminTok, body),
			400, codePlatformKillAckRequired)
	}
	if rows := killRows(t, e.store); len(rows) != 0 {
		t.Fatalf("persisted rows = %d, want 0 (no row on a rejected ack)", len(rows))
	}
}

// TestPlatformKill_EnvAdminWithAck: the acked platform kill persists the
// Phase-1 global row shape — scope 'platform', both ids NULL.
func TestPlatformKill_EnvAdminWithAck(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "POST", "/api/v1/platform/kill", adminTok,
		map[string]any{"ack": platformKillAck, "flatten": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp platformKillResponse
	decodeJSON(t, rec, &resp)
	if resp.EventID == "" || resp.KillEpoch < 1 || !resp.Flatten ||
		resp.RecordedAt != formatTime(testNow) {
		t.Fatalf("kill response = %+v", resp)
	}
	rows := killRows(t, e.store)
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.EventID != resp.EventID || row.Kind != "kill" || row.Scope != "platform" ||
		row.StrategyID != nil || row.TenantID != nil || row.ActorID != "env-admin" ||
		row.KillEpoch == nil || *row.KillEpoch != resp.KillEpoch ||
		row.Flatten == nil || !*row.Flatten {
		t.Fatalf("persisted row = %+v", row)
	}
}

// TestPlatformKill_TenantRole403: NO tenant role may kill the platform —
// even owner/admin with a correct ack are 403.
func TestPlatformKill_TenantRole403(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")
	owner := seedUserToken(t, e.store, "tenant-a", RoleOwner, "db-a-owner")
	body := map[string]any{"ack": platformKillAck}
	wantError(t, e.do(t, "POST", "/api/v1/platform/kill", owner, body), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/platform/kill", toks.aAdmin, body), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/platform/kill", toks.aTrader, body), 403, codeForbidden)
	if rows := killRows(t, e.store); len(rows) != 0 {
		t.Fatalf("persisted rows = %d, want 0", len(rows))
	}
}

// TestTenantKill_FlattenPersisted: the EXTENDED tenant kill accepts the
// optional flatten field, persists it, and echoes it in the response.
func TestTenantKill_FlattenPersisted(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")
	rec := e.do(t, "POST", "/api/v1/tenants/tenant-a/kill", toks.aAdmin,
		map[string]any{"flatten": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp tenantKillResponse
	decodeJSON(t, rec, &resp)
	if resp.TenantID != "tenant-a" || resp.KillEpoch < 1 || !resp.Flatten {
		t.Fatalf("kill response = %+v", resp)
	}
	rows := killRows(t, e.store)
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Scope != "tenant" || row.StrategyID != nil ||
		row.TenantID == nil || *row.TenantID != "tenant-a" ||
		row.Flatten == nil || !*row.Flatten {
		t.Fatalf("persisted row = %+v", row)
	}
}

// TestTenantKill_EmptyBodyBackwardCompat: the v1 bodies — zero bytes and
// the empty object {} — both remain valid and mean flatten=false.
func TestTenantKill_EmptyBodyBackwardCompat(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedTwoTenants(t, e, "paper")

	rec := e.do(t, "POST", "/api/v1/tenants/tenant-a/kill", toks.aAdmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-body kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp tenantKillResponse
	decodeJSON(t, rec, &resp)
	if resp.TenantID != "tenant-a" || resp.KillEpoch < 1 || resp.Flatten {
		t.Fatalf("kill response = %+v", resp)
	}
	if rec := e.do(t, "POST", "/api/v1/tenants/tenant-b/kill", adminTok, struct{}{}); rec.Code != http.StatusOK {
		t.Fatalf("{} kill status = %d (body %q)", rec.Code, rec.Body.String())
	}
	rows := killRows(t, e.store)
	if len(rows) != 2 {
		t.Fatalf("persisted rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if row.Flatten == nil || *row.Flatten {
			t.Errorf("row %s flatten = %v, want false", row.EventID, row.Flatten)
		}
	}
}

// blockingSafetyDriver signals every invocation on called, then blocks on
// release: a 200 while it is blocked proves the handler never waits on the
// drive (safety-wiring.md invariant 1).
type blockingSafetyDriver struct {
	called  chan struct{}
	release chan struct{}
}

func (d *blockingSafetyDriver) DriveSafetyEffects(context.Context) error {
	d.called <- struct{}{}
	<-d.release
	return nil
}

// TestKillInvokesSafetyDriverAsync: all three kill tiers invoke the
// SafetyDriver seam after the append, asynchronously — the response
// returns while the driver is still blocked.
func TestKillInvokesSafetyDriverAsync(t *testing.T) {
	drv := &blockingSafetyDriver{called: make(chan struct{}, 3), release: make(chan struct{})}
	e := newEnv(t, func(cfg *Config) { cfg.SafetyDriver = drv })
	seedTwoTenants(t, e, "paper")
	t.Cleanup(func() { close(drv.release) })

	kills := []struct {
		name, path string
		body       any
	}{
		{"strategy", "/api/v1/strategies/" + strat1 + "/kill", nil},
		{"tenant", "/api/v1/tenants/tenant-a/kill", nil},
		{"platform", "/api/v1/platform/kill", map[string]any{"ack": platformKillAck}},
	}
	for _, k := range kills {
		// release is not closed until cleanup: the driver goroutine stays
		// blocked, so a returned 200 cannot have waited on it.
		rec := e.do(t, "POST", k.path, adminTok, k.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s kill status = %d (body %q)", k.name, rec.Code, rec.Body.String())
		}
		select {
		case <-drv.called:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s kill: SafetyDriver never invoked", k.name)
		}
	}
}
