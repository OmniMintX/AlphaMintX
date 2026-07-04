package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CreatePendingApproval inserts the restart-safe L1 timer row for a verdict:
// deadline_at = createdAt + timeoutSeconds (default 600, risk-limits.md).
// Idempotent: a pending row that already exists is left untouched (a pending
// item is superseded by its approvals row, never mutated).
func (s *Store) CreatePendingApproval(verdictID, strategyID string, createdAt time.Time, timeoutSeconds int) error {
	deadline := createdAt.Add(time.Duration(timeoutSeconds) * time.Second)
	_, err := s.db.Exec(`INSERT INTO pending_approvals (verdict_id, strategy_id, created_at, deadline_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (verdict_id) DO NOTHING`,
		verdictID, strategyID, formatTime(createdAt), formatTime(deadline))
	return err
}

// ResolveApproval records the single decision for a verdict through one
// INSERT-or-conflict transaction (UNIQUE verdict_id): the first decision —
// human POST or timer expiry — wins, ever. If a decision is already
// recorded, the stored Approval is returned with inserted=false so the
// caller can answer 409 with the recorded outcome. A verdict that is not
// pending approval (and has no recorded decision) returns ErrNotPending.
func (s *Store) ResolveApproval(a Approval) (Approval, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Approval{}, false, err
	}
	defer rollback(tx)

	if existing, ok, err := scanApproval(tx.QueryRow(
		approvalSelect+` WHERE verdict_id = ?`, a.VerdictID)); err != nil {
		return Approval{}, false, err
	} else if ok {
		return existing, false, nil
	}

	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pending_approvals WHERE verdict_id = ?`,
		a.VerdictID).Scan(&pending); err != nil {
		return Approval{}, false, err
	}
	if pending == 0 {
		return Approval{}, false, fmt.Errorf("verdict %s: %w", a.VerdictID, ErrNotPending)
	}

	var reasons any
	if a.Outcome == OutcomeApprovedButBlocked {
		b, err := json.Marshal(a.PreflightReasons)
		if err != nil {
			return Approval{}, false, err
		}
		reasons = string(b)
	}
	if _, err := tx.Exec(`INSERT INTO approvals
		(approval_id, verdict_id, proposal_id, outcome, preflight_reasons, decided_by, decided_at, timeout_seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ApprovalID, a.VerdictID, a.ProposalID, a.Outcome, reasons,
		a.DecidedBy, a.DecidedAt, a.TimeoutSeconds); err != nil {
		return Approval{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Approval{}, false, err
	}
	return a, true, nil
}

// ExpirePendingApprovals is the startup/periodic sweep: every pending item
// (a pending_approvals row with no approvals row) whose deadline_at <= now
// is resolved as outcome=timeout, decided_by="timeout" — restart-safe
// default-deny. Returns the timeout decisions recorded by this sweep; a
// concurrent human decision still wins (first decision per verdict, ever).
func (s *Store) ExpirePendingApprovals(now time.Time) ([]Approval, error) {
	rows, err := s.db.Query(`SELECT p.verdict_id, v.proposal_id, p.created_at, p.deadline_at
		FROM pending_approvals p
		JOIN verdicts v ON v.verdict_id = p.verdict_id
		LEFT JOIN approvals a ON a.verdict_id = p.verdict_id
		WHERE a.verdict_id IS NULL
		ORDER BY p.deadline_at, p.verdict_id`)
	if err != nil {
		return nil, err
	}
	type pendingItem struct {
		verdictID, proposalID, createdAt, deadlineAt string
	}
	var candidates []pendingItem
	for rows.Next() {
		var it pendingItem
		if err := rows.Scan(&it.verdictID, &it.proposalID, &it.createdAt, &it.deadlineAt); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var expired []Approval
	for _, it := range candidates {
		deadline, err := time.Parse(time.RFC3339Nano, it.deadlineAt)
		if err != nil {
			return expired, fmt.Errorf("pending %s deadline_at %q: %w", it.verdictID, it.deadlineAt, err)
		}
		if deadline.After(now) {
			continue
		}
		created, err := time.Parse(time.RFC3339Nano, it.createdAt)
		if err != nil {
			return expired, fmt.Errorf("pending %s created_at %q: %w", it.verdictID, it.createdAt, err)
		}
		recorded, inserted, err := s.ResolveApproval(Approval{
			ApprovalID:     uuid.NewString(),
			VerdictID:      it.verdictID,
			ProposalID:     it.proposalID,
			Outcome:        OutcomeTimeout,
			DecidedBy:      TimeoutDecider,
			DecidedAt:      formatTime(now),
			TimeoutSeconds: int(deadline.Sub(created) / time.Second),
		})
		if err != nil {
			return expired, err
		}
		if inserted {
			expired = append(expired, recorded)
		}
	}
	return expired, nil
}

const approvalSelect = `SELECT approval_id, verdict_id, proposal_id, outcome,
	preflight_reasons, decided_by, decided_at, timeout_seconds FROM approvals`

type rowScanner interface{ Scan(dest ...any) error }

// scanApproval scans one approvals row; ok=false on sql.ErrNoRows.
func scanApproval(row rowScanner) (Approval, bool, error) {
	var a Approval
	var reasons sql.NullString
	err := row.Scan(&a.ApprovalID, &a.VerdictID, &a.ProposalID, &a.Outcome,
		&reasons, &a.DecidedBy, &a.DecidedAt, &a.TimeoutSeconds)
	if err == sql.ErrNoRows {
		return Approval{}, false, nil
	}
	if err != nil {
		return Approval{}, false, err
	}
	if reasons.Valid {
		if err := json.Unmarshal([]byte(reasons.String), &a.PreflightReasons); err != nil {
			return Approval{}, false, fmt.Errorf("approval %s preflight_reasons: %w", a.ApprovalID, err)
		}
	}
	return a, true, nil
}
