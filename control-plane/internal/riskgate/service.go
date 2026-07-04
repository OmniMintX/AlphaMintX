package riskgate

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// ErrIdempotencyConflict: a duplicate proposal_id arrived with a different
// payload (canonical hash mismatch, proposal-contract.md rule 6). No
// re-evaluation, no second verdict.
var ErrIdempotencyConflict = errors.New(contract.CodeIdempotencyConflict)

type stored struct {
	verdict contract.Verdict
	hash    [sha256.Size]byte
}

// Service wraps the pure Evaluate with the stateful gate rules: step 0b
// idempotency (unique proposal_id, in-memory in Phase 0) and per-strategy
// serialization of evaluations (risk-limits.md "Gate evaluation order").
type Service struct {
	mu         sync.Mutex
	strategyMu map[string]*sync.Mutex
	verdicts   map[string]stored // keyed by proposal_id
}

// NewService returns an empty in-memory gate service.
func NewService() *Service {
	return &Service{
		strategyMu: make(map[string]*sync.Mutex),
		verdicts:   make(map[string]stored),
	}
}

// Evaluate serializes per strategy_id, then applies step 0b: a duplicate
// proposal_id returns the ORIGINAL verdict verbatim with no re-evaluation.
func (s *Service) Evaluate(p *contract.Proposal, limits RiskLimits, state RuntimeState, now time.Time) (contract.Verdict, error) {
	hash, err := hashProposal(p)
	if err != nil {
		return contract.Verdict{}, err
	}

	lock := s.strategyLock(p.StrategyID)
	lock.Lock()
	defer lock.Unlock()

	if v, ok, err := s.lookup(p.ProposalID, hash); err != nil || ok {
		return v, err
	}
	verdict := Evaluate(p, limits, state, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Insert-or-fetch: a concurrent duplicate under a different strategy_id
	// could have committed first; the first persisted verdict wins.
	if st, ok := s.verdicts[p.ProposalID]; ok {
		return st.verdict, nil
	}
	s.verdicts[p.ProposalID] = stored{verdict: verdict, hash: hash}
	return verdict, nil
}

func (s *Service) lookup(proposalID string, hash [sha256.Size]byte) (contract.Verdict, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.verdicts[proposalID]
	if !ok {
		return contract.Verdict{}, false, nil
	}
	if st.hash != hash {
		return contract.Verdict{}, true, ErrIdempotencyConflict
	}
	return st.verdict, true, nil
}

func (s *Service) strategyLock(strategyID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.strategyMu[strategyID]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.strategyMu[strategyID] = m
	return m
}

func hashProposal(p *contract.Proposal) ([sha256.Size]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}
