// Package contract holds Go types mirroring contracts/proposal.schema.json
// and contracts/riskverdict.schema.json plus the beyond-schema validation
// rules from docs/specs/proposal-contract.md.
package contract

import (
	"fmt"
	"regexp"
)

// SchemaVersion is the contract version this control-plane supports.
const SchemaVersion = "1.0"

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	symbolPattern = regexp.MustCompile(`^[A-Z0-9]{2,15}/[A-Z0-9]{2,10}$`)
)

// Action is the proposal action enum.
type Action string

const (
	ActionOpenLong  Action = "open_long"
	ActionOpenShort Action = "open_short"
	ActionClose     Action = "close"
	ActionHold      Action = "hold"
)

// IsOpen reports whether the action opens or increases exposure.
func (a Action) IsOpen() bool { return a == ActionOpenLong || a == ActionOpenShort }

// Entry mirrors proposal.schema.json#/properties/entry.
type Entry struct {
	Type       string   `json:"type"`
	LimitPrice *Decimal `json:"limit_price,omitempty"`
}

// AnalystSummary mirrors $defs/analyst_summary.
type AnalystSummary struct {
	Signal     string  `json:"signal"`
	Confidence float64 `json:"confidence"`
	Summary    string  `json:"summary"`
}

// AnalystSummaries requires exactly market, news, fundamental.
type AnalystSummaries struct {
	Market      AnalystSummary `json:"market"`
	News        AnalystSummary `json:"news"`
	Fundamental AnalystSummary `json:"fundamental"`
}

// ModelCost mirrors $defs/model_cost.
type ModelCost struct {
	Node         string  `json:"node"`
	Model        string  `json:"model"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      Decimal `json:"cost_usd"`
}

// Proposal mirrors proposal.schema.json (TradeProposal v1).
type Proposal struct {
	SchemaVersion    string           `json:"schema_version"`
	ProposalID       string           `json:"proposal_id"`
	StrategyID       string           `json:"strategy_id"`
	AgentTraceID     string           `json:"agent_trace_id"`
	CreatedAt        UTCTime          `json:"created_at"`
	Symbol           string           `json:"symbol"`
	Action           Action           `json:"action"`
	SizeQuote        Decimal          `json:"size_quote"`
	Entry            Entry            `json:"entry"`
	StopLoss         *Decimal         `json:"stop_loss,omitempty"`
	TakeProfit       *Decimal         `json:"take_profit,omitempty"`
	TimeInForce      string           `json:"time_in_force"`
	Confidence       float64          `json:"confidence"`
	Reasoning        string           `json:"reasoning"`
	AnalystSummaries AnalystSummaries `json:"analyst_summaries"`
	DebateSummary    string           `json:"debate_summary"`
	ModelCosts       []ModelCost      `json:"model_costs"`
}

// Violation is a single beyond-schema validation failure.
type Violation struct {
	Code    string
	Message string
}

func violation(code, format string, args ...any) Violation {
	return Violation{Code: code, Message: fmt.Sprintf(format, args...)}
}
