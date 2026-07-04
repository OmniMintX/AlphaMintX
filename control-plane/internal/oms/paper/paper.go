// Package paper is the Phase-0 in-memory paper OMS: immediate-fill market
// orders at a supplied mark price, exchange-resident-style protective stops,
// and the order-class rules of docs/specs/risk-limits.md (ENTRY vs
// PROTECTIVE/reduce-only).
package paper

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Class is the normative order class: kill-switch, watchdog, and cancel
// sweeps act on ENTRY orders only; PROTECTIVE orders are reduce-only.
type Class string

const (
	ClassEntry      Class = "ENTRY"
	ClassProtective Class = "PROTECTIVE"
)

// Side of an order.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// Status of an order.
type Status string

const (
	StatusOpen     Status = "open"
	StatusFilled   Status = "filled"
	StatusCanceled Status = "canceled"
)

// Order is a paper order. PROTECTIVE orders are always reduce-only: they can
// never open or flip a position.
type Order struct {
	ID         string
	StrategyID string
	Symbol     string
	Class      Class
	Side       Side
	Type       string // market | limit | stop
	ReduceOnly bool
	QtyBase    decimal.Decimal
	LimitPrice decimal.Decimal // limit orders
	StopPrice  decimal.Decimal // protective stops
	FillPrice  decimal.Decimal
	Status     Status
	// KillEpoch observed at gate evaluation; submissions carrying a stale
	// epoch are rejected (OMS kill re-check, risk-limits.md).
	KillEpoch int64
}

// Position is a signed base-quantity position (+ long, - short).
type Position struct {
	Symbol     string
	QtyBase    decimal.Decimal
	EntryPrice decimal.Decimal
}

// ErrKillEpochStale is returned when a submission's kill-epoch predates the
// current one (KILL_SWITCH_ACTIVE at the OMS boundary).
var ErrKillEpochStale = errors.New("KILL_SWITCH_ACTIVE: kill-epoch stale at submission")

type positionKey struct{ strategyID, symbol string }

// OMS is the in-memory paper order manager.
type OMS struct {
	mu        sync.Mutex
	killEpoch int64
	orders    map[string]*Order
	positions map[positionKey]*Position

	// placeProtective is the protective-placement seam; tests inject
	// failures to exercise the SL placement contingency.
	placeProtective func(*Order) error
}

// New returns an empty paper OMS.
func New() *OMS {
	o := &OMS{
		orders:    make(map[string]*Order),
		positions: make(map[positionKey]*Position),
	}
	o.placeProtective = func(*Order) error { return nil }
	return o
}

// SetProtectivePlacementHook overrides protective stop placement (tests only).
func (o *OMS) SetProtectivePlacementHook(f func(*Order) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.placeProtective = f
}

// KillEpoch returns the current kill epoch.
func (o *OMS) KillEpoch() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.killEpoch
}

// Orders returns a snapshot copy of all orders.
func (o *OMS) Orders() []Order {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Order, 0, len(o.orders))
	for _, ord := range o.orders {
		out = append(out, *ord)
	}
	return out
}

// Position returns the current position for (strategy, symbol), if any.
func (o *OMS) Position(strategyID, symbol string) (Position, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.positions[positionKey{strategyID, symbol}]
	if !ok || p.QtyBase.IsZero() {
		return Position{}, false
	}
	return *p, true
}

func sideSign(s Side) decimal.Decimal {
	if s == SideBuy {
		return decimal.NewFromInt(1)
	}
	return decimal.NewFromInt(-1)
}

func closeSide(qty decimal.Decimal) Side {
	if qty.Sign() > 0 {
		return SideSell
	}
	return SideBuy
}

func newOrderID() string { return uuid.NewString() }

var (
	errUnknownEntryType      = fmt.Errorf("entry type must be market or limit")
	errNonPositiveSize       = fmt.Errorf("size_quote must be strictly positive")
	errNonPositiveLimitPrice = fmt.Errorf("limit_price must be strictly positive for limit entries")
	errNonPositiveMarkPrice  = fmt.Errorf("mark_price must be strictly positive for market entries")
)
