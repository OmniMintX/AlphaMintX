package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

func seedTenant(t *testing.T, s *Store, tenantID string) {
	t.Helper()
	if err := s.CreateTenant(Tenant{TenantID: tenantID, Name: tenantID, CreatedAt: formatTime(testNow)}); err != nil {
		t.Fatalf("CreateTenant(%s): %v", tenantID, err)
	}
}

func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

// ingestTrace persists a proposal chain (ids from base) plus a trace with
// the given started_at and model costs; returns the run_id.
func ingestTrace(t *testing.T, s *Store, strategyID string, base, tick int, startedAt string, costs []TraceModelCost) string {
	t.Helper()
	proposalID, _, runID := insertChain(t, s, base, strategyID, tick)
	env := testTrace(t, strategyID, runID, &proposalID)
	env.TickNumber = tick
	env.StartedAt = mustTime(t, startedAt)
	env.ModelCosts = costs
	if _, err := s.InsertTrace(env, testNow); err != nil {
		t.Fatalf("InsertTrace(%s): %v", runID, err)
	}
	return runID
}

func mustClose(t *testing.T, s *Store, tenantID, period string) (Invoice, []InvoiceLine) {
	t.Helper()
	invoice, lines, err := s.ClosePeriod(tenantID, period, testNow)
	if err != nil {
		t.Fatalf("ClosePeriod(%s, %s): %v", tenantID, period, err)
	}
	return invoice, lines
}

func decEq(t *testing.T, got, want string) {
	t.Helper()
	g, err := decimal.NewFromString(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	w, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatalf("parse %q: %v", want, err)
	}
	if !g.Equal(w) {
		t.Fatalf("decimal = %q, want %q", got, want)
	}
}

// TestClosePeriodWatermarkExactlyOnce: a trace ingested AFTER its period
// closed bills as carry_over on the NEXT close and never twice; the closed
// invoice is immutable; a duplicate close answers ErrPeriodClosed.
func TestClosePeriodWatermarkExactlyOnce(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))

	ingestTrace(t, s, uid(1), 10, 0, "2026-06-15T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
	})
	invoice, lines := mustClose(t, s, "tenant-1", "2026-06")
	if invoice.InvoiceID != "inv-tenant-1-2026-06" || invoice.TotalUSD != "0.001" || invoice.LineCount != 1 {
		t.Fatalf("June invoice = %+v", invoice)
	}
	if len(lines) != 1 || lines[0].EntryType != EntryTypeUsage || lines[0].OriginalPeriod != nil ||
		lines[0].LineID != "inv-tenant-1-2026-06#0" || lines[0].InputTokens != 100 {
		t.Fatalf("June lines = %+v", lines)
	}

	// Late June-dated trace lands after the close: the June invoice is
	// immutable, so the cost rides the NEXT close as carry_over.
	ingestTrace(t, s, uid(1), 20, 1, "2026-06-20T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.002")},
	})
	julyInvoice, julyLines := mustClose(t, s, "tenant-1", "2026-07")
	if julyInvoice.TotalUSD != "0.002" || len(julyLines) != 1 {
		t.Fatalf("July invoice = %+v lines = %+v", julyInvoice, julyLines)
	}
	if julyLines[0].EntryType != EntryTypeCarryOver || julyLines[0].OriginalPeriod == nil ||
		*julyLines[0].OriginalPeriod != "2026-06" {
		t.Fatalf("July line = %+v, want carry_over from 2026-06", julyLines[0])
	}

	// The June invoice is byte-identical on read-back: billed exactly once.
	stored, storedLines, err := s.GetInvoice("inv-tenant-1-2026-06", "tenant-1")
	if err != nil || !reflect.DeepEqual(stored, invoice) || !reflect.DeepEqual(storedLines, lines) {
		t.Fatalf("June read-back = %+v %+v err=%v", stored, storedLines, err)
	}
	if _, _, err := s.ClosePeriod("tenant-1", "2026-06", testNow); !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("duplicate close err = %v, want ErrPeriodClosed", err)
	}
}

// TestClosePeriodOutOfOrderCloses: closing 2026-03 before 2026-02 is LEGAL;
// the rowid watermark keeps every row billed exactly once — the earlier
// period's later close sees an empty window.
func TestClosePeriodOutOfOrderCloses(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))
	ingestTrace(t, s, uid(1), 10, 0, "2026-02-10T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 10, OutputTokens: 2, CostUSD: mustDec(t, "0.001")},
	})
	ingestTrace(t, s, uid(1), 20, 1, "2026-03-10T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 20, OutputTokens: 4, CostUSD: mustDec(t, "0.002")},
	})

	march, marchLines := mustClose(t, s, "tenant-1", "2026-03")
	decEq(t, march.TotalUSD, "0.003")
	if len(marchLines) != 2 {
		t.Fatalf("March lines = %+v", marchLines)
	}
	// Deterministic order: usage before carry_over.
	if marchLines[0].EntryType != EntryTypeUsage || marchLines[1].EntryType != EntryTypeCarryOver ||
		*marchLines[1].OriginalPeriod != "2026-02" {
		t.Fatalf("March line order = %+v", marchLines)
	}

	feb, febLines := mustClose(t, s, "tenant-1", "2026-02")
	if feb.TotalUSD != "0" || feb.LineCount != 0 || len(febLines) != 0 {
		t.Fatalf("out-of-order Feb close = %+v lines=%+v, want empty (already billed)", feb, febLines)
	}
}

// TestClosePeriodEmptyIsLegal: an empty period closes with zero lines and
// total "0"; an unknown tenant is ErrNotFound.
func TestClosePeriodEmptyIsLegal(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	invoice, lines := mustClose(t, s, "tenant-1", "2026-06")
	if invoice.TotalUSD != "0" || invoice.LineCount != 0 || len(lines) != 0 {
		t.Fatalf("empty close = %+v lines=%+v, want zero lines, total \"0\"", invoice, lines)
	}
	if _, _, err := s.ClosePeriod("no-such-tenant", "2026-06", testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant err = %v, want ErrNotFound", err)
	}
}

// TestClosePeriodConcurrentIngestBoundary: a trace ingest committing around
// a concurrent close lands entirely inside or entirely outside the
// watermark window (SQLite's single writer serializes the transactions).
// Every trace fans out two rows summing 0.003, so both invoice totals must
// be exact multiples of 0.003 — a split trace would break divisibility —
// and together they must cover every row exactly once.
func TestClosePeriodConcurrentIngestBoundary(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))

	// Proposal chains are seeded up front; ONLY the trace ingests (the
	// model_costs writers) race the close.
	const traces = 10
	envs := make([]*TraceEnvelope, traces)
	for i := 0; i < traces; i++ {
		proposalID, _, runID := insertChain(t, s, 100+i*10, uid(1), i)
		env := testTrace(t, uid(1), runID, &proposalID)
		env.TickNumber = i
		env.StartedAt = mustTime(t, "2026-06-15T12:00:00Z")
		env.ModelCosts = []TraceModelCost{
			{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
			{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.002")},
		}
		envs[i] = env
	}
	var wg sync.WaitGroup
	errCh := make(chan error, traces)
	for _, env := range envs {
		wg.Add(1)
		go func(env *TraceEnvelope) {
			defer wg.Done()
			_, err := s.InsertTrace(env, testNow)
			errCh <- err
		}(env)
	}
	juneInvoice, _ := mustClose(t, s, "tenant-1", "2026-06")
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent InsertTrace: %v", err)
		}
	}
	julyInvoice, _ := mustClose(t, s, "tenant-1", "2026-07")

	perTrace := decimal.RequireFromString("0.003")
	juneTotal := decimal.RequireFromString(juneInvoice.TotalUSD)
	julyTotal := decimal.RequireFromString(julyInvoice.TotalUSD)
	if !juneTotal.Mod(perTrace).IsZero() || !julyTotal.Mod(perTrace).IsZero() {
		t.Fatalf("totals %q + %q: a trace was split across the close boundary", juneInvoice.TotalUSD, julyInvoice.TotalUSD)
	}
	if want := perTrace.Mul(decimal.NewFromInt(traces)); !juneTotal.Add(julyTotal).Equal(want) {
		t.Fatalf("totals %q + %q != %q: rows dropped or double-billed", juneInvoice.TotalUSD, julyInvoice.TotalUSD, want)
	}
}

// TestCloseDeterminism: the same DB state closes to a byte-identical
// invoice id, line order, and totals (two independent stores compared).
func TestCloseDeterminism(t *testing.T) {
	build := func() (Invoice, []InvoiceLine) {
		s := openStore(t)
		seedTenant(t, s, "tenant-1")
		createStrategy(t, s, uid(1))
		createStrategy(t, s, uid(2))
		ingestTrace(t, s, uid(2), 10, 0, "2026-06-15T12:00:00Z", []TraceModelCost{
			{Node: "trader", Model: "model-b", InputTokens: 10, OutputTokens: 2, CostUSD: mustDec(t, "0.002")},
			{Node: "news_analyst", Model: "model-a", InputTokens: 30, OutputTokens: 6, CostUSD: mustDec(t, "0.004")},
		})
		ingestTrace(t, s, uid(1), 20, 0, "2026-05-20T12:00:00Z", []TraceModelCost{
			{Node: "trader", Model: "model-a", InputTokens: 20, OutputTokens: 4, CostUSD: mustDec(t, "0.003")},
		})
		invoice, lines := mustClose(t, s, "tenant-1", "2026-06")
		return invoice, lines
	}
	i1, l1 := build()
	i2, l2 := build()
	if !reflect.DeepEqual(i1, i2) || !reflect.DeepEqual(l1, l2) {
		t.Fatalf("close is not deterministic:\n%+v\n%+v\n%+v\n%+v", i1, i2, l1, l2)
	}
	// usage lines (strategy uid(2)) precede the carry_over line, models
	// ascending within the strategy.
	if len(l1) != 3 || l1[0].Model != "model-a" || l1[1].Model != "model-b" ||
		l1[2].EntryType != EntryTypeCarryOver || l1[2].StrategyID != uid(1) {
		t.Fatalf("line order = %+v", l1)
	}
}

// TestLedgerMatchesModelCosts: ledger-vs-model_costs drift is impossible by
// same-transaction construction — the ledger sums equal the row sums.
func TestLedgerMatchesModelCosts(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))
	for i := 0; i < 3; i++ {
		ingestTrace(t, s, uid(1), 10+i*10, i, "2026-06-15T12:00:00Z", []TraceModelCost{
			{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
			{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.0005")},
		})
	}
	var ledgerTokens int
	var ledgerCost string
	if err := s.db.QueryRow(`SELECT tokens_used, cost_usd_used FROM token_budget_ledger
		WHERE strategy_id = ? AND utc_date = '2026-06-15'`, uid(1)).Scan(&ledgerTokens, &ledgerCost); err != nil {
		t.Fatalf("ledger row: %v", err)
	}
	rows, err := s.db.Query(`SELECT input_tokens, output_tokens, cost_usd FROM model_costs WHERE strategy_id = ?`, uid(1))
	if err != nil {
		t.Fatalf("model_costs: %v", err)
	}
	defer rows.Close()
	rowTokens, rowCost := 0, decimal.Zero
	for rows.Next() {
		var in, out int
		var cost string
		if err := rows.Scan(&in, &out, &cost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rowTokens += in + out
		rowCost = rowCost.Add(decimal.RequireFromString(cost))
	}
	if ledgerTokens != rowTokens || !decimal.RequireFromString(ledgerCost).Equal(rowCost) {
		t.Fatalf("ledger (%d, %s) != model_costs (%d, %s)", ledgerTokens, ledgerCost, rowTokens, rowCost)
	}
}

// TestBillingDecimalRoundTrip: every computed money output (invoice
// amounts/totals, reconciliation sums) is a normalized ADR-0003 decimal
// string that round-trips exactly; stored metering inputs stay verbatim.
func TestBillingDecimalRoundTrip(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))
	reqID := uid(50)
	ingestTrace(t, s, uid(1), 10, 0, "2026-06-15T12:00:00Z", []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
			CostUSD: mustDec(t, "0.0005"), RequestID: &reqID},
		{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.0005")},
	})
	// The imported cost keeps its ORIGINAL string verbatim (trailing zeros
	// preserved: stored evidence, never normalized).
	if _, _, err := s.InsertMeteringRecords("export-1", []MeteringRecord{{
		RequestID: reqID, StrategyID: uid(1), Model: "stub",
		InputTokens: 100, OutputTokens: 20, CostUSD: "0.000500", MeteredAt: "2026-06-15T12:00:05Z",
	}}, testNow); err != nil {
		t.Fatalf("InsertMeteringRecords: %v", err)
	}
	var storedCost string
	if err := s.db.QueryRow(`SELECT cost_usd FROM metering_records WHERE request_id = ?`, reqID).Scan(&storedCost); err != nil {
		t.Fatalf("metering read-back: %v", err)
	}
	if storedCost != "0.000500" {
		t.Fatalf("metering cost_usd = %q, want the verbatim input \"0.000500\"", storedCost)
	}

	invoice, lines := mustClose(t, s, "tenant-1", "2026-06")
	run, _, err := s.Reconcile("tenant-1", "2026-06", testNow)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	outputs := []string{invoice.TotalUSD, lines[0].AmountUSD, run.MatchedClientCostUSD,
		run.OrphanClientCostUSD, run.EstimatedClientCostUSD, run.UnattributedClientCostUSD, run.InvoiceTotalUSD}
	for _, v := range outputs {
		d, err := contract.ParseDecimal(v)
		if err != nil {
			t.Fatalf("output %q is not an ADR-0003 decimal: %v", v, err)
		}
		if d.Decimal().String() != v {
			t.Fatalf("output %q is not shopspring-normalized (round-trips to %q)", v, d.Decimal().String())
		}
	}
	// 0.0005 + 0.0005 sums exactly, normalized: "0.001".
	if invoice.TotalUSD != "0.001" {
		t.Fatalf("total = %q, want \"0.001\"", invoice.TotalUSD)
	}
}

// TestInsertMeteringRecordsIdempotentAndConflict: identical re-imports
// no-op per record (cost by DECIMAL value), a disagreeing record rejects
// the WHOLE batch atomically.
func TestInsertMeteringRecordsIdempotentAndConflict(t *testing.T) {
	s := openStore(t)
	seedTenant(t, s, "tenant-1")
	createStrategy(t, s, uid(1))
	base := MeteringRecord{
		RequestID: uid(50), StrategyID: uid(1), Model: "stub",
		InputTokens: 100, OutputTokens: 20, CostUSD: "0.50", MeteredAt: "2026-06-15T12:00:05Z",
	}
	if imported, skipped, err := s.InsertMeteringRecords("export-1", []MeteringRecord{base}, testNow); err != nil || imported != 1 || skipped != 0 {
		t.Fatalf("first import = (%d, %d, %v)", imported, skipped, err)
	}
	// Re-import with the cost differing ONLY in trailing zeros: no-op.
	dup := base
	dup.CostUSD = "0.5000"
	if imported, skipped, err := s.InsertMeteringRecords("export-2", []MeteringRecord{dup}, testNow); err != nil || imported != 0 || skipped != 1 {
		t.Fatalf("idempotent re-import = (%d, %d, %v)", imported, skipped, err)
	}
	// A disagreeing record rejects the whole batch: the fresh record must
	// NOT persist.
	conflicting := base
	conflicting.OutputTokens = 21
	fresh := base
	fresh.RequestID = uid(51)
	if _, _, err := s.InsertMeteringRecords("export-3", []MeteringRecord{fresh, conflicting}, testNow); !errors.Is(err, ErrMeteringConflict) {
		t.Fatalf("conflict err = %v, want ErrMeteringConflict", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM metering_records`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("metering_records count = %d err=%v, want 1 (atomic batch)", n, err)
	}
}
