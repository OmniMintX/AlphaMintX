package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func tracePath(strategyID string) string {
	return "/api/v1/strategies/" + strategyID + "/traces"
}

func TestTraceIngestion(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	proposalID, _, runID := insertChain(t, e.store, 10, strat1, 0)
	env := testTraceEnvelope(t, strat1, runID, &proposalID)

	// Fresh ingest answers 200 (not 201): the agent client treats exactly
	// 200 as success, matching the proposals envelope precedent.
	rec := e.do(t, "POST", tracePath(strat1), agent1Tok, env)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["run_id"] != runID {
		t.Fatalf("body = %v", body)
	}

	// Duplicate with the same payload: idempotent no-op 200.
	rec = e.do(t, "POST", tracePath(strat1), agent1Tok, env)
	if rec.Code != http.StatusOK {
		t.Fatalf("duplicate status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// Same run_id, different payload: 409 IDEMPOTENCY_CONFLICT.
	conflicting := testTraceEnvelope(t, strat1, runID, &proposalID)
	conflicting.DebateSummary = "a different summary"
	wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, conflicting), 409, codeIdempotencyConflict)
}

func TestTraceScopeAndRunChecks(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	createStrategy(t, e.store, strat2, "paper")
	proposal1, _, _ := insertChain(t, e.store, 10, strat1, 0)
	_, _, run2 := insertChain(t, e.store, 20, strat2, 0)

	// Body strategy_id outside the token/path scope.
	env := testTraceEnvelope(t, strat2, run2, nil)
	wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, env), 403, codeStrategyScopeMismatch)

	// A run owned by another strategy is indistinguishable from unknown.
	env = testTraceEnvelope(t, strat1, run2, &proposal1)
	wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, env), 404, codeUnknownRun)

	env = testTraceEnvelope(t, strat1, uid(90), &proposal1)
	wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, env), 404, codeUnknownRun)
}

func TestTraceValidation(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	proposalID, _, runID := insertChain(t, e.store, 10, strat1, 0)

	badProposal := "not-a-uuid"
	cases := []struct {
		name   string
		mutate func(env *store.TraceEnvelope)
	}{
		{"schema version", func(env *store.TraceEnvelope) { env.SchemaVersion = "2.0" }},
		{"run id", func(env *store.TraceEnvelope) { env.RunID = "not-a-uuid" }},
		{"tick number", func(env *store.TraceEnvelope) { env.TickNumber = -1 }},
		{"analyst signal", func(env *store.TraceEnvelope) { env.AnalystSummaries.News.Signal = "sideways" }},
		{"debate score", func(env *store.TraceEnvelope) { env.DebateRounds[0].BullScore = 1.5 }},
		{"debate summary", func(env *store.TraceEnvelope) { env.DebateSummary = strings.Repeat("x", 4001) }},
		{"proposal id", func(env *store.TraceEnvelope) { env.ProposalID = &badProposal }},
		{"model costs", func(env *store.TraceEnvelope) {
			env.ModelCosts = make([]contract.ModelCost, 33)
		}},
		{"budget date", func(env *store.TraceEnvelope) { env.BudgetState.UTCDate = "20260704" }},
		{"transcripts size", func(env *store.TraceEnvelope) {
			env.Transcripts = map[string]string{"trader": strings.Repeat("b", maxTranscriptBytes+1)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testTraceEnvelope(t, strat1, runID, &proposalID)
			tc.mutate(env)
			wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, env), 400, codeSchemaInvalid)
		})
	}

	t.Run("unknown field", func(t *testing.T) {
		raw, err := json.Marshal(testTraceEnvelope(t, strat1, runID, &proposalID))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		m["unexpected"] = json.RawMessage(`true`)
		withExtra, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, withExtra), 400, codeSchemaInvalid)
	})
}

func TestTraceBodyTooLarge(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	proposalID, _, runID := insertChain(t, e.store, 10, strat1, 0)

	env := testTraceEnvelope(t, strat1, runID, &proposalID)
	env.Transcripts = map[string]string{"trader": strings.Repeat("a", maxBodyBytes)}
	wantError(t, e.do(t, "POST", tracePath(strat1), agent1Tok, env), 413, codeBodyTooLarge)
}
