package store

import (
	"encoding/json"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// Timestamps in row structs are pre-formatted RFC 3339 UTC "Z" strings and
// money/size fields are decimal strings (ADR-0003), exactly as stored.

// Strategy mirrors the strategies table.
type Strategy struct {
	StrategyID     string `json:"strategy_id"`
	TenantID       string `json:"tenant_id"`
	Name           string `json:"name"`
	LifecycleState string `json:"lifecycle_state"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// LifecycleTransition mirrors the append-only lifecycle_transitions table.
type LifecycleTransition struct {
	TransitionID string `json:"transition_id"`
	StrategyID   string `json:"strategy_id"`
	FromState    string `json:"from_state"`
	ToState      string `json:"to_state"`
	ActorID      string `json:"actor_id"`
	ActorRole    string `json:"actor_role"`
	Reason       string `json:"reason"`
	RecordedAt   string `json:"recorded_at"`
}

// Run mirrors the runs table.
type Run struct {
	RunID       string  `json:"run_id"`
	StrategyID  string  `json:"strategy_id"`
	TickNumber  int     `json:"tick_number"`
	CreatedAt   string  `json:"created_at"`
	CompletedAt *string `json:"completed_at"`
}

// ProposalSubmission is the transport envelope for proposal ingest: the
// wrapper (NOT the TradeProposal contract) carries tick_number so the run
// row can be created at proposal ingest (persistence-and-api.md Row rules).
type ProposalSubmission struct {
	TickNumber int                `json:"tick_number"`
	Proposal   *contract.Proposal `json:"proposal"`
}

// Approval outcomes (approvals.outcome CHECK constraint).
const (
	OutcomeApproved           = "approved"
	OutcomeApprovedButBlocked = "approved_but_blocked"
	OutcomeRejected           = "rejected"
	OutcomeTimeout            = "timeout"
)

// TimeoutDecider is the decided_by principal recorded by timer expiry.
const TimeoutDecider = "timeout"

// Approval mirrors the append-only approvals table (ApprovalDecision record).
type Approval struct {
	ApprovalID       string   `json:"approval_id"`
	VerdictID        string   `json:"verdict_id"`
	ProposalID       string   `json:"proposal_id"`
	Outcome          string   `json:"outcome"`
	PreflightReasons []string `json:"preflight_reasons,omitempty"` // non-nil iff approved_but_blocked
	DecidedBy        string   `json:"decided_by"`
	DecidedAt        string   `json:"decided_at"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
}

// Order mirrors the orders table.
type Order struct {
	OrderID     string  `json:"order_id"`
	ProposalID  *string `json:"proposal_id"` // NOT NULL iff origin == "proposal"
	Origin      string  `json:"origin"`
	StrategyID  string  `json:"strategy_id"`
	Symbol      string  `json:"symbol"`
	Class       string  `json:"class"`
	Side        string  `json:"side"`
	Type        string  `json:"type"`
	ReduceOnly  bool    `json:"reduce_only"`
	QtyBase     string  `json:"qty_base"`
	LimitPrice  *string `json:"limit_price"`
	StopPrice   *string `json:"stop_price"`
	FillPrice   *string `json:"fill_price"`
	KillEpoch   int64   `json:"kill_epoch"`
	Status      string  `json:"status"`
	SubmittedAt string  `json:"submitted_at"`
	FilledAt    *string `json:"filled_at"`
}

// Fill mirrors the append-only fills table.
type Fill struct {
	FillID    string `json:"fill_id"`
	OrderID   string `json:"order_id"`
	QtyBase   string `json:"qty_base"`
	FillPrice string `json:"fill_price"`
	FeeQuote  string `json:"fee_quote"`
	FillTS    string `json:"fill_ts"`
}

// Position mirrors the mutable positions snapshot. EntryPrice is
// fee-EXCLUSIVE; fees accumulate only in FeesQuote (Row rules).
type Position struct {
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
	QtyBase    string `json:"qty_base"`
	EntryPrice string `json:"entry_price"`
	FeesQuote  string `json:"fees_quote"`
	UpdatedAt  string `json:"updated_at"`
}

// RejectedSubmission mirrors the append-only rejected_submissions table
// (malformed submissions that never earned a verdict).
type RejectedSubmission struct {
	RejectionID string  `json:"rejection_id"`
	StrategyID  *string `json:"strategy_id"`
	ReceivedAt  string  `json:"received_at"`
	Reason      string  `json:"reason"`
	PayloadJSON string  `json:"payload_json"`
}

// KillBreakerEvent mirrors the append-only kill_breaker_events table: the
// persisted kill/breaker intent, written BEFORE any side effect executes.
type KillBreakerEvent struct {
	EventID    string  `json:"event_id"`
	Kind       string  `json:"kind"` // "kill" or "breaker"
	Scope      string  `json:"scope"`
	StrategyID *string `json:"strategy_id"`
	KillEpoch  *int64  `json:"kill_epoch"`
	Flatten    *bool   `json:"flatten"`
	TriggerRef *string `json:"trigger_ref"`
	ActorID    string  `json:"actor_id"`
	RecordedAt string  `json:"recorded_at"`
}

// RunDetail embeds everything the run-detail endpoint returns; contract
// payloads are returned verbatim from their payload_json columns.
type RunDetail struct {
	Run       Run             `json:"run"`
	Proposal  json.RawMessage `json:"proposal"`
	Verdict   json.RawMessage `json:"verdict"`
	Trace     json.RawMessage `json:"trace"`
	Orders    []Order         `json:"orders"`
	Fills     []Fill          `json:"fills"`
	Approvals []Approval      `json:"approvals"`
}
