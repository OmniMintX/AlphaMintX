// Package fake provides a deterministic in-process exchange.Exchange for
// scenario tests (docs/specs/live-oms-and-reconciler.md §Test obligations):
// scripted orders, one-shot fault injection, fill/event emission, listen-key
// expiry, and venue-reset simulation. No network, no goroutines.
package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

// Venue is a thread-safe scripted venue. User events (fills, listen-key
// expiry) are buffered on a single channel (capacity 256) shared by every
// StreamUserData call; deterministic tests drain it as they go.
type Venue struct {
	mu          sync.Mutex
	now         func() time.Time
	orders      map[string]exchange.OrderState // by clientOrderID
	trades      map[string][]exchange.Trade    // by venue symbol
	tradeIDs    map[string]int64               // per-symbol monotone counter
	balances    map[string]exchange.Balance
	failNext    map[string]error // one-shot fault per operation name
	events      chan exchange.UserEvent
	nextOrderID int64
	listenKeys  int
}

var _ exchange.Exchange = (*Venue)(nil)

// NewVenue returns an empty venue.
func NewVenue() *Venue {
	return &Venue{
		now:      func() time.Time { return time.Now().UTC() },
		orders:   map[string]exchange.OrderState{},
		trades:   map[string][]exchange.Trade{},
		tradeIDs: map[string]int64{},
		balances: map[string]exchange.Balance{},
		failNext: map[string]error{},
		events:   make(chan exchange.UserEvent, 256),
	}
}

// FailNext arms a one-shot fault: the next call of the named operation
// ("PlaceOrder", "QueryOrder", "CancelOrder", "OpenOrders", "MyTrades",
// "Balances", "ExchangeInfo", "NewListenKey", "KeepAliveListenKey",
// "StreamUserData", "ServerTime") returns err instead of executing.
func (v *Venue) FailNext(op string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.failNext[op] = err
}

// AddOpenOrder rests an order keyed by its ClientOrderID, assigning an
// exchange order id when the given one is zero.
func (v *Venue) AddOpenOrder(o exchange.OrderState) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if o.ExchangeOrderID == 0 {
		v.nextOrderID++
		o.ExchangeOrderID = v.nextOrderID
	}
	if o.Status == "" {
		o.Status = "NEW"
	}
	v.orders[o.ClientOrderID] = o
}

// SetBalance sets one asset's free/locked balance.
func (v *Venue) SetBalance(asset, free, locked string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.balances[asset] = exchange.Balance{Asset: asset, Free: free, Locked: locked}
}

// SetNow overrides the venue clock (deterministic scenario tests).
func (v *Venue) SetNow(now func() time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.now = now
}

// ExpireListenKey emits a listenKeyExpired event on the stream.
func (v *Venue) ExpireListenKey() {
	v.mu.Lock()
	ev := exchange.UserEvent{Kind: exchange.UserEventListenKeyExpired, EventTime: v.now()}
	v.mu.Unlock()
	v.events <- ev
}

// Reset simulates a venue wipe (testnet reset): orders and trades cleared,
// per-symbol trade-id counters restart from 1, order ids restart, balances
// reset. Armed faults and the event stream are untouched.
func (v *Venue) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.orders = map[string]exchange.OrderState{}
	v.trades = map[string][]exchange.Trade{}
	v.tradeIDs = map[string]int64{}
	v.balances = map[string]exchange.Balance{}
	v.nextOrderID = 0
}

// Fill executes qty at price with zero commission (see FillWithCommission).
func (v *Venue) Fill(clientOrderID, qty, price string) error {
	return v.FillWithCommission(clientOrderID, qty, price, "0", "")
}

// FillWithCommission executes qty at price against the resting order with
// an explicit commission, minting the symbol's next trade id, updating
// executed/cumulative quantities and status (FILLED once executed >=
// original), and emitting an executionReport TRADE event.
func (v *Venue) FillWithCommission(clientOrderID, qty, price, commission, commissionAsset string) error {
	v.mu.Lock()
	o, ok := v.orders[clientOrderID]
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("fake: no order %q", clientOrderID)
	}
	fillQty, err := decimal.NewFromString(qty)
	if err != nil {
		v.mu.Unlock()
		return fmt.Errorf("fake: fill qty: %w", err)
	}
	fillPrice, err := decimal.NewFromString(price)
	if err != nil {
		v.mu.Unlock()
		return fmt.Errorf("fake: fill price: %w", err)
	}
	executed := dec(o.ExecutedQty).Add(fillQty)
	cumQuote := dec(o.CumQuoteQty).Add(fillQty.Mul(fillPrice))
	o.ExecutedQty = executed.String()
	o.CumQuoteQty = cumQuote.String()
	o.Status = "PARTIALLY_FILLED"
	if orig := dec(o.OrigQty); orig.IsPositive() && executed.GreaterThanOrEqual(orig) {
		o.Status = "FILLED"
	}
	o.UpdatedAt = v.now()
	v.orders[clientOrderID] = o
	tradeID := v.tradeIDs[o.VenueSymbol] + 1
	v.tradeIDs[o.VenueSymbol] = tradeID
	v.trades[o.VenueSymbol] = append(v.trades[o.VenueSymbol], exchange.Trade{
		ID:              tradeID,
		ExchangeOrderID: o.ExchangeOrderID,
		VenueSymbol:     o.VenueSymbol,
		ClientOrderID:   o.ClientOrderID,
		Price:           price,
		Qty:             qty,
		Commission:      commission,
		CommissionAsset: commissionAsset,
		IsBuyer:         o.Side == "BUY",
		Time:            o.UpdatedAt,
	})
	ev := exchange.UserEvent{
		Kind:            exchange.UserEventExecutionReport,
		VenueSymbol:     o.VenueSymbol,
		ClientOrderID:   o.ClientOrderID,
		Side:            o.Side,
		OrderType:       o.Type,
		ExecType:        "TRADE",
		OrderStatus:     o.Status,
		ExchangeOrderID: o.ExchangeOrderID,
		LastQty:         qty,
		LastPrice:       price,
		CumQty:          o.ExecutedQty,
		Commission:      commission,
		CommissionAsset: commissionAsset,
		TradeID:         tradeID,
		EventTime:       o.UpdatedAt,
	}
	v.mu.Unlock()
	v.events <- ev
	return nil
}

// pop returns and clears the one-shot fault armed for op, if any. Caller
// holds the mutex.
func (v *Venue) pop(op string) error {
	err := v.failNext[op]
	if err != nil {
		delete(v.failNext, op)
	}
	return err
}

// ExchangeInfo returns permissive default filters for each requested symbol.
func (v *Venue) ExchangeInfo(_ context.Context, venueSymbols []string) (exchange.Filters, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("ExchangeInfo"); err != nil {
		return nil, err
	}
	f := make(exchange.Filters, len(venueSymbols))
	for _, s := range venueSymbols {
		f[s] = exchange.SymbolFilters{
			TickSize: "0.01", StepSize: "0.00001", MinQty: "0.00001",
			MaxQty: "9000", MinNotional: "5",
		}
	}
	return f, nil
}

// PlaceOrder rests a NEW order; a duplicate NewClientOrderID is a
// DefiniteReject (-2010), matching the venue.
func (v *Venue) PlaceOrder(_ context.Context, req exchange.PlaceRequest) (exchange.Ack, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("PlaceOrder"); err != nil {
		return exchange.Ack{}, err
	}
	if _, exists := v.orders[req.NewClientOrderID]; exists {
		return exchange.Ack{}, &exchange.VenueError{
			Op: "PlaceOrder", Class: exchange.ClassDefiniteReject,
			VenueCode: -2010, VenueMsg: "Duplicate order sent.",
		}
	}
	v.nextOrderID++
	now := v.now()
	v.orders[req.NewClientOrderID] = exchange.OrderState{
		VenueSymbol:     req.VenueSymbol,
		ExchangeOrderID: v.nextOrderID,
		ClientOrderID:   req.NewClientOrderID,
		Status:          "NEW",
		Side:            req.Side,
		Type:            req.Type,
		Price:           req.Price,
		StopPrice:       req.StopPrice,
		OrigQty:         req.Qty,
		ExecutedQty:     "0",
		CumQuoteQty:     "0",
		UpdatedAt:       now,
	}
	return exchange.Ack{ExchangeOrderID: v.nextOrderID, Status: "NEW", TransactTime: now}, nil
}

// QueryOrder returns the order or a NotFound (-2013).
func (v *Venue) QueryOrder(_ context.Context, venueSymbol, origClientOrderID string) (exchange.OrderState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("QueryOrder"); err != nil {
		return exchange.OrderState{}, err
	}
	o, ok := v.orders[origClientOrderID]
	if !ok || o.VenueSymbol != venueSymbol {
		return exchange.OrderState{}, notFound("QueryOrder", -2013, "Order does not exist.")
	}
	return o, nil
}

// CancelOrder cancels an open order; missing or already-terminal orders are
// NotFound (-2011, cancel reject).
func (v *Venue) CancelOrder(_ context.Context, venueSymbol, origClientOrderID string) (exchange.OrderState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("CancelOrder"); err != nil {
		return exchange.OrderState{}, err
	}
	o, ok := v.orders[origClientOrderID]
	if !ok || o.VenueSymbol != venueSymbol || !open(o) {
		return exchange.OrderState{}, notFound("CancelOrder", -2011, "Unknown order sent.")
	}
	o.Status = "CANCELED"
	o.UpdatedAt = v.now()
	v.orders[origClientOrderID] = o
	return o, nil
}

// OpenOrders lists NEW/PARTIALLY_FILLED orders for the symbol, ordered by
// exchange order id for determinism.
func (v *Venue) OpenOrders(_ context.Context, venueSymbol string) ([]exchange.OrderState, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("OpenOrders"); err != nil {
		return nil, err
	}
	var out []exchange.OrderState
	for _, o := range v.orders {
		if o.VenueSymbol == venueSymbol && open(o) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExchangeOrderID < out[j].ExchangeOrderID })
	return out, nil
}

// MyTrades pages trades with the adapter's param-selection semantics:
// fromID > 0 filters ids >= fromID, else a nonzero startTime filters by
// time; limit > 0 truncates.
func (v *Venue) MyTrades(_ context.Context, venueSymbol string, fromID int64, startTime time.Time, limit int) ([]exchange.Trade, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("MyTrades"); err != nil {
		return nil, err
	}
	var out []exchange.Trade
	for _, t := range v.trades[venueSymbol] {
		if fromID > 0 {
			if t.ID < fromID {
				continue
			}
		} else if !startTime.IsZero() && t.Time.Before(startTime) {
			continue
		}
		out = append(out, t)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Balances returns the configured balances sorted by asset.
func (v *Venue) Balances(_ context.Context) ([]exchange.Balance, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("Balances"); err != nil {
		return nil, err
	}
	out := make([]exchange.Balance, 0, len(v.balances))
	for _, b := range v.balances {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset < out[j].Asset })
	return out, nil
}

// NewListenKey mints a sequential key.
func (v *Venue) NewListenKey(_ context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("NewListenKey"); err != nil {
		return "", err
	}
	v.listenKeys++
	return fmt.Sprintf("fake-listen-key-%d", v.listenKeys), nil
}

// KeepAliveListenKey succeeds unless a fault is armed.
func (v *Venue) KeepAliveListenKey(_ context.Context, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.pop("KeepAliveListenKey")
}

// StreamUserData returns the shared event channel; every call returns the
// same channel, which is never closed by the fake.
func (v *Venue) StreamUserData(_ context.Context, _ string) (<-chan exchange.UserEvent, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("StreamUserData"); err != nil {
		return nil, err
	}
	return v.events, nil
}

// ServerTime returns the fake's clock.
func (v *Venue) ServerTime(_ context.Context) (time.Time, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.pop("ServerTime"); err != nil {
		return time.Time{}, err
	}
	return v.now(), nil
}

// open reports whether the order still rests on the venue book.
func open(o exchange.OrderState) bool {
	return o.Status == "NEW" || o.Status == "PARTIALLY_FILLED"
}

// notFound builds the venue's NotFound-class error.
func notFound(op string, code int, msg string) *exchange.VenueError {
	return &exchange.VenueError{Op: op, Class: exchange.ClassNotFound, VenueCode: code, VenueMsg: msg}
}

// dec parses a venue decimal string, treating "" as zero.
func dec(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, _ := decimal.NewFromString(s)
	return d
}
