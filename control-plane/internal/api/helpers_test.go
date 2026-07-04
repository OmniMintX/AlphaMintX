package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

const (
	readTok   = "test-read-token"
	opTok     = "test-operator-token"
	agent1Tok = "test-agent-token-1"
	agent2Tok = "test-agent-token-2"
)

var (
	testNow = time.Date(2026, 7, 4, 12, 30, 0, 0, time.UTC)
	strat1  = uid(1)
	strat2  = uid(2)
)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

type testEnv struct {
	store *store.Store
	marks *marketdata.Store
	sub   *stubSubmitter
	srv   *Server
}

// newEnv builds a server over a fresh store with all three token classes
// configured (agent tokens scoped to strat1 and strat2) and a fixed clock.
func newEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	marks, err := marketdata.NewStore(60 * time.Second)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sub := &stubSubmitter{}
	cfg := Config{
		Store:             st,
		Marks:             marks,
		Submitter:         sub,
		ReadToken:         readTok,
		OperatorToken:     opTok,
		OperatorPrincipal: "trader-1",
		AgentTokens:       map[string]string{strat1: agent1Tok, strat2: agent2Tok},
		Now:               func() time.Time { return testNow },
		Logf:              func(string, ...any) {},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &testEnv{store: st, marks: marks, sub: sub, srv: New(cfg)}
}

// do performs one request; body may be nil, []byte, or any JSON-marshalable
// value. token=="" sends no Authorization header.
func (e *testEnv) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// wantError asserts status + error code on an error response.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	var body errorBody
	decodeJSON(t, rec, &body)
	if body.Code != code {
		t.Fatalf("code = %q, want %q", body.Code, code)
	}
}

type stubSubmitter struct {
	mu    sync.Mutex
	err   error // returned by SubmitApproved (tests inject OMS failures)
	calls []store.VerdictMeta
}

func (s *stubSubmitter) SubmitApproved(m store.VerdictMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, m)
	return s.err
}

func (s *stubSubmitter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
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

func createStrategy(t *testing.T, s *store.Store, strategyID, state string) {
	t.Helper()
	err := s.CreateStrategy(store.Strategy{
		StrategyID: strategyID, TenantID: "tenant-1", Name: "strategy-" + strategyID,
		LifecycleState: state, CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
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

// insertChain persists proposal (at tick) -> verdict for an existing
// strategy and returns the ids used: proposal = uid(base), verdict =
// uid(base+1), run = uid(base+2).
func insertChain(t *testing.T, s *store.Store, base int, strategyID string, tick int) (proposalID, verdictID, runID string) {
	t.Helper()
	proposalID, verdictID, runID = uid(base), uid(base+1), uid(base+2)
	p := testProposal(t, proposalID, strategyID, runID)
	if _, err := s.InsertProposal(store.ProposalSubmission{TickNumber: tick, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	if _, err := s.InsertVerdict(testVerdict(t, verdictID, proposalID)); err != nil {
		t.Fatalf("InsertVerdict: %v", err)
	}
	return proposalID, verdictID, runID
}

func testTraceEnvelope(t *testing.T, strategyID, runID string, proposalID *string) *store.TraceEnvelope {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	return &store.TraceEnvelope{
		SchemaVersion: contract.SchemaVersion, StrategyID: strategyID, RunID: runID, TickNumber: 0,
		StartedAt:        mustTime(t, "2026-07-04T12:00:00Z"),
		CompletedAt:      mustTime(t, "2026-07-04T12:00:09Z"),
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateRounds: []store.DebateRound{{
			RoundIndex: 0, BullArgument: "up", BullScore: 0.6, BearArgument: "down", BearScore: 0.4,
		}},
		DebateSummary: "no edge either way",
		ProposalID:    proposalID,
		ModelCosts: []contract.ModelCost{
			{Node: "trader", Model: "stub", InputTokens: 100, OutputTokens: 20, CostUSD: mustDec(t, "0.001")},
		},
		BudgetState: store.BudgetState{UTCDate: "2026-07-04", TokensUsed: 120, CostUSDUsed: mustDec(t, "0.001")},
	}
}
