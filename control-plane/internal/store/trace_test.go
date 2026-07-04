package store

import (
	"database/sql"
	"errors"
	"testing"
)

func ledgerRow(t *testing.T, s *Store, strategyID, utcDate string) (tokens int, cost string, exists bool) {
	t.Helper()
	err := s.db.QueryRow(`SELECT tokens_used, cost_usd_used FROM token_budget_ledger
		WHERE strategy_id = ? AND utc_date = ?`, strategyID, utcDate).Scan(&tokens, &cost)
	if err != nil {
		return 0, "", false
	}
	return tokens, cost, true
}

func TestInsertTraceIdempotentWithLedger(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, _, runID := insertChain(t, s, 10, strategyID, 0)

	env := testTrace(t, strategyID, runID, &proposalID)
	if inserted, err := s.InsertTrace(env, testNow); err != nil || !inserted {
		t.Fatalf("InsertTrace: inserted=%v err=%v, want true, nil", inserted, err)
	}

	// Ledger incremented exactly once from the ingested model_costs
	// (100+20 + 50+10 tokens; 0.001 + 0.0005 USD), on started_at's UTC day.
	tokens, cost, ok := ledgerRow(t, s, strategyID, "2026-07-04")
	if !ok || tokens != 180 || cost != "0.0015" {
		t.Fatalf("ledger = (%d, %q, %v), want (180, \"0.0015\", true)", tokens, cost, ok)
	}

	// Retry with the identical payload: no-op, all three writes skipped.
	if inserted, err := s.InsertTrace(env, testNow); err != nil || inserted {
		t.Fatalf("retry: inserted=%v err=%v, want false, nil", inserted, err)
	}
	if tokens, cost, _ = ledgerRow(t, s, strategyID, "2026-07-04"); tokens != 180 || cost != "0.0015" {
		t.Errorf("ledger after retry = (%d, %q), want unchanged (180, \"0.0015\")", tokens, cost)
	}
	var costRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_costs WHERE run_id = ?`, runID).Scan(&costRows); err != nil {
		t.Fatalf("count model_costs: %v", err)
	}
	if costRows != 2 {
		t.Errorf("model_costs rows = %d, want 2 (no double fan-out)", costRows)
	}

	// Same run_id, different payload: 409 IDEMPOTENCY_CONFLICT, nothing written.
	changed := testTrace(t, strategyID, runID, &proposalID)
	changed.DebateSummary = "a different payload for the same run_id"
	if _, err := s.InsertTrace(changed, testNow); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different payload: err = %v, want ErrIdempotencyConflict", err)
	}
	if tokens, cost, _ = ledgerRow(t, s, strategyID, "2026-07-04"); tokens != 180 || cost != "0.0015" {
		t.Errorf("ledger after conflict = (%d, %q), want unchanged", tokens, cost)
	}

	// Trace ingest sets runs.completed_at.
	detail, err := s.GetRunDetail(strategyID, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if detail.Run.CompletedAt == nil || *detail.Run.CompletedAt != "2026-07-04T12:00:09Z" {
		t.Errorf("run completed_at = %v, want 2026-07-04T12:00:09Z", detail.Run.CompletedAt)
	}
}

func TestInsertTraceAccumulatesLedgerAcrossRuns(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalA, _, runA := insertChain(t, s, 10, strategyID, 0)
	proposalB, _, runB := insertChain(t, s, 20, strategyID, 1)

	if _, err := s.InsertTrace(testTrace(t, strategyID, runA, &proposalA), testNow); err != nil {
		t.Fatalf("trace A: %v", err)
	}
	envB := testTrace(t, strategyID, runB, &proposalB)
	envB.TickNumber = 1
	if _, err := s.InsertTrace(envB, testNow); err != nil {
		t.Fatalf("trace B: %v", err)
	}
	// shopspring Decimal.String() normalizes trailing zeros: 0.0015+0.0015 = "0.003".
	if tokens, cost, _ := ledgerRow(t, s, strategyID, "2026-07-04"); tokens != 360 || cost != "0.003" {
		t.Errorf("ledger = (%d, %q), want (360, \"0.003\")", tokens, cost)
	}
}

func TestInsertTraceUnknownRun(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	env := testTrace(t, strategyID, uid(99), nil)
	if _, err := s.InsertTrace(env, testNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: err = %v, want ErrNotFound", err)
	}
}

// TestInsertTraceRequestIDFanOut: the fan-out copies request_id and the
// estimated flag onto model_costs (absent estimated => 0, never inferred
// from estimated_cost_nodes); a request_id UNIQUE conflict stores the LATER
// row with request_id NULL — a join-key defect never drops cost or fails
// the trace (billing-and-metering.md §Ingest fan-out).
func TestInsertTraceRequestIDFanOut(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	proposalA, _, runA := insertChain(t, s, 10, uid(1), 0)
	reqID := uid(50)
	envA := testTrace(t, uid(1), runA, &proposalA)
	envA.ModelCosts = []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
			CostUSD: mustDec(t, "0.001"), RequestID: &reqID},
		{Node: "news_analyst", Model: "stub", InputTokens: 40, OutputTokens: 0,
			CostUSD: mustDec(t, "0.0002"), Estimated: boolPtr(true)},
	}
	envA.EstimatedCostNodes = []string{"news_analyst"}
	if _, err := s.InsertTrace(envA, testNow); err != nil {
		t.Fatalf("InsertTrace A: %v", err)
	}
	row := func(runID, node string) (reqID sql.NullString, estimated bool) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT request_id, is_estimated FROM model_costs
			WHERE run_id = ? AND node = ?`, runID, node).Scan(&reqID, &estimated); err != nil {
			t.Fatalf("model_costs (%s, %s): %v", runID, node, err)
		}
		return reqID, estimated
	}
	if got, estimated := row(runA, "trader"); !got.Valid || got.String != reqID || estimated {
		t.Fatalf("trader row = (%+v, %v), want (request_id %s, is_estimated 0)", got, estimated, reqID)
	}
	if got, estimated := row(runA, "news_analyst"); got.Valid || !estimated {
		t.Fatalf("news_analyst row = (%+v, %v), want (NULL, is_estimated 1)", got, estimated)
	}

	// A second trace squatting the SAME request_id ingests successfully
	// with its row conflict-nulled: cost lands, join key drops.
	proposalB, _, runB := insertChain(t, s, 20, uid(1), 1)
	envB := testTrace(t, uid(1), runB, &proposalB)
	envB.TickNumber = 1
	envB.ModelCosts = []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 7, OutputTokens: 3,
			CostUSD: mustDec(t, "0.003"), RequestID: &reqID},
	}
	if inserted, err := s.InsertTrace(envB, testNow); err != nil || !inserted {
		t.Fatalf("InsertTrace B: inserted=%v err=%v, want true, nil", inserted, err)
	}
	var gotCost string
	var gotReq sql.NullString
	if err := s.db.QueryRow(`SELECT request_id, cost_usd FROM model_costs WHERE run_id = ?`,
		runB).Scan(&gotReq, &gotCost); err != nil {
		t.Fatalf("run B row: %v", err)
	}
	if gotReq.Valid || gotCost != "0.003" {
		t.Fatalf("run B row = (%+v, %q), want (NULL, \"0.003\")", gotReq, gotCost)
	}
	// The victim's attribution is untouched.
	if got, _ := row(runA, "trader"); !got.Valid || got.String != reqID {
		t.Fatalf("victim row = %+v, want request_id %s intact", got, reqID)
	}
}

// TestInsertTraceSameEnvelopeDuplicateRequestID: ONE envelope whose
// model_costs carry the SAME request_id twice. The trace still ingests,
// the first row keeps the id (first-writer-wins), the second is
// conflict-nulled, and both costs land — the ledger takes the FULL sum (a
// join-key defect never drops cost or fails the trace).
func TestInsertTraceSameEnvelopeDuplicateRequestID(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	proposalID, _, runID := insertChain(t, s, 10, uid(1), 0)
	reqID := uid(50)
	env := testTrace(t, uid(1), runID, &proposalID)
	env.ModelCosts = []TraceModelCost{
		{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20,
			CostUSD: mustDec(t, "0.001"), RequestID: &reqID},
		{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10,
			CostUSD: mustDec(t, "0.0005"), RequestID: &reqID},
	}
	if inserted, err := s.InsertTrace(env, testNow); err != nil || !inserted {
		t.Fatalf("InsertTrace: inserted=%v err=%v, want true, nil", inserted, err)
	}
	rows, err := s.db.Query(`SELECT request_id, cost_usd FROM model_costs
		WHERE run_id = ? ORDER BY rowid`, runID)
	if err != nil {
		t.Fatalf("model_costs: %v", err)
	}
	defer rows.Close()
	var reqIDs []sql.NullString
	var costs []string
	for rows.Next() {
		var req sql.NullString
		var cost string
		if err := rows.Scan(&req, &cost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		reqIDs = append(reqIDs, req)
		costs = append(costs, cost)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(reqIDs) != 2 {
		t.Fatalf("model_costs rows = %d, want 2 (duplicate never drops cost)", len(reqIDs))
	}
	if !reqIDs[0].Valid || reqIDs[0].String != reqID || reqIDs[1].Valid {
		t.Fatalf("request_ids = %+v, want (%s, NULL) — first keeps the id, second nulled", reqIDs, reqID)
	}
	if costs[0] != "0.001" || costs[1] != "0.0005" {
		t.Fatalf("costs = %+v, want both intact (\"0.001\", \"0.0005\")", costs)
	}
	// The ledger increments by the FULL sum (100+20 + 50+10; 0.001+0.0005).
	if tokens, cost, ok := ledgerRow(t, s, uid(1), "2026-07-04"); !ok || tokens != 180 || cost != "0.0015" {
		t.Fatalf("ledger = (%d, %q, %v), want (180, \"0.0015\", true)", tokens, cost, ok)
	}
}
