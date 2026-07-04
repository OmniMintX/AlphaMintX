package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/shopspring/decimal"
)

// Market selects the Binance venue a BinanceFeed connects to.
type Market string

const (
	MarketSpot    Market = "spot"    // spot mark = last trade price
	MarketFutures Market = "futures" // USDT-M venue mark price
)

// Production endpoints and the normative keepalive/backoff parameters
// (docs/specs/market-data.md §BinanceFeed).
const (
	spotWSURL         = "wss://stream.binance.com:9443"
	spotRESTURL       = "https://api.binance.com"
	futuresWSURL      = "wss://fstream.binance.com"
	futuresRESTURL    = "https://fapi.binance.com"
	defaultWatchdog   = 60 * time.Second
	defaultBackoffMin = 100 * time.Millisecond
	defaultBackoffMax = 30 * time.Second
)

// BinanceConfig configures a BinanceFeed. Market is required; every other
// field defaults to its production value when zero. WSURL/RESTURL exist so
// tests can dial an httptest fake instead of the real venue.
type BinanceConfig struct {
	Market     Market
	WSURL      string        // base WS URL, e.g. "wss://stream.binance.com:9443"
	RESTURL    string        // base REST URL, e.g. "https://api.binance.com"
	HTTPClient *http.Client  // used for both the REST bootstrap and the WS dial
	Watchdog   time.Duration // silent-connection cutoff (spec: 60 s)
	BackoffMin time.Duration // reconnect backoff floor (spec: 100 ms)
	BackoffMax time.Duration // reconnect backoff cap (spec: 30 s)
}

// BinanceFeed streams mark prices over one combined-stream Binance WS
// connection per venue, bootstrapping via REST on start and on every
// reconnect so ticks missed while disconnected never leave a stale gap.
type BinanceFeed struct {
	cfg BinanceConfig
}

var _ Feed = (*BinanceFeed)(nil)

// NewBinanceFeed validates the config and fills production defaults.
func NewBinanceFeed(cfg BinanceConfig) (*BinanceFeed, error) {
	switch cfg.Market {
	case MarketSpot:
		if cfg.WSURL == "" {
			cfg.WSURL = spotWSURL
		}
		if cfg.RESTURL == "" {
			cfg.RESTURL = spotRESTURL
		}
	case MarketFutures:
		if cfg.WSURL == "" {
			cfg.WSURL = futuresWSURL
		}
		if cfg.RESTURL == "" {
			cfg.RESTURL = futuresRESTURL
		}
	default:
		return nil, fmt.Errorf("marketdata: unknown market %q", cfg.Market)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Watchdog <= 0 {
		cfg.Watchdog = defaultWatchdog
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = defaultBackoffMax
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = cfg.BackoffMin
	}
	return &BinanceFeed{cfg: cfg}, nil
}

// Subscribe opens the combined stream for the given canonical symbols. The
// returned channel is closed when ctx is done; connection failures are
// retried with backoff, never surfaced as channel closure.
func (f *BinanceFeed) Subscribe(ctx context.Context, symbols []string) (<-chan Tick, error) {
	if len(symbols) == 0 {
		return nil, fmt.Errorf("marketdata: no symbols to subscribe")
	}
	canonical := make(map[string]string, len(symbols)) // venue -> canonical
	streams := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		venue, err := ToBinance(sym)
		if err != nil {
			return nil, err
		}
		canonical[venue] = sym
		streams = append(streams, f.streamName(venue))
	}
	dialURL := f.cfg.WSURL + "/stream?streams=" + strings.Join(streams, "/")
	ch := make(chan Tick, len(symbols))
	go f.run(ctx, dialURL, canonical, ch)
	return ch, nil
}

// streamName is the per-symbol stream: spot last-trade (the same price basis
// as the ticker/price bootstrap) or the futures venue mark price.
func (f *BinanceFeed) streamName(venue string) string {
	if f.cfg.Market == MarketFutures {
		return strings.ToLower(venue) + "@markPrice@1s"
	}
	return strings.ToLower(venue) + "@trade"
}

// run owns the connection lifecycle: dial, REST re-snapshot, read until the
// watchdog or the connection fails, then reconnect with exponential backoff
// (100 ms -> 30 s cap, jittered; well inside Binance's ~300 attempts / 5 min
// limit). Backoff resets on every successful (re-)subscribe.
func (f *BinanceFeed) run(ctx context.Context, dialURL string, canonical map[string]string, ch chan<- Tick) {
	defer close(ch)
	delay := f.cfg.BackoffMin
	for {
		_ = f.connectOnce(ctx, dialURL, canonical, ch, func() { delay = f.cfg.BackoffMin })
		if ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, jitter(delay)) {
			return
		}
		delay *= 2
		if delay > f.cfg.BackoffMax {
			delay = f.cfg.BackoffMax
		}
	}
}

// connectOnce dials the combined stream, re-runs the REST bootstrap before
// consuming any WS tick (spec: re-snapshot on every reconnect), then reads
// until the connection dies or stays silent past the watchdog. The
// coder/websocket Read loop answers server pings automatically, satisfying
// the 20 s-ping / 60 s-pong keepalive.
func (f *BinanceFeed) connectOnce(ctx context.Context, dialURL string, canonical map[string]string, ch chan<- Tick, onSubscribed func()) error {
	conn, _, err := websocket.Dial(ctx, dialURL, &websocket.DialOptions{HTTPClient: f.cfg.HTTPClient})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	if err := f.bootstrap(ctx, canonical, ch); err != nil {
		return err
	}
	onSubscribed()
	for {
		// Silent-connection watchdog: each read must complete within
		// cfg.Watchdog of the previous message, else the connection is dead.
		readCtx, cancel := context.WithTimeout(ctx, f.cfg.Watchdog)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return err
		}
		tick, ok := f.parseMessage(data, canonical)
		if !ok {
			continue
		}
		select {
		case ch <- tick:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// snapshotRow covers both bootstrap endpoints: spot ticker/price rows carry
// price (last trade, the same basis as the @trade stream); futures
// premiumIndex rows carry markPrice and the venue event time in ms.
type snapshotRow struct {
	Symbol    string `json:"symbol"`
	Price     string `json:"price"`
	MarkPrice string `json:"markPrice"`
	Time      int64  `json:"time"`
}

// bootstrap snapshots current prices via REST and emits them as ticks ahead
// of any WS tick from the current connection.
func (f *BinanceFeed) bootstrap(ctx context.Context, canonical map[string]string, ch chan<- Tick) error {
	path := "/api/v3/ticker/price"
	if f.cfg.Market == MarketFutures {
		path = "/fapi/v1/premiumIndex"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.cfg.RESTURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("marketdata: bootstrap %s: status %d", path, resp.StatusCode)
	}
	var rows []snapshotRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return fmt.Errorf("marketdata: bootstrap %s: %w", path, err)
	}
	now := time.Now().UTC()
	for _, row := range rows {
		sym, ok := canonical[row.Symbol]
		if !ok {
			continue
		}
		tick, err := f.snapshotTick(sym, row, now)
		if err != nil {
			return err
		}
		select {
		case ch <- tick:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// snapshotTick converts one bootstrap row into a Tick. Spot rows have no
// venue timestamp, so the snapshot is stamped with the local receive time.
func (f *BinanceFeed) snapshotTick(symbol string, row snapshotRow, now time.Time) (Tick, error) {
	raw := row.Price
	if f.cfg.Market == MarketFutures {
		raw = row.MarkPrice
	}
	price, err := decimal.NewFromString(raw)
	if err != nil {
		return Tick{}, fmt.Errorf("marketdata: bootstrap price %q for %s: %w", raw, symbol, err)
	}
	ts := now
	if f.cfg.Market == MarketFutures && row.Time > 0 {
		ts = time.UnixMilli(row.Time).UTC()
	}
	t := Tick{Symbol: symbol, Mark: price, TS: ts}
	if f.cfg.Market == MarketSpot {
		t.Last = price
	}
	return t, nil
}

// combinedMsg is the combined-stream envelope; wsEvent covers both the spot
// trade payload (price p, trade time T) and the futures markPriceUpdate
// payload (mark price p, event time E).
type combinedMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

// The TradeID and SettlePrice fields exist only to pin encoding/json's
// case-insensitive fallback: without them the spot "t" (trade id) and
// futures "P" (estimated settle price) keys would clobber the "T"/"p" fields.
type wsEvent struct {
	EventType   string `json:"e"`
	EventTime   int64  `json:"E"` // ms
	Symbol      string `json:"s"`
	Price       string `json:"p"`
	TradeTime   int64  `json:"T"` // ms, spot trade events only
	TradeID     int64  `json:"t"`
	SettlePrice string `json:"P"`
}

// parseMessage converts one combined-stream message into a Tick. Messages
// for unsubscribed symbols and frames that fail to parse are skipped: a bad
// frame must not tear down an otherwise healthy connection.
func (f *BinanceFeed) parseMessage(data []byte, canonical map[string]string) (Tick, bool) {
	var msg combinedMsg
	if err := json.Unmarshal(data, &msg); err != nil || len(msg.Data) == 0 {
		return Tick{}, false
	}
	var ev wsEvent
	if err := json.Unmarshal(msg.Data, &ev); err != nil {
		return Tick{}, false
	}
	sym, ok := canonical[ev.Symbol]
	if !ok {
		return Tick{}, false
	}
	price, err := decimal.NewFromString(ev.Price)
	if err != nil {
		return Tick{}, false
	}
	tsMillis := ev.EventTime
	if f.cfg.Market == MarketSpot && ev.TradeTime > 0 {
		tsMillis = ev.TradeTime
	}
	if tsMillis <= 0 {
		return Tick{}, false
	}
	t := Tick{Symbol: sym, Mark: price, TS: time.UnixMilli(tsMillis).UTC()}
	if f.cfg.Market == MarketSpot {
		t.Last = price
	}
	return t, true
}

// jitter draws uniformly from [d/2, d], keeping reconnect storms desynced.
func jitter(d time.Duration) time.Duration {
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// sleepCtx sleeps for d, returning false if ctx finished first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
