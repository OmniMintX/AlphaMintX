package live

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	mrand "math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange/fake"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

// seqTokens is the deterministic intent-token source: the n-th token is 16
// bytes of value n, so attempt ids are predictable (idN).
type seqTokens struct{ n byte }

func (s *seqTokens) Read(p []byte) (int, error) {
	s.n++
	for i := range p {
		p[i] = s.n
	}
	return len(p), nil
}

// tokenN is the token seqTokens yields on its n-th read.
func tokenN(n byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{n}, 16))
}

// idN is the deterministic clientOrderId of token n, attempt a.
func idN(n byte, attempt int) string { return attemptID(tokenN(n), attempt) }

// env is one scenario harness: a real store, the scripted fake venue, a
// fresh-mark cache, and a live OMS with deterministic tokens, a settable
// fixed clock (shared with the venue), and a recording no-op sleeper.
type env struct {
	t      *testing.T
	st     *store.Store
	venue  *fake.Venue
	marks  *marketdata.Store
	oms    *OMS
	slept  []time.Duration
	now    time.Time
	tokens *seqTokens // shared across restarts so attempt ids never collide
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.CreateStrategy(store.Strategy{
		StrategyID: uid(1), TenantID: "tenant-1", Name: "s1",
		LifecycleState: "live_l1", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	marks, err := marketdata.NewStore(60 * time.Second)
	if err != nil {
		t.Fatalf("marketdata.NewStore: %v", err)
	}
	marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("64000"), TS: testNow})
	e := &env{t: t, st: st, venue: fake.NewVenue(), marks: marks,
		now: testNow, tokens: &seqTokens{}}
	e.venue.SetNow(func() time.Time { return e.now })
	e.oms = e.newOMS()
	return e
}

// newOMS builds a live OMS over the env's store and venue (also used to
// simulate a restart: a SECOND OMS over the same durable state).
func (e *env) newOMS() *OMS { return e.newOMSWith(Tuning{}) }

// newOMSWith is newOMS with explicit tuning (the zero value means the
// normative defaults).
func (e *env) newOMSWith(tun Tuning) *OMS {
	e.t.Helper()
	o, err := New(Config{
		Store:                 e.st,
		Exchange:              e.venue,
		Symbols:               []string{"BTC/USDT"},
		Marks:                 e.marks,
		AllocatedCapitalQuote: decimal.NewFromInt(10000),
		Tuning:                tun,
		Now:                   func() time.Time { return e.now },
		TokenReader:           e.tokens,
		Sleep:                 func(d time.Duration) { e.slept = append(e.slept, d) },
		Rand:                  mrand.New(mrand.NewSource(1)), // deterministic jitter
		Logf:                  e.t.Logf,
	})
	if err != nil {
		e.t.Fatalf("live.New: %v", err)
	}
	return o
}

// reconcile runs one synchronous reconcile (startup or on-demand).
func (e *env) reconcile() {
	e.t.Helper()
	if err := e.oms.TriggerRun(context.Background(), false); err != nil {
		e.t.Fatalf("TriggerRun: %v", err)
	}
}

// events returns the recon audit rows of one kind.
func (e *env) events(kind string) []store.OMSReconEvent {
	e.t.Helper()
	out, err := e.st.ListOMSReconEvents(store.OMSReconEventFilter{Kind: kind})
	if err != nil {
		e.t.Fatalf("ListOMSReconEvents(%s): %v", kind, err)
	}
	return out
}

// order fetches the orders row whose LATEST attempt id is clientOrderID.
func (e *env) order(clientOrderID string) store.LiveOrder {
	e.t.Helper()
	ord, err := e.st.GetLiveOrderByClientOrderID(clientOrderID)
	if err != nil {
		e.t.Fatalf("order(%s): %v", clientOrderID, err)
	}
	return ord
}

// fills returns the booked fills of the order addressed by clientOrderID's
// journal row (attributes superseded attempt ids too).
func (e *env) fills(clientOrderID string) []store.VenueFill {
	e.t.Helper()
	intent, err := e.st.GetOrderIntent(clientOrderID)
	if err != nil {
		e.t.Fatalf("GetOrderIntent(%s): %v", clientOrderID, err)
	}
	out, err := e.st.ListFillsByOrder(intent.OrderID)
	if err != nil {
		e.t.Fatalf("ListFillsByOrder(%s): %v", intent.OrderID, err)
	}
	return out
}

// submitEntry submits one gate-approved limit entry through the full
// SubmitApproved seam: proposal (at tick) -> approve verdict -> submit.
// base numbers the chain's ids/tick so repeated submissions never collide.
func (e *env) submitEntry(base int) error {
	return e.submitEntryWith(base, nil)
}

// submitEntryWith is submitEntry after applying mutate to the proposal
// (protective stops, size overrides).
func (e *env) submitEntryWith(base int, mutate func(*contract.Proposal)) error {
	e.t.Helper()
	p := testProposal(e.t, uid(base), uid(1), uid(base+1))
	if mutate != nil {
		mutate(p)
	}
	meta := insertChain(e.t, e.st, base, p)
	return e.oms.SubmitApproved(meta)
}

// position returns the strategy's BTC/USDT book, ok=false when absent.
func (e *env) position() (store.Position, bool) {
	e.t.Helper()
	rows, err := e.st.ListPositions(uid(1))
	if err != nil {
		e.t.Fatalf("ListPositions: %v", err)
	}
	for _, p := range rows {
		if p.Symbol == "BTC/USDT" {
			return p, true
		}
	}
	return store.Position{}, false
}

// venueOpen lists the venue's open BTCUSDT orders.
func (e *env) venueOpen() []exchange.OrderState {
	e.t.Helper()
	open, err := e.venue.OpenOrders(context.Background(), "BTCUSDT")
	if err != nil {
		e.t.Fatalf("OpenOrders: %v", err)
	}
	return open
}

// journalOrder writes the crash state "journaled, never sent": the
// pending_new orders row plus its attempt-0 intent, exactly as
// journalAndSend commits them (origin kill: no proposal chain needed).
func (e *env) journalOrder(token string) (string, store.OrderIntent) {
	e.t.Helper()
	orderID := newUUID()
	row := store.Order{
		OrderID: orderID, Origin: "kill", StrategyID: uid(1), Symbol: "BTC/USDT",
		Class: "ENTRY", Side: "buy", Type: "limit", QtyBase: "0.01562",
		Status: "pending_new", SubmittedAt: formatTime(testNow),
	}
	limit := "64000"
	row.LimitPrice = &limit
	intent := store.OrderIntent{
		ClientOrderID: attemptID(token, 0), IntentToken: token, Attempt: 0,
		OrderID: orderID, StrategyID: uid(1), Symbol: "BTC/USDT", VenueSymbol: "BTCUSDT",
		Side: "buy", Type: "limit", QtyBase: "0.01562", LimitPrice: &limit,
		Origin: "kill", JournaledAt: formatTime(testNow),
	}
	if err := e.st.InsertJournaledOrder(row, intent); err != nil {
		e.t.Fatalf("InsertJournaledOrder: %v", err)
	}
	return orderID, intent
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

// testProposal builds a schema-valid open_long limit entry proposal
// (size 1000 quote at limit 64000 => normalized qty 0.01562).
func testProposal(t *testing.T, proposalID, strategyID, runID string) *contract.Proposal {
	t.Helper()
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	limit := mustDec(t, "64000")
	return &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    proposalID, StrategyID: strategyID, AgentTraceID: runID,
		CreatedAt: mustTime(t, "2026-07-04T12:00:00Z"),
		Symbol:    "BTC/USDT", Action: contract.ActionOpenLong,
		SizeQuote: mustDec(t, "1000"), Entry: contract.Entry{Type: "limit", LimitPrice: &limit},
		TimeInForce: "gtc", Confidence: 0.9, Reasoning: "test",
		AnalystSummaries: contract.AnalystSummaries{Market: sum, News: sum, Fundamental: sum},
		DebateSummary:    "test",
		ModelCosts: []contract.ModelCost{
			{Node: "trader", Model: "stub", InputTokens: 10, OutputTokens: 5, CostUSD: mustDec(t, "0.001")},
		},
	}
}

// insertChain persists proposal (tick = base) -> approve verdict and
// returns the VerdictMeta the Submitter receives.
func insertChain(t *testing.T, s *store.Store, base int, p *contract.Proposal) store.VerdictMeta {
	t.Helper()
	if _, err := s.InsertProposal(store.ProposalSubmission{TickNumber: base, Proposal: p}, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	v := &contract.Verdict{
		SchemaVersion: contract.SchemaVersion,
		VerdictID:     uid(base + 2), ProposalID: p.ProposalID,
		Decision: contract.DecisionApprove, Reasons: []contract.Reason{},
		LimitsSnapshot: contract.LimitsSnapshot{
			SymbolWhitelist:  []string{"BTC/USDT"},
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
