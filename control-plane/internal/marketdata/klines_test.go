package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// klineRow renders one Binance kline row with trailing fields, exercising
// the >= 6 tolerance and verbatim string preservation.
func klineRow(openTime int64, o, h, l, c, v string) string {
	return fmt.Sprintf(`[%d,%q,%q,%q,%q,%q,%d,"0",0,"0","0","0"]`,
		openTime, o, h, l, c, v, openTime+59999)
}

func TestFetchKlinesPaginatesAndPreservesStrings(t *testing.T) {
	const intervalMS = 60_000
	var (
		mu     sync.Mutex
		starts []int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/klines" {
			t.Errorf("path = %q, want /api/v3/klines", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbol"); got != "BTCUSDT" {
			t.Errorf("symbol = %q, want BTCUSDT", got)
		}
		if got := r.URL.Query().Get("interval"); got != "1m" {
			t.Errorf("interval = %q, want 1m", got)
		}
		if got := r.URL.Query().Get("limit"); got != strconv.Itoa(klinesPageLimit) {
			t.Errorf("limit = %q, want %d", got, klinesPageLimit)
		}
		start, err := strconv.ParseInt(r.URL.Query().Get("startTime"), 10, 64)
		if err != nil {
			t.Errorf("startTime: %v", err)
		}
		end, _ := strconv.ParseInt(r.URL.Query().Get("endTime"), 10, 64)
		mu.Lock()
		starts = append(starts, start)
		mu.Unlock()
		rows := make([]json.RawMessage, 0, klinesPageLimit)
		// Binance returns grid-aligned candles regardless of startTime.
		first := ((start + intervalMS - 1) / intervalMS) * intervalMS
		for ts := first; ts <= end && len(rows) < klinesPageLimit; ts += intervalMS {
			rows = append(rows, json.RawMessage(klineRow(ts, "64000.50", "64100.00", "63900.10", "64050.00", "10.5")))
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	// 1500 one-minute candles force two pages under the 1000-row cap.
	startMS := int64(1_700_000_040_000) // 60s-grid-aligned
	endMS := startMS + 1499*intervalMS
	got, err := FetchKlines(context.Background(), BinanceConfig{Market: MarketSpot, RESTURL: srv.URL},
		"BTC/USDT", "1m", startMS, endMS)
	if err != nil {
		t.Fatalf("FetchKlines: %v", err)
	}
	if len(got) != 1500 {
		t.Fatalf("len = %d, want 1500", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 {
		t.Fatalf("requests = %d (%v), want 2", len(starts), starts)
	}
	if wantSecond := startMS + 999*intervalMS + 1; starts[1] != wantSecond {
		t.Fatalf("second page startTime = %d, want %d", starts[1], wantSecond)
	}
	first, last := got[0], got[len(got)-1]
	if first.OpenTime != startMS || last.OpenTime != endMS {
		t.Fatalf("open_time range [%d, %d], want [%d, %d]", first.OpenTime, last.OpenTime, startMS, endMS)
	}
	// Verbatim venue strings: trailing zeros survive (never float64).
	if first.Open != "64000.50" || first.High != "64100.00" || first.Low != "63900.10" ||
		first.Close != "64050.00" || first.Volume != "10.5" {
		t.Fatalf("verbatim strings lost: %+v", first)
	}
}

func TestFetchKlinesFuturesPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/klines" {
			t.Errorf("path = %q, want /fapi/v1/klines", r.URL.Path)
		}
		fmt.Fprintf(w, "[%s]", klineRow(1_700_000_000_000, "1", "2", "0.5", "1.5", "3"))
	}))
	defer srv.Close()

	got, err := FetchKlines(context.Background(), BinanceConfig{Market: MarketFutures, RESTURL: srv.URL},
		"ETH/USDT", "1h", 1_700_000_000_000, 1_700_000_000_000)
	if err != nil {
		t.Fatalf("FetchKlines: %v", err)
	}
	if len(got) != 1 || got[0].Close != "1.5" {
		t.Fatalf("got %+v, want one kline with close 1.5", got)
	}
}

func TestFetchKlinesErrors(t *testing.T) {
	status := http.StatusTeapot
	body := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	ctx := context.Background()
	base := BinanceConfig{Market: MarketSpot, RESTURL: srv.URL}

	if _, err := FetchKlines(ctx, base, "BTC/USDT", "1m", 2, 1); err == nil {
		t.Fatal("inverted window accepted, want error")
	}
	if _, err := FetchKlines(ctx, BinanceConfig{Market: "margin", RESTURL: srv.URL}, "BTC/USDT", "1m", 1, 2); err == nil {
		t.Fatal("unknown market accepted, want error")
	}
	if _, err := FetchKlines(ctx, base, "BTCUSDT", "1m", 1, 2); err == nil {
		t.Fatal("venue-form symbol accepted, want error")
	}
	if _, err := FetchKlines(ctx, base, "BTC/USDT", "1m", 1, 2); err == nil {
		t.Fatal("non-200 accepted, want error")
	}

	status = http.StatusOK
	body = `[[1,"not-a-decimal","2","0.5","1.5","3",2]]`
	if _, err := FetchKlines(ctx, base, "BTC/USDT", "1m", 1, 2); err == nil {
		t.Fatal("non-decimal price accepted, want error")
	}
	body = `[[1,"1","2"]]`
	if _, err := FetchKlines(ctx, base, "BTC/USDT", "1m", 1, 2); err == nil {
		t.Fatal("short row accepted, want error")
	}
}
