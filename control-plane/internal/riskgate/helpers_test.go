package riskgate

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

func mustDec(t *testing.T, s string) contract.Decimal {
	t.Helper()
	d, err := contract.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

func decPtr(t *testing.T, s string) *contract.Decimal {
	t.Helper()
	d := mustDec(t, s)
	return &d
}

func mustTime(t *testing.T, s string) contract.UTCTime {
	t.Helper()
	u, err := contract.ParseUTCTime(s)
	if err != nil {
		t.Fatalf("ParseUTCTime(%q): %v", s, err)
	}
	return u
}

func summary() contract.AnalystSummary {
	return contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "s"}
}

// baseProposal: open_long BTC/USDT 1000 at limit 100, stop 98 (2% distance,
// worst_case 20), tp 105, confidence 0.7.
func baseProposal(t *testing.T) *contract.Proposal {
	t.Helper()
	return &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
		StrategyID:    "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e",
		AgentTraceID:  "c3d4e5f6-a7b8-4c9d-8e0f-2a3b4c5d6e7f",
		CreatedAt:     mustTime(t, "2026-07-04T12:00:00Z"),
		Symbol:        "BTC/USDT",
		Action:        contract.ActionOpenLong,
		SizeQuote:     mustDec(t, "1000"),
		Entry:         contract.Entry{Type: "limit", LimitPrice: decPtr(t, "100")},
		StopLoss:      decPtr(t, "98"),
		TakeProfit:    decPtr(t, "105"),
		TimeInForce:   "gtc",
		Confidence:    0.7,
		Reasoning:     "test",
		AnalystSummaries: contract.AnalystSummaries{
			Market: summary(), News: summary(), Fundamental: summary(),
		},
		DebateSummary: "d",
		ModelCosts:    []contract.ModelCost{},
	}
}

// closeProposal: close BTC/USDT, full close, no stop/tp.
func closeProposal(t *testing.T) *contract.Proposal {
	t.Helper()
	p := baseProposal(t)
	p.Action = contract.ActionClose
	p.SizeQuote = mustDec(t, "0")
	p.Entry = contract.Entry{Type: "market"}
	p.StopLoss = nil
	p.TakeProfit = nil
	return p
}

func baseLimits() RiskLimits {
	return RiskLimits{
		SymbolWhitelist:             []string{"BTC/USDT"},
		MaxOpenPositions:            3,
		PerPositionNotionalCapQuote: decimal.NewFromInt(2000),
		DailyLossLimitQuote:         decimal.NewFromInt(500),
		MaxDrawdownPct:              decimal.NewFromInt(10),
		MaxLossAtStopQuote:          decimal.NewFromInt(50),
		MinStopDistancePct:          decimal.RequireFromString("0.1"),
		MaxStopDistancePct:          decimal.NewFromInt(25),
		MaxOrdersPerMinute:          6,
		RequireStopLoss:             true,
		AllocatedCapitalQuote:       decimal.NewFromInt(10000),
		AccountingQuote:             "USDT",
		StalenessThresholdSeconds:   60,
		L1ApprovalTimeoutSeconds:    600,
	}
}

func baseState() RuntimeState {
	return RuntimeState{
		Autonomy:              AutonomyL3,
		EquityQuote:           decimal.NewFromInt(10000),
		PeakEquityQuote:       decimal.NewFromInt(10000),
		DailyRealizedPnLQuote: decimal.Zero,
		MarkPrice:             decimal.NewFromInt(100),
	}
}

func baseNow(t *testing.T) time.Time {
	t.Helper()
	return mustTime(t, "2026-07-04T12:00:01Z").Time()
}

func hasCode(v contract.Verdict, code string) bool {
	for _, r := range v.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}
