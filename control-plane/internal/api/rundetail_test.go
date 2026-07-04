package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func TestRunDetail(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "live_l1")
	proposalID, verdictID, runID := insertChain(t, e.store, 10, strat1, 0)
	if err := e.store.CreatePendingApproval(verdictID, strat1, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	if _, err := e.store.InsertTrace(testTraceEnvelope(t, strat1, runID, &proposalID), testNow); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs/"+uid(9), readTok, nil), 404, codeUnknownRun)
	// A run under a different strategy id is indistinguishable from unknown.
	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+uid(9)+"/runs/"+runID, readTok, nil), 404, codeUnknownRun)

	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs/"+runID, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	decodeJSON(t, rec, &m)
	wantKeys := []string{"approvals", "fills", "orders", "pending_approval", "proposal", "run", "trace", "verdict"}
	if got := sortedKeys(m); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}

	// Arrays are never null.
	for _, k := range []string{"orders", "fills", "approvals"} {
		if string(m[k]) != "[]" {
			t.Fatalf("%s = %s, want []", k, m[k])
		}
	}

	// The embedded trace has schema_version stripped, other fields verbatim.
	var trace map[string]json.RawMessage
	if err := json.Unmarshal(m["trace"], &trace); err != nil {
		t.Fatalf("trace: %v", err)
	}
	if _, ok := trace["schema_version"]; ok {
		t.Fatal("trace still carries schema_version")
	}
	if string(trace["run_id"]) != `"`+runID+`"` {
		t.Fatalf("trace run_id = %s", trace["run_id"])
	}

	// Proposal and verdict payloads are returned verbatim (incl. version).
	var proposal map[string]json.RawMessage
	if err := json.Unmarshal(m["proposal"], &proposal); err != nil {
		t.Fatalf("proposal: %v", err)
	}
	if string(proposal["schema_version"]) != `"1.0"` || string(proposal["proposal_id"]) != `"`+proposalID+`"` {
		t.Fatalf("proposal = %s", m["proposal"])
	}

	var pending store.PendingApproval
	if err := json.Unmarshal(m["pending_approval"], &pending); err != nil {
		t.Fatalf("pending_approval: %v", err)
	}
	if pending.VerdictID != verdictID || pending.DeadlineAt != "2026-07-04T12:40:00Z" {
		t.Fatalf("pending_approval = %+v", pending)
	}

	// The trace set the run's completed_at.
	var run store.Run
	if err := json.Unmarshal(m["run"], &run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.CompletedAt == nil || *run.CompletedAt != "2026-07-04T12:00:09Z" {
		t.Fatalf("run = %+v", run)
	}
}

func TestRunDetailNoVerdict(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	runID := uid(42)
	p := testProposal(t, uid(40), strat1, runID)
	if _, err := e.store.InsertProposal(store.ProposalSubmission{TickNumber: 0, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}

	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs/"+runID, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var m map[string]json.RawMessage
	decodeJSON(t, rec, &m)
	for _, k := range []string{"verdict", "trace", "pending_approval"} {
		if string(m[k]) != "null" {
			t.Fatalf("%s = %s, want null", k, m[k])
		}
	}
}

func TestRunDetailPendingSuperseded(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "live_l1")
	_, verdictID, runID := insertChain(t, e.store, 10, strat1, 0)
	if err := e.store.CreatePendingApproval(verdictID, strat1, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	rec := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok,
		approvalRequest{VerdictID: verdictID, Approved: false})
	if rec.Code != http.StatusOK {
		t.Fatalf("approval status = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs/"+runID, readTok, nil)
	var m map[string]json.RawMessage
	decodeJSON(t, rec, &m)
	if string(m["pending_approval"]) != "null" {
		t.Fatalf("pending_approval = %s, want null once decided", m["pending_approval"])
	}
	var approvals []store.Approval
	if err := json.Unmarshal(m["approvals"], &approvals); err != nil {
		t.Fatalf("approvals: %v", err)
	}
	if len(approvals) != 1 || approvals[0].Outcome != store.OutcomeRejected {
		t.Fatalf("approvals = %+v", approvals)
	}
}
