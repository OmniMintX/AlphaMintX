package store

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

// TestReconcileRequiresClosedPeriod: reconcile-before-close is
// ErrPeriodOpen (the invoice is the comparison target); an unknown tenant
// is ErrNotFound.
func TestReconcileRequiresClosedPeriod(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	if _, _, err := s.Reconcile("tenant-1", "2026-06", testNow); !errors.Is(err, ErrPeriodOpen) {
		t.Fatalf("open period err = %v, want ErrPeriodOpen", err)
	}
	if _, _, err := s.Reconcile("no-such-tenant", "2026-06", testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant err = %v, want ErrNotFound", err)
	}
}

// TestReconcileGolden runs a synthetic gateway export against known
// model_costs (billing-and-metering.md §Test requirements): (a) full match
// passes with zero discrepancies and the exact identity; (b) one injected
// case per class classifies exactly once, with MISMATCH_TOKENS and
// ATTRIBUTION_MISMATCH flipping status to fail and the others not. Every
// case carries the SAME baseline: one matched pair (r1) plus one
// unattributed (NULL request_id) row, so the identity is never vacuous.
func TestReconcileGolden(t *testing.T) {
	r := func(i int) string { return uid(50 + i) }
	baselineGateway := MeteringRecord{
		RequestID: r(1), StrategyID: uid(1), Model: "stub",
		InputTokens: 100, OutputTokens: 20, CostUSD: "0.001", MeteredAt: "2026-06-15T12:01:00Z",
	}
	cases := []struct {
		name        string
		clientExtra []TraceModelCost // appended to the June trace
		gateway     []MeteringRecord // appended to the import
		wantClass   string           // "" = zero discrepancies
		wantStatus  string
	}{
		{name: "full match", wantStatus: "pass"},
		{
			name: "orphan client", wantClass: ClassOrphanClient, wantStatus: "pass",
			clientExtra: []TraceModelCost{{Node: "trader", Model: "stub", InputTokens: 10,
				OutputTokens: 2, CostUSD: mustDec(t, "0.002"), RequestID: strPtr(r(2))}},
		},
		{
			name: "orphan gateway", wantClass: ClassOrphanGateway, wantStatus: "pass",
			gateway: []MeteringRecord{{RequestID: r(3), StrategyID: uid(1), Model: "stub",
				InputTokens: 5, OutputTokens: 1, CostUSD: "0.0002", MeteredAt: "2026-06-20T12:00:00Z"}},
		},
		{
			name: "estimated client", wantClass: ClassEstimatedClient, wantStatus: "pass",
			clientExtra: []TraceModelCost{{Node: "trader", Model: "stub", InputTokens: 400,
				OutputTokens: 0, CostUSD: mustDec(t, "0.003"), RequestID: strPtr(r(4)), Estimated: boolPtr(true)}},
		},
		{
			name: "mismatch tokens", wantClass: ClassMismatchTokens, wantStatus: "fail",
			clientExtra: []TraceModelCost{{Node: "trader", Model: "stub", InputTokens: 100,
				OutputTokens: 20, CostUSD: mustDec(t, "0.001"), RequestID: strPtr(r(5))}},
			gateway: []MeteringRecord{{RequestID: r(5), StrategyID: uid(1), Model: "stub",
				InputTokens: 101, OutputTokens: 20, CostUSD: "0.001", MeteredAt: "2026-06-15T12:02:00Z"}},
		},
		{
			name: "mismatch cost", wantClass: ClassMismatchCost, wantStatus: "pass",
			clientExtra: []TraceModelCost{{Node: "trader", Model: "stub", InputTokens: 100,
				OutputTokens: 20, CostUSD: mustDec(t, "0.001"), RequestID: strPtr(r(6))}},
			gateway: []MeteringRecord{{RequestID: r(6), StrategyID: uid(1), Model: "stub",
				InputTokens: 100, OutputTokens: 20, CostUSD: "0.002", MeteredAt: "2026-06-15T12:02:00Z"}},
		},
		{
			// The forged join: the client row's request_id resolves to a
			// DIFFERENT strategy's gateway record (metered outside the
			// period so it cannot ALSO classify ORPHAN_GATEWAY).
			name: "attribution mismatch", wantClass: ClassAttributionMismatch, wantStatus: "fail",
			clientExtra: []TraceModelCost{{Node: "trader", Model: "stub", InputTokens: 100,
				OutputTokens: 20, CostUSD: mustDec(t, "0.001"), RequestID: strPtr(r(7))}},
			gateway: []MeteringRecord{{RequestID: r(7), StrategyID: uid(2), Model: "stub",
				InputTokens: 100, OutputTokens: 20, CostUSD: "0.001", MeteredAt: "2026-05-20T12:00:00Z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			seedTenant(t, s, "tenant-1")
			createStrategy(t, s, uid(1))
			createStrategy(t, s, uid(2))
			costs := append([]TraceModelCost{
				{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
					CostUSD: mustDec(t, "0.001"), RequestID: strPtr(r(1))},
				{Node: "overflow_aggregate", Model: "aggregate", InputTokens: 30,
					OutputTokens: 6, CostUSD: mustDec(t, "0.0001")},
			}, tc.clientExtra...)
			ingestTrace(t, s, uid(1), 10, 0, "2026-06-15T12:00:00Z", costs)
			records := append([]MeteringRecord{baselineGateway}, tc.gateway...)
			if _, _, err := s.InsertMeteringRecords("export-1", records, testNow); err != nil {
				t.Fatalf("InsertMeteringRecords: %v", err)
			}
			invoice, _ := mustClose(t, s, "tenant-1", "2026-06")

			run, ds, err := s.Reconcile("tenant-1", "2026-06", testNow)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if run.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (discrepancies %+v)", run.Status, tc.wantStatus, ds)
			}
			wantCount := 0
			if tc.wantClass != "" {
				wantCount = 1
			}
			if len(ds) != wantCount || run.DiscrepancyCount != wantCount {
				t.Fatalf("discrepancies = %+v (count %d), want exactly %d", ds, run.DiscrepancyCount, wantCount)
			}
			if wantCount == 1 && ds[0].Class != tc.wantClass {
				t.Errorf("class = %q, want %q", ds[0].Class, tc.wantClass)
			}
			// The arithmetic identity holds EXACTLY on every run: the four
			// classes partition precisely the rows the invoice sums.
			sum := decimal.RequireFromString(run.MatchedClientCostUSD).
				Add(decimal.RequireFromString(run.OrphanClientCostUSD)).
				Add(decimal.RequireFromString(run.EstimatedClientCostUSD)).
				Add(decimal.RequireFromString(run.UnattributedClientCostUSD))
			if !sum.Equal(decimal.RequireFromString(invoice.TotalUSD)) || run.InvoiceTotalUSD != invoice.TotalUSD {
				t.Errorf("identity: %s+%s+%s+%s != total %s", run.MatchedClientCostUSD,
					run.OrphanClientCostUSD, run.EstimatedClientCostUSD,
					run.UnattributedClientCostUSD, invoice.TotalUSD)
			}
			if run.MatchedCount < 1 || decimal.RequireFromString(run.MatchedClientCostUSD).IsZero() {
				t.Errorf("run = %+v, want the baseline pair matched (non-vacuous)", run)
			}
			if run.UnattributedClientCostUSD != "0.0001" {
				t.Errorf("unattributed = %q, want the aggregate row's \"0.0001\"", run.UnattributedClientCostUSD)
			}
			// Append-only: a re-run appends a FRESH run row.
			rerun, _, err := s.Reconcile("tenant-1", "2026-06", testNow)
			if err != nil || rerun.ReconID == run.ReconID || rerun.Status != run.Status {
				t.Errorf("re-run = %+v err=%v, want a fresh identical-status run", rerun, err)
			}
		})
	}
}

// TestReconcileEstimatedClientWithGatewayRecord: an is_estimated=1 client
// row whose request_id ALSO has an in-window gateway record. Precedence
// rule 2 wins — the row counts under estimated_client_cost_usd and is
// never matched — so the unmatched gateway record ALSO enumerates as
// ORPHAN_GATEWAY: two discrepancy classes from one id. Neither class fails
// a run (PASS definition, billing-and-metering.md §Reconciliation) and the
// arithmetic identity still holds exactly.
func TestReconcileEstimatedClientWithGatewayRecord(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))
	reqID := uid(58)
	ingestTrace(t, s, uid(1), 10, 0, "2026-06-15T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 400, OutputTokens: 0,
			CostUSD: mustDec(t, "0.003"), RequestID: &reqID, Estimated: boolPtr(true)},
	})
	if _, _, err := s.InsertMeteringRecords("export-1", []MeteringRecord{{
		RequestID: reqID, StrategyID: uid(1), Model: "stub",
		InputTokens: 400, OutputTokens: 0, CostUSD: "0.003", MeteredAt: "2026-06-15T12:01:00Z",
	}}, testNow); err != nil {
		t.Fatalf("InsertMeteringRecords: %v", err)
	}
	invoice, _ := mustClose(t, s, "tenant-1", "2026-06")

	run, ds, err := s.Reconcile("tenant-1", "2026-06", testNow)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The row is a SUM TERM under estimated_client, never matched.
	if run.EstimatedClientCostUSD != "0.003" || run.MatchedCount != 0 || run.MatchedClientCostUSD != "0" {
		t.Fatalf("run = %+v, want estimated \"0.003\" and zero matches", run)
	}
	classes := map[string]int{}
	for _, d := range ds {
		if d.RequestID == nil || *d.RequestID != reqID {
			t.Fatalf("discrepancy = %+v, want request_id %s", d, reqID)
		}
		classes[d.Class]++
	}
	if len(ds) != 2 || classes[ClassEstimatedClient] != 1 || classes[ClassOrphanGateway] != 1 {
		t.Fatalf("discrepancies = %+v, want one ESTIMATED_CLIENT and one ORPHAN_GATEWAY for %s", ds, reqID)
	}
	// Neither class flips status to fail; the identity holds exactly.
	if run.Status != "pass" {
		t.Fatalf("status = %q, want pass (ESTIMATED_CLIENT and ORPHAN_GATEWAY never fail)", run.Status)
	}
	sum := decimal.RequireFromString(run.MatchedClientCostUSD).
		Add(decimal.RequireFromString(run.OrphanClientCostUSD)).
		Add(decimal.RequireFromString(run.EstimatedClientCostUSD)).
		Add(decimal.RequireFromString(run.UnattributedClientCostUSD))
	if !sum.Equal(decimal.RequireFromString(invoice.TotalUSD)) || run.InvoiceTotalUSD != invoice.TotalUSD {
		t.Fatalf("identity: class sums != total %s (run %+v)", invoice.TotalUSD, run)
	}
}
