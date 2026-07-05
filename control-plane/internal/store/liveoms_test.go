package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testLiveOrder(orderID, strategyID, status string) Order {
	return Order{
		OrderID: orderID, Origin: "proposal", StrategyID: strategyID, Symbol: "BTC/USDT",
		Class: "ENTRY", Side: "buy", Type: "limit", QtyBase: "0.1",
		Status: status, SubmittedAt: formatTime(testNow),
	}
}

func testIntent(clientOrderID string, attempt int, orderID, strategyID string) OrderIntent {
	// The intent token is the id minus its "-<attempt>" suffix, so attempts
	// of one intent share a token and distinct intents never collide.
	token := strings.TrimSuffix(clientOrderID, fmt.Sprintf("-%d", attempt))
	return OrderIntent{
		ClientOrderID: clientOrderID, IntentToken: token, Attempt: attempt,
		OrderID: orderID, StrategyID: strategyID, Symbol: "BTC/USDT", VenueSymbol: "BTCUSDT",
		Side: "buy", Type: "limit", QtyBase: "0.1", Origin: "proposal",
		JournaledAt: formatTime(testNow),
	}
}

// insertVenueFill inserts a live fill via the INSERT OR IGNORE dedup path
// (partial unique index on (venue_epoch, venue_symbol, exchange_trade_id))
// and reports whether the row landed.
func insertVenueFill(t *testing.T, s *Store, fillID, orderID, venueSymbol string, tradeID, epoch int64) bool {
	t.Helper()
	res, err := s.db.Exec(`INSERT OR IGNORE INTO fills
		(fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts, venue_symbol, exchange_trade_id, venue_epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fillID, orderID, "0.1", "64000", "3.2", formatTime(testNow), venueSymbol, tradeID, epoch)
	if err != nil {
		t.Fatalf("insert venue fill %s: %v", fillID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	return n == 1
}

func TestLiveOMSMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "pending_new")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.InsertOrderIntent(testIntent("amx1-a-0", 0, uid(30), uid(1))); err != nil {
		t.Fatalf("InsertOrderIntent: %v", err)
	}
	if err := s.RecordIntentAttempt(testIntent("amx1-a-1", 1, uid(30), uid(1))); err != nil {
		t.Fatalf("RecordIntentAttempt: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A second Open on the migrated DB must be a no-op: no duplicate
	// columns, indexes intact, existing rows served unchanged.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	for _, col := range []struct{ table, name string }{
		{"orders", "client_order_id"}, {"orders", "exchange_order_id"},
		{"fills", "venue_symbol"}, {"fills", "exchange_trade_id"}, {"fills", "venue_epoch"},
	} {
		var n int
		if err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_info('%s')
			WHERE name = '%s'`, col.table, col.name)).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(%s): %v", col.table, err)
		}
		if n != 1 {
			t.Errorf("%s.%s column count = %d, want 1", col.table, col.name, n)
		}
	}
	var idx int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index'
		AND name IN ('idx_orders_client_order_id', 'idx_fills_venue_trade')`).Scan(&idx); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if idx != 2 {
		t.Errorf("partial unique indexes = %d, want 2", idx)
	}
	intents, err := s.ListPendingNewIntents()
	if err != nil {
		t.Fatalf("ListPendingNewIntents: %v", err)
	}
	if len(intents) != 1 || intents[0].ClientOrderID != "amx1-a-1" || intents[0].Attempt != 1 {
		t.Errorf("latest intents after reopen = %+v, want the attempt-1 row only", intents)
	}
}

func TestFillVenueTradeDedup(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "open")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}

	// Paper fills carry NULL venue columns and are excluded from the
	// partial index: any number of them insert freely.
	for i := 0; i < 2; i++ {
		f := Fill{FillID: uid(40 + i), OrderID: uid(30), QtyBase: "0.05",
			FillPrice: "64000", FeeQuote: "1.6", FillTS: formatTime(testNow)}
		if err := s.InsertFill(f); err != nil {
			t.Fatalf("InsertFill #%d: %v", i, err)
		}
	}

	// A venue fill replay (same epoch/symbol/trade id, fresh fill_id) is a
	// no-op; the same trade id in a NEW epoch inserts (dedup is per epoch).
	if !insertVenueFill(t, s, uid(50), uid(30), "BTCUSDT", 42, 0) {
		t.Fatal("first venue fill: not inserted")
	}
	if insertVenueFill(t, s, uid(51), uid(30), "BTCUSDT", 42, 0) {
		t.Error("replayed venue fill inserted, want INSERT OR IGNORE no-op")
	}
	if !insertVenueFill(t, s, uid(52), uid(30), "BTCUSDT", 42, 1) {
		t.Error("same trade id in a new venue epoch: not inserted")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM fills`).Scan(&n); err != nil {
		t.Fatalf("count fills: %v", err)
	}
	if n != 4 {
		t.Errorf("fills rows = %d, want 4 (2 paper + 2 venue)", n)
	}
}

func TestIntentClaimLifecycle(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "pending_new")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.InsertOrderIntent(testIntent("amx1-a-0", 0, uid(30), uid(1))); err != nil {
		t.Fatalf("InsertOrderIntent: %v", err)
	}
	claimedAt := formatTime(testNow)

	if claimed, err := s.RecordIntentClaim("amx1-a-0", claimedAt); err != nil || !claimed {
		t.Fatalf("first claim: claimed=%v err=%v, want true, nil", claimed, err)
	}
	if claimed, err := s.RecordIntentClaim("amx1-a-0", claimedAt); err != nil || claimed {
		t.Errorf("second claim: claimed=%v err=%v, want false, nil", claimed, err)
	}
	if err := s.RecordIntentClaimRevoked("amx1-a-0", claimedAt); err != nil {
		t.Fatalf("RecordIntentClaimRevoked: %v", err)
	}
	if claimed, err := s.RecordIntentClaim("amx1-a-0", claimedAt); err != nil || claimed {
		t.Errorf("claim after revoke: claimed=%v err=%v, want false, nil", claimed, err)
	}
	if err := s.RecordIntentClaimRevoked("amx1-a-0", claimedAt); err != nil {
		t.Errorf("second revoke: err = %v, want idempotent nil", err)
	}

	// An UNCLAIMED attempt can be revoked too (R2 crash-before-send); the
	// sender's later claim then fails.
	if err := s.InsertOrderIntent(testIntent("amx1-b-0", 0, uid(30), uid(1))); err != nil {
		t.Fatalf("InsertOrderIntent b: %v", err)
	}
	if err := s.RecordIntentClaimRevoked("amx1-b-0", claimedAt); err != nil {
		t.Fatalf("revoke unclaimed: %v", err)
	}
	if claimed, err := s.RecordIntentClaim("amx1-b-0", claimedAt); err != nil || claimed {
		t.Errorf("claim of revoked-unclaimed: claimed=%v err=%v, want false, nil", claimed, err)
	}

	if _, err := s.RecordIntentClaim("amx1-missing-0", claimedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("claim unknown intent: err = %v, want ErrNotFound", err)
	}
	if err := s.RecordIntentClaimRevoked("amx1-missing-0", claimedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke unknown intent: err = %v, want ErrNotFound", err)
	}
}

func TestIntentClaimRaceOneWinner(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "pending_new")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.InsertOrderIntent(testIntent("amx1-race-0", 0, uid(30), uid(1))); err != nil {
		t.Fatalf("InsertOrderIntent: %v", err)
	}

	const contenders = 8
	results := make([]bool, contenders)
	errs := make([]error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.RecordIntentClaim("amx1-race-0", formatTime(testNow))
		}(i)
	}
	wg.Wait()
	winners := 0
	for i := 0; i < contenders; i++ {
		if errs[i] != nil {
			t.Fatalf("contender %d: %v", i, errs[i])
		}
		if results[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("claim winners = %d, want exactly 1", winners)
	}
}

func TestRecordIntentAttemptBumpsLatestID(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "pending_new")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if err := s.InsertOrderIntent(testIntent("amx1-a-0", 0, uid(30), uid(1))); err != nil {
		t.Fatalf("InsertOrderIntent: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE orders SET client_order_id = 'amx1-a-0' WHERE order_id = ?`,
		uid(30)); err != nil {
		t.Fatalf("seed client_order_id: %v", err)
	}
	if err := s.RecordIntentAttempt(testIntent("amx1-a-1", 1, uid(30), uid(1))); err != nil {
		t.Fatalf("RecordIntentAttempt: %v", err)
	}
	intents, err := s.ListPendingNewIntents()
	if err != nil {
		t.Fatalf("ListPendingNewIntents: %v", err)
	}
	if len(intents) != 1 || intents[0].ClientOrderID != "amx1-a-1" ||
		intents[0].Attempt != 1 || intents[0].ClaimedAt != nil {
		t.Errorf("latest intent = %+v, want unclaimed attempt 1", intents)
	}
	if err := s.RecordIntentAttempt(testIntent("amx1-c-0", 0, uid(31), uid(1))); !errors.Is(err, ErrNotFound) {
		t.Errorf("attempt for unknown order: err = %v, want ErrNotFound", err)
	}
	if err := s.RecordExchangeAck(uid(30), "12345"); err != nil {
		t.Fatalf("RecordExchangeAck: %v", err)
	}
	if err := s.RecordExchangeAck(uid(31), "12345"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ack for unknown order: err = %v, want ErrNotFound", err)
	}
}

func TestRecordOrderStatusMonotone(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "pending_new")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	steps := []struct{ write, want string }{
		{"open", "open"},                         // rank 0 -> 1
		{"partially_filled", "partially_filled"}, // rank 1 -> 2
		{"open", "partially_filled"},             // regression: no-op
		{"pending_new", "partially_filled"},      // regression: no-op
		{"filled", "filled"},                     // rank 2 -> 3 (terminal)
		{"canceled", "filled"},                   // terminal immutable
		{"open", "filled"},                       // terminal immutable
	}
	for _, step := range steps {
		got, err := s.RecordOrderStatus(uid(30), step.write)
		if err != nil {
			t.Fatalf("RecordOrderStatus(%s): %v", step.write, err)
		}
		if got != step.want {
			t.Errorf("RecordOrderStatus(%s) = %s, want %s", step.write, got, step.want)
		}
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM orders WHERE order_id = ?`, uid(30)).
		Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "filled" {
		t.Errorf("persisted status = %s, want filled", status)
	}
	if _, err := s.RecordOrderStatus(uid(30), "teleported"); err == nil {
		t.Error("unknown status accepted, want error")
	}
	if _, err := s.RecordOrderStatus(uid(31), "open"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown order: err = %v, want ErrNotFound", err)
	}
}

func TestVenueEpochMaxAndWatermark(t *testing.T) {
	s := openStore(t)
	if _, ok, err := s.CurrentVenueEpoch(); err != nil || ok {
		t.Fatalf("empty venue_epochs: ok=%v err=%v, want false, nil", ok, err)
	}
	if err := s.InsertVenueEpoch(VenueEpoch{VenueEpoch: 0, StartedAt: formatTime(testNow),
		Reason: "initial", DetailsJSON: "{}"}); err != nil {
		t.Fatalf("InsertVenueEpoch 0: %v", err)
	}
	if err := s.InsertVenueEpoch(VenueEpoch{VenueEpoch: 1, StartedAt: formatTime(testNow),
		Reason: "venue_reset_accepted", DetailsJSON: "{}"}); err != nil {
		t.Fatalf("InsertVenueEpoch 1: %v", err)
	}
	e, ok, err := s.CurrentVenueEpoch()
	if err != nil || !ok {
		t.Fatalf("CurrentVenueEpoch: ok=%v err=%v", ok, err)
	}
	if e.VenueEpoch != 1 || e.Reason != "venue_reset_accepted" {
		t.Errorf("current epoch = %+v, want MAX row (epoch 1)", e)
	}

	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "open")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	if _, ok, err := s.FillWatermark(0, "BTCUSDT"); err != nil || ok {
		t.Fatalf("cold-start watermark: ok=%v err=%v, want false, nil", ok, err)
	}
	insertVenueFill(t, s, uid(50), uid(30), "BTCUSDT", 5, 0)
	insertVenueFill(t, s, uid(51), uid(30), "BTCUSDT", 7, 0)
	insertVenueFill(t, s, uid(52), uid(30), "BTCUSDT", 3, 1)
	if w, ok, err := s.FillWatermark(0, "BTCUSDT"); err != nil || !ok || w != 7 {
		t.Errorf("watermark(0, BTCUSDT) = %d ok=%v err=%v, want 7, true, nil", w, ok, err)
	}
	if w, ok, err := s.FillWatermark(1, "BTCUSDT"); err != nil || !ok || w != 3 {
		t.Errorf("watermark(1, BTCUSDT) = %d ok=%v err=%v, want 3, true, nil", w, ok, err)
	}
	if _, ok, err := s.FillWatermark(1, "ETHUSDT"); err != nil || ok {
		t.Errorf("watermark(1, ETHUSDT): ok=%v err=%v, want false, nil", ok, err)
	}
}

func TestProtectiveObligationsAndPendingFees(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	if err := s.InsertOrder(testLiveOrder(uid(30), uid(1), "open")); err != nil {
		t.Fatalf("InsertOrder: %v", err)
	}
	later := "2026-07-04T12:01:00Z"
	for i, due := range []string{later, formatTime(testNow)} {
		err := s.InsertProtectiveObligation(ProtectiveObligation{
			ObligationID: uid(60 + i), EntryOrderID: uid(30), StrategyID: uid(1),
			Kind: "sl", DueAt: due, CreatedAt: formatTime(testNow),
		})
		if err != nil {
			t.Fatalf("InsertProtectiveObligation #%d: %v", i, err)
		}
	}
	open, err := s.ListOpenProtectiveObligations()
	if err != nil {
		t.Fatalf("ListOpenProtectiveObligations: %v", err)
	}
	if len(open) != 2 || open[0].ObligationID != uid(61) {
		t.Fatalf("open obligations = %+v, want 2 rows due_at ascending", open)
	}
	if err := s.RecordProtectiveSatisfied(uid(61), later); err != nil {
		t.Fatalf("RecordProtectiveSatisfied: %v", err)
	}
	if err := s.RecordProtectiveSatisfied(uid(61), later); err != nil {
		t.Errorf("second satisfy: err = %v, want idempotent nil", err)
	}
	if err := s.RecordProtectiveSatisfied(uid(69), later); !errors.Is(err, ErrNotFound) {
		t.Errorf("satisfy unknown obligation: err = %v, want ErrNotFound", err)
	}
	if open, err = s.ListOpenProtectiveObligations(); err != nil || len(open) != 1 {
		t.Errorf("open obligations after satisfy = %+v err=%v, want 1 row", open, err)
	}

	insertVenueFill(t, s, uid(50), uid(30), "BTCUSDT", 42, 0)
	err = s.InsertPendingFillFee(PendingFillFee{FillID: uid(50), Commission: "0.0001",
		CommissionAsset: "BNB", RecordedAt: formatTime(testNow)})
	if err != nil {
		t.Fatalf("InsertPendingFillFee: %v", err)
	}
	if fees, err := s.ListUnconvertedPendingFillFees(); err != nil || len(fees) != 1 {
		t.Fatalf("unconverted fees = %+v err=%v, want 1 row", fees, err)
	}
	if err := s.RecordFeeConverted(uid(50), later); err != nil {
		t.Fatalf("RecordFeeConverted: %v", err)
	}
	if err := s.RecordFeeConverted(uid(50), later); err != nil {
		t.Errorf("second convert: err = %v, want idempotent nil", err)
	}
	if err := s.RecordFeeConverted(uid(59), later); !errors.Is(err, ErrNotFound) {
		t.Errorf("convert unknown fee: err = %v, want ErrNotFound", err)
	}
	if fees, err := s.ListUnconvertedPendingFillFees(); err != nil || len(fees) != 0 {
		t.Errorf("unconverted fees after convert = %+v err=%v, want none", fees, err)
	}
}

func TestOMSReconEvents(t *testing.T) {
	s := openStore(t)
	runID := uid(70)
	for i, kind := range []string{"run_started", "fill_backfilled", "run_completed"} {
		tradeID := int64(42)
		e := OMSReconEvent{EventID: uid(80 + i), Kind: kind, RunID: &runID,
			DetailsJSON: "{}", RecordedAt: formatTime(testNow)}
		if kind == "fill_backfilled" {
			e.ExchangeTradeID = &tradeID
		}
		if err := s.AppendOMSReconEvent(e); err != nil {
			t.Fatalf("AppendOMSReconEvent(%s): %v", kind, err)
		}
	}
	if err := s.AppendOMSReconEvent(OMSReconEvent{EventID: uid(89), Kind: "not_a_kind",
		DetailsJSON: "{}", RecordedAt: formatTime(testNow)}); err == nil {
		t.Error("event with unknown kind accepted, want CHECK violation")
	}
	all, err := s.ListOMSReconEvents(OMSReconEventFilter{RunID: runID})
	if err != nil {
		t.Fatalf("ListOMSReconEvents: %v", err)
	}
	if len(all) != 3 || all[0].Kind != "run_started" || all[2].Kind != "run_completed" {
		t.Fatalf("run events = %+v, want 3 rows in insertion order", all)
	}
	got, err := s.ListOMSReconEvents(OMSReconEventFilter{Kind: "fill_backfilled"})
	if err != nil {
		t.Fatalf("ListOMSReconEvents(kind): %v", err)
	}
	if len(got) != 1 || got[0].ExchangeTradeID == nil || *got[0].ExchangeTradeID != 42 {
		t.Errorf("fill_backfilled events = %+v, want 1 row with trade id 42", got)
	}
	if limited, err := s.ListOMSReconEvents(OMSReconEventFilter{Limit: 2}); err != nil || len(limited) != 2 {
		t.Errorf("limited events = %+v err=%v, want 2 rows", limited, err)
	}
}
