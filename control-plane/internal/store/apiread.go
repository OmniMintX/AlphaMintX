package store

import (
	"database/sql"
	"fmt"
)

// Read-only helpers backing the HTTP API (persistence-and-api.md §HTTP API
// and §Approval preflight). None of them mutate anything.

// PendingApproval mirrors the pending_approvals table (restart-safe L1
// timer state). Timestamps are RFC 3339 UTC strings, exactly as stored.
type PendingApproval struct {
	VerdictID  string `json:"verdict_id"`
	StrategyID string `json:"strategy_id"`
	CreatedAt  string `json:"created_at"`
	DeadlineAt string `json:"deadline_at"`
}

// GetPendingApproval returns the pending_approvals row for a verdict;
// ok=false when none exists. The row is returned even after a decision
// superseded it (rows are never mutated); callers decide relevance by
// checking for an approvals row.
func (s *Store) GetPendingApproval(verdictID string) (PendingApproval, bool, error) {
	var p PendingApproval
	err := s.db.QueryRow(`SELECT verdict_id, strategy_id, created_at, deadline_at
		FROM pending_approvals WHERE verdict_id = ?`, verdictID).
		Scan(&p.VerdictID, &p.StrategyID, &p.CreatedAt, &p.DeadlineAt)
	if err == sql.ErrNoRows {
		return PendingApproval{}, false, nil
	}
	if err != nil {
		return PendingApproval{}, false, err
	}
	return p, true, nil
}

// VerdictMeta carries the extracted verdict/proposal columns the approval
// endpoint needs for routing and preflight (indexing/filtering only, per the
// Payload rule — never a contract reconstruction).
type VerdictMeta struct {
	VerdictID   string
	ProposalID  string
	StrategyID  string
	Symbol      string
	Decision    string
	EvaluatedAt string
}

// GetVerdictMeta resolves verdict -> proposal -> strategy for one verdict,
// or ErrNotFound.
func (s *Store) GetVerdictMeta(verdictID string) (VerdictMeta, error) {
	var m VerdictMeta
	err := s.db.QueryRow(`SELECT v.verdict_id, v.proposal_id, p.strategy_id, p.symbol, v.decision, v.evaluated_at
		FROM verdicts v JOIN proposals p ON p.proposal_id = v.proposal_id
		WHERE v.verdict_id = ?`, verdictID).
		Scan(&m.VerdictID, &m.ProposalID, &m.StrategyID, &m.Symbol, &m.Decision, &m.EvaluatedAt)
	if err == sql.ErrNoRows {
		return VerdictMeta{}, fmt.Errorf("verdict %s: %w", verdictID, ErrNotFound)
	}
	return m, err
}

// MaxKillEpoch returns the highest persisted kill epoch affecting a strategy
// (global scope or the strategy's own) recorded strictly after the RFC 3339
// UTC cutoff; 0 when none. The approval preflight uses it to detect a
// kill-epoch change since the verdict was evaluated.
func (s *Store) MaxKillEpoch(strategyID, afterRFC3339 string) (int64, error) {
	var epoch int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(kill_epoch), 0) FROM kill_breaker_events
		WHERE kind = 'kill' AND (strategy_id IS NULL OR strategy_id = ?) AND recorded_at > ?`,
		strategyID, afterRFC3339).Scan(&epoch)
	return epoch, err
}

// RunStrategy returns the owning strategy_id of a run, or ErrNotFound.
func (s *Store) RunStrategy(runID string) (string, error) {
	var strategyID string
	err := s.db.QueryRow(`SELECT strategy_id FROM runs WHERE run_id = ?`, runID).Scan(&strategyID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	return strategyID, err
}
