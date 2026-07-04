package store

import (
	"bytes"
	"errors"
	"testing"
)

func TestNormalizePage(t *testing.T) {
	tests := []struct{ page, limit, wantPage, wantLimit int }{
		{0, 0, 1, DefaultPageLimit},
		{-3, -1, 1, DefaultPageLimit},
		{2, 50, 2, 50},
		{1, 1000, 1, MaxPageLimit},
	}
	for _, tc := range tests {
		if p, l := normalizePage(tc.page, tc.limit); p != tc.wantPage || l != tc.wantLimit {
			t.Errorf("normalizePage(%d, %d) = (%d, %d), want (%d, %d)",
				tc.page, tc.limit, p, l, tc.wantPage, tc.wantLimit)
		}
	}
}

func TestListRunsPaginatesTickDesc(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	for tick := 0; tick < 3; tick++ {
		insertChain(t, s, 10*(tick+1), strategyID, tick)
	}

	page1, total, err := s.ListRuns(strategyID, 1, 2)
	if err != nil {
		t.Fatalf("ListRuns page 1: %v", err)
	}
	if total != 3 || len(page1) != 2 || page1[0].TickNumber != 2 || page1[1].TickNumber != 1 {
		t.Fatalf("page 1 = %+v total=%d, want ticks [2 1] of 3", page1, total)
	}
	page2, _, err := s.ListRuns(strategyID, 2, 2)
	if err != nil || len(page2) != 1 || page2[0].TickNumber != 0 {
		t.Fatalf("page 2 = %+v err=%v, want tick [0]", page2, err)
	}
	if empty, total, err := s.ListRuns(uid(99), 1, 2); err != nil || total != 0 || len(empty) != 0 {
		t.Errorf("unknown strategy: (%v, %d, %v), want empty page", empty, total, err)
	}
}

func TestGetStrategyNotFound(t *testing.T) {
	s := openStore(t)
	if _, err := s.GetStrategy(uid(99)); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetStrategy unknown: err = %v, want ErrNotFound", err)
	}
}

func TestGetRunDetailFullChain(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, verdictID, runID := insertChain(t, s, 10, strategyID, 0)

	if err := s.CreatePendingApproval(verdictID, strategyID, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	if _, _, err := s.ResolveApproval(approval(uid(30), verdictID, proposalID, OutcomeApproved, "operator-1")); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	limitPrice := "64250.50"
	order := Order{
		OrderID: uid(40), ProposalID: &proposalID, Origin: "proposal", StrategyID: strategyID,
		Symbol: "BTC/USDT", Class: "ENTRY", Side: "buy", Type: "limit", ReduceOnly: false,
		QtyBase: "0.0234", LimitPrice: &limitPrice, KillEpoch: 0, Status: "filled",
		SubmittedAt: formatTime(testNow),
	}
	if err := s.InsertOrder(order); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.InsertFill(Fill{
		FillID: uid(41), OrderID: uid(40), QtyBase: "0.0234", FillPrice: "64250.50",
		FeeQuote: "0.75", FillTS: formatTime(testNow),
	}); err != nil {
		t.Fatalf("InsertFill: %v", err)
	}
	if _, err := s.InsertTrace(testTrace(t, strategyID, runID, &proposalID), testNow); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	d, err := s.GetRunDetail(strategyID, runID)
	if err != nil {
		t.Fatalf("GetRunDetail: %v", err)
	}
	if d.Run.RunID != runID || d.Run.TickNumber != 0 {
		t.Errorf("run = %+v, want run_id %s tick 0", d.Run, runID)
	}
	// Contract payloads come back verbatim from payload_json (canonical bytes).
	wantProposal, _, err := canonicalJSON(testProposal(t, proposalID, strategyID, runID))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if !bytes.Equal(d.Proposal, wantProposal) {
		t.Errorf("proposal payload not verbatim:\n got %s\nwant %s", d.Proposal, wantProposal)
	}
	wantVerdict, _, err := canonicalJSON(testVerdict(t, verdictID, proposalID))
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	if !bytes.Equal(d.Verdict, wantVerdict) {
		t.Errorf("verdict payload not verbatim:\n got %s\nwant %s", d.Verdict, wantVerdict)
	}
	if len(d.Trace) == 0 || !bytes.Contains(d.Trace, []byte(runID)) {
		t.Errorf("trace payload = %s, want stored trace envelope", d.Trace)
	}
	if len(d.Orders) != 1 || d.Orders[0].OrderID != uid(40) ||
		d.Orders[0].ProposalID == nil || *d.Orders[0].ProposalID != proposalID ||
		d.Orders[0].LimitPrice == nil || *d.Orders[0].LimitPrice != limitPrice ||
		d.Orders[0].StopPrice != nil || d.Orders[0].FilledAt != nil {
		t.Errorf("orders = %+v, want the inserted order with nullables intact", d.Orders)
	}
	if len(d.Fills) != 1 || d.Fills[0].FillID != uid(41) || d.Fills[0].FeeQuote != "0.75" {
		t.Errorf("fills = %+v, want the inserted fill", d.Fills)
	}
	if len(d.Approvals) != 1 || d.Approvals[0].Outcome != OutcomeApproved {
		t.Errorf("approvals = %+v, want the approved decision", d.Approvals)
	}
}

func TestGetRunDetailSparseAndNotFound(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID := uid(10)
	runID := uid(12)
	p := testProposal(t, proposalID, strategyID, runID)
	if _, err := s.InsertProposal(ProposalSubmission{TickNumber: 0, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	d, err := s.GetRunDetail(strategyID, runID)
	if err != nil {
		t.Fatalf("GetRunDetail sparse: %v", err)
	}
	if d.Proposal == nil || d.Verdict != nil || d.Trace != nil ||
		len(d.Orders) != 0 || len(d.Fills) != 0 || len(d.Approvals) != 0 {
		t.Errorf("sparse detail = %+v, want proposal only", d)
	}
	if _, err := s.GetRunDetail(uid(99), runID); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong strategy: err = %v, want ErrNotFound", err)
	}
}
