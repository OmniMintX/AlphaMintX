package contract

// Reason codes used by contract validation and the Risk Gate. Per SS-25 the
// set is open (SCREAMING_SNAKE, <=64 chars); consumers treat unknown codes as
// opaque. These are the codes named by the v1 specs.
const (
	CodeSchemaInvalid            = "SCHEMA_INVALID"
	CodeUnsupportedSchemaVersion = "UNSUPPORTED_SCHEMA_VERSION"
	CodeIdempotencyConflict      = "IDEMPOTENCY_CONFLICT"
	CodeKillSwitchActive         = "KILL_SWITCH_ACTIVE"
	CodeProposalStale            = "PROPOSAL_STALE"
	CodeMarkPriceUnavailable     = "MARK_PRICE_UNAVAILABLE"
	CodeDailyLossLimitBreached   = "DAILY_LOSS_LIMIT_BREACHED"
	CodeMaxDrawdownBreached      = "MAX_DRAWDOWN_BREACHED"
	CodeSymbolNotWhitelisted     = "SYMBOL_NOT_WHITELISTED"
	CodeMissingStopLoss          = "MISSING_STOP_LOSS"
	CodeInvalidStopPlacement     = "INVALID_STOP_PLACEMENT"
	CodeInvalidTakeProfit        = "INVALID_TAKE_PROFIT_PLACEMENT"
	CodeInvalidSize              = "INVALID_SIZE"
	CodeLowConfidence            = "LOW_CONFIDENCE"
	CodeRiskPerTradeExceeded     = "RISK_PER_TRADE_EXCEEDED"
	CodeOrderRateExceeded        = "ORDER_RATE_EXCEEDED"
	CodeMaxPositionsReached      = "MAX_POSITIONS_REACHED"
	CodeNotionalCapZero          = "NOTIONAL_CAP_ZERO"
	CodeNotionalCapClipped       = "NOTIONAL_CAP_CLIPPED"
	CodeEscalatedAboveEnvelope   = "ESCALATED_ABOVE_ENVELOPE"
)
