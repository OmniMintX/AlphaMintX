package live

import (
	"errors"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func ambiguousErr(op string) *exchange.VenueError {
	return &exchange.VenueError{Op: op, Class: exchange.ClassAmbiguous, VenueMsg: "timeout"}
}

// S3: an ambiguous placement resolves by query — the venue HAS the order,
// the sender adopts it, and NO duplicate is ever sent.
func TestSubmit_AmbiguousResolvesPresent(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	// The venue processed the (timed-out) send: the order exists under the
	// deterministic attempt-0 id before the fault fires.
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: idN(1, 0), Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.venue.FailNext("PlaceOrder", ambiguousErr("PlaceOrder"))

	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	ord := e.order(idN(1, 0))
	if ord.Status != "open" || ord.ExchangeOrderID == nil {
		t.Errorf("order = status %s, exchange_order_id %v; want open with ack", ord.Status, ord.ExchangeOrderID)
	}
	if got := e.venueOpen(); len(got) != 1 {
		t.Errorf("venue open orders = %d, want exactly 1 (no duplicate sent)", len(got))
	}
	if evs := e.events("intent_resolved_present"); len(evs) != 1 {
		t.Errorf("intent_resolved_present events = %d, want 1", len(evs))
	}
}

// S4: ambiguous, then query-NotFound poisons the id; attempt+1 places; a
// LATE materialization of the poisoned id is canceled by the Reconciler.
func TestSubmit_PoisonedLateArrivalOpen(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.venue.FailNext("PlaceOrder", ambiguousErr("PlaceOrder"))

	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	ord := e.order(idN(1, 1)) // the LATEST attempt id after the retry
	if ord.Status != "open" || ord.ExchangeOrderID == nil {
		t.Fatalf("attempt-1 order = status %s, ack %v; want open, acked", ord.Status, ord.ExchangeOrderID)
	}
	if evs := e.events("intent_resolved_absent"); len(evs) != 1 {
		t.Fatalf("intent_resolved_absent events = %d, want 1 (poisoned)", len(evs))
	}
	// The poisoned id materializes late, still open: canceled REGARDLESS of
	// shape (it duplicates a still-attributed order).
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: idN(1, 0), Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	e.reconcile()
	if evs := e.events("orphan_canceled"); len(evs) != 1 || evs[0].DetailsJSON == "" {
		t.Fatalf("orphan_canceled events = %+v, want 1", evs)
	}
	if got := e.venueOpen(); len(got) != 1 || got[0].ClientOrderID != idN(1, 1) {
		t.Errorf("venue open after reconcile = %+v, want only the attempt-1 order", got)
	}
}

// S4 (filled variant): the late poisoned order (partly) filled — the fills
// are real and OURS: booked to the intent's strategy + duplicate_exposure.
func TestSubmit_PoisonedLateArrivalFilled(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.venue.FailNext("PlaceOrder", ambiguousErr("PlaceOrder"))
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: idN(1, 0), Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	if err := e.venue.Fill(idN(1, 0), "0.01", "64000"); err != nil {
		t.Fatalf("venue.Fill: %v", err)
	}
	e.reconcile()

	fills := e.fills(idN(1, 0))
	if len(fills) != 1 || fills[0].QtyBase != "0.01" || fills[0].ExchangeTradeID != 1 {
		t.Fatalf("booked fills = %+v, want the one venue trade", fills)
	}
	if evs := e.events("duplicate_exposure"); len(evs) != 1 {
		t.Errorf("duplicate_exposure events = %d, want 1", len(evs))
	}
	pos, ok := e.position()
	if !ok || pos.QtyBase != "0.01" {
		t.Errorf("position = %+v ok=%v, want qty_base 0.01", pos, ok)
	}
	if evs := e.events("cum_qty_mismatch"); len(evs) != 0 {
		t.Errorf("cum_qty_mismatch events = %d, want 0 (suppressed for duplicate_exposure)", len(evs))
	}
}

// S14: the kill epoch bumps between journal and send — the order is
// rejected with reason kill_epoch_stale; the id is retired, NEVER sent.
func TestSubmit_KillEpochRecheck(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	epoch := int64(1)
	strat := uid(1)
	e.oms.afterJournal = func() {
		err := e.st.AppendKillBreakerEvent(store.KillBreakerEvent{
			EventID: uid(80), Kind: "kill", Scope: "strategy", StrategyID: &strat,
			KillEpoch: &epoch, ActorID: "admin-1", RecordedAt: formatTime(testNow),
		})
		if err != nil {
			t.Fatalf("AppendKillBreakerEvent: %v", err)
		}
	}
	if err := e.submitEntry(10); !errors.Is(err, ErrKillEpochStale) {
		t.Fatalf("SubmitApproved err = %v, want ErrKillEpochStale", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "rejected" {
		t.Errorf("order status = %s, want rejected", ord.Status)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (never sent)", len(got))
	}
	evs := e.events("intent_resolved_absent")
	if len(evs) != 1 || evs[0].DetailsJSON != `{"reason":"kill_epoch_stale"}` {
		t.Errorf("intent_resolved_absent = %+v, want one kill_epoch_stale row", evs)
	}
}

// S15: submissions before the startup reconcile completes are DROPPED
// (never queued) with reason RECONCILE_PENDING in rejected_submissions.
func TestSubmit_ReconcilePending(t *testing.T) {
	e := newEnv(t) // NO reconcile: the OMS is closed
	if err := e.submitEntry(10); !errors.Is(err, ErrReconcilePending) {
		t.Fatalf("SubmitApproved err = %v, want ErrReconcilePending", err)
	}
	drops, err := e.st.RejectedSubmissions(uid(1))
	if err != nil {
		t.Fatalf("RejectedSubmissions: %v", err)
	}
	if len(drops) != 1 || drops[0].Reason != "RECONCILE_PENDING" {
		t.Fatalf("rejected_submissions = %+v, want one RECONCILE_PENDING row", drops)
	}
	orders, err := e.st.ListNonTerminalLiveOrders()
	if err != nil {
		t.Fatalf("ListNonTerminalLiveOrders: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("live orders = %d, want 0 (dropped, never journaled)", len(orders))
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0", len(got))
	}
}

// S17: a throttled placement (429 WITH Retry-After) resends the SAME
// attempt id after the interval — exactly one venue order, id not poisoned.
func TestSubmit_ThrottledSameIdResend(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	retryAfter := 250 * time.Millisecond
	e.venue.FailNext("PlaceOrder", &exchange.VenueError{
		Op: "PlaceOrder", Class: exchange.ClassThrottled,
		VenueMsg: "Too many requests.", RetryAfter: retryAfter,
	})
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	found := false
	for _, d := range e.slept {
		if d == retryAfter {
			found = true
		}
	}
	if !found {
		t.Errorf("slept = %v, want the Retry-After interval %v honored", e.slept, retryAfter)
	}
	got := e.venueOpen()
	if len(got) != 1 || got[0].ClientOrderID != idN(1, 0) {
		t.Fatalf("venue open = %+v, want exactly one order under the SAME attempt-0 id", got)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" || ord.ExchangeOrderID == nil {
		t.Errorf("order = status %s, ack %v; want open, acked", ord.Status, ord.ExchangeOrderID)
	}
	if evs := e.events("intent_resolved_absent"); len(evs) != 0 {
		t.Errorf("intent_resolved_absent events = %d, want 0 (id never poisoned)", len(evs))
	}
}

// C1: the kill epoch bumps while a throttled placement sleeps — the resend
// re-runs the pre-send checks, so the attempt is retired, NEVER resent.
func TestSubmit_ThrottledKillEpochRecheck(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.venue.FailNext("PlaceOrder", &exchange.VenueError{
		Op: "PlaceOrder", Class: exchange.ClassThrottled,
		VenueMsg: "Too many requests.", RetryAfter: 250 * time.Millisecond,
	})
	epoch := int64(1)
	strat := uid(1)
	e.oms.sleep = func(time.Duration) {
		if err := e.st.AppendKillBreakerEvent(store.KillBreakerEvent{
			EventID: uid(80), Kind: "kill", Scope: "strategy", StrategyID: &strat,
			KillEpoch: &epoch, ActorID: "admin-1", RecordedAt: formatTime(testNow),
		}); err != nil {
			t.Fatalf("AppendKillBreakerEvent: %v", err)
		}
	}
	if err := e.submitEntry(10); !errors.Is(err, ErrKillEpochStale) {
		t.Fatalf("SubmitApproved err = %v, want ErrKillEpochStale", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "rejected" {
		t.Errorf("order status = %s, want rejected", ord.Status)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (never resent)", len(got))
	}
	evs := e.events("intent_resolved_absent")
	if len(evs) != 1 || evs[0].DetailsJSON != `{"reason":"kill_epoch_stale"}` {
		t.Errorf("intent_resolved_absent = %+v, want one kill_epoch_stale row", evs)
	}
}

// C1 (claim variant): the claim is revoked while a throttled placement
// sleeps — the resend re-reads the claim and MUST NOT transmit (the
// revoker owns the intent's resolution).
func TestSubmit_ThrottledClaimRevokeRecheck(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.venue.FailNext("PlaceOrder", &exchange.VenueError{
		Op: "PlaceOrder", Class: exchange.ClassThrottled,
		VenueMsg: "Too many requests.", RetryAfter: 250 * time.Millisecond,
	})
	e.oms.sleep = func(time.Duration) {
		if err := e.st.RecordIntentClaimRevoked(idN(1, 0), formatTime(testNow)); err != nil {
			t.Fatalf("RecordIntentClaimRevoked: %v", err)
		}
	}
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v (a lost claim is NOT an error)", err)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (the revoked claim never transmitted)", len(got))
	}
	if ord := e.order(idN(1, 0)); ord.Status != "pending_new" {
		t.Errorf("order status = %s, want pending_new (the revoker owns resolution)", ord.Status)
	}
}
