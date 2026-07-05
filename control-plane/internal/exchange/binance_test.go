package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testAPIKey = "test-api-key"
	// The HMAC vector secret/query/signature from Binance's signed-endpoint
	// documentation example.
	testSecret    = "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
	vectorPayload = "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559"
	vectorSig     = "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"

	fixedMillis = int64(1499827319559)
)

// testBinance wires the adapter to an httptest server with a fixed clock.
func testBinance(t *testing.T, handler http.Handler) *Binance {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	b := NewBinance(EnvTestnet, testAPIKey, testSecret, srv.Client())
	b.baseURL = srv.URL
	b.now = func() time.Time { return time.UnixMilli(fixedMillis) }
	return b
}

func TestSignVector(t *testing.T) {
	b := NewBinance(EnvTestnet, testAPIKey, testSecret, nil)
	if got := b.sign(vectorPayload); got != vectorSig {
		t.Fatalf("sign = %s, want %s", got, vectorSig)
	}
}

func TestPlaceOrderSignedRequest(t *testing.T) {
	var captured *http.Request
	b := testBinance(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		w.Write([]byte(`{"orderId":42,"status":"NEW","transactTime":1499827319600}`))
	}))
	ack, err := b.PlaceOrder(context.Background(), PlaceRequest{
		VenueSymbol: "BTCUSDT", Side: "BUY", Type: "LIMIT", Qty: "0.5",
		Price: "30000", NewClientOrderID: "amx1-abc-0", TimeInForce: "GTC",
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ack.ExchangeOrderID != 42 || ack.Status != "NEW" ||
		!ack.TransactTime.Equal(time.UnixMilli(1499827319600).UTC()) {
		t.Fatalf("ack = %+v", ack)
	}
	if captured.Method != http.MethodPost || captured.URL.Path != "/api/v3/order" {
		t.Fatalf("request = %s %s", captured.Method, captured.URL.Path)
	}
	if got := captured.Header.Get("X-MBX-APIKEY"); got != testAPIKey {
		t.Fatalf("X-MBX-APIKEY = %q", got)
	}
	q := captured.URL.Query()
	for key, want := range map[string]string{
		"symbol": "BTCUSDT", "side": "BUY", "type": "LIMIT", "quantity": "0.5",
		"price": "30000", "newClientOrderId": "amx1-abc-0", "timeInForce": "GTC",
		"timestamp": "1499827319559", "recvWindow": "5000",
	} {
		if got := q.Get(key); got != want {
			t.Fatalf("query %s = %q, want %q", key, got, want)
		}
	}
	// Deterministic signature: the signature is appended last, over the
	// exact encoded query that precedes it.
	raw := captured.URL.RawQuery
	i := strings.LastIndex(raw, "&signature=")
	if i < 0 {
		t.Fatalf("no signature in query %q", raw)
	}
	if got, want := raw[i+len("&signature="):], b.sign(raw[:i]); got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func TestMyTradesParamSelection(t *testing.T) {
	start := time.UnixMilli(1700000000000).UTC()
	tests := []struct {
		name      string
		fromID    int64
		startTime time.Time
		limit     int
		wantFrom  string
		wantStart string
		wantLimit string
	}{
		{"fromId wins when positive", 42, start, 1000, "42", "", "1000"},
		{"startTime when fromID zero", 0, start, 500, "", "1700000000000", "500"},
		{"neither when both zero", 0, time.Time{}, 0, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var q url.Values
			b := testBinance(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q = r.URL.Query()
				w.Write([]byte(`[]`))
			}))
			if _, err := b.MyTrades(context.Background(), "BTCUSDT", tc.fromID, tc.startTime, tc.limit); err != nil {
				t.Fatalf("MyTrades: %v", err)
			}
			if got := q.Get("fromId"); got != tc.wantFrom {
				t.Fatalf("fromId = %q, want %q", got, tc.wantFrom)
			}
			if got := q.Get("startTime"); got != tc.wantStart {
				t.Fatalf("startTime = %q, want %q", got, tc.wantStart)
			}
			if got := q.Get("limit"); got != tc.wantLimit {
				t.Fatalf("limit = %q, want %q", got, tc.wantLimit)
			}
		})
	}
}

func TestHTTPErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		wantClass  Class
		wantCode   int
		wantRetry  time.Duration
	}{
		{"insufficient balance", 400, "", `{"code":-2010,"msg":"Account has insufficient balance for requested action."}`, ClassDefiniteReject, -2010, 0},
		{"order does not exist", 400, "", `{"code":-2013,"msg":"Order does not exist."}`, ClassNotFound, -2013, 0},
		{"cancel reject", 400, "", `{"code":-2011,"msg":"Unknown order sent."}`, ClassNotFound, -2011, 0},
		{"throttled with retry-after", 429, "7", `{"code":-1003,"msg":"Too many requests."}`, ClassThrottled, -1003, 7 * time.Second},
		{"throttled without retry-after", 429, "", `{"code":-1003,"msg":"Too many requests."}`, ClassAmbiguous, -1003, 0},
		{"maintenance 5xx", 500, "", `{}`, ClassAmbiguous, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := testBinance(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			_, err := b.PlaceOrder(context.Background(), PlaceRequest{
				VenueSymbol: "BTCUSDT", Side: "BUY", Type: "MARKET", Qty: "1",
				NewClientOrderID: "amx1-abc-0",
			})
			if err == nil {
				t.Fatal("PlaceOrder: expected error")
			}
			if got := Classify(err); got != tc.wantClass {
				t.Fatalf("class = %s, want %s", got, tc.wantClass)
			}
			ve, ok := err.(*VenueError)
			if !ok {
				t.Fatalf("error type %T, want *VenueError", err)
			}
			if ve.VenueCode != tc.wantCode || ve.RetryAfter != tc.wantRetry {
				t.Fatalf("code/retry = %d/%s, want %d/%s", ve.VenueCode, ve.RetryAfter, tc.wantCode, tc.wantRetry)
			}
			assertRedacted(t, err.Error())
		})
	}
}

func TestTransportErrorRedaction(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	b := NewBinance(EnvTestnet, testAPIKey, testSecret, srv.Client())
	b.baseURL = srv.URL
	srv.Close() // force a connection error carrying the URL in url.Error
	_, err := b.Balances(context.Background())
	if err == nil {
		t.Fatal("Balances: expected error")
	}
	if got := Classify(err); got != ClassAmbiguous {
		t.Fatalf("class = %s, want ambiguous", got)
	}
	assertRedacted(t, err.Error())
}

func TestClockSkewRetryOnce(t *testing.T) {
	tests := []struct {
		name        string
		skewForever bool
		wantOrders  int
		wantErr     bool
	}{
		{"second attempt succeeds with refreshed offset", false, 2, false},
		{"second -1021 is definite reject", true, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var orderCalls, timeCalls int
			var lastTimestamp string
			b := testBinance(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/time":
					timeCalls++
					w.Write([]byte(`{"serverTime":` + "1499827325559" + `}`))
				case "/api/v3/order":
					orderCalls++
					lastTimestamp = r.URL.Query().Get("timestamp")
					if orderCalls == 1 || tc.skewForever {
						w.WriteHeader(400)
						w.Write([]byte(`{"code":-1021,"msg":"Timestamp for this request is outside of the recvWindow."}`))
						return
					}
					w.Write([]byte(`{"orderId":7,"status":"NEW","transactTime":1499827325600}`))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			_, err := b.PlaceOrder(context.Background(), PlaceRequest{
				VenueSymbol: "BTCUSDT", Side: "BUY", Type: "MARKET", Qty: "1",
				NewClientOrderID: "amx1-abc-0",
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && Classify(err) != ClassDefiniteReject {
				t.Fatalf("class = %s, want definite_reject", Classify(err))
			}
			if orderCalls != tc.wantOrders || timeCalls != 1 {
				t.Fatalf("orderCalls = %d (want %d), timeCalls = %d (want 1)", orderCalls, tc.wantOrders, timeCalls)
			}
			// The retried request signs with the refreshed offset: its
			// timestamp equals the venue clock exactly (fixed local clock).
			if want := "1499827325559"; lastTimestamp != want {
				t.Fatalf("retry timestamp = %s, want %s", lastTimestamp, want)
			}
		})
	}
}

func TestSymbolMappingRoundTrip(t *testing.T) {
	canonical := []string{"BTC/USDT", "ETH/BTC", "SOL/USDC"}
	for _, c := range canonical {
		venue, err := VenueSymbol(c)
		if err != nil {
			t.Fatalf("VenueSymbol(%q): %v", c, err)
		}
		back, ok := CanonicalSymbol(venue, canonical)
		if !ok || back != c {
			t.Fatalf("round trip %q -> %q -> (%q, %v)", c, venue, back, ok)
		}
	}
	if got, err := VenueSymbol("BTC/USDT"); err != nil || got != "BTCUSDT" {
		t.Fatalf("VenueSymbol(BTC/USDT) = %q, %v", got, err)
	}
	for _, bad := range []string{"BTCUSDT", "/USDT", "BTC/", "BTC/USD/T", ""} {
		if _, err := VenueSymbol(bad); err == nil {
			t.Fatalf("VenueSymbol(%q): expected error", bad)
		}
	}
	if _, ok := CanonicalSymbol("DOGEUSDT", canonical); ok {
		t.Fatal("CanonicalSymbol(DOGEUSDT): expected miss")
	}
}

func TestBaseURLsPerEnv(t *testing.T) {
	tn := NewBinance(EnvTestnet, "k", "s", nil)
	if tn.baseURL != testnetRESTURL || tn.wsURL != testnetWSURL {
		t.Fatalf("testnet URLs = %s / %s", tn.baseURL, tn.wsURL)
	}
	pr := NewBinance(EnvProd, "k", "s", nil)
	if pr.baseURL != prodRESTURL || pr.wsURL != prodWSURL {
		t.Fatalf("prod URLs = %s / %s", pr.baseURL, pr.wsURL)
	}
}
