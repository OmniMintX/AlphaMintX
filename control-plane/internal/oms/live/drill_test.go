package live

import (
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestTestnetDrill_OutageRestart is the normative real-venue drill for PLAN
// Phase 3 exit criterion 1 (live-oms-and-reconciler.md §Test obligations,
// "Non-vacuous evidence for exit criterion 1"): (1) marketable limit orders
// on a liquid testnet symbol; (2) the OMS "dies" between journal-commit and
// ack for one order while at least one fill is un-consumed; (3) a restart;
// (4) the startup run appended run_completed AND >=1 intent_resolved_present
// (orphan adoption) AND >=1 fill_backfilled whose exchange_trade_id matches
// a REAL venue trade id (gap detection) AND per-order SUM(fills.qty_base)
// equals the venue's executedQty; (5) a SECOND restart with zero duplicate
// fills and a watermark that resumes > 0. Zero adopted intents or zero
// backfilled fills FAIL — the criterion cannot be satisfied vacuously.
func TestTestnetDrill_OutageRestart(t *testing.T) {
	apiKey := os.Getenv("CONTROLPLANE_BINANCE_API_KEY")
	apiSecret := os.Getenv("CONTROLPLANE_BINANCE_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		t.Skip("testnet drill: CONTROLPLANE_BINANCE_API_KEY and CONTROLPLANE_BINANCE_API_SECRET not set")
	}
	if env := os.Getenv("CONTROLPLANE_BINANCE_ENV"); env != "" && env != "testnet" {
		t.Skipf("testnet drill: CONTROLPLANE_BINANCE_ENV=%q (the drill trades against testnet only)", env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	drillStart := time.Now().UTC().Add(-time.Minute) // venue-clock margin

	bn := exchange.NewBinance(exchange.EnvTestnet, apiKey, apiSecret, nil)
	st, err := store.Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	nowStr := formatTime(time.Now())
	if err := st.CreateStrategy(store.Strategy{
		StrategyID: uid(1), TenantID: "tenant-1", Name: "drill",
		LifecycleState: "live_l1", CreatedAt: nowStr, UpdatedAt: nowStr,
	}); err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	last := testnetLastPrice(t, ctx)
	marks, err := marketdata.NewStore(5 * time.Minute)
	if err != nil {
		t.Fatalf("marketdata.NewStore: %v", err)
	}
	marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: last, TS: time.Now().UTC()})

	raw, err := bn.ExchangeInfo(ctx, []string{"BTCUSDT"})
	if err != nil {
		t.Fatalf("ExchangeInfo: %v", err)
	}
	sf, err := parseFilters("BTCUSDT", raw["BTCUSDT"])
	if err != nil {
		t.Fatalf("parseFilters: %v", err)
	}
	// Marketable limit: 2% through the book, tick-aligned (spec §Config:
	// testnet fills can be sparse — drills use marketable limit orders on
	// liquid symbols to guarantee executions).
	limit := floorToStep(last.Mul(decimal.RequireFromString("1.02")), sf.tick)
	sizeQuote := sf.minNotional.Mul(decimal.NewFromInt(3))
	if floor := decimal.NewFromInt(20); sizeQuote.LessThan(floor) {
		sizeQuote = floor
	}

	tokens := &recordingTokens{}
	newDrillOMS := func() *OMS {
		t.Helper()
		o, err := New(Config{
			Store: st, Exchange: bn, Symbols: []string{"BTC/USDT"}, Marks: marks,
			AllocatedCapitalQuote: decimal.NewFromInt(10000), VenueEnv: "testnet",
			TokenReader: tokens, Logf: t.Logf,
		})
		if err != nil {
			t.Fatalf("live.New: %v", err)
		}
		return o
	}

	// OMS #1: the mandatory startup reconcile opens the gate, then step (1)
	// — a marketable limit entry through the FULL journal-before-send path.
	oms1 := newDrillOMS()
	if err := oms1.TriggerRun(ctx, false); err != nil {
		t.Fatalf("startup TriggerRun: %v", err)
	}
	p := testProposal(t, uid(10), uid(1), uid(11))
	lp := mustDec(t, limit.String())
	p.SizeQuote = mustDec(t, sizeQuote.String())
	p.Entry.LimitPrice = &lp
	if err := oms1.SubmitApproved(insertChain(t, st, 10, p)); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if len(tokens.minted) == 0 {
		t.Fatal("no intent token minted by the submit path")
	}
	clientID1 := latestAttemptID(t, st, tokens.minted[0])
	waitVenueFilled(t, ctx, bn, clientID1)
	// Order #1's executions are now UN-CONSUMED: oms1 never ran a stream or
	// a second reconcile, so no fill is booked locally (the WS-outage half
	// of step 2).

	// Step (2): order #2 crashes between journal-commit and ack — the
	// journal row commits (invariant 3), the placement HTTP goes out, and
	// the process "dies" before the ack lands: the store keeps pending_new
	// with no exchange_order_id.
	qty2 := floorToStep(sizeQuote.Div(limit), sf.step)
	if qty2.LessThan(sf.minQty) || qty2.Mul(limit).LessThan(sf.minNotional) {
		t.Fatalf("drill qty %s below venue minima (minQty %s, minNotional %s)",
			qty2, sf.minQty, sf.minNotional)
	}
	token2 := randomToken(t)
	clientID2 := attemptID(token2, 0)
	orderID2 := newUUID()
	limitStr := limit.String()
	now2 := formatTime(time.Now())
	if err := st.InsertJournaledOrder(store.Order{
		OrderID: orderID2, Origin: "kill", StrategyID: uid(1), Symbol: "BTC/USDT",
		Class: "ENTRY", Side: "buy", Type: "limit", QtyBase: qty2.String(),
		LimitPrice: &limitStr, Status: "pending_new", SubmittedAt: now2,
	}, store.OrderIntent{
		ClientOrderID: clientID2, IntentToken: token2, Attempt: 0,
		OrderID: orderID2, StrategyID: uid(1), Symbol: "BTC/USDT", VenueSymbol: "BTCUSDT",
		Side: "buy", Type: "limit", QtyBase: qty2.String(), LimitPrice: &limitStr,
		Origin: "kill", JournaledAt: now2,
	}); err != nil {
		t.Fatalf("InsertJournaledOrder: %v", err)
	}
	if _, err := bn.PlaceOrder(ctx, exchange.PlaceRequest{
		VenueSymbol: "BTCUSDT", Side: "BUY", Type: "LIMIT", Qty: qty2.String(),
		Price: limitStr, NewClientOrderID: clientID2, TimeInForce: "GTC",
	}); err != nil {
		t.Fatalf("PlaceOrder (order #2): %v", err)
	}
	waitVenueFilled(t, ctx, bn, clientID2) // the ack itself is DROPPED

	// Step (3): restart — a fresh OMS process over the same durable store.
	doneBefore := len(listEvents(t, st, "run_completed"))
	oms2 := newDrillOMS()
	if err := oms2.TriggerRun(ctx, false); err != nil {
		t.Fatalf("restart TriggerRun: %v", err)
	}

	// Step (4): the startup run appended run_completed...
	if got := len(listEvents(t, st, "run_completed")); got != doneBefore+1 {
		t.Errorf("run_completed rows = %d, want %d", got, doneBefore+1)
	}
	// ...AND >=1 intent_resolved_present adopting the crash-dropped order...
	requireAdopted(t, st, clientID2)
	// ...AND >=1 fill_backfilled whose exchange_trade_id matches a REAL
	// venue trade id...
	venueTrades, err := bn.MyTrades(ctx, "BTCUSDT", 0, drillStart, 0)
	if err != nil {
		t.Fatalf("MyTrades: %v", err)
	}
	requireBackfilledFromVenue(t, st, venueTrades)
	// ...AND per-order SUM(fills.qty_base) == the venue's executedQty.
	requireCumQtyIdentity(t, ctx, st, bn, "BTCUSDT", clientID1)
	requireCumQtyIdentity(t, ctx, st, bn, "BTCUSDT", clientID2)

	// Step (5): a SECOND restart — zero duplicate fills, and the watermark
	// resumes > 0 (never re-booking from the epoch's cold start).
	counts := fillCounts(t, st, clientID1, clientID2)
	oms3 := newDrillOMS()
	if err := oms3.TriggerRun(ctx, false); err != nil {
		t.Fatalf("second-restart TriggerRun: %v", err)
	}
	requireNoDuplicateFills(t, st, counts, clientID1, clientID2)
	requireWatermarkResumes(t, st, "BTCUSDT")
}

// TestFakeDrill_OutageRestart is the CI-provable twin of the testnet drill:
// the SAME five steps and assertions against the deterministic fake venue,
// so exit criterion 1's evidence is reproducible without venue keys.
func TestFakeDrill_OutageRestart(t *testing.T) {
	e := newEnv(t)
	e.reconcile() // OMS #1: the mandatory startup reconcile opens the gate

	// Step (1): a marketable limit entry through the FULL journal path.
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	clientID1 := idN(1, 0)

	// Step (2): the outage. Order #1 executes twice at the venue while the
	// stream is never consumed; order #2 crashes between journal-commit and
	// ack — journaled locally, resting AND filled at the venue, no ack.
	if err := e.venue.Fill(clientID1, "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if err := e.venue.Fill(clientID1, "0.01062", "64100"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	_, intent2 := e.journalOrder(tokenN(9))
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: intent2.ClientOrderID, Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	if err := e.venue.Fill(intent2.ClientOrderID, "0.01562", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	// Step (3): restart — a second OMS over the same durable store.
	e.oms = e.newOMS()
	e.reconcile()

	// Step (4): the startup run appended run_completed...
	if done := e.events("run_completed"); len(done) != 2 {
		t.Errorf("run_completed rows = %d, want 2 (first start + restart)", len(done))
	}
	// ...AND >=1 intent_resolved_present adopting the crash-dropped order...
	requireAdopted(t, e.st, intent2.ClientOrderID)
	// ...AND >=1 fill_backfilled whose exchange_trade_id matches a REAL
	// fake-venue trade id...
	venueTrades, err := e.venue.MyTrades(context.Background(), "BTCUSDT", 0, time.Time{}, 0)
	if err != nil {
		t.Fatalf("MyTrades: %v", err)
	}
	requireBackfilledFromVenue(t, e.st, venueTrades)
	// ...AND per-order SUM(fills.qty_base) == the venue's executedQty.
	requireCumQtyIdentity(t, context.Background(), e.st, e.venue, "BTCUSDT", clientID1)
	requireCumQtyIdentity(t, context.Background(), e.st, e.venue, "BTCUSDT", intent2.ClientOrderID)
	if got := len(fillsOf(t, e.st, clientID1)); got != 2 {
		t.Errorf("order #1 fills = %d, want the 2 outage executions booked", got)
	}
	if got := len(fillsOf(t, e.st, intent2.ClientOrderID)); got != 1 {
		t.Errorf("order #2 fills = %d, want the 1 adopted execution booked", got)
	}

	// Step (5): a SECOND restart — zero duplicate fills, watermark > 0.
	counts := fillCounts(t, e.st, clientID1, intent2.ClientOrderID)
	e.oms = e.newOMS()
	e.reconcile()
	requireNoDuplicateFills(t, e.st, counts, clientID1, intent2.ClientOrderID)
	requireWatermarkResumes(t, e.st, "BTCUSDT")
	if done := e.events("run_completed"); len(done) != 3 {
		t.Errorf("run_completed rows after the second restart = %d, want 3", len(done))
	}
}

// listEvents returns the recon audit rows of one kind (store-level twin of
// env.events for the drills that run without the fake harness).
func listEvents(t *testing.T, st *store.Store, kind string) []store.OMSReconEvent {
	t.Helper()
	out, err := st.ListOMSReconEvents(store.OMSReconEventFilter{Kind: kind})
	if err != nil {
		t.Fatalf("ListOMSReconEvents(%s): %v", kind, err)
	}
	return out
}

// fillsOf returns the booked fills of the order whose LATEST attempt id is
// clientOrderID.
func fillsOf(t *testing.T, st *store.Store, clientOrderID string) []store.VenueFill {
	t.Helper()
	ord, err := st.GetLiveOrderByClientOrderID(clientOrderID)
	if err != nil {
		t.Fatalf("GetLiveOrderByClientOrderID(%s): %v", clientOrderID, err)
	}
	fills, err := st.ListFillsByOrder(ord.OrderID)
	if err != nil {
		t.Fatalf("ListFillsByOrder(%s): %v", ord.OrderID, err)
	}
	return fills
}

// fillCounts snapshots the booked-fill count per client order id (the
// second-restart zero-duplicates baseline).
func fillCounts(t *testing.T, st *store.Store, clientOrderIDs ...string) map[string]int {
	t.Helper()
	out := make(map[string]int, len(clientOrderIDs))
	for _, id := range clientOrderIDs {
		out[id] = len(fillsOf(t, st, id))
	}
	return out
}

// requireAdopted asserts >=1 intent_resolved_present exists (orphan
// adoption) and that the crash-dropped attempt id is among the adopted.
// Zero adoptions FAIL the drill — never satisfied vacuously.
func requireAdopted(t *testing.T, st *store.Store, clientOrderID string) {
	t.Helper()
	adopted := listEvents(t, st, "intent_resolved_present")
	if len(adopted) == 0 {
		t.Fatal("intent_resolved_present events = 0: zero adopted intents cannot satisfy exit criterion 1")
	}
	for _, ev := range adopted {
		if ev.ClientOrderID != nil && *ev.ClientOrderID == clientOrderID {
			return
		}
	}
	t.Errorf("no intent_resolved_present row adopted %s", clientOrderID)
}

// requireBackfilledFromVenue asserts >=1 fill_backfilled exists (gap
// detection) and every one carries an exchange_trade_id present in the
// venue's own trade history. Zero backfills FAIL the drill.
func requireBackfilledFromVenue(t *testing.T, st *store.Store, venueTrades []exchange.Trade) {
	t.Helper()
	backfilled := listEvents(t, st, "fill_backfilled")
	if len(backfilled) == 0 {
		t.Fatal("fill_backfilled events = 0: zero backfilled fills cannot satisfy exit criterion 1")
	}
	venueIDs := make(map[int64]bool, len(venueTrades))
	for _, tr := range venueTrades {
		venueIDs[tr.ID] = true
	}
	for _, ev := range backfilled {
		if ev.ExchangeTradeID == nil || !venueIDs[*ev.ExchangeTradeID] {
			t.Errorf("fill_backfilled %s: exchange_trade_id %v is not a real venue trade id",
				ev.EventID, ev.ExchangeTradeID)
		}
	}
}

// requireCumQtyIdentity asserts the exchange-is-truth identity for one
// order: SUM(fills.qty_base) equals the venue's executedQty.
func requireCumQtyIdentity(t *testing.T, ctx context.Context, st *store.Store, ex exchange.Exchange, venueSym, clientOrderID string) {
	t.Helper()
	state, err := ex.QueryOrder(ctx, venueSym, clientOrderID)
	if err != nil {
		t.Fatalf("QueryOrder(%s): %v", clientOrderID, err)
	}
	sum := decimal.Zero
	for _, f := range fillsOf(t, st, clientOrderID) {
		sum = sum.Add(decimal.RequireFromString(f.QtyBase))
	}
	if want := decimal.RequireFromString(state.ExecutedQty); !sum.Equal(want) {
		t.Errorf("order %s: SUM(fills.qty_base) = %s, want venue executedQty %s",
			clientOrderID, sum, want)
	}
}

// requireNoDuplicateFills asserts a restart re-booked NOTHING: per-order
// fill counts are unchanged and no two fills share a venue trade identity.
func requireNoDuplicateFills(t *testing.T, st *store.Store, before map[string]int, clientOrderIDs ...string) {
	t.Helper()
	seen := make(map[int64]string)
	for _, id := range clientOrderIDs {
		fills := fillsOf(t, st, id)
		if len(fills) != before[id] {
			t.Errorf("order %s fills after the second restart = %d, want still %d",
				id, len(fills), before[id])
		}
		for _, f := range fills {
			if prev, dup := seen[f.ExchangeTradeID]; dup {
				t.Errorf("exchange_trade_id %d booked twice (fills %s and %s)",
					f.ExchangeTradeID, prev, f.FillID)
			}
			seen[f.ExchangeTradeID] = f.FillID
		}
	}
}

// requireWatermarkResumes asserts the current epoch's fill watermark is
// warm (> 0): the second restart resumed from MAX(exchange_trade_id), not
// from the epoch's cold start.
func requireWatermarkResumes(t *testing.T, st *store.Store, venueSym string) {
	t.Helper()
	epoch, ok, err := st.CurrentVenueEpoch()
	if err != nil || !ok {
		t.Fatalf("CurrentVenueEpoch: ok=%v err=%v", ok, err)
	}
	wm, warm, err := st.FillWatermark(epoch.VenueEpoch, venueSym)
	if err != nil {
		t.Fatalf("FillWatermark: %v", err)
	}
	if !warm || wm <= 0 {
		t.Errorf("fill watermark = %d (warm=%v), want > 0 after the drill's fills", wm, warm)
	}
}

// recordingTokens draws real CSPRNG intent tokens and records each mint so
// the drill can address the venue orders the OMS created: testnet retains
// client ids across runs, so a deterministic source would collide.
type recordingTokens struct{ minted []string }

func (r *recordingTokens) Read(p []byte) (int, error) {
	if _, err := io.ReadFull(crand.Reader, p); err != nil {
		return 0, err
	}
	r.minted = append(r.minted, base64.RawURLEncoding.EncodeToString(p))
	return len(p), nil
}

// randomToken mints one CSPRNG intent token outside the OMS (the
// crash-dropped order is journaled by the test, not by the submit path).
func randomToken(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := io.ReadFull(crand.Reader, b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// latestAttemptID resolves the attempt id that ended up on the orders row
// for one intent token (ambiguity resolution may have advanced attempts).
func latestAttemptID(t *testing.T, st *store.Store, token string) string {
	t.Helper()
	for a := 0; a <= 9; a++ {
		id := attemptID(token, a)
		if _, err := st.GetLiveOrderByClientOrderID(id); err == nil {
			return id
		}
	}
	t.Fatalf("no orders row carries an attempt id of token %s", token)
	return ""
}

// testnetLastPrice reads the venue's public last price for BTCUSDT.
func testnetLastPrice(t *testing.T, ctx context.Context) decimal.Decimal {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://testnet.binance.vision/api/v3/ticker/price?symbol=BTCUSDT", nil)
	if err != nil {
		t.Fatalf("ticker request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ticker fetch: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Price string `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("ticker decode: %v", err)
	}
	price, err := decimal.NewFromString(out.Price)
	if err != nil || price.Sign() <= 0 {
		t.Fatalf("ticker price %q: %v", out.Price, err)
	}
	return price
}

// waitVenueFilled polls QueryOrder until the marketable limit reaches
// FILLED (testnet executions can lag). A drill without executions cannot be
// non-vacuous, so the timeout FAILS instead of skipping.
func waitVenueFilled(t *testing.T, ctx context.Context, ex exchange.Exchange, clientOrderID string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	status := "unknown"
	for time.Now().Before(deadline) {
		state, err := ex.QueryOrder(ctx, "BTCUSDT", clientOrderID)
		if err == nil {
			if state.Status == "FILLED" {
				return
			}
			status = state.Status
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("order %s never reached FILLED on testnet (last status %s): no executions means a vacuous drill",
		clientOrderID, status)
}
