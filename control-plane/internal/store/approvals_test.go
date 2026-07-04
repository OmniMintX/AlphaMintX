package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func approval(approvalID, verdictID, proposalID, outcome, decidedBy string) Approval {
	return Approval{
		ApprovalID: approvalID, VerdictID: verdictID, ProposalID: proposalID,
		Outcome: outcome, DecidedBy: decidedBy, DecidedAt: formatTime(testNow), TimeoutSeconds: 600,
	}
}

func TestResolveApprovalFirstDecisionWins(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, verdictID, _ := insertChain(t, s, 10, strategyID, 0)
	if err := s.CreatePendingApproval(verdictID, strategyID, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}

	first := approval(uid(30), verdictID, proposalID, OutcomeApproved, "operator-1")
	if recorded, inserted, err := s.ResolveApproval(first); err != nil || !inserted || recorded.Outcome != OutcomeApproved {
		t.Fatalf("first decision: (%+v, %v, %v), want inserted approved", recorded, inserted, err)
	}

	// A second decision — double-click, human-vs-timeout race — returns the
	// recorded outcome, inserted=false (the caller answers 409 with it).
	second := approval(uid(31), verdictID, proposalID, OutcomeTimeout, TimeoutDecider)
	recorded, inserted, err := s.ResolveApproval(second)
	if err != nil || inserted {
		t.Fatalf("second decision: inserted=%v err=%v, want false, nil", inserted, err)
	}
	if recorded.ApprovalID != uid(30) || recorded.Outcome != OutcomeApproved || recorded.DecidedBy != "operator-1" {
		t.Errorf("second decision recorded = %+v, want the first (approved) row", recorded)
	}
}

func TestResolveApprovalNotPending(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, verdictID, _ := insertChain(t, s, 10, strategyID, 0)

	_, _, err := s.ResolveApproval(approval(uid(30), verdictID, proposalID, OutcomeApproved, "operator-1"))
	if !errors.Is(err, ErrNotPending) {
		t.Errorf("verdict without pending row: err = %v, want ErrNotPending", err)
	}
}

func TestResolveApprovalPreflightReasonsRoundTrip(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, verdictID, _ := insertChain(t, s, 10, strategyID, 0)
	if err := s.CreatePendingApproval(verdictID, strategyID, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	blocked := approval(uid(30), verdictID, proposalID, OutcomeApprovedButBlocked, "operator-1")
	blocked.PreflightReasons = []string{"KILL_SWITCH_ACTIVE", "MARK_PRICE_UNAVAILABLE"}
	if _, inserted, err := s.ResolveApproval(blocked); err != nil || !inserted {
		t.Fatalf("resolve blocked: inserted=%v err=%v", inserted, err)
	}
	recorded, _, err := s.ResolveApproval(approval(uid(31), verdictID, proposalID, OutcomeRejected, "operator-2"))
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if len(recorded.PreflightReasons) != 2 || recorded.PreflightReasons[0] != "KILL_SWITCH_ACTIVE" {
		t.Errorf("preflight_reasons = %v, want the recorded pair", recorded.PreflightReasons)
	}
}

// TestResolveApprovalRace: approve and timeout race through the single
// INSERT-or-conflict transaction; exactly one wins and both callers observe
// the same recorded outcome.
func TestResolveApprovalRace(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	proposalID, verdictID, _ := insertChain(t, s, 10, strategyID, 0)
	if err := s.CreatePendingApproval(verdictID, strategyID, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}

	attempts := []Approval{
		approval(uid(30), verdictID, proposalID, OutcomeApproved, "operator-1"),
		approval(uid(31), verdictID, proposalID, OutcomeTimeout, TimeoutDecider),
	}
	recorded := make([]Approval, len(attempts))
	inserted := make([]bool, len(attempts))
	var wg sync.WaitGroup
	for i, a := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			recorded[i], inserted[i], err = s.ResolveApproval(a)
			if err != nil {
				t.Errorf("ResolveApproval %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if inserted[0] == inserted[1] {
		t.Fatalf("inserted = %v, want exactly one winner", inserted)
	}
	if recorded[0].ApprovalID != recorded[1].ApprovalID || recorded[0].Outcome != recorded[1].Outcome {
		t.Errorf("racers observed different decisions: %+v vs %+v", recorded[0], recorded[1])
	}
}

func TestExpirePendingApprovalsSweepsOnlyPastDeadline(t *testing.T) {
	s := openStore(t)
	strategyID := uid(1)
	createStrategy(t, s, strategyID)
	_, pastVerdict, _ := insertChain(t, s, 10, strategyID, 0)
	_, futureVerdict, _ := insertChain(t, s, 20, strategyID, 1)

	// Past: created two hours ago with a 600 s timeout. Future: created now.
	if err := s.CreatePendingApproval(pastVerdict, strategyID, testNow.Add(-2*time.Hour), 600); err != nil {
		t.Fatalf("pending past: %v", err)
	}
	if err := s.CreatePendingApproval(futureVerdict, strategyID, testNow, 600); err != nil {
		t.Fatalf("pending future: %v", err)
	}

	expired, err := s.ExpirePendingApprovals(testNow)
	if err != nil {
		t.Fatalf("ExpirePendingApprovals: %v", err)
	}
	if len(expired) != 1 || expired[0].VerdictID != pastVerdict {
		t.Fatalf("expired = %+v, want only the past-deadline item", expired)
	}
	if expired[0].Outcome != OutcomeTimeout || expired[0].DecidedBy != TimeoutDecider || expired[0].TimeoutSeconds != 600 {
		t.Errorf("timeout record = %+v, want outcome=timeout decided_by=timeout timeout_seconds=600", expired[0])
	}

	// The sweep is idempotent: resolved items are never expired twice.
	if again, err := s.ExpirePendingApprovals(testNow); err != nil || len(again) != 0 {
		t.Errorf("second sweep = (%v, %v), want empty, nil", again, err)
	}
}
