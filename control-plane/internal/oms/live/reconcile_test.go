package live

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// S1: crash after send, before ack — the journal has the intent, the venue
// has the order. The startup reconcile ADOPTS it (orphan adoption).
func TestReconcile_IntentResolvedPresent(t *testing.T) {
	e := newEnv(t)
	_, intent := e.journalOrder(tokenN(9))
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: intent.ClientOrderID, Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()

	ord := e.order(intent.ClientOrderID)
	if ord.Status != "open" || ord.ExchangeOrderID == nil {
		t.Errorf("adopted order = status %s, ack %v; want open, acked", ord.Status, ord.ExchangeOrderID)
	}
	if evs := e.events("intent_resolved_present"); len(evs) != 1 {
		t.Errorf("intent_resolved_present events = %d, want 1", len(evs))
	}
	done := e.events("run_completed")
	if len(done) != 1 || done[0].DetailsJSON == "" {
		t.Fatalf("run_completed = %+v, want 1", done)
	}
	if !e.oms.Reconciled() {
		t.Error("OMS still closed after a completed startup reconcile")
	}
}

// S2: crash after journal; the venue never received the order — resolved
// as a never-acked absence (rejected; the id is retired).
func TestReconcile_IntentResolvedAbsent(t *testing.T) {
	e := newEnv(t)
	_, intent := e.journalOrder(tokenN(9))
	e.reconcile()

	if ord := e.order(intent.ClientOrderID); ord.Status != "rejected" {
		t.Errorf("order status = %s, want rejected", ord.Status)
	}
	if evs := e.events("intent_resolved_absent"); len(evs) != 1 {
		t.Errorf("intent_resolved_absent events = %d, want 1", len(evs))
	}
}

// S5: fills executed during a WS outage are backfilled from the watermark,
// each booked EXACTLY once; a second run books nothing.
func TestReconcile_GapBackfill(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	// Two venue executions; the stream is never consumed (outage).
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64100"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()

	fills := e.fills(idN(1, 0))
	if len(fills) != 2 || fills[0].ExchangeTradeID != 1 || fills[1].ExchangeTradeID != 2 {
		t.Fatalf("fills = %+v, want venue trades 1 and 2 booked once each", fills)
	}
	ord := e.order(idN(1, 0))
	if ord.Status != "partially_filled" || ord.FillPrice == nil || *ord.FillPrice != "64050" {
		t.Errorf("order = status %s, fill_price %v; want partially_filled at VWAP 64050", ord.Status, ord.FillPrice)
	}
	pos, ok := e.position()
	if !ok || pos.QtyBase != "0.01" || pos.EntryPrice != "64050" {
		t.Errorf("position = %+v ok=%v, want qty 0.01 entry 64050", pos, ok)
	}
	// Replay run: the watermark and dedup make it a no-op.
	e.reconcile()
	if again := e.fills(idN(1, 0)); len(again) != 2 {
		t.Errorf("fills after second run = %d, want still 2", len(again))
	}
}

// S7: partial fill, RESTART (a second OMS over the same store and venue),
// remainder fills; the derived executed quantity converges to the venue's.
func TestReconcile_PartialFillRestart(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	if ord := e.order(idN(1, 0)); ord.Status != "partially_filled" {
		t.Fatalf("pre-restart status = %s, want partially_filled", ord.Status)
	}

	e.oms = e.newOMS() // restart: fresh process over the durable state
	e.reconcile()      // startup reconcile
	if err := e.venue.Fill(idN(1, 0), "0.01062", "64000"); err != nil {
		t.Fatalf("Fill remainder: %v", err)
	}
	e.reconcile()

	fills := e.fills(idN(1, 0))
	if len(fills) != 2 {
		t.Fatalf("fills = %d, want 2 (no duplicates across restart)", len(fills))
	}
	sum, err := e.oms.bookedQty(e.order(idN(1, 0)).OrderID)
	if err != nil {
		t.Fatalf("bookedQty: %v", err)
	}
	if sum.String() != "0.01562" {
		t.Errorf("derived executed qty = %s, want 0.01562 (== venue executedQty)", sum)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "filled" {
		t.Errorf("status = %s, want filled", ord.Status)
	}
	if evs := e.events("cum_qty_mismatch"); len(evs) != 0 {
		t.Errorf("cum_qty_mismatch = %d, want 0 (converged)", len(evs))
	}
}

// S8: cancel acked first, the pre-cancel partial execution arrives later in
// backfill — the fill books normally, status KEEPS canceled (venue truth),
// and fill_after_terminal is appended.
func TestReconcile_CancelFillRace(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if _, err := e.venue.CancelOrder(context.Background(), "BTCUSDT", idN(1, 0)); err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	e.reconcile()

	ord := e.order(idN(1, 0))
	if ord.Status != "canceled" {
		t.Errorf("status = %s, want canceled KEPT after the late fill", ord.Status)
	}
	if fills := e.fills(idN(1, 0)); len(fills) != 1 || fills[0].QtyBase != "0.005" {
		t.Errorf("fills = %+v, want the pre-cancel execution booked", fills)
	}
	if evs := e.events("fill_after_terminal"); len(evs) != 1 {
		t.Errorf("fill_after_terminal events = %d, want 1", len(evs))
	}
	if pos, ok := e.position(); !ok || pos.QtyBase != "0.005" {
		t.Errorf("position = %+v ok=%v, want the real 0.005 exposure", pos, ok)
	}
}

// S10: out-of-namespace venue orders are NEVER canceled or adopted — one
// foreign_order_ignored sighting per order id per run.
func TestReconcile_ForeignOrderIgnored(t *testing.T) {
	e := newEnv(t)
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: "human-order-1", Status: "NEW",
		Side: "SELL", Type: "LIMIT", Price: "70000", OrigQty: "1",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()

	if evs := e.events("foreign_order_ignored"); len(evs) != 1 {
		t.Fatalf("foreign_order_ignored events = %d, want 1 per run", len(evs))
	}
	if got := e.venueOpen(); len(got) != 1 || got[0].ClientOrderID != "human-order-1" {
		t.Fatalf("venue open = %+v, want the foreign order untouched", got)
	}
	// A stream event for the foreign order is likewise ignored.
	if err := e.oms.handleUserEvent(exchange.UserEvent{
		Kind: exchange.UserEventExecutionReport, VenueSymbol: "BTCUSDT",
		ClientOrderID: "human-order-1", ExecType: "TRADE", OrderStatus: "PARTIALLY_FILLED",
		LastQty: "0.5", LastPrice: "70000", TradeID: 7, EventTime: testNow,
	}); err != nil {
		t.Fatalf("handleUserEvent: %v", err)
	}
	orders, err := e.st.ListNonTerminalLiveOrders()
	if err != nil {
		t.Fatalf("ListNonTerminalLiveOrders: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("local live orders = %d, want 0 (never adopted)", len(orders))
	}
}

// S11: the Reconciler revokes a claimed-but-unsent attempt; the sender's
// pre-transmit re-check then MUST NOT transmit (claims beat clocks).
func TestReconcile_ClaimRevokeBlocksSend(t *testing.T) {
	e := newEnv(t)
	_, intent := e.journalOrder(tokenN(9))
	// The sender claimed, then stalled (backoff/crash window).
	if ok, err := e.st.RecordIntentClaim(intent.ClientOrderID, formatTime(testNow)); err != nil || !ok {
		t.Fatalf("RecordIntentClaim = %v, %v; want claimed", ok, err)
	}
	// The Reconciler revokes first and resolves the absence.
	e.reconcile()
	if ord := e.order(intent.ClientOrderID); ord.Status != "rejected" {
		t.Fatalf("order status = %s, want rejected (resolved absent)", ord.Status)
	}
	// The stalled sender wakes up and runs its pre-transmit re-check.
	next, err := e.oms.transmit(context.Background(), intent)
	if next != nil || err != nil {
		t.Fatalf("transmit = %v, %v; want nil, nil (revoker owns resolution)", next, err)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open = %d, want 0 (the revoked claim never transmitted)", len(got))
	}
}

// S11 (late-send variant): a late send slips out AFTER revocation — the
// next run detects the venue order via its journal row and cancels it with
// reason late_send_detected.
func TestReconcile_LateSendDetected(t *testing.T) {
	e := newEnv(t)
	_, intent := e.journalOrder(tokenN(9))
	if ok, err := e.st.RecordIntentClaim(intent.ClientOrderID, formatTime(testNow)); err != nil || !ok {
		t.Fatalf("RecordIntentClaim = %v, %v; want claimed", ok, err)
	}
	e.reconcile() // revokes + resolves absent (rejected)

	// The HTTP was already on the wire: the order materializes at the venue.
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: intent.ClientOrderID, Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()

	evs := e.events("orphan_canceled")
	if len(evs) != 1 || evs[0].DetailsJSON == "" {
		t.Fatalf("orphan_canceled events = %+v, want 1", evs)
	}
	if want := `"reason":"late_send_detected"`; !strings.Contains(evs[0].DetailsJSON, want) {
		t.Errorf("orphan_canceled details = %s, want %s", evs[0].DetailsJSON, want)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open = %d, want 0 (late send canceled)", len(got))
	}
}

// S18: a previously ACKED order returning NotFound is the venue-reset
// alarm: venue_reset + expired + RECONCILE_PENDING; ALL sends refused.
func TestReconcile_VenueResetDetect(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile() // books the partial; order partially_filled
	e.venue.Reset()
	e.reconcile()

	evs := e.events("venue_reset")
	if len(evs) != 1 {
		t.Fatalf("venue_reset events = %d, want 1", len(evs))
	}
	// AN-1a companion alert: same kind, ref_id = the recon event_id,
	// committed with the event (alert-notifier.md).
	alerts, err := e.st.ListSafetyAlerts(store.SafetyAlertFilter{Kind: "venue_reset"})
	if err != nil {
		t.Fatalf("ListSafetyAlerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].RefID == nil || *alerts[0].RefID != evs[0].EventID {
		t.Fatalf("companion alerts = %+v, want one venue_reset alert with ref_id %s",
			alerts, evs[0].EventID)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "expired" {
		t.Errorf("order status = %s, want expired (never a quiet reject)", ord.Status)
	}
	if e.oms.Reconciled() {
		t.Error("OMS accepts sends during RECONCILE_PENDING-due-to-reset")
	}
	if err := e.submitEntry(20); !errors.Is(err, ErrReconcilePending) {
		t.Errorf("submit during reset err = %v, want ErrReconcilePending", err)
	}
}

// S18 (accept): the operator acknowledgment bumps the venue epoch; the
// watermark/dedup re-namespace and positions are NOT auto-zeroed.
func TestReconcile_VenueResetAccept(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.Reset()
	e.reconcile() // detects the reset

	if err := e.oms.TriggerRun(context.Background(), true); err != nil {
		t.Fatalf("TriggerRun(accept_venue_reset): %v", err)
	}
	epoch, ok, err := e.st.CurrentVenueEpoch()
	if err != nil || !ok || epoch.VenueEpoch != 1 || epoch.Reason != "venue_reset_accepted" {
		t.Fatalf("venue epoch = %+v ok=%v err=%v, want epoch 1 by acceptance", epoch, ok, err)
	}
	if !e.oms.Reconciled() {
		t.Error("OMS still closed after acceptance + startup-grade run")
	}
	// Watermark re-namespaced: the new epoch starts cold.
	if _, wmOK, err := e.st.FillWatermark(1, "BTCUSDT"); err != nil || wmOK {
		t.Errorf("FillWatermark(epoch 1) ok=%v err=%v, want cold start", wmOK, err)
	}
	// Positions are NOT auto-zeroed: zeroing books is an operator decision.
	if pos, ok := e.position(); !ok || pos.QtyBase != "0.005" {
		t.Errorf("position = %+v ok=%v, want untouched 0.005", pos, ok)
	}
	// Sends work against the new venue world.
	if err := e.submitEntry(20); err != nil {
		t.Errorf("submit after acceptance: %v", err)
	}
}

// M4 (R3): an in-namespace venue order with NO journal row (a DB
// regression) is an UNATTRIBUTABLE orphan: canceled, evented with reason
// unattributable and no strategy attribution.
func TestReconcile_UnattributableOrphanCanceled(t *testing.T) {
	e := newEnv(t)
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: idN(9, 0), Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()

	evs := e.events("orphan_canceled")
	if len(evs) != 1 || !strings.Contains(evs[0].DetailsJSON, `"reason":"unattributable"`) {
		t.Fatalf("orphan_canceled = %+v, want one unattributable row", evs)
	}
	if evs[0].StrategyID != nil {
		t.Errorf("strategy_id = %q, want nil (no attribution exists)", *evs[0].StrategyID)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open = %d, want 0 (orphan canceled)", len(got))
	}
}

// M4 (invariant 11): an unattributable PROTECTIVE-shaped orphan is NEVER
// canceled — orphan_protective_left + alert; the order keeps resting.
func TestReconcile_UnattributableProtectiveLeft(t *testing.T) {
	e := newEnv(t)
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: idN(9, 0), Status: "NEW",
		Side: "SELL", Type: "STOP_LOSS", StopPrice: "60000", OrigQty: "0.01",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()

	if evs := e.events("orphan_protective_left"); len(evs) != 1 {
		t.Fatalf("orphan_protective_left events = %d, want 1", len(evs))
	}
	if evs := e.events("orphan_canceled"); len(evs) != 0 {
		t.Errorf("orphan_canceled events = %d, want 0 (never cancel a protective)", len(evs))
	}
	if got := e.venueOpen(); len(got) != 1 || got[0].ClientOrderID != idN(9, 0) {
		t.Errorf("venue open = %+v, want the protective orphan left resting", got)
	}
}

// countingExchange wraps the fake venue and counts ExchangeInfo calls.
type countingExchange struct {
	exchange.Exchange
	exchangeInfoCalls int
}

func (c *countingExchange) ExchangeInfo(ctx context.Context, venueSymbols []string) (exchange.Filters, error) {
	c.exchangeInfoCalls++
	return c.Exchange.ExchangeInfo(ctx, venueSymbols)
}

// M3: a venue filter-violation reject (-1013) marks the loaded filters
// stale — the NEXT reconcile's R1 refreshes them well before the normal
// refresh interval elapses; a clean run does not refresh again.
func TestReconcile_FilterRejectTriggersRefresh(t *testing.T) {
	e := newEnv(t)
	cx := &countingExchange{Exchange: e.venue}
	o, err := New(Config{
		Store: e.st, Exchange: cx, Symbols: []string{"BTC/USDT"}, Marks: e.marks,
		AllocatedCapitalQuote: decimal.NewFromInt(10000),
		Now:                   func() time.Time { return e.now },
		TokenReader:           e.tokens,
		Sleep:                 func(time.Duration) {},
		Logf:                  t.Logf,
	})
	if err != nil {
		t.Fatalf("live.New: %v", err)
	}
	e.oms = o
	e.reconcile()
	if cx.exchangeInfoCalls != 1 {
		t.Fatalf("ExchangeInfo calls after startup = %d, want 1", cx.exchangeInfoCalls)
	}

	e.venue.FailNext("PlaceOrder", &exchange.VenueError{
		Op: "PlaceOrder", Class: exchange.ClassDefiniteReject,
		VenueCode: -1013, VenueMsg: "Filter failure: LOT_SIZE",
	})
	if err := e.submitEntry(10); !errors.Is(err, ErrExchangeRejected) {
		t.Fatalf("SubmitApproved err = %v, want ErrExchangeRejected", err)
	}
	e.reconcile() // stale-triggered refresh, far inside the 24 h expiry
	if cx.exchangeInfoCalls != 2 {
		t.Errorf("ExchangeInfo calls after filter reject = %d, want 2 (stale refresh)", cx.exchangeInfoCalls)
	}
	e.reconcile() // clean run: filters fresh again, no refresh due
	if cx.exchangeInfoCalls != 2 {
		t.Errorf("ExchangeInfo calls after clean run = %d, want still 2", cx.exchangeInfoCalls)
	}
}
