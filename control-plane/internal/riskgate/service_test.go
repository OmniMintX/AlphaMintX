package riskgate

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// Step 0b: a duplicate proposal_id returns the ORIGINAL verdict verbatim —
// no re-evaluation, even if runtime state changed in between.
func TestDuplicateProposalReturnsOriginalVerdict(t *testing.T) {
	svc := NewService()
	p := baseProposal(t)
	limits, state, now := baseLimits(), baseState(), baseNow(t)

	first, err := svc.Evaluate(p, limits, state, now)
	if err != nil {
		t.Fatalf("first evaluate: %v", err)
	}
	if first.Decision != contract.DecisionApprove {
		t.Fatalf("first decision = %s, want approve", first.Decision)
	}

	// State now has an active kill; a re-evaluation would reject. The
	// duplicate must still return the original approve verdict.
	state.KillActive = true
	second, err := svc.Evaluate(p, limits, state, now)
	if err != nil {
		t.Fatalf("duplicate evaluate: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("duplicate verdict differs from original:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if first.VerdictID != second.VerdictID {
		t.Errorf("verdict_id changed on duplicate: %s vs %s", first.VerdictID, second.VerdictID)
	}
}

// Contract rule 6: duplicate proposal_id with a different payload is an
// idempotency conflict, never a re-evaluation.
func TestDuplicateWithDifferentPayloadConflicts(t *testing.T) {
	svc := NewService()
	p := baseProposal(t)
	if _, err := svc.Evaluate(p, baseLimits(), baseState(), baseNow(t)); err != nil {
		t.Fatalf("first evaluate: %v", err)
	}
	altered := baseProposal(t)
	altered.SizeQuote = mustDec(t, "999")
	_, err := svc.Evaluate(altered, baseLimits(), baseState(), baseNow(t))
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
}

// Concurrent duplicate deliveries must produce exactly one verdict
// (exercised under -race; per-strategy evaluations are serialized).
func TestConcurrentDuplicatesOneVerdict(t *testing.T) {
	svc := NewService()
	p := baseProposal(t)
	limits, state, now := baseLimits(), baseState(), baseNow(t)

	const n = 32
	verdicts := make([]contract.Verdict, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := svc.Evaluate(p, limits, state, now)
			if err != nil {
				t.Errorf("evaluate: %v", err)
				return
			}
			verdicts[i] = v
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if verdicts[i].VerdictID != verdicts[0].VerdictID {
			t.Fatalf("got more than one verdict for one proposal_id")
		}
	}
}
