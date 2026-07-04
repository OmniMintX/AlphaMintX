package omsbridge

import (
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createStrategy(t *testing.T, s *store.Store, strategyID string) {
	t.Helper()
	err := s.CreateStrategy(store.Strategy{
		StrategyID: strategyID, TenantID: "tenant-1", Name: "strategy-" + strategyID,
		LifecycleState: "paper", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("CreateStrategy(%s): %v", strategyID, err)
	}
}

func newMarks(t *testing.T) *marketdata.Store {
	t.Helper()
	m, err := marketdata.NewStore(60 * time.Second)
	if err != nil {
		t.Fatalf("marketdata.NewStore: %v", err)
	}
	return m
}

func putMark(m *marketdata.Store, symbol, price string, ts time.Time) {
	m.Put(marketdata.Tick{Symbol: symbol, Mark: decimal.RequireFromString(price), TS: ts})
}

// newBridge builds a bridge over st with a mutable clock; fill model:
// 10 bps market slippage, 5 bps taker, 2 bps maker; allocated capital 10000.
func newBridge(t *testing.T, st *store.Store, marks *marketdata.Store, clock *time.Time) *Bridge {
	t.Helper()
	b, err := New(Config{
		Store:                 st,
		Marks:                 marks,
		FillModel:             paper.FillModel{MarketSlippageBps: "10", TakerFeeBps: "5", MakerFeeBps: "2"},
		AllocatedCapitalQuote: decimal.NewFromInt(10000),
		Now:                   func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatalf("omsbridge.New: %v", err)
	}
	return b
}

func mustDec(t *testing.T, v string) contract.Decimal {
	t.Helper()
	d, err := contract.ParseDecimal(v)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", v, err)
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

// testProposal builds a schema-valid proposal; mutate tweaks action-specific
// fields (entry type, stop, TP, size).
func testProposal(t *testing.T, proposalID, strategyID, runID string, action contract.Action, mutate func(*contract.Proposal)) *contract.Proposal {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	p := &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    proposalID, StrategyID: strategyID, AgentTraceID: runID,
		CreatedAt: mustTime(t, "2026-07-04T12:00:00Z"),
		Symbol:    "BTC/USDT", Action: action,
		SizeQuote: mustDec(t, "0"), Entry: contract.Entry{Type: "market"},
		TimeInForce: "gtc", Confidence: 0.9, Reasoning: "test",
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateSummary:    "test",
		ModelCosts: []contract.ModelCost{
			{Node: "trader", Model: "stub", InputTokens: 10, OutputTokens: 5, CostUSD: mustDec(t, "0.001")},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// insertChain persists proposal (at tick) -> approve verdict and returns
// the VerdictMeta the Submitter receives. verdict = uid(base), run/trace id
// is the proposal's AgentTraceID.
func insertChain(t *testing.T, s *store.Store, base, tick int, p *contract.Proposal) store.VerdictMeta {
	t.Helper()
	if _, err := s.InsertProposal(store.ProposalSubmission{TickNumber: tick, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	v := &contract.Verdict{
		SchemaVersion: contract.SchemaVersion,
		VerdictID:     uid(base), ProposalID: p.ProposalID,
		Decision: contract.DecisionApprove, Reasons: []contract.Reason{},
		LimitsSnapshot: contract.LimitsSnapshot{
			SymbolWhitelist:  []string{"BTC/USDT", "ETH/USDT"},
			MaxOpenPositions: 3, PerPositionNotionalCapQuote: mustDec(t, "2000"),
			DailyLossLimitQuote: mustDec(t, "500"), MaxDrawdownPct: 10,
			MaxOrdersPerMinute: 6, RequireStopLoss: true,
			EquityQuote: mustDec(t, "10000"), PeakEquityQuote: mustDec(t, "10000"),
			DailyRealizedPnlQuote: contract.NewSignedDecimal(decimal.Zero),
			MarkPrice:             mustDec(t, "64000"),
		},
		EvaluatedAt: mustTime(t, "2026-07-04T12:00:00Z"),
	}
	if _, err := s.InsertVerdict(v); err != nil {
		t.Fatalf("InsertVerdict: %v", err)
	}
	return store.VerdictMeta{
		VerdictID: v.VerdictID, ProposalID: p.ProposalID, StrategyID: p.StrategyID,
		Symbol: p.Symbol, Decision: string(v.Decision), EvaluatedAt: v.EvaluatedAt.String(),
	}
}
