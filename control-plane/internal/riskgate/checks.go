package riskgate

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// checkOpen runs the open-only reject checks (gate steps 4-7 plus the
// low-confidence rule of proposal-contract.md rule 4). close is exempt from
// all of these (step 3). Returns the first failing reason.
func checkOpen(p *contract.Proposal, limits RiskLimits, state RuntimeState, entry, worst decimal.Decimal) *contract.Reason {
	// Contract rule 4: confidence < 0.3 MUST map to hold; a non-hold
	// proposal below the threshold is rejected (exits stay exempt).
	if p.Confidence < LowConfidenceThreshold {
		return &contract.Reason{
			Code:    contract.CodeLowConfidence,
			Message: fmt.Sprintf("confidence %.2f below %.1f must be action=hold", p.Confidence, LowConfidenceThreshold),
		}
	}

	// Step 4: circuit breaker / daily loss, including reserved worst-case
	// exposure of pending ENTRY orders. Loss accounting already includes
	// fees and funding (Definitions).
	if state.BreakerActive {
		return &contract.Reason{
			Code:    contract.CodeDailyLossLimitBreached,
			Message: "circuit breaker active: effective autonomy demoted to L0 until the next UTC day",
		}
	}
	dailyLoss := decimal.Zero
	if state.DailyRealizedPnLQuote.Sign() < 0 {
		dailyLoss = state.DailyRealizedPnLQuote.Neg()
	}
	dailyLoss = dailyLoss.Add(state.ReservedWorstCaseQuote)
	if dailyLoss.Add(worst).GreaterThanOrEqual(limits.DailyLossLimitQuote) {
		return &contract.Reason{
			Code:    contract.CodeDailyLossLimitBreached,
			Message: fmt.Sprintf("daily loss %s + worst_case(order) %s >= daily_loss_limit_quote %s (UTC day)", dailyLoss, worst, limits.DailyLossLimitQuote),
		}
	}

	// Step 5: max drawdown vs peak equity ((peak-equity)/peak*100 > limit).
	peak := state.PeakEquityQuote
	if peak.Sign() > 0 && peak.Sub(state.EquityQuote).Mul(hundred).GreaterThan(limits.MaxDrawdownPct.Mul(peak)) {
		return &contract.Reason{
			Code:    contract.CodeMaxDrawdownBreached,
			Message: fmt.Sprintf("equity %s below peak %s by more than max_drawdown_pct %s", state.EquityQuote, peak, limits.MaxDrawdownPct),
		}
	}

	// Step 6: symbol whitelist (exact string equality on canonical form;
	// empty list denies all opens).
	if !slices.Contains(limits.SymbolWhitelist, p.Symbol) {
		return &contract.Reason{
			Code:    contract.CodeSymbolNotWhitelisted,
			Message: fmt.Sprintf("symbol %s not in whitelist", p.Symbol),
		}
	}

	// Step 7: stop-loss present + placement + per-trade risk bound.
	if p.StopLoss == nil {
		if limits.RequireStopLoss {
			return &contract.Reason{
				Code:    contract.CodeMissingStopLoss,
				Message: fmt.Sprintf("stop_loss required for %s", p.Action),
			}
		}
		return nil
	}
	stop := p.StopLoss.Decimal()
	// Re-check placement against the resolved entry price: for market
	// entries Validate() could not see the mark (contract rules 1-2).
	if vs := contract.CheckStopAndTarget(p.Action, entry, stop, p.TakeProfit); len(vs) > 0 {
		return &contract.Reason{Code: vs[0].Code, Message: vs[0].Message}
	}
	distPct := entry.Sub(stop).Abs().Mul(hundred).Div(entry)
	if distPct.LessThan(limits.MinStopDistancePct) || distPct.GreaterThan(limits.MaxStopDistancePct) {
		return &contract.Reason{
			Code:    contract.CodeInvalidStopPlacement,
			Message: fmt.Sprintf("stop distance %s%% outside [%s%%, %s%%] of entry", distPct.Round(4), limits.MinStopDistancePct, limits.MaxStopDistancePct),
		}
	}
	if worst.GreaterThan(limits.MaxLossAtStopQuote) {
		return &contract.Reason{
			Code:    contract.CodeRiskPerTradeExceeded,
			Message: fmt.Sprintf("worst_case(order) %s > max_loss_at_stop_quote %s", worst, limits.MaxLossAtStopQuote),
		}
	}
	return nil
}

// checkEnvelope applies gate step 11 to the post-clip effective size.
// Direction-flip escalation needs same-UTC-day trade history and lands with
// the Phase-1 persistence layer.
func checkEnvelope(p *contract.Proposal, limits RiskLimits, state RuntimeState, effectiveSize decimal.Decimal) *contract.Reason {
	if state.Autonomy != AutonomyL2 || limits.L2Envelope == nil {
		return nil
	}
	env := limits.L2Envelope
	if effectiveSize.GreaterThan(env.MaxSizeQuote) {
		return &contract.Reason{
			Code:    contract.CodeEscalatedAboveEnvelope,
			Message: fmt.Sprintf("post-clip size %s > l2_max_size_quote %s: escalated to L1 approval", effectiveSize, env.MaxSizeQuote),
		}
	}
	if !slices.Contains(env.AllowedSymbols, p.Symbol) {
		return &contract.Reason{
			Code:    contract.CodeEscalatedAboveEnvelope,
			Message: fmt.Sprintf("symbol %s not in l2_allowed_symbols: escalated to L1 approval", p.Symbol),
		}
	}
	return nil
}

func approve(p *contract.Proposal, limits RiskLimits, state RuntimeState, now time.Time) contract.Verdict {
	return build(p, contract.DecisionApprove, nil, []contract.Reason{}, limits, state, now)
}

func reject(p *contract.Proposal, limits RiskLimits, state RuntimeState, now time.Time, reasons ...contract.Reason) contract.Verdict {
	return build(p, contract.DecisionReject, nil, reasons, limits, state, now)
}

func build(p *contract.Proposal, decision contract.Decision, clipped *contract.Decimal, reasons []contract.Reason, limits RiskLimits, state RuntimeState, now time.Time) contract.Verdict {
	return contract.Verdict{
		SchemaVersion:    contract.SchemaVersion,
		VerdictID:        uuid.NewString(),
		ProposalID:       p.ProposalID,
		Decision:         decision,
		ClippedSizeQuote: clipped,
		Reasons:          reasons,
		LimitsSnapshot:   snapshot(limits, state),
		EvaluatedAt:      contract.NewUTCTime(now),
	}
}
