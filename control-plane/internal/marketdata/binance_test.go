package marketdata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsBase converts an httptest http:// base URL into its ws:// form.
func wsBase(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func recvTick(t *testing.T, ch <-chan Tick) Tick {
	t.Helper()
	select {
	case tick, ok := <-ch:
		if !ok {
			t.Fatal("tick channel closed early")
		}
		return tick
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tick")
	}
	return Tick{}
}

func TestNewBinanceFeedValidation(t *testing.T) {
	tests := []struct {
		name    string
		market  Market
		wantErr bool
	}{
		{"spot", MarketSpot, false},
		{"futures", MarketFutures, false},
		{"empty market", Market(""), true},
		{"unknown market", Market("margin"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBinanceFeed(BinanceConfig{Market: tc.market})
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewBinanceFeed(%q) error = %v, wantErr %v", tc.market, err, tc.wantErr)
			}
		})
	}
}

func TestBinanceFeedSubscribeValidation(t *testing.T) {
	feed, err := NewBinanceFeed(BinanceConfig{Market: MarketSpot})
	if err != nil {
		t.Fatalf("NewBinanceFeed: %v", err)
	}
	if _, err := feed.Subscribe(context.Background(), nil); err == nil {
		t.Fatal("empty subscribe accepted, want error")
	}
	if _, err := feed.Subscribe(context.Background(), []string{"BTCUSDT"}); err == nil {
		t.Fatal("venue-form symbol accepted, want error")
	}
}

func TestParseMessage(t *testing.T) {
	canonical := map[string]string{"BTCUSDT": "BTC/USDT"}
	tests := []struct {
		name     string
		market   Market
		raw      string
		wantOK   bool
		wantMark string
		wantTS   time.Time
	}{
		{
			"spot trade uses trade time",
			MarketSpot,
			`{"stream":"btcusdt@trade","data":{"e":"trade","E":1717243200000,"s":"BTCUSDT","p":"64200.5","T":1717243200123}}`,
			true, "64200.5", time.UnixMilli(1717243200123).UTC(),
		},
		{
			"futures mark price uses event time",
			MarketFutures,
			`{"stream":"btcusdt@markPrice@1s","data":{"e":"markPriceUpdate","E":1717243201000,"s":"BTCUSDT","p":"64190.75"}}`,
			true, "64190.75", time.UnixMilli(1717243201000).UTC(),
		},
		{
			"spot trade id must not clobber trade time",
			MarketSpot,
			`{"stream":"btcusdt@trade","data":{"e":"trade","E":1717243200000,"s":"BTCUSDT","p":"64200.5","T":1717243200123,"t":99}}`,
			true, "64200.5", time.UnixMilli(1717243200123).UTC(),
		},
		{
			"futures settle price must not clobber mark price",
			MarketFutures,
			`{"stream":"btcusdt@markPrice@1s","data":{"e":"markPriceUpdate","E":1717243201000,"s":"BTCUSDT","p":"64190.75","P":"64000"}}`,
			true, "64190.75", time.UnixMilli(1717243201000).UTC(),
		},
		{"unsubscribed symbol skipped", MarketSpot,
			`{"stream":"ethusdt@trade","data":{"e":"trade","E":1,"s":"ETHUSDT","p":"3005.5","T":2}}`,
			false, "", time.Time{}},
		{"invalid json skipped", MarketSpot, `not json`, false, "", time.Time{}},
		{"missing data skipped", MarketSpot, `{"stream":"btcusdt@trade"}`, false, "", time.Time{}},
		{"bad price skipped", MarketSpot,
			`{"stream":"btcusdt@trade","data":{"e":"trade","E":1,"s":"BTCUSDT","p":"oops","T":2}}`,
			false, "", time.Time{}},
		{"missing timestamps skipped", MarketSpot,
			`{"stream":"btcusdt@trade","data":{"e":"trade","s":"BTCUSDT","p":"64200.5"}}`,
			false, "", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			feed, err := NewBinanceFeed(BinanceConfig{Market: tc.market})
			if err != nil {
				t.Fatalf("NewBinanceFeed: %v", err)
			}
			tick, ok := feed.parseMessage([]byte(tc.raw), canonical)
			if ok != tc.wantOK {
				t.Fatalf("parseMessage ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if tick.Symbol != "BTC/USDT" {
				t.Fatalf("Symbol = %q, want BTC/USDT", tick.Symbol)
			}
			if tick.Mark.String() != tc.wantMark {
				t.Fatalf("Mark = %s, want %s", tick.Mark, tc.wantMark)
			}
			if !tick.TS.Equal(tc.wantTS) {
				t.Fatalf("TS = %s, want %s", tick.TS, tc.wantTS)
			}
			if tc.market == MarketSpot && tick.Last.String() != tc.wantMark {
				t.Fatalf("spot Last = %s, want %s", tick.Last, tc.wantMark)
			}
		})
	}
}

// TestBinanceFeedBootstrapAndStream drives a spot feed against an httptest
// fake: the REST snapshot must arrive on the channel before any WS tick,
// unsubscribed snapshot rows are filtered, and malformed WS frames are
// skipped without killing the connection.
func TestBinanceFeedBootstrapAndStream(t *testing.T) {
	var snapshots atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/ticker/price", func(w http.ResponseWriter, r *http.Request) {
		snapshots.Add(1)
		fmt.Fprint(w, `[{"symbol":"BTCUSDT","price":"64180.1"},{"symbol":"ETHUSDT","price":"3005.5"},{"symbol":"XRPUSDT","price":"0.5"}]`)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.RawQuery, "streams=btcusdt@trade/ethusdt@trade"; got != want {
			t.Errorf("stream query = %q, want %q", got, want)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer conn.CloseNow()
		msgs := []string{
			`{"stream":"btcusdt@trade","data":{"e":"trade","E":1717243200000,"s":"BTCUSDT","p":"64200.5","T":1717243200123}}`,
			`not json`,
			`{"stream":"ethusdt@trade","data":{"e":"trade","E":1717243201000,"s":"ETHUSDT","p":"3010.25","T":1717243201456}}`,
		}
		for _, m := range msgs {
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(m)); err != nil {
				return
			}
		}
		<-conn.CloseRead(context.Background()).Done() // hold until the client hangs up
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	feed, err := NewBinanceFeed(BinanceConfig{
		Market:     MarketSpot,
		WSURL:      wsBase(srv.URL),
		RESTURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewBinanceFeed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := feed.Subscribe(ctx, []string{"BTC/USDT", "ETH/USDT"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := []struct {
		symbol string
		mark   string
	}{
		{"BTC/USDT", "64180.1"}, // bootstrap, snapshot row order
		{"ETH/USDT", "3005.5"},  // bootstrap
		{"BTC/USDT", "64200.5"}, // WS
		{"ETH/USDT", "3010.25"}, // WS
	}
	got := make([]Tick, len(want))
	for i, w := range want {
		tick := recvTick(t, ch)
		if tick.Symbol != w.symbol || tick.Mark.String() != w.mark {
			t.Fatalf("tick[%d] = (%s, %s), want (%s, %s)", i, tick.Symbol, tick.Mark, w.symbol, w.mark)
		}
		if tick.Last.String() != w.mark {
			t.Fatalf("tick[%d] Last = %s, want %s (spot mark = last trade)", i, tick.Last, w.mark)
		}
		if tick.TS.IsZero() {
			t.Fatalf("tick[%d] has zero TS", i)
		}
		got[i] = tick
	}
	// The WS ticks must carry the exchange trade time, not local time.
	if ts := time.UnixMilli(1717243200123).UTC(); !got[2].TS.Equal(ts) {
		t.Fatalf("WS tick[2] TS = %s, want %s", got[2].TS, ts)
	}
	if ts := time.UnixMilli(1717243201456).UTC(); !got[3].TS.Equal(ts) {
		t.Fatalf("WS tick[3] TS = %s, want %s", got[3].TS, ts)
	}
	if got := snapshots.Load(); got != 1 {
		t.Fatalf("bootstrap ran %d times, want 1", got)
	}
}

// TestBinanceFeedWatchdogReconnect proves a connected-but-silent socket is
// reaped by the last-message watchdog, and that every reconnect re-runs the
// REST bootstrap before WS ticks resume.
func TestBinanceFeedWatchdogReconnect(t *testing.T) {
	var dials, snapshots atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/ticker/price", func(w http.ResponseWriter, r *http.Request) {
		snapshots.Add(1)
		fmt.Fprint(w, `[{"symbol":"BTCUSDT","price":"100"}]`)
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		n := dials.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if n >= 2 {
			msg := `{"stream":"btcusdt@trade","data":{"e":"trade","E":1717243202000,"s":"BTCUSDT","p":"101","T":1717243202000}}`
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
				return
			}
		}
		// Stay silent: the first connection must be reaped by the watchdog.
		<-conn.CloseRead(context.Background()).Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	feed, err := NewBinanceFeed(BinanceConfig{
		Market:     MarketSpot,
		WSURL:      wsBase(srv.URL),
		RESTURL:    srv.URL,
		HTTPClient: srv.Client(),
		Watchdog:   150 * time.Millisecond,
		BackoffMin: 10 * time.Millisecond,
		BackoffMax: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewBinanceFeed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := feed.Subscribe(ctx, []string{"BTC/USDT"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Bootstrap ticks (mark 100) arrive per connection; the post-reconnect
	// WS tick (mark 101) proves the stream resumed after the watchdog fired.
	saw101 := false
	for i := 0; i < 20 && !saw101; i++ {
		saw101 = recvTick(t, ch).Mark.String() == "101"
	}
	if !saw101 {
		t.Fatal("never received the post-reconnect WS tick")
	}
	if got := dials.Load(); got < 2 {
		t.Fatalf("dials = %d, want >= 2 (watchdog reconnect)", got)
	}
	if got := snapshots.Load(); got < 2 {
		t.Fatalf("snapshots = %d, want >= 2 (re-snapshot on reconnect)", got)
	}
}
