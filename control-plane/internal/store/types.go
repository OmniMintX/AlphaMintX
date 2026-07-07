package store

import (
	"encoding/json"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// Timestamps in row structs are pre-formatted RFC 3339 UTC "Z" strings and
// money/size fields are decimal strings (ADR-0003), exactly as stored.

// Strategy mirrors the strategies table. RoleModels is the per-pipeline-role
// model override map, stored in the role_models TEXT column as raw JSON
// (” ⇒ nil map, rendered by omitempty as an absent field).
type Strategy struct {
	StrategyID     string            `json:"strategy_id"`
	TenantID       string            `json:"tenant_id"`
	Name           string            `json:"name"`
	LifecycleState string            `json:"lifecycle_state"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	RoleModels     map[string]string `json:"role_models,omitempty"`
}

// marshalRoleModels renders the role_models column value: an empty map
// stores ” (legacy rows and no-override rows are indistinguishable).
func marshalRoleModels(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// unmarshalRoleModels parses the role_models column value: ” ⇒ nil map.
func unmarshalRoleModels(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
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
	TakeProfit  *string `json:"take_profit"` // TP obligation carried on a resting entry
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
// RealizedPnLQuote is the cumulative realized PnL net of ALL fees; it
// survives a flat book so restarts restore the paper OMS books verbatim.
type Position struct {
	StrategyID       string `json:"strategy_id"`
	Symbol           string `json:"symbol"`
	QtyBase          string `json:"qty_base"`
	EntryPrice       string `json:"entry_price"`
	FeesQuote        string `json:"fees_quote"`
	RealizedPnLQuote string `json:"realized_pnl_quote"`
	UpdatedAt        string `json:"updated_at"`
}

// StrategyState mirrors the mutable strategy_state snapshot: the
// realized-only equity figures the Risk Gate hydrator folds unrealized PnL
// into (risk-limits.md Definitions). DailyRealizedPnLQuote belongs to
// UTCDate; a write on a later UTC day rolls it over to zero first.
type StrategyState struct {
	StrategyID            string `json:"strategy_id"`
	EquityQuote           string `json:"equity_quote"`
	PeakEquityQuote       string `json:"peak_equity_quote"`
	DailyRealizedPnLQuote string `json:"daily_realized_pnl_quote"`
	UTCDate               string `json:"utc_date"`
	UpdatedAt             string `json:"updated_at"`
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
// TenantID is set on tenant-scope kills only (multi-tenant-rbac.md); Phase 1
// rows read it NULL (global or strategy scope, exactly as before).
type KillBreakerEvent struct {
	EventID    string  `json:"event_id"`
	Kind       string  `json:"kind"` // "kill" or "breaker"
	Scope      string  `json:"scope"`
	StrategyID *string `json:"strategy_id"`
	TenantID   *string `json:"tenant_id"`
	KillEpoch  *int64  `json:"kill_epoch"`
	Flatten    *bool   `json:"flatten"`
	TriggerRef *string `json:"trigger_ref"`
	ActorID    string  `json:"actor_id"`
	RecordedAt string  `json:"recorded_at"`
}

// SafetyAlert mirrors the append-only safety_alerts table
// (safety-wiring.md §Alerts): kind is an OPEN set (no CHECK); StrategyID
// and RefID are the nullable dedupe keys.
type SafetyAlert struct {
	AlertID     string  `json:"alert_id"`
	Kind        string  `json:"kind"`
	StrategyID  *string `json:"strategy_id"`
	RefID       *string `json:"ref_id"`
	DetailsJSON string  `json:"details_json"`
	RecordedAt  string  `json:"recorded_at"`
}

// Tenant mirrors the tenants table (multi-tenant-rbac.md §Tables).
type Tenant struct {
	TenantID  string `json:"tenant_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// APIToken mirrors the api_tokens table MINUS token_hash: the hash is
// write-only after mint (no-read-back invariant) and never crosses the
// store boundary on reads. Role is set iff Principal == "user"; StrategyID
// iff Principal == "agent" (table CHECK).
type APIToken struct {
	TokenID    string  `json:"token_id"`
	TenantID   string  `json:"tenant_id"`
	Principal  string  `json:"principal"` // "user" or "agent"
	Role       *string `json:"role"`
	StrategyID *string `json:"strategy_id"`
	Label      string  `json:"label"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
	RevokedAt  *string `json:"revoked_at"`
}

// User mirrors the users table MINUS password_hash: the bcrypt hash is
// write-only after save (no-read-back invariant) and never crosses the
// store boundary on reads — UserByEmail hands it to the login comparison
// only. TenantID is NULL iff Role == "platform_admin" (table CHECK).
type User struct {
	UserID     string  `json:"user_id"`
	TenantID   *string `json:"tenant_id"`
	Email      string  `json:"email"`
	Role       string  `json:"role"`
	CreatedAt  string  `json:"created_at"`
	DisabledAt *string `json:"disabled_at"`
}

// WebSession mirrors the web_sessions table MINUS token_hash (no-read-back
// invariant): a mutable snapshot whose ONLY legal mutation sets revoked_at
// once (multi-tenant-rbac.md §Password auth).
type WebSession struct {
	SessionID string  `json:"session_id"`
	UserID    string  `json:"user_id"`
	CreatedAt string  `json:"created_at"`
	ExpiresAt string  `json:"expires_at"`
	RevokedAt *string `json:"revoked_at"`
}

// RiskLimitChange mirrors the append-only risk_limit_changes table: one row
// per changed field (old -> new, actor, timestamp), replayed rowid-ascending
// into the effective-limits overlay (multi-tenant-rbac.md §Runtime limit
// changes).
type RiskLimitChange struct {
	ChangeID   string  `json:"change_id"`
	StrategyID string  `json:"strategy_id"`
	Field      string  `json:"field"`
	OldValue   *string `json:"old_value"`
	NewValue   string  `json:"new_value"`
	ActorID    string  `json:"actor_id"`
	ChangedAt  string  `json:"changed_at"`
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
