package api

import (
	"net/http"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// ingestJuneTrace persists a proposal chain plus a June-dated trace so the
// 2026-06 period is closeable at the fixed July test clock.
func ingestJuneTrace(t *testing.T, e *testEnv, strategyID string, base, tick int, costs []store.TraceModelCost) {
	t.Helper()
	proposalID, _, runID := insertChain(t, e.store, base, strategyID, tick)
	env := testTraceEnvelope(t, strategyID, runID, &proposalID)
	env.TickNumber = tick
	env.StartedAt = mustTime(t, "2026-06-15T12:00:00Z")
	env.ModelCosts = costs
	if _, err := e.store.InsertTrace(env, testNow); err != nil {
		t.Fatalf("InsertTrace(%s): %v", runID, err)
	}
}

func closePeriodAPI(t *testing.T, e *testEnv, tenantID, period string) closePeriodResponse {
	t.Helper()
	rec := e.do(t, "POST", "/api/v1/billing/periods/close", adminTok,
		periodRequest{TenantID: tenantID, Period: period})
	if rec.Code != http.StatusOK {
		t.Fatalf("close %s %s: status = %d (body %q)", tenantID, period, rec.Code, rec.Body.String())
	}
	var resp closePeriodResponse
	decodeJSON(t, rec, &resp)
	return resp
}

func reconcileAPI(t *testing.T, e *testEnv, tenantID, period string) reconcileResponse {
	t.Helper()
	rec := e.do(t, "POST", "/api/v1/billing/reconcile", adminTok,
		periodRequest{TenantID: tenantID, Period: period})
	if rec.Code != http.StatusOK {
		t.Fatalf("reconcile %s %s: status = %d (body %q)", tenantID, period, rec.Code, rec.Body.String())
	}
	var resp reconcileResponse
	decodeJSON(t, rec, &resp)
	return resp
}

// seedBilledTenants builds tenant-a/tenant-b with one June trace each and
// closes both 2026-06 periods; returns the tenants' DB tokens.
func seedBilledTenants(t *testing.T, e *testEnv) twoTenants {
	t.Helper()
	toks := seedTwoTenants(t, e, "paper")
	ingestJuneTrace(t, e, strat1, 10, 0, []store.TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
	})
	ingestJuneTrace(t, e, strat2, 20, 0, []store.TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.002")},
	})
	closePeriodAPI(t, e, "tenant-a", "2026-06")
	closePeriodAPI(t, e, "tenant-b", "2026-06")
	return toks
}

// TestBillingIsolation_ForeignInvoice404: a foreign invoice_id answers the
// SAME 404 as a nonexistent one (no cross-tenant existence oracle); the
// owning tenant's admin reads it fine, and viewer/trader read NO invoices.
func TestBillingIsolation_ForeignInvoice404(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedBilledTenants(t, e)
	bAdmin := seedUserToken(t, e.store, "tenant-b", RoleAdmin, "db-b-admin")

	rec := e.do(t, "GET", "/api/v1/billing/invoices/inv-tenant-a-2026-06", toks.aAdmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("own invoice: status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var got invoiceResponse
	decodeJSON(t, rec, &got)
	if got.Invoice.TotalUSD != "0.001" || len(got.Lines) != 1 {
		t.Fatalf("own invoice = %+v", got)
	}
	wantError(t, e.do(t, "GET", "/api/v1/billing/invoices/inv-tenant-a-2026-06", bAdmin, nil),
		404, codeUnknownInvoice)
	wantError(t, e.do(t, "GET", "/api/v1/billing/invoices/inv-no-such-2026-06", bAdmin, nil),
		404, codeUnknownInvoice)
	// Invoices are financial records: viewer and trader are 403 FORBIDDEN.
	wantError(t, e.do(t, "GET", "/api/v1/billing/invoices", toks.aViewer, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/billing/invoices/inv-tenant-a-2026-06", toks.aTrader, nil),
		403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/billing/invoices", toks.aAgent, nil), 403, codeForbidden)
}

// TestBillingIsolation_ForeignReconciliation404: same shape for recon runs.
func TestBillingIsolation_ForeignReconciliation404(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedBilledTenants(t, e)
	bAdmin := seedUserToken(t, e.store, "tenant-b", RoleAdmin, "db-b-admin")
	runA := reconcileAPI(t, e, "tenant-a", "2026-06").Run

	rec := e.do(t, "GET", "/api/v1/billing/reconciliations/"+runA.ReconID, toks.aAdmin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("own reconciliation: status = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "GET", "/api/v1/billing/reconciliations/"+runA.ReconID, bAdmin, nil),
		404, codeUnknownReconciliation)
	wantError(t, e.do(t, "GET", "/api/v1/billing/reconciliations/"+uid(97), bAdmin, nil),
		404, codeUnknownReconciliation)
}

// TestBillingIsolation_ListsExcludeForeignRows: list items AND totals are
// tenant-scoped for tenant principals; the platform read and env-admin
// classes list every tenant.
func TestBillingIsolation_ListsExcludeForeignRows(t *testing.T) {
	e := newEnv(t, nil)
	toks := seedBilledTenants(t, e)
	reconcileAPI(t, e, "tenant-a", "2026-06")
	reconcileAPI(t, e, "tenant-b", "2026-06")

	var invoices page[store.Invoice]
	rec := e.do(t, "GET", "/api/v1/billing/invoices", toks.aAdmin, nil)
	decodeJSON(t, rec, &invoices)
	if rec.Code != http.StatusOK || invoices.Total != 1 || len(invoices.Items) != 1 ||
		invoices.Items[0].TenantID != "tenant-a" {
		t.Fatalf("tenant-a invoices = %+v (status %d)", invoices, rec.Code)
	}
	for _, tok := range []string{readTok, adminTok} {
		rec = e.do(t, "GET", "/api/v1/billing/invoices", tok, nil)
		decodeJSON(t, rec, &invoices)
		if rec.Code != http.StatusOK || invoices.Total != 2 {
			t.Fatalf("platform invoices = %+v (status %d), want both tenants", invoices, rec.Code)
		}
	}
	var runs page[store.ReconciliationRun]
	rec = e.do(t, "GET", "/api/v1/billing/reconciliations", toks.aAdmin, nil)
	decodeJSON(t, rec, &runs)
	if rec.Code != http.StatusOK || runs.Total != 1 || runs.Items[0].TenantID != "tenant-a" {
		t.Fatalf("tenant-a reconciliations = %+v (status %d)", runs, rec.Code)
	}
}

func meteringRecordFixture(requestID, strategyID string) meteringRecordRequest {
	return meteringRecordRequest{
		RequestID: requestID, StrategyID: strategyID, Model: "stub",
		InputTokens: 100, OutputTokens: 20, CostUSD: "0.50", MeteredAt: "2026-06-15T12:00:05Z",
	}
}

func postMetering(t *testing.T, e *testEnv, body meteringRequest) meteringResponse {
	t.Helper()
	rec := e.do(t, "POST", "/api/v1/billing/metering", adminTok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("metering import: status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp meteringResponse
	decodeJSON(t, rec, &resp)
	return resp
}

// TestMeteringImportIdempotentAndConflict: identical re-imports no-op per
// record (cost_usd by DECIMAL value, trailing zeros ignored); a request_id
// disagreeing with its stored content 409-rejects the whole batch.
func TestMeteringImportIdempotentAndConflict(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")

	resp := postMetering(t, e, meteringRequest{Source: "export-1",
		Records: []meteringRecordRequest{meteringRecordFixture(uid(50), strat1)}})
	if resp.Imported != 1 || resp.Skipped != 0 {
		t.Fatalf("first import = %+v", resp)
	}
	// Re-import with the cost differing ONLY in trailing zeros: no-op 200.
	dup := meteringRecordFixture(uid(50), strat1)
	dup.CostUSD = "0.5000"
	resp = postMetering(t, e, meteringRequest{Source: "export-2",
		Records: []meteringRecordRequest{dup}})
	if resp.Imported != 0 || resp.Skipped != 1 {
		t.Fatalf("idempotent re-import = %+v", resp)
	}
	// Same request_id, different content: 409, whole batch rejected.
	conflicting := meteringRecordFixture(uid(50), strat1)
	conflicting.OutputTokens = 21
	wantError(t, e.do(t, "POST", "/api/v1/billing/metering", adminTok, meteringRequest{
		Source:  "export-3",
		Records: []meteringRecordRequest{meteringRecordFixture(uid(51), strat1), conflicting},
	}), 409, codeMeteringConflict)
	// The batch was atomic: uid(51) never persisted and imports fresh.
	resp = postMetering(t, e, meteringRequest{Source: "export-4",
		Records: []meteringRecordRequest{meteringRecordFixture(uid(51), strat1)}})
	if resp.Imported != 1 {
		t.Fatalf("post-conflict import = %+v, want the record fresh (409 batch was atomic)", resp)
	}
}

// TestMeteringImportValidation: records without request_id are REJECTED,
// as are unresolvable strategies/aliases, malformed decimals, and negative
// token counts — one bad record rejects the ENTIRE batch, nothing persists.
func TestMeteringImportValidation(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")

	cases := []struct {
		name   string
		mutate func(r *meteringRecordRequest)
	}{
		{"missing request_id", func(r *meteringRecordRequest) { r.RequestID = "" }},
		{"unresolvable strategy", func(r *meteringRecordRequest) { r.StrategyID = uid(99) }},
		{"unknown alias", func(r *meteringRecordRequest) { r.StrategyID = ""; r.APIKeyAlias = "nope" }},
		{"malformed decimal", func(r *meteringRecordRequest) { r.CostUSD = "0.50USD" }},
		{"negative tokens", func(r *meteringRecordRequest) { r.InputTokens = -1 }},
		{"malformed metered_at", func(r *meteringRecordRequest) { r.MeteredAt = "2026-06-15 12:00:05" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := meteringRecordFixture(uid(60), strat1)
			tc.mutate(&bad)
			wantError(t, e.do(t, "POST", "/api/v1/billing/metering", adminTok, meteringRequest{
				Source:  "export-1",
				Records: []meteringRecordRequest{meteringRecordFixture(uid(61), strat1), bad},
			}), 400, codeInvalidMeteringRecord)
		})
	}
	// Nothing from any rejected batch persisted: the good record is fresh.
	resp := postMetering(t, e, meteringRequest{Source: "export-2",
		Records: []meteringRecordRequest{meteringRecordFixture(uid(61), strat1)}})
	if resp.Imported != 1 {
		t.Fatalf("import after rejections = %+v, want fresh (atomic batches)", resp)
	}
}

// TestMeteringImportChunkedAndAlias: a large export split over multiple
// POSTs is safe by per-record idempotency, and api_key_alias records
// resolve through alias_map.
func TestMeteringImportChunkedAndAlias(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	aliased := meteringRecordFixture(uid(50), "")
	aliased.APIKeyAlias = "key-alpha"
	body := meteringRequest{
		Source:   "export-1",
		AliasMap: map[string]string{"key-alpha": strat1},
		Records:  []meteringRecordRequest{aliased, meteringRecordFixture(uid(51), strat1)},
	}
	if resp := postMetering(t, e, body); resp.Imported != 2 {
		t.Fatalf("chunk 1 = %+v", resp)
	}
	// Chunk 2 overlaps chunk 1 (a re-POSTed failed chunk): overlap no-ops.
	body.Records = []meteringRecordRequest{meteringRecordFixture(uid(51), strat1), meteringRecordFixture(uid(52), strat1)}
	if resp := postMetering(t, e, body); resp.Imported != 1 || resp.Skipped != 1 {
		t.Fatalf("chunk 2 = %+v, want 1 imported, 1 skipped", resp)
	}
}

// TestBillingPeriodEndpoints pins the close/reconcile status codes: a
// running month cannot close (400), a duplicate close is 409 PERIOD_CLOSED
// (operator retry safe), reconcile-before-close is 409 PERIOD_OPEN, and an
// unknown tenant is 404.
func TestBillingPeriodEndpoints(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	ingestJuneTrace(t, e, strat1, 10, 0, []store.TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
	})

	closeBody := func(tenantID, period string) periodRequest {
		return periodRequest{TenantID: tenantID, Period: period}
	}
	// testNow is 2026-07-04: July is the running month.
	wantError(t, e.do(t, "POST", "/api/v1/billing/periods/close", adminTok,
		closeBody("tenant-1", "2026-07")), 400, codeInvalidPeriod)
	for _, bad := range []string{"", "202606", "2026-13", "2026-6"} {
		wantError(t, e.do(t, "POST", "/api/v1/billing/periods/close", adminTok,
			closeBody("tenant-1", bad)), 400, codeInvalidPeriod)
		wantError(t, e.do(t, "POST", "/api/v1/billing/reconcile", adminTok,
			closeBody("tenant-1", bad)), 400, codeInvalidPeriod)
	}
	wantError(t, e.do(t, "POST", "/api/v1/billing/periods/close", adminTok,
		closeBody("no-such-tenant", "2026-06")), 404, codeUnknownTenant)
	wantError(t, e.do(t, "POST", "/api/v1/billing/reconcile", adminTok,
		closeBody("no-such-tenant", "2026-06")), 404, codeUnknownTenant)
	wantError(t, e.do(t, "POST", "/api/v1/billing/reconcile", adminTok,
		closeBody("tenant-1", "2026-06")), 409, codePeriodOpen)

	closed := closePeriodAPI(t, e, "tenant-1", "2026-06")
	if closed.Invoice.InvoiceID != "inv-tenant-1-2026-06" || closed.Invoice.TotalUSD != "0.001" {
		t.Fatalf("closed invoice = %+v", closed.Invoice)
	}
	wantError(t, e.do(t, "POST", "/api/v1/billing/periods/close", adminTok,
		closeBody("tenant-1", "2026-06")), 409, codePeriodClosed)
	if run := reconcileAPI(t, e, "tenant-1", "2026-06").Run; run.Status != "pass" {
		t.Fatalf("reconcile run = %+v", run)
	}
}

// TestBillingForgedRequestIDSquat: a forger squatting a victim's request_id
// nulls the VICTIM's attribution (unattributed; own invoice unchanged) and
// the forger's reconciliation surfaces ATTRIBUTION_MISMATCH against the
// victim's gateway record.
func TestBillingForgedRequestIDSquat(t *testing.T) {
	e := newEnv(t, nil)
	seedTwoTenants(t, e, "paper")
	forged := uid(50)

	// The forger (tenant-b, strat2) ingests FIRST: first-writer-wins on the
	// partial unique index.
	ingestJuneTrace(t, e, strat2, 20, 0, []store.TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
			CostUSD: mustDec(t, "0.002"), RequestID: &forged},
	})
	// The victim (tenant-a, strat1) ingests the SAME id: conflict-nulled,
	// cost still lands.
	ingestJuneTrace(t, e, strat1, 10, 0, []store.TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
			CostUSD: mustDec(t, "0.001"), RequestID: &forged},
	})
	// The gateway truth: the request belongs to the VICTIM's strategy.
	postMetering(t, e, meteringRequest{Source: "export-1", Records: []meteringRecordRequest{
		{RequestID: forged, StrategyID: strat1, Model: "stub",
			InputTokens: 100, OutputTokens: 20, CostUSD: "0.001", MeteredAt: "2026-06-15T12:00:05Z"},
	}})

	// The victim is never mis-billed: its invoice total is its own cost.
	victimInvoice := closePeriodAPI(t, e, "tenant-a", "2026-06").Invoice
	if victimInvoice.TotalUSD != "0.001" {
		t.Fatalf("victim invoice = %+v, want its own 0.001", victimInvoice)
	}
	closePeriodAPI(t, e, "tenant-b", "2026-06")

	// Victim run: the conflict-nulled row is unattributed; the gateway
	// record is its orphan (no ATTRIBUTION_MISMATCH on the victim side).
	victim := reconcileAPI(t, e, "tenant-a", "2026-06")
	if victim.Run.UnattributedClientCostUSD != "0.001" {
		t.Fatalf("victim run = %+v, want the nulled row unattributed", victim.Run)
	}
	for _, d := range victim.Discrepancies {
		if d.Class != store.ClassOrphanGateway {
			t.Fatalf("victim discrepancy = %+v, want ORPHAN_GATEWAY only", d)
		}
	}
	// Forger run: the join fails the strategy constraint — the row stays
	// orphan_client and ATTRIBUTION_MISMATCH surfaces the forgery (fail).
	forger := reconcileAPI(t, e, "tenant-b", "2026-06")
	if forger.Run.Status != "fail" || forger.Run.OrphanClientCostUSD != "0.002" {
		t.Fatalf("forger run = %+v, want fail with the row orphaned", forger.Run)
	}
	if len(forger.Discrepancies) != 1 || forger.Discrepancies[0].Class != store.ClassAttributionMismatch {
		t.Fatalf("forger discrepancies = %+v, want exactly ATTRIBUTION_MISMATCH", forger.Discrepancies)
	}
}
