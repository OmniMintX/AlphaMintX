// Package exchange is the venue adapter for the live OMS: a typed Binance
// spot REST + user-data-stream client (docs/specs/live-oms-and-reconciler.md
// §Exchange adapter interface). It has no store access and no business
// policy; all money/quantity values are decimal strings VERBATIM from the
// venue, and symbols crossing this boundary are venue symbols ("BTCUSDT") —
// the adapter alone maps to/from canonical BASE/QUOTE form.
package exchange

import (
	"context"
	"time"
)

// Exchange is the venue adapter seam the live OMS depends on. Every error
// returned by an implementation is classified into exactly one Class
// (errors.go); callers branch on Classify, never on venue-specific shapes.
type Exchange interface {
	// ExchangeInfo returns the trading filters for the given venue symbols.
	ExchangeInfo(ctx context.Context, venueSymbols []string) (Filters, error)
	// PlaceOrder submits one order carrying the caller's newClientOrderId.
	PlaceOrder(ctx context.Context, req PlaceRequest) (Ack, error)
	// QueryOrder fetches one order by its original client order id.
	QueryOrder(ctx context.Context, venueSymbol, origClientOrderID string) (OrderState, error)
	// CancelOrder cancels one order by its original client order id.
	CancelOrder(ctx context.Context, venueSymbol, origClientOrderID string) (OrderState, error)
	// OpenOrders lists the venue's open orders for one symbol.
	OpenOrders(ctx context.Context, venueSymbol string) ([]OrderState, error)
	// MyTrades pages account trades: by fromId when fromID > 0, else by
	// startTime when nonzero (cold-start bootstrap only — see the spec's
	// paging handoff rule); limit <= 0 uses the venue default.
	MyTrades(ctx context.Context, venueSymbol string, fromID int64, startTime time.Time, limit int) ([]Trade, error)
	// Balances returns free/locked per asset (flatten sizing, R6 sanity).
	Balances(ctx context.Context) ([]Balance, error)
	// NewListenKey opens a user-data stream key.
	NewListenKey(ctx context.Context) (string, error)
	// KeepAliveListenKey extends the key's lifetime.
	KeepAliveListenKey(ctx context.Context, key string) error
	// StreamUserData streams user-data events for the key; the channel is
	// closed when ctx is done or the connection fails (no auto-reconnect —
	// the caller owns reconnect + reconcile).
	StreamUserData(ctx context.Context, key string) (<-chan UserEvent, error)
	// ServerTime returns the venue clock (also refreshes any skew offset).
	ServerTime(ctx context.Context) (time.Time, error)
}

// SymbolFilters are the per-symbol trading filters from exchangeInfo:
// PRICE_FILTER tickSize, LOT_SIZE stepSize/minQty/maxQty, and the
// NOTIONAL/MIN_NOTIONAL minimum, all verbatim decimal strings.
type SymbolFilters struct {
	TickSize    string
	StepSize    string
	MinQty      string
	MaxQty      string
	MinNotional string
}

// Filters maps venue symbol -> its trading filters.
type Filters map[string]SymbolFilters

// PlaceRequest is one order placement. Price/StopPrice/TimeInForce are
// omitted from the request when empty.
type PlaceRequest struct {
	VenueSymbol      string
	Side             string // BUY | SELL
	Type             string // LIMIT | MARKET | STOP_LOSS_LIMIT | ...
	Qty              string
	Price            string
	StopPrice        string
	NewClientOrderID string
	TimeInForce      string // GTC | IOC | FOK
}

// Ack is the venue's placement acknowledgment.
type Ack struct {
	ExchangeOrderID int64
	Status          string
	TransactTime    time.Time
}

// OrderState is one venue order as reported by query/cancel/openOrders.
type OrderState struct {
	VenueSymbol     string
	ExchangeOrderID int64
	ClientOrderID   string
	Status          string
	Side            string
	Type            string
	Price           string
	StopPrice       string
	OrigQty         string
	ExecutedQty     string
	CumQuoteQty     string
	UpdatedAt       time.Time
}

// Trade is one account trade from myTrades. ClientOrderID is empty when the
// venue endpoint does not report it (Binance myTrades attributes by orderId).
type Trade struct {
	ID              int64
	ExchangeOrderID int64
	VenueSymbol     string
	ClientOrderID   string
	Price           string
	Qty             string
	Commission      string
	CommissionAsset string
	IsBuyer         bool
	Time            time.Time
}

// Balance is one asset's free/locked balance.
type Balance struct {
	Asset  string
	Free   string
	Locked string
}

// UserEvent kinds.
const (
	UserEventExecutionReport  = "executionReport"
	UserEventListenKeyExpired = "listenKeyExpired"
)

// UserEvent is a tagged union over user-data-stream events: Kind selects the
// variant, and the executionReport fields are populated only when
// Kind == UserEventExecutionReport.
type UserEvent struct {
	Kind string

	// executionReport fields.
	VenueSymbol     string
	ClientOrderID   string
	Side            string
	OrderType       string
	ExecType        string // NEW | TRADE | CANCELED | REJECTED | EXPIRED
	OrderStatus     string
	ExchangeOrderID int64
	LastQty         string
	LastPrice       string
	CumQty          string
	Commission      string
	CommissionAsset string
	TradeID         int64
	EventTime       time.Time
}
