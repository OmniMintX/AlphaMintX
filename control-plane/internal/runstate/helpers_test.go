package runstate

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "control-plane.db"))
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
		LifecycleState: "live_l3", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("CreateStrategy(%s): %v", strategyID, err)
	}
}

func newHydrator(t *testing.T, s *store.Store) (*Hydrator, *marketdata.Store) {
	t.Helper()
	marks, err := marketdata.NewStore(60 * time.Second)
	if err != nil {
		t.Fatalf("marketdata.NewStore: %v", err)
	}
	return &Hydrator{Store: s, Marks: marks, AllocatedCapitalQuote: decimal.NewFromInt(10000)}, marks
}

func putMark(m *marketdata.Store, symbol, price string, ts time.Time) {
	m.Put(marketdata.Tick{Symbol: symbol, Mark: decimal.RequireFromString(price), TS: ts})
}

func upsertPosition(t *testing.T, s *store.Store, strategyID, symbol, qty, entry string) {
	t.Helper()
	err := s.UpsertPosition(store.Position{
		StrategyID: strategyID, Symbol: symbol, QtyBase: qty, EntryPrice: entry,
		FeesQuote: "0", RealizedPnLQuote: "0", UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("UpsertPosition: %v", err)
	}
}

func upsertState(t *testing.T, s *store.Store, strategyID, equity, peak, daily, utcDate string) {
	t.Helper()
	err := s.UpsertStrategyState(store.StrategyState{
		StrategyID: strategyID, EquityQuote: equity, PeakEquityQuote: peak,
		DailyRealizedPnLQuote: daily, UTCDate: utcDate, UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("UpsertStrategyState: %v", err)
	}
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

// insertVerdictChain persists a proposal (at tick) and its verdict with the
// given action, decision, and evaluated_at, for rate-window counting.
func insertVerdictChain(t *testing.T, s *store.Store, base, tick int, strategyID string, action contract.Action, decision contract.Decision, evaluatedAt string) {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	p := &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    uid(base), StrategyID: strategyID, AgentTraceID: uid(base + 2),
		CreatedAt: mustTime(t, "2026-07-04T11:59:00Z"),
		Symbol:    "BTC/USDT", Action: action,
		SizeQuote: mustDec(t, "0"), Entry: contract.Entry{Type: "market"},
		TimeInForce: "gtc", Confidence: 0.9, Reasoning: "test",
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateSummary:    "test",
		ModelCosts: []contract.ModelCost{
			{Node: "trader", Model: "stub", InputTokens: 10, OutputTokens: 5, CostUSD: mustDec(t, "0.001")},
		},
	}
	if _, err := s.InsertProposal(store.ProposalSubmission{TickNumber: tick, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	v := &contract.Verdict{
		SchemaVersion: contract.SchemaVersion,
		VerdictID:     uid(base + 1), ProposalID: uid(base),
		Decision: decision, Reasons: []contract.Reason{},
		LimitsSnapshot: contract.LimitsSnapshot{
			SymbolWhitelist:  []string{"BTC/USDT"},
			MaxOpenPositions: 3, PerPositionNotionalCapQuote: mustDec(t, "2000"),
			DailyLossLimitQuote: mustDec(t, "500"), MaxDrawdownPct: 10,
			MaxOrdersPerMinute: 6, RequireStopLoss: true,
			EquityQuote: mustDec(t, "10000"), PeakEquityQuote: mustDec(t, "10000"),
			DailyRealizedPnlQuote: contract.NewSignedDecimal(decimal.Zero),
			MarkPrice:             mustDec(t, "64000"),
		},
		EvaluatedAt: mustTime(t, evaluatedAt),
	}
	if _, err := s.InsertVerdict(v); err != nil {
		t.Fatalf("InsertVerdict: %v", err)
	}
}
