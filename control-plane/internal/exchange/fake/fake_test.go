package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange/fake"
)

// assertDecEq compares two venue decimal strings numerically (the fake's
// arithmetic may render "1.0" where a test expects 1).
func assertDecEq(t *testing.T, got, want, field string) {
	t.Helper()
	g, err := decimal.NewFromString(got)
	if err != nil {
		t.Fatalf("%s = %q: %v", field, got, err)
	}
	w, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatalf("%s want %q: %v", field, want, err)
	}
	if !g.Equal(w) {
		t.Fatalf("%s = %s, want %s", field, got, want)
	}
}

func place(t *testing.T, v *fake.Venue, clientOrderID string) exchange.Ack {
	t.Helper()
	ack, err := v.PlaceOrder(context.Background(), exchange.PlaceRequest{
		VenueSymbol: "BTCUSDT", Side: "BUY", Type: "LIMIT", Qty: "1",
		Price: "100", NewClientOrderID: clientOrderID, TimeInForce: "GTC",
	})
	if err != nil {
		t.Fatalf("PlaceOrder(%s): %v", clientOrderID, err)
	}
	return ack
}

func recvEvent(t *testing.T, ch <-chan exchange.UserEvent) exchange.UserEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for user event")
		return exchange.UserEvent{}
	}
}

func TestFillEmitsEventAndTrade(t *testing.T) {
	v := fake.NewVenue()
	ctx := context.Background()
	ack := place(t, v, "amx1-tok-0")
	ch, err := v.StreamUserData(ctx, "key")
	if err != nil {
		t.Fatalf("StreamUserData: %v", err)
	}

	if err := v.Fill("amx1-tok-0", "0.4", "100"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	ev := recvEvent(t, ch)
	if ev.Kind != exchange.UserEventExecutionReport || ev.ExecType != "TRADE" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ClientOrderID != "amx1-tok-0" || ev.ExchangeOrderID != ack.ExchangeOrderID ||
		ev.TradeID != 1 || ev.LastQty != "0.4" || ev.OrderStatus != "PARTIALLY_FILLED" {
		t.Fatalf("event = %+v", ev)
	}
	assertDecEq(t, ev.CumQty, "0.4", "event CumQty")

	trades, err := v.MyTrades(ctx, "BTCUSDT", 0, time.Time{}, 0)
	if err != nil {
		t.Fatalf("MyTrades: %v", err)
	}
	if len(trades) != 1 || trades[0].ID != 1 || trades[0].Qty != "0.4" ||
		trades[0].ClientOrderID != "amx1-tok-0" {
		t.Fatalf("trades = %+v", trades)
	}

	if err := v.Fill("amx1-tok-0", "0.6", "100"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if ev := recvEvent(t, ch); ev.TradeID != 2 || ev.OrderStatus != "FILLED" {
		t.Fatalf("second event = %+v", ev)
	}
	state, err := v.QueryOrder(ctx, "BTCUSDT", "amx1-tok-0")
	if err != nil {
		t.Fatalf("QueryOrder: %v", err)
	}
	if state.Status != "FILLED" {
		t.Fatalf("state = %+v", state)
	}
	assertDecEq(t, state.ExecutedQty, "1", "ExecutedQty")
	assertDecEq(t, state.CumQuoteQty, "100", "CumQuoteQty")

	// fromId paging picks up only the second trade.
	trades, err = v.MyTrades(ctx, "BTCUSDT", 2, time.Time{}, 0)
	if err != nil {
		t.Fatalf("MyTrades(fromID=2): %v", err)
	}
	if len(trades) != 1 || trades[0].ID != 2 {
		t.Fatalf("fromId page = %+v", trades)
	}
}

func TestFailNextFiresOnce(t *testing.T) {
	v := fake.NewVenue()
	inject := &exchange.VenueError{Op: "PlaceOrder", Class: exchange.ClassThrottled, RetryAfter: time.Second}
	v.FailNext("PlaceOrder", inject)

	_, err := v.PlaceOrder(context.Background(), exchange.PlaceRequest{
		VenueSymbol: "BTCUSDT", Side: "BUY", Type: "MARKET", Qty: "1",
		NewClientOrderID: "amx1-tok-0",
	})
	if !errors.Is(err, inject) {
		t.Fatalf("first call err = %v, want injected fault", err)
	}
	place(t, v, "amx1-tok-0") // second call succeeds
}

func TestResetRestartsTradeIDs(t *testing.T) {
	v := fake.NewVenue()
	ctx := context.Background()
	v.SetBalance("USDT", "1000", "0")
	place(t, v, "amx1-tok-0")
	if err := v.Fill("amx1-tok-0", "1", "100"); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	v.Reset()

	if open, err := v.OpenOrders(ctx, "BTCUSDT"); err != nil || len(open) != 0 {
		t.Fatalf("OpenOrders after reset = %v, %v", open, err)
	}
	if trades, err := v.MyTrades(ctx, "BTCUSDT", 0, time.Time{}, 0); err != nil || len(trades) != 0 {
		t.Fatalf("MyTrades after reset = %v, %v", trades, err)
	}
	if bals, err := v.Balances(ctx); err != nil || len(bals) != 0 {
		t.Fatalf("Balances after reset = %v, %v", bals, err)
	}
	if _, err := v.QueryOrder(ctx, "BTCUSDT", "amx1-tok-0"); exchange.Classify(err) != exchange.ClassNotFound {
		t.Fatalf("QueryOrder after reset: %v", err)
	}

	// The same clientOrderId is placeable again and trade ids restart at 1.
	place(t, v, "amx1-tok-0")
	if err := v.Fill("amx1-tok-0", "1", "100"); err != nil {
		t.Fatalf("Fill after reset: %v", err)
	}
	trades, err := v.MyTrades(ctx, "BTCUSDT", 0, time.Time{}, 0)
	if err != nil || len(trades) != 1 || trades[0].ID != 1 {
		t.Fatalf("trades after reset = %+v, %v", trades, err)
	}
}

func TestExpireListenKeyEmitsEvent(t *testing.T) {
	v := fake.NewVenue()
	ch, err := v.StreamUserData(context.Background(), "key")
	if err != nil {
		t.Fatalf("StreamUserData: %v", err)
	}
	v.ExpireListenKey()
	if ev := recvEvent(t, ch); ev.Kind != exchange.UserEventListenKeyExpired {
		t.Fatalf("event = %+v", ev)
	}
}
