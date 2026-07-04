package store

import (
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
