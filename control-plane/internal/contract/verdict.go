package contract

// Decision is the RiskVerdict decision enum.
type Decision string

const (
	DecisionApprove  Decision = "approve"
	DecisionReject   Decision = "reject"
	DecisionClip     Decision = "clip"
	DecisionEscalate Decision = "escalate"
)

// Reason is a machine code plus human message.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// LimitsSnapshot mirrors riskverdict.schema.json#/properties/limits_snapshot:
// the limit values in force at evaluation time PLUS the runtime inputs the
// gate actually evaluated, so verdicts are reproducible.
type LimitsSnapshot struct {
	SymbolWhitelist             []string      `json:"symbol_whitelist"`
	MaxOpenPositions            int           `json:"max_open_positions"`
	PerPositionNotionalCapQuote Decimal       `json:"per_position_notional_cap_quote"`
	DailyLossLimitQuote         Decimal       `json:"daily_loss_limit_quote"`
	MaxDrawdownPct              float64       `json:"max_drawdown_pct"`
	MaxOrdersPerMinute          int           `json:"max_orders_per_minute"`
	RequireStopLoss             bool          `json:"require_stop_loss"`
	L2MaxSizeQuote              *Decimal      `json:"l2_max_size_quote,omitempty"`
	L2AllowedSymbols            []string      `json:"l2_allowed_symbols,omitempty"`
	EquityQuote                 Decimal       `json:"equity_quote"`
	PeakEquityQuote             Decimal       `json:"peak_equity_quote"`
	DailyRealizedPnlQuote       SignedDecimal `json:"daily_realized_pnl_quote"`
	OpenPositionsCount          int           `json:"open_positions_count"`
	PendingEntryOrdersCount     int           `json:"pending_entry_orders_count"`
	MarkPrice                   Decimal       `json:"mark_price"`
}

// Verdict mirrors riskverdict.schema.json (RiskVerdict v1).
type Verdict struct {
	SchemaVersion    string         `json:"schema_version"`
	VerdictID        string         `json:"verdict_id"`
	ProposalID       string         `json:"proposal_id"`
	Decision         Decision       `json:"decision"`
	ClippedSizeQuote *Decimal       `json:"clipped_size_quote,omitempty"`
	Reasons          []Reason       `json:"reasons"`
	LimitsSnapshot   LimitsSnapshot `json:"limits_snapshot"`
	EvaluatedAt      UTCTime        `json:"evaluated_at"`
}
