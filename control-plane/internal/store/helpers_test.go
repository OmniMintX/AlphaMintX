package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustDec(t *testing.T, v string) contract.Decimal {
	t.Helper()
	d, err := contract.ParseDecimal(v)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", v, err)
	}
	return d
}

func mustSigned(t *testing.T, v string) contract.SignedDecimal {
	t.Helper()
	d, err := contract.ParseSignedDecimal(v)
	if err != nil {
		t.Fatalf("ParseSignedDecimal(%q): %v", v, err)
	}
	return d
}

func mustTime(t *testing.T, v string) contract.UTCTime {
	t.Helper()
	u, err := contract.ParseUTCTime(v)
	if err != nil {
		t.Fatalf("ParseUTCTime(%q): %v", v, err)
	}
	return u
}

func createStrategy(t *testing.T, s *Store, strategyID string) {
	t.Helper()
	err := s.CreateStrategy(Strategy{
		StrategyID: strategyID, TenantID: "tenant-1", Name: "strategy-" + strategyID,
		LifecycleState: "paper", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("CreateStrategy(%s): %v", strategyID, err)
	}
}

func testProposal(t *testing.T, proposalID, strategyID, traceID string) *contract.Proposal {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	return &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    proposalID, StrategyID: strategyID, AgentTraceID: traceID,
		CreatedAt: mustTime(t, "2026-07-04T12:00:00Z"),
		Symbol:    "BTC/USDT", Action: contract.ActionHold,
		SizeQuote: mustDec(t, "0"), Entry: contract.Entry{Type: "market"},
		TimeInForce: "gtc", Confidence: 0.5, Reasoning: "hold: no edge",
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateSummary:    "no edge either way",
		ModelCosts: []contract.ModelCost{
			{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
			{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.0005")},
		},
	}
}

func testVerdict(t *testing.T, verdictID, proposalID string) *contract.Verdict {
	t.Helper()
	return &contract.Verdict{
		SchemaVersion: contract.SchemaVersion,
		VerdictID:     verdictID, ProposalID: proposalID,
		Decision: contract.DecisionApprove, Reasons: []contract.Reason{},
		LimitsSnapshot: contract.LimitsSnapshot{
			SymbolWhitelist:  []string{"BTC/USDT"},
			MaxOpenPositions: 3, PerPositionNotionalCapQuote: mustDec(t, "2000.00"),
			DailyLossLimitQuote: mustDec(t, "500.00"), MaxDrawdownPct: 10,
			MaxOrdersPerMinute: 6, RequireStopLoss: true,
			EquityQuote: mustDec(t, "10000.00"), PeakEquityQuote: mustDec(t, "10000.00"),
			DailyRealizedPnlQuote: mustSigned(t, "0"), OpenPositionsCount: 0,
			PendingEntryOrdersCount: 0, MarkPrice: mustDec(t, "64000.00"),
		},
		EvaluatedAt: mustTime(t, "2026-07-04T12:00:03Z"),
	}
}

func testTrace(t *testing.T, strategyID, runID string, proposalID *string) *TraceEnvelope {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	return &TraceEnvelope{
		SchemaVersion: "1.0", StrategyID: strategyID, RunID: runID, TickNumber: 0,
		StartedAt:        mustTime(t, "2026-07-04T12:00:00Z"),
		CompletedAt:      mustTime(t, "2026-07-04T12:00:09Z"),
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateRounds: []DebateRound{{
			RoundIndex: 0, BullArgument: "up", BullScore: 0.6, BearArgument: "down", BearScore: 0.4,
		}},
		DebateSummary: "no edge either way",
		ProposalID:    proposalID,
		ModelCosts: []TraceModelCost{
			{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
			{Node: "market_analyst", Model: "stub", InputTokens: 50, OutputTokens: 10, CostUSD: mustDec(t, "0.0005")},
		},
		BudgetState: BudgetState{UTCDate: "2026-07-04", TokensUsed: 180, CostUSDUsed: mustDec(t, "0.0015")},
	}
}

// insertChain persists proposal (at tick) -> verdict for an existing
// strategy and returns the ids used: proposal = uid(base), verdict =
// uid(base+1), run = uid(base+2).
func insertChain(t *testing.T, s *Store, base int, strategyID string, tick int) (proposalID, verdictID, runID string) {
	t.Helper()
	proposalID, verdictID, runID = uid(base), uid(base+1), uid(base+2)
	p := testProposal(t, proposalID, strategyID, runID)
	if _, err := s.InsertProposal(ProposalSubmission{TickNumber: tick, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	if _, err := s.InsertVerdict(testVerdict(t, verdictID, proposalID)); err != nil {
		t.Fatalf("InsertVerdict: %v", err)
	}
	return proposalID, verdictID, runID
}
