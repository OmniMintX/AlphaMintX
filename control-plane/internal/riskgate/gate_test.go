package riskgate

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

func TestEvaluateTable(t *testing.T) {
	l2 := func(maxSize string, symbols ...string) *L2Envelope {
		return &L2Envelope{MaxSizeQuote: decimal.RequireFromString(maxSize), AllowedSymbols: symbols}
	}
	tests := []struct {
		name     string
		proposal func(t *testing.T) *contract.Proposal
		mutateP  func(p *contract.Proposal)
		mutateL  func(l *RiskLimits)
		mutateS  func(s *RuntimeState)
		now      func(created time.Time) time.Time
		want     contract.Decision
		wantCode string
	}{
		{name: "approve happy path", want: contract.DecisionApprove},
		{name: "hold approves even at low confidence",
			mutateP: func(p *contract.Proposal) {
				p.Action = contract.ActionHold
				p.SizeQuote, _ = contract.ParseDecimal("0")
				p.Entry = contract.Entry{Type: "market"}
				p.StopLoss, p.TakeProfit = nil, nil
				p.Confidence = 0.1
			},
			want: contract.DecisionApprove},
		{name: "unsupported schema version",
			mutateP: func(p *contract.Proposal) { p.SchemaVersion = "2.0" },
			want:    contract.DecisionReject, wantCode: contract.CodeUnsupportedSchemaVersion},
		{name: "non-canonical symbol is schema invalid",
			mutateP: func(p *contract.Proposal) { p.Symbol = "btc-usdt" },
			want:    contract.DecisionReject, wantCode: contract.CodeSchemaInvalid},
		{name: "stale created_at",
			now:  func(created time.Time) time.Time { return created.Add(61 * time.Second) },
			want: contract.DecisionReject, wantCode: contract.CodeProposalStale},
		{name: "created_at in the future beyond skew",
			now:  func(created time.Time) time.Time { return created.Add(-6 * time.Second) },
			want: contract.DecisionReject, wantCode: contract.CodeProposalStale},
		{name: "kill-switch rejects opens",
			mutateS: func(s *RuntimeState) { s.KillActive, s.KillEpoch = true, 7 },
			want:    contract.DecisionReject, wantCode: contract.CodeKillSwitchActive},
		{name: "kill-switch rejects close too",
			proposal: closeProposal,
			mutateS:  func(s *RuntimeState) { s.KillActive = true },
			want:     contract.DecisionReject, wantCode: contract.CodeKillSwitchActive},
		{name: "close bypasses whitelist daily-loss drawdown positions and cap",
			proposal: closeProposal,
			mutateP:  func(p *contract.Proposal) { p.Symbol = "SOL/USDT" },
			mutateL:  func(l *RiskLimits) { l.PerPositionNotionalCapQuote = decimal.Zero },
			mutateS: func(s *RuntimeState) {
				s.BreakerActive = true
				s.DailyRealizedPnLQuote = decimal.NewFromInt(-600)
				s.EquityQuote = decimal.NewFromInt(8000)
				s.OpenPositionsCount = 3
			},
			want: contract.DecisionApprove},
		{name: "low confidence open rejected",
			mutateP: func(p *contract.Proposal) { p.Confidence = 0.29 },
			want:    contract.DecisionReject, wantCode: contract.CodeLowConfidence},
		{name: "circuit breaker active",
			mutateS: func(s *RuntimeState) { s.BreakerActive = true },
			want:    contract.DecisionReject, wantCode: contract.CodeDailyLossLimitBreached},
		{name: "daily loss plus worst case breaches",
			mutateS: func(s *RuntimeState) { s.DailyRealizedPnLQuote = decimal.NewFromInt(-490) },
			want:    contract.DecisionReject, wantCode: contract.CodeDailyLossLimitBreached},
		// Boundary pins for the >= comparison (checks.go step 4): base
		// worst_case = 1000*|100-98|/100 = 20, limit = 500.
		{name: "daily loss exactly at limit rejects",
			mutateS: func(s *RuntimeState) { s.DailyRealizedPnLQuote = decimal.NewFromInt(-480) },
			want:    contract.DecisionReject, wantCode: contract.CodeDailyLossLimitBreached},
		{name: "daily loss one cent below limit approves",
			mutateS: func(s *RuntimeState) { s.DailyRealizedPnLQuote = decimal.RequireFromString("-479.99") },
			want:    contract.DecisionApprove},
		{name: "reserved pending-entry worst case counts",
			mutateS: func(s *RuntimeState) { s.ReservedWorstCaseQuote = decimal.NewFromInt(480) },
			want:    contract.DecisionReject, wantCode: contract.CodeDailyLossLimitBreached},
		{name: "drawdown breach",
			mutateS: func(s *RuntimeState) { s.EquityQuote = decimal.NewFromInt(8900) },
			want:    contract.DecisionReject, wantCode: contract.CodeMaxDrawdownBreached},
		{name: "symbol not whitelisted",
			mutateP: func(p *contract.Proposal) { p.Symbol = "ETH/USDT" },
			want:    contract.DecisionReject, wantCode: contract.CodeSymbolNotWhitelisted},
		{name: "stop distance below minimum",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("99.95"); p.StopLoss = &d },
			want:    contract.DecisionReject, wantCode: contract.CodeInvalidStopPlacement},
		{name: "stop distance above maximum",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("70"); p.StopLoss = &d },
			want:    contract.DecisionReject, wantCode: contract.CodeInvalidStopPlacement},
		{name: "market entry stop on wrong side of mark",
			mutateP: func(p *contract.Proposal) {
				p.Entry = contract.Entry{Type: "market"}
				d, _ := contract.ParseDecimal("101")
				p.StopLoss = &d
				p.TakeProfit = nil
			},
			want: contract.DecisionReject, wantCode: contract.CodeInvalidStopPlacement},
		{name: "market entry with zero mark price rejects not panics",
			mutateP: func(p *contract.Proposal) {
				p.Entry = contract.Entry{Type: "market"}
				p.TakeProfit = nil
			},
			mutateS: func(s *RuntimeState) { s.MarkPrice = decimal.Zero },
			want:    contract.DecisionReject, wantCode: contract.CodeMarkPriceUnavailable},
		{name: "limit entry unaffected by zero mark price",
			mutateS: func(s *RuntimeState) { s.MarkPrice = decimal.Zero },
			want:    contract.DecisionApprove},
		{name: "close approves with zero mark price",
			proposal: closeProposal,
			mutateS:  func(s *RuntimeState) { s.MarkPrice = decimal.Zero },
			want:     contract.DecisionApprove},
		{name: "per-trade risk bound exceeded",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("90"); p.StopLoss = &d },
			want:    contract.DecisionReject, wantCode: contract.CodeRiskPerTradeExceeded},
		{name: "order rate window full",
			mutateS: func(s *RuntimeState) { s.EntryOrdersInLastMinute = 6 },
			want:    contract.DecisionReject, wantCode: contract.CodeOrderRateExceeded},
		{name: "close subject to order rate",
			proposal: closeProposal,
			mutateS:  func(s *RuntimeState) { s.EntryOrdersInLastMinute = 6 },
			want:     contract.DecisionReject, wantCode: contract.CodeOrderRateExceeded},
		{name: "position count includes pending entries",
			mutateS: func(s *RuntimeState) { s.OpenPositionsCount, s.PendingEntryOrdersCount = 2, 1 },
			want:    contract.DecisionReject, wantCode: contract.CodeMaxPositionsReached},
		{name: "notional cap zero rejects opens",
			mutateL: func(l *RiskLimits) { l.PerPositionNotionalCapQuote = decimal.Zero },
			want:    contract.DecisionReject, wantCode: contract.CodeNotionalCapZero},
		{name: "oversize clipped to cap",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("2500"); p.SizeQuote = d },
			want:    contract.DecisionClip, wantCode: contract.CodeNotionalCapClipped},
		{name: "L2 above envelope size escalates",
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("500", "BTC/USDT") },
			mutateS: func(s *RuntimeState) { s.Autonomy = AutonomyL2 },
			want:    contract.DecisionEscalate, wantCode: contract.CodeEscalatedAboveEnvelope},
		{name: "L2 symbol outside envelope escalates",
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("5000", "ETH/USDT") },
			mutateS: func(s *RuntimeState) { s.Autonomy = AutonomyL2 },
			want:    contract.DecisionEscalate, wantCode: contract.CodeEscalatedAboveEnvelope},
		{name: "L2 within envelope approves",
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("1500", "BTC/USDT") },
			mutateS: func(s *RuntimeState) { s.Autonomy = AutonomyL2 },
			want:    contract.DecisionApprove},
		{name: "envelope evaluated on post-clip size",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("2500"); p.SizeQuote = d },
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("2000", "BTC/USDT") },
			mutateS: func(s *RuntimeState) { s.Autonomy = AutonomyL2 },
			want:    contract.DecisionClip, wantCode: contract.CodeNotionalCapClipped},
		{name: "post-clip size above envelope escalates",
			mutateP: func(p *contract.Proposal) { d, _ := contract.ParseDecimal("2500"); p.SizeQuote = d },
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("1800", "BTC/USDT") },
			mutateS: func(s *RuntimeState) { s.Autonomy = AutonomyL2 },
			want:    contract.DecisionEscalate, wantCode: contract.CodeEscalatedAboveEnvelope},
		{name: "L3 ignores L2 envelope",
			mutateL: func(l *RiskLimits) { l.L2Envelope = l2("500", "ETH/USDT") },
			want:    contract.DecisionApprove},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProposal(t)
			if tc.proposal != nil {
				p = tc.proposal(t)
			}
			if tc.mutateP != nil {
				tc.mutateP(p)
			}
			limits, state := baseLimits(), baseState()
			if tc.mutateL != nil {
				tc.mutateL(&limits)
			}
			if tc.mutateS != nil {
				tc.mutateS(&state)
			}
			now := baseNow(t)
			if tc.now != nil {
				now = tc.now(p.CreatedAt.Time())
			}
			v := Evaluate(p, limits, state, now)
			if v.Decision != tc.want {
				t.Fatalf("decision = %s, want %s (reasons %+v)", v.Decision, tc.want, v.Reasons)
			}
			if tc.wantCode != "" && !hasCode(v, tc.wantCode) {
				t.Errorf("reasons %+v missing code %s", v.Reasons, tc.wantCode)
			}
			if tc.want == contract.DecisionApprove && len(v.Reasons) != 0 {
				t.Errorf("approve should carry no reasons, got %+v", v.Reasons)
			}
			assertSnapshotComplete(t, v, limits, state)
		})
	}
}

func assertSnapshotComplete(t *testing.T, v contract.Verdict, limits RiskLimits, state RuntimeState) {
	t.Helper()
	snap := v.LimitsSnapshot
	if snap.MaxOpenPositions != limits.MaxOpenPositions ||
		snap.MaxOrdersPerMinute != limits.MaxOrdersPerMinute ||
		snap.RequireStopLoss != limits.RequireStopLoss {
		t.Errorf("snapshot limits mismatch: %+v", snap)
	}
	if !snap.EquityQuote.Decimal().Equal(state.EquityQuote) ||
		!snap.PeakEquityQuote.Decimal().Equal(state.PeakEquityQuote) ||
		!snap.DailyRealizedPnlQuote.Decimal().Equal(state.DailyRealizedPnLQuote) ||
		!snap.MarkPrice.Decimal().Equal(state.MarkPrice) {
		t.Errorf("snapshot runtime inputs mismatch: %+v", snap)
	}
	if snap.OpenPositionsCount != state.OpenPositionsCount ||
		snap.PendingEntryOrdersCount != state.PendingEntryOrdersCount {
		t.Errorf("snapshot counts mismatch: %+v", snap)
	}
	if v.SchemaVersion != contract.SchemaVersion || v.EvaluatedAt.String() == "" {
		t.Errorf("verdict header incomplete: %+v", v)
	}
}

func TestKillEpochRecordedInVerdict(t *testing.T) {
	p := baseProposal(t)
	state := baseState()
	state.KillActive, state.KillEpoch = true, 7
	v := Evaluate(p, baseLimits(), state, baseNow(t))
	if v.Decision != contract.DecisionReject || !hasCode(v, contract.CodeKillSwitchActive) {
		t.Fatalf("want KILL_SWITCH_ACTIVE reject, got %+v", v)
	}
	if !strings.Contains(v.Reasons[0].Message, "kill-epoch 7") {
		t.Errorf("kill-epoch not recorded in verdict reason: %q", v.Reasons[0].Message)
	}
}

func TestClipSetsClippedSizeQuote(t *testing.T) {
	p := baseProposal(t)
	p.SizeQuote = mustDec(t, "2500")
	v := Evaluate(p, baseLimits(), baseState(), baseNow(t))
	if v.Decision != contract.DecisionClip {
		t.Fatalf("decision = %s, want clip", v.Decision)
	}
	if v.ClippedSizeQuote == nil || !v.ClippedSizeQuote.Decimal().Equal(decimal.NewFromInt(2000)) {
		t.Fatalf("clipped_size_quote = %v, want 2000", v.ClippedSizeQuote)
	}
	if v.ClippedSizeQuote.Decimal().Sign() <= 0 ||
		!v.ClippedSizeQuote.Decimal().LessThan(p.SizeQuote.Decimal()) {
		t.Errorf("clipped_size_quote must be > 0 and < size_quote")
	}
}

func TestApproveNeverClipsAndHasNoClippedSize(t *testing.T) {
	v := Evaluate(baseProposal(t), baseLimits(), baseState(), baseNow(t))
	if v.Decision != contract.DecisionApprove || v.ClippedSizeQuote != nil {
		t.Fatalf("want plain approve, got %+v", v)
	}
}

// Size EXACTLY at the notional cap is not oversize: the clip comparison is
// strict (evaluate.go step 10), so the verdict approves un-clipped with no
// clipped_size_quote.
func TestSizeExactlyAtCapApprovesWithoutClip(t *testing.T) {
	p := baseProposal(t)
	p.SizeQuote = mustDec(t, "2000") // == PerPositionNotionalCapQuote
	v := Evaluate(p, baseLimits(), baseState(), baseNow(t))
	if v.Decision != contract.DecisionApprove || v.ClippedSizeQuote != nil {
		t.Fatalf("size at exact cap must approve un-clipped, got %+v", v)
	}
}
