package riskgate

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// LowConfidenceThreshold: confidence below this MUST map to hold
// (proposal-contract.md rule 4); the gate rejects non-hold proposals under it.
const LowConfidenceThreshold = 0.3

// Evaluate runs the numbered gate steps of docs/specs/risk-limits.md over a
// parsed proposal. It is a pure function; per-strategy serialization and
// idempotency (step 0b) live in Service. Every verdict embeds the full
// limits_snapshot. First failing check decides.
func Evaluate(p *contract.Proposal, limits RiskLimits, state RuntimeState, now time.Time) contract.Verdict {
	// Step 0a (parseable documents): schema/version failures become verdicts.
	if p.SchemaVersion != contract.SchemaVersion {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeUnsupportedSchemaVersion,
			Message: fmt.Sprintf("schema_version %q not supported (supported: %q)", p.SchemaVersion, contract.SchemaVersion),
		})
	}
	if vs := p.Validate(); len(vs) > 0 {
		reasons := make([]contract.Reason, 0, len(vs))
		for _, v := range vs {
			reasons = append(reasons, contract.Reason{Code: v.Code, Message: v.Message})
		}
		return reject(p, limits, state, now, reasons...)
	}

	// Step 1: kill-switch is a standing condition; rejects everything in
	// scope including close. The observed kill-epoch is recorded.
	if state.KillActive {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeKillSwitchActive,
			Message: fmt.Sprintf("kill-switch active (kill-epoch %d); killed positions are managed by the kill procedure, not proposals", state.KillEpoch),
		})
	}

	// Step 2: staleness against the authoritative control-plane clock,
	// with a future clock-skew guard (proposal-contract.md rule 5).
	threshold := limits.StalenessThresholdSeconds
	if threshold <= 0 {
		threshold = DefaultStalenessThresholdSeconds
	}
	created := p.CreatedAt.Time()
	if now.Sub(created) > time.Duration(threshold)*time.Second {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeProposalStale,
			Message: fmt.Sprintf("created_at %s older than %ds at control-plane receipt", p.CreatedAt, threshold),
		})
	}
	if created.Sub(now) > FutureSkewToleranceSeconds*time.Second {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeProposalStale,
			Message: fmt.Sprintf("created_at %s is more than %ds in the future (clock-skew guard)", p.CreatedAt, FutureSkewToleranceSeconds),
		})
	}

	// hold proposals skip steps 3-11: approve verdict, no order.
	if p.Action == contract.ActionHold {
		return approve(p, limits, state, now)
	}

	// Step 3: exit exemption. close skips steps 4-7 and 9-11, stays subject
	// to step 8, and is submitted reduce-only. Never blocked except by kill.
	isClose := p.Action == contract.ActionClose

	entry := entryPrice(p, &state)
	// Zero-price guard: the worst-case-loss and stop-distance math below
	// divides by the resolved entry price. A market entry with no usable
	// mark (feed down, symbol unpriced) MUST reject, never panic: every
	// schema-valid proposal yields exactly one verdict.
	if p.Action.IsOpen() && entry.Sign() <= 0 {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeMarkPriceUnavailable,
			Message: fmt.Sprintf("resolved entry price %s for %s entry is not strictly positive (mark price unavailable)", entry, p.Entry.Type),
		})
	}
	var worst decimal.Decimal
	if p.Action.IsOpen() && p.StopLoss != nil {
		worst = worstCase(p.SizeQuote.Decimal(), entry, p.StopLoss.Decimal(), state.EstTakerFeeRate)
	}

	if !isClose {
		if r := checkOpen(p, limits, state, entry, worst); r != nil {
			return reject(p, limits, state, now, *r)
		}
	}

	// Step 8: order rate (opens and closes). An approved verdict reserves a
	// rate token, so an already-full window rejects the next submission.
	// Safety-path submissions never reach the gate and are exempt.
	if state.EntryOrdersInLastMinute >= limits.MaxOrdersPerMinute {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeOrderRateExceeded,
			Message: fmt.Sprintf("%d orders in the last 60s at limit %d", state.EntryOrdersInLastMinute, limits.MaxOrdersPerMinute),
		})
	}

	if isClose {
		return approve(p, limits, state, now)
	}

	// Step 9: open positions PLUS pending un-filled ENTRY orders.
	slots := state.OpenPositionsCount + state.PendingEntryOrdersCount
	if slots >= limits.MaxOpenPositions {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeMaxPositionsReached,
			Message: fmt.Sprintf("%d open positions + %d pending entry orders at limit %d", state.OpenPositionsCount, state.PendingEntryOrdersCount, limits.MaxOpenPositions),
		})
	}

	// Step 10: notional cap. Cap "0" rejects all opens; oversize is clipped,
	// not rejected.
	notionalCap := limits.PerPositionNotionalCapQuote
	if notionalCap.IsZero() {
		return reject(p, limits, state, now, contract.Reason{
			Code:    contract.CodeNotionalCapZero,
			Message: "per_position_notional_cap_quote is 0: all opens rejected",
		})
	}
	effectiveSize := p.SizeQuote.Decimal()
	var clipReason *contract.Reason
	if effectiveSize.GreaterThan(notionalCap) {
		effectiveSize = notionalCap
		clipReason = &contract.Reason{
			Code:    contract.CodeNotionalCapClipped,
			Message: fmt.Sprintf("size_quote %s clipped to per_position_notional_cap_quote %s", p.SizeQuote, notionalCap),
		}
	}

	// Step 11: L2 envelope on the POST-CLIP effective size; above-envelope
	// escalates to the L1 approval flow instead of rejecting.
	if r := checkEnvelope(p, limits, state, effectiveSize); r != nil {
		reasons := []contract.Reason{*r}
		if clipReason != nil {
			reasons = append([]contract.Reason{*clipReason}, reasons...)
		}
		return build(p, contract.DecisionEscalate, nil, reasons, limits, state, now)
	}

	if clipReason != nil {
		clipped := contract.NewDecimal(effectiveSize)
		return build(p, contract.DecisionClip, &clipped, []contract.Reason{*clipReason}, limits, state, now)
	}
	return approve(p, limits, state, now)
}
