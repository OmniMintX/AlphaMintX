// Package riskgate implements the deterministic Risk Gate of
// docs/specs/risk-limits.md. No LLM is involved (invariant 1); the gate is a
// pure function over (proposal, RiskLimits, RuntimeState, now).
package riskgate

import (
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// Autonomy is the strategy's effective autonomy level (L0-L3).
type Autonomy int

const (
	AutonomyL0 Autonomy = iota // advisor: proposals persisted, no orders
	AutonomyL1                 // copilot: per-proposal human approval
	AutonomyL2                 // semi-auto within the L2 envelope
	AutonomyL3                 // full-auto
)

// L2Envelope bounds what an L2 strategy may auto-execute; above-envelope
// proposals escalate to the L1 approval flow.
type L2Envelope struct {
	MaxSizeQuote   decimal.Decimal
	AllowedSymbols []string
}

// RiskLimits mirrors RiskLimits v1 (docs/specs/risk-limits.md). All
// quote-denominated fields are in AccountingQuote. Admin-only mutable.
type RiskLimits struct {
	SymbolWhitelist             []string
	MaxOpenPositions            int
	PerPositionNotionalCapQuote decimal.Decimal
	DailyLossLimitQuote         decimal.Decimal
	MaxDrawdownPct              decimal.Decimal // percent of peak equity
	MaxLossAtStopQuote          decimal.Decimal
	MinStopDistancePct          decimal.Decimal // percent of entry
	MaxStopDistancePct          decimal.Decimal // percent of entry
	MaxOrdersPerMinute          int
	RequireStopLoss             bool
	AllocatedCapitalQuote       decimal.Decimal
	AccountingQuote             string
	StalenessThresholdSeconds   int // default 60
	L1ApprovalTimeoutSeconds    int // default 600
	L2Envelope                  *L2Envelope
}

// RuntimeState carries the runtime inputs the gate evaluates; they are
// captured verbatim in the verdict's limits_snapshot for reproducibility.
type RuntimeState struct {
	KillActive bool
	// KillEpoch is the persisted kill epoch observed at evaluation; the OMS
	// rejects submissions carrying a stale epoch (risk-limits.md, OMS rules).
	KillEpoch     int64
	BreakerActive bool
	Autonomy      Autonomy

	EquityQuote     decimal.Decimal
	PeakEquityQuote decimal.Decimal
	// DailyRealizedPnLQuote is the signed PnL of the current UTC day,
	// including fees and funding (risk-limits.md Definitions).
	DailyRealizedPnLQuote decimal.Decimal
	// ReservedWorstCaseQuote is the reserved worst-case exposure of pending
	// un-filled ENTRY orders (gate step 4 accounting).
	ReservedWorstCaseQuote decimal.Decimal

	OpenPositionsCount      int
	PendingEntryOrdersCount int
	// EntryOrdersInLastMinute counts proposal-originated ENTRY submissions
	// in the sliding 60 s window; safety-path submissions are exempt and
	// MUST NOT be counted by the caller.
	EntryOrdersInLastMinute int

	MarkPrice decimal.Decimal
	// EstTakerFeeRate is the estimated taker fee fraction used in
	// worst_case(order); zero when fees are not modeled (Phase 0).
	EstTakerFeeRate decimal.Decimal
}

// DefaultStalenessThresholdSeconds is the contract rule 5 default.
const DefaultStalenessThresholdSeconds = 60

// FutureSkewToleranceSeconds is the clock-skew guard of contract rule 5.
const FutureSkewToleranceSeconds = 5

var hundred = decimal.NewFromInt(100)

// entryPrice resolves the price the stop/size math is anchored to:
// limit_price for limit entries, current mark for market entries.
func entryPrice(p *contract.Proposal, state *RuntimeState) decimal.Decimal {
	if p.Entry.Type == "limit" && p.Entry.LimitPrice != nil {
		return p.Entry.LimitPrice.Decimal()
	}
	return state.MarkPrice
}

// worstCase computes worst_case(order) per risk-limits.md Definitions:
// size_quote x |entry - stop| / entry + estimated taker fees.
func worstCase(sizeQuote, entry, stop, feeRate decimal.Decimal) decimal.Decimal {
	loss := sizeQuote.Mul(entry.Sub(stop).Abs()).Div(entry)
	return loss.Add(sizeQuote.Mul(feeRate))
}

// snapshot captures the evaluated limits and runtime inputs into the
// verdict's limits_snapshot (required on every verdict).
func snapshot(limits RiskLimits, state RuntimeState) contract.LimitsSnapshot {
	snap := contract.LimitsSnapshot{
		SymbolWhitelist:             append([]string{}, limits.SymbolWhitelist...),
		MaxOpenPositions:            limits.MaxOpenPositions,
		PerPositionNotionalCapQuote: contract.NewDecimal(limits.PerPositionNotionalCapQuote),
		DailyLossLimitQuote:         contract.NewDecimal(limits.DailyLossLimitQuote),
		MaxDrawdownPct:              limits.MaxDrawdownPct.InexactFloat64(),
		MaxOrdersPerMinute:          limits.MaxOrdersPerMinute,
		RequireStopLoss:             limits.RequireStopLoss,
		EquityQuote:                 contract.NewDecimal(state.EquityQuote),
		PeakEquityQuote:             contract.NewDecimal(state.PeakEquityQuote),
		DailyRealizedPnlQuote:       contract.NewSignedDecimal(state.DailyRealizedPnLQuote),
		OpenPositionsCount:          state.OpenPositionsCount,
		PendingEntryOrdersCount:     state.PendingEntryOrdersCount,
		MarkPrice:                   contract.NewDecimal(state.MarkPrice),
	}
	if limits.L2Envelope != nil {
		l2max := contract.NewDecimal(limits.L2Envelope.MaxSizeQuote)
		snap.L2MaxSizeQuote = &l2max
		snap.L2AllowedSymbols = append([]string{}, limits.L2Envelope.AllowedSymbols...)
	}
	return snap
}
