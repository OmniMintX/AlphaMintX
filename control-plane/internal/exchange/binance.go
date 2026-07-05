package exchange

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Env selects the Binance venue. Base URLs are hardcoded per env and NOT
// configurable (spec §Config: no URL-swap foot-gun); tests override the
// unexported baseURL/wsURL fields to dial an httptest fake.
type Env string

const (
	EnvTestnet Env = "testnet"
	EnvProd    Env = "prod"
)

const (
	testnetRESTURL = "https://testnet.binance.vision"
	testnetWSURL   = "wss://stream.testnet.binance.vision"
	prodRESTURL    = "https://api.binance.com"
	prodWSURL      = "wss://stream.binance.com:9443"

	defaultRecvWindow = 5000 * time.Millisecond
)

// Binance is the spot venue adapter. Signed requests carry timestamp (local
// clock plus a maintained server-time offset) and recvWindow, HMAC-SHA256
// signed over the query string.
type Binance struct {
	env        Env
	apiKey     string
	apiSecret  string
	httpClient *http.Client

	// RecvWindow is sent as recvWindow on every signed request. Default
	// 5000 ms (spec §Config recv_window_ms).
	RecvWindow time.Duration

	baseURL string // test-only override; constructors keep the env value
	wsURL   string // test-only override; constructors keep the env value
	now     func() time.Time

	mu         sync.Mutex
	timeOffset time.Duration // venue clock minus local clock
}

var _ Exchange = (*Binance)(nil)

// NewBinance builds an adapter for the given venue env. Any env other than
// EnvProd resolves to the testnet endpoints (fail toward the safe venue).
// A nil httpClient uses http.DefaultClient.
func NewBinance(env Env, apiKey, apiSecret string, httpClient *http.Client) *Binance {
	b := &Binance{
		env:        env,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		httpClient: httpClient,
		RecvWindow: defaultRecvWindow,
		baseURL:    testnetRESTURL,
		wsURL:      testnetWSURL,
		now:        time.Now,
	}
	if env == EnvProd {
		b.baseURL = prodRESTURL
		b.wsURL = prodWSURL
	}
	if b.httpClient == nil {
		b.httpClient = http.DefaultClient
	}
	return b
}

// VenueSymbol maps a canonical BASE/QUOTE symbol to the Binance concatenated
// uppercase form: "BTC/USDT" -> "BTCUSDT".
func VenueSymbol(canonical string) (string, error) {
	base, quote, ok := strings.Cut(canonical, "/")
	if !ok || base == "" || quote == "" || strings.Contains(quote, "/") {
		return "", fmt.Errorf("exchange: invalid canonical symbol %q (want BASE/QUOTE)", canonical)
	}
	return strings.ToUpper(base) + strings.ToUpper(quote), nil
}

// CanonicalSymbol reverses VenueSymbol against the provided canonical-symbols
// list (the deployment's configured symbols). It reports false when the
// venue symbol matches none of them.
func CanonicalSymbol(venueSymbol string, canonicalSymbols []string) (string, bool) {
	for _, c := range canonicalSymbols {
		if v, err := VenueSymbol(c); err == nil && v == strings.ToUpper(venueSymbol) {
			return c, true
		}
	}
	return "", false
}

// ExchangeInfo fetches trading filters for the given venue symbols.
func (b *Binance) ExchangeInfo(ctx context.Context, venueSymbols []string) (Filters, error) {
	const op = "ExchangeInfo"
	syms, err := json.Marshal(venueSymbols)
	if err != nil {
		return nil, ambiguous(op, err)
	}
	var resp struct {
		Symbols []struct {
			Symbol  string `json:"symbol"`
			Filters []struct {
				FilterType  string `json:"filterType"`
				TickSize    string `json:"tickSize"`
				StepSize    string `json:"stepSize"`
				MinQty      string `json:"minQty"`
				MaxQty      string `json:"maxQty"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	q := url.Values{"symbols": {string(syms)}}
	if err := b.doPublic(ctx, op, "/api/v3/exchangeInfo", q, &resp); err != nil {
		return nil, err
	}
	filters := make(Filters, len(resp.Symbols))
	for _, s := range resp.Symbols {
		var f SymbolFilters
		for _, fl := range s.Filters {
			switch fl.FilterType {
			case "PRICE_FILTER":
				f.TickSize = fl.TickSize
			case "LOT_SIZE":
				f.StepSize, f.MinQty, f.MaxQty = fl.StepSize, fl.MinQty, fl.MaxQty
			case "NOTIONAL", "MIN_NOTIONAL":
				f.MinNotional = fl.MinNotional
			}
		}
		filters[s.Symbol] = f
	}
	return filters, nil
}

// orderJSON covers the order shapes from place/query/cancel/openOrders.
type orderJSON struct {
	Symbol            string `json:"symbol"`
	OrderID           int64  `json:"orderId"`
	ClientOrderID     string `json:"clientOrderId"`
	OrigClientOrderID string `json:"origClientOrderId"`
	Status            string `json:"status"`
	Side              string `json:"side"`
	Type              string `json:"type"`
	Price             string `json:"price"`
	StopPrice         string `json:"stopPrice"`
	OrigQty           string `json:"origQty"`
	ExecutedQty       string `json:"executedQty"`
	CumQuoteQty       string `json:"cummulativeQuoteQty"`
	UpdateTime        int64  `json:"updateTime"`
	TransactTime      int64  `json:"transactTime"`
}

// toOrderState maps a venue order row. Cancel responses put the order's own
// id in origClientOrderId (clientOrderId is the cancel request's id), so the
// original id wins when present.
func toOrderState(j orderJSON) OrderState {
	cid := j.ClientOrderID
	if j.OrigClientOrderID != "" {
		cid = j.OrigClientOrderID
	}
	ts := j.UpdateTime
	if ts == 0 {
		ts = j.TransactTime
	}
	return OrderState{
		VenueSymbol:     j.Symbol,
		ExchangeOrderID: j.OrderID,
		ClientOrderID:   cid,
		Status:          j.Status,
		Side:            j.Side,
		Type:            j.Type,
		Price:           j.Price,
		StopPrice:       j.StopPrice,
		OrigQty:         j.OrigQty,
		ExecutedQty:     j.ExecutedQty,
		CumQuoteQty:     j.CumQuoteQty,
		UpdatedAt:       time.UnixMilli(ts).UTC(),
	}
}

// PlaceOrder submits one order (POST /api/v3/order).
func (b *Binance) PlaceOrder(ctx context.Context, req PlaceRequest) (Ack, error) {
	q := url.Values{}
	q.Set("symbol", req.VenueSymbol)
	q.Set("side", req.Side)
	q.Set("type", req.Type)
	q.Set("quantity", req.Qty)
	q.Set("newClientOrderId", req.NewClientOrderID)
	if req.Price != "" {
		q.Set("price", req.Price)
	}
	if req.StopPrice != "" {
		q.Set("stopPrice", req.StopPrice)
	}
	if req.TimeInForce != "" {
		q.Set("timeInForce", req.TimeInForce)
	}
	var resp orderJSON
	if err := b.doSigned(ctx, "PlaceOrder", http.MethodPost, "/api/v3/order", q, &resp); err != nil {
		return Ack{}, err
	}
	return Ack{
		ExchangeOrderID: resp.OrderID,
		Status:          resp.Status,
		TransactTime:    time.UnixMilli(resp.TransactTime).UTC(),
	}, nil
}

// QueryOrder fetches one order by origClientOrderId (GET /api/v3/order).
func (b *Binance) QueryOrder(ctx context.Context, venueSymbol, origClientOrderID string) (OrderState, error) {
	q := url.Values{"symbol": {venueSymbol}, "origClientOrderId": {origClientOrderID}}
	var resp orderJSON
	if err := b.doSigned(ctx, "QueryOrder", http.MethodGet, "/api/v3/order", q, &resp); err != nil {
		return OrderState{}, err
	}
	return toOrderState(resp), nil
}

// CancelOrder cancels one order by origClientOrderId (DELETE /api/v3/order).
func (b *Binance) CancelOrder(ctx context.Context, venueSymbol, origClientOrderID string) (OrderState, error) {
	q := url.Values{"symbol": {venueSymbol}, "origClientOrderId": {origClientOrderID}}
	var resp orderJSON
	if err := b.doSigned(ctx, "CancelOrder", http.MethodDelete, "/api/v3/order", q, &resp); err != nil {
		return OrderState{}, err
	}
	return toOrderState(resp), nil
}

// OpenOrders lists open orders for one symbol (GET /api/v3/openOrders).
func (b *Binance) OpenOrders(ctx context.Context, venueSymbol string) ([]OrderState, error) {
	q := url.Values{"symbol": {venueSymbol}}
	var resp []orderJSON
	if err := b.doSigned(ctx, "OpenOrders", http.MethodGet, "/api/v3/openOrders", q, &resp); err != nil {
		return nil, err
	}
	out := make([]OrderState, len(resp))
	for i, j := range resp {
		out[i] = toOrderState(j)
	}
	return out, nil
}

// MyTrades pages account trades (GET /api/v3/myTrades): fromId when
// fromID > 0, else startTime when nonzero (bootstrap only — spec paging
// handoff rule); limit <= 0 uses the venue default.
func (b *Binance) MyTrades(ctx context.Context, venueSymbol string, fromID int64, startTime time.Time, limit int) ([]Trade, error) {
	q := url.Values{"symbol": {venueSymbol}}
	if fromID > 0 {
		q.Set("fromId", strconv.FormatInt(fromID, 10))
	} else if !startTime.IsZero() {
		q.Set("startTime", strconv.FormatInt(startTime.UnixMilli(), 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp []struct {
		ID              int64  `json:"id"`
		OrderID         int64  `json:"orderId"`
		Symbol          string `json:"symbol"`
		Price           string `json:"price"`
		Qty             string `json:"qty"`
		Commission      string `json:"commission"`
		CommissionAsset string `json:"commissionAsset"`
		IsBuyer         bool   `json:"isBuyer"`
		Time            int64  `json:"time"`
	}
	if err := b.doSigned(ctx, "MyTrades", http.MethodGet, "/api/v3/myTrades", q, &resp); err != nil {
		return nil, err
	}
	out := make([]Trade, len(resp))
	for i, t := range resp {
		out[i] = Trade{
			ID:              t.ID,
			ExchangeOrderID: t.OrderID,
			VenueSymbol:     t.Symbol,
			Price:           t.Price,
			Qty:             t.Qty,
			Commission:      t.Commission,
			CommissionAsset: t.CommissionAsset,
			IsBuyer:         t.IsBuyer,
			Time:            time.UnixMilli(t.Time).UTC(),
		}
	}
	return out, nil
}

// Balances returns free/locked per asset (GET /api/v3/account).
func (b *Binance) Balances(ctx context.Context) ([]Balance, error) {
	var resp struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := b.doSigned(ctx, "Balances", http.MethodGet, "/api/v3/account", url.Values{}, &resp); err != nil {
		return nil, err
	}
	out := make([]Balance, len(resp.Balances))
	for i, bl := range resp.Balances {
		out[i] = Balance{Asset: bl.Asset, Free: bl.Free, Locked: bl.Locked}
	}
	return out, nil
}

// NewListenKey opens a user-data stream (POST /api/v3/userDataStream;
// API-key header, no signature).
func (b *Binance) NewListenKey(ctx context.Context) (string, error) {
	var resp struct {
		ListenKey string `json:"listenKey"`
	}
	if err := b.doKeyed(ctx, "NewListenKey", http.MethodPost, "/api/v3/userDataStream", nil, &resp); err != nil {
		return "", err
	}
	return resp.ListenKey, nil
}

// KeepAliveListenKey extends the key (PUT /api/v3/userDataStream).
func (b *Binance) KeepAliveListenKey(ctx context.Context, key string) error {
	q := url.Values{"listenKey": {key}}
	return b.doKeyed(ctx, "KeepAliveListenKey", http.MethodPut, "/api/v3/userDataStream", q, nil)
}

// StreamUserData dials the user-data WS and decodes frames into UserEvents.
// The channel closes on ctx cancellation or connection failure; frames that
// are not executionReport/listenKeyExpired are skipped.
func (b *Binance) StreamUserData(ctx context.Context, key string) (<-chan UserEvent, error) {
	conn, _, err := websocket.Dial(ctx, b.wsURL+"/ws/"+key, &websocket.DialOptions{HTTPClient: b.httpClient})
	if err != nil {
		return nil, ambiguous("StreamUserData", err)
	}
	ch := make(chan UserEvent, 64)
	go func() {
		defer close(ch)
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			ev, ok := parseUserEvent(data)
			if !ok {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// ServerTime fetches the venue clock (GET /api/v3/time) and refreshes the
// signing offset.
func (b *Binance) ServerTime(ctx context.Context) (time.Time, error) {
	var resp struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := b.doPublic(ctx, "ServerTime", "/api/v3/time", nil, &resp); err != nil {
		return time.Time{}, err
	}
	st := time.UnixMilli(resp.ServerTime).UTC()
	b.mu.Lock()
	b.timeOffset = st.Sub(b.now())
	b.mu.Unlock()
	return st, nil
}

// sign returns the lowercase-hex HMAC-SHA256 of payload under the secret.
func (b *Binance) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// timestamp is the signing timestamp: local clock plus the maintained
// server-time offset, in venue milliseconds.
func (b *Binance) timestamp() int64 {
	b.mu.Lock()
	off := b.timeOffset
	b.mu.Unlock()
	return b.now().Add(off).UnixMilli()
}

// doSigned executes one signed call. On venue code -1021 (clock skew) it
// refreshes the offset via ServerTime and retries ONCE; a second -1021
// surfaces as-is (a 4xx with code classifies as DefiniteReject).
func (b *Binance) doSigned(ctx context.Context, op, method, path string, params url.Values, out any) error {
	err := b.doSignedOnce(ctx, op, method, path, params, out)
	var ve *VenueError
	if errors.As(err, &ve) && ve.VenueCode == -1021 {
		if _, terr := b.ServerTime(ctx); terr == nil {
			err = b.doSignedOnce(ctx, op, method, path, params, out)
		}
	}
	return err
}

func (b *Binance) doSignedOnce(ctx context.Context, op, method, path string, params url.Values, out any) error {
	q := url.Values{}
	for k, vs := range params {
		q[k] = vs
	}
	q.Set("timestamp", strconv.FormatInt(b.timestamp(), 10))
	q.Set("recvWindow", strconv.FormatInt(b.RecvWindow.Milliseconds(), 10))
	encoded := q.Encode()
	encoded += "&signature=" + b.sign(encoded)
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path+"?"+encoded, nil)
	if err != nil {
		return ambiguous(op, err)
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)
	return b.do(op, req, out)
}

// doKeyed executes an API-key-only call (userDataStream endpoints).
func (b *Binance) doKeyed(ctx context.Context, op, method, path string, params url.Values, out any) error {
	u := b.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return ambiguous(op, err)
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)
	return b.do(op, req, out)
}

// doPublic executes an unauthenticated GET (exchangeInfo, time).
func (b *Binance) doPublic(ctx context.Context, op, path string, params url.Values, out any) error {
	u := b.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ambiguous(op, err)
	}
	return b.do(op, req, out)
}

func (b *Binance) do(op string, req *http.Request, out any) error {
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return ambiguous(op, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ambiguous(op, err)
	}
	if resp.StatusCode != http.StatusOK {
		return venueError(op, resp, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &VenueError{Op: op, Class: ClassAmbiguous, VenueMsg: "malformed venue response"}
	}
	return nil
}

// ambiguous wraps a transport-level failure as Ambiguous. url.Error is
// unwrapped so the message never carries the request URL (spec Redaction).
func ambiguous(op string, err error) *VenueError {
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		msg = ue.Err.Error()
	}
	return &VenueError{Op: op, Class: ClassAmbiguous, VenueMsg: msg}
}

// venueError builds the classified error for a non-200 response, carrying
// only the venue code and msg (spec Redaction).
func venueError(op string, resp *http.Response, body []byte) *VenueError {
	var ve struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(body, &ve)
	class, ra := classify(resp.StatusCode, ve.Code, parseRetryAfter(resp.Header.Get("Retry-After")))
	return &VenueError{Op: op, Class: class, VenueCode: ve.Code, VenueMsg: ve.Msg, RetryAfter: ra}
}

// parseRetryAfter parses a delay-seconds Retry-After header value.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// userDataMsg decodes executionReport and listenKeyExpired frames. The pin
// fields ("C","O","T","Z","I") exist only to stop encoding/json's
// case-insensitive fallback from clobbering the tagged single-letter keys
// (same pattern as marketdata's wsEvent).
type userDataMsg struct {
	EventType       string `json:"e"`
	EventTime       int64  `json:"E"` // ms
	Symbol          string `json:"s"`
	ClientOrderID   string `json:"c"`
	Side            string `json:"S"`
	OrderType       string `json:"o"`
	ExecType        string `json:"x"`
	OrderStatus     string `json:"X"`
	OrderID         int64  `json:"i"`
	LastQty         string `json:"l"`
	LastPrice       string `json:"L"`
	CumQty          string `json:"z"`
	Commission      string `json:"n"`
	CommissionAsset string `json:"N"`
	TradeID         int64  `json:"t"`

	OrigClientPin  string `json:"C"`
	OrderCreatePin int64  `json:"O"`
	TransactPin    int64  `json:"T"`
	CumQuotePin    string `json:"Z"`
	IgnorePin      int64  `json:"I"`
}

// parseUserEvent converts one WS frame into a UserEvent. Unknown event types
// and frames that fail to parse are skipped: a bad frame must not tear down
// an otherwise healthy connection.
func parseUserEvent(data []byte) (UserEvent, bool) {
	var m userDataMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return UserEvent{}, false
	}
	switch m.EventType {
	case UserEventListenKeyExpired:
		return UserEvent{
			Kind:      UserEventListenKeyExpired,
			EventTime: time.UnixMilli(m.EventTime).UTC(),
		}, true
	case UserEventExecutionReport:
		return UserEvent{
			Kind:            UserEventExecutionReport,
			VenueSymbol:     m.Symbol,
			ClientOrderID:   m.ClientOrderID,
			Side:            m.Side,
			OrderType:       m.OrderType,
			ExecType:        m.ExecType,
			OrderStatus:     m.OrderStatus,
			ExchangeOrderID: m.OrderID,
			LastQty:         m.LastQty,
			LastPrice:       m.LastPrice,
			CumQty:          m.CumQty,
			Commission:      m.Commission,
			CommissionAsset: m.CommissionAsset,
			TradeID:         m.TradeID,
			EventTime:       time.UnixMilli(m.EventTime).UTC(),
		}, true
	}
	return UserEvent{}, false
}
