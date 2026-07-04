// Package paper is the in-memory paper OMS with the fill model v2 of
// docs/specs/market-data.md §Fill model v2: flat-bps directional slippage
// for market semantics, taker/maker fees recorded separately on fills,
// exchange-resident-style protective stops/take-profits with deterministic
// per-tick trigger processing, and the order-class rules of
// docs/specs/risk-limits.md (ENTRY vs PROTECTIVE/reduce-only).
package paper

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
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
	Type       string // market | limit | stop | take_profit
	ReduceOnly bool
	QtyBase    decimal.Decimal
	LimitPrice decimal.Decimal // limit entries and take-profit trigger price
	StopPrice  decimal.Decimal // protective stops
	// TakeProfit is the TP obligation carried on a resting entry; the TP
	// order itself stores its trigger price in LimitPrice (limit semantics).
	TakeProfit decimal.Decimal
	FillPrice  decimal.Decimal
	// FeeQuote is the fee paid on the fill, recorded SEPARATELY: fees are
	// never baked into the fill price (fee-EXCLUSIVE, market-data.md).
	FeeQuote decimal.Decimal
	Status   Status
	// KillEpoch observed at gate evaluation; submissions carrying a stale
	// epoch are rejected (OMS kill re-check, risk-limits.md).
	KillEpoch int64
}

// Position is a signed base-quantity position (+ long, - short). FeesQuote
// accumulates the fees paid over the current position lifecycle (it restarts
// when a position reopens from flat). RealizedPnLQuote is the cumulative
// realized PnL for (strategy, symbol) net of ALL fees — fees are realized
// when paid, so after a full round trip it equals
// exit proceeds − entry cost − Σ fees (entry + exit).
type Position struct {
	Symbol           string
	QtyBase          decimal.Decimal
	EntryPrice       decimal.Decimal
	FeesQuote        decimal.Decimal
	RealizedPnLQuote decimal.Decimal
}

// FillModel is the fill model v2 configuration (market-data.md
// §`fill_model` configuration): all three fields are REQUIRED decimal
// strings in basis points. There are NO hidden defaults — a missing,
// negative, or malformed field is a construction error, never a silent
// zero.
type FillModel struct {
	MarketSlippageBps string `json:"market_slippage_bps"`
	TakerFeeBps       string `json:"taker_fee_bps"`
	MakerFeeBps       string `json:"maker_fee_bps"`
}

// fillModel is the parsed, validated form used on the fill paths.
type fillModel struct {
	slippageBps decimal.Decimal
	takerBps    decimal.Decimal
	makerBps    decimal.Decimal
}

// parseFillModel validates the three REQUIRED fields; contract.ParseDecimal
// enforces the decimal-as-string form and rejects negatives and empties.
// market_slippage_bps must be < 10000: at 10000 bps (100%) a sell-side
// slipped price is zero and beyond it negative — a construction error, never
// a downstream division-by-zero panic.
func parseFillModel(fm FillModel) (fillModel, error) {
	parse := func(field, s string) (decimal.Decimal, error) {
		d, err := contract.ParseDecimal(s)
		if err != nil {
			return decimal.Decimal{}, fmt.Errorf("fill_model.%s: %w", field, err)
		}
		return d.Decimal(), nil
	}
	var (
		out fillModel
		err error
	)
	if out.slippageBps, err = parse("market_slippage_bps", fm.MarketSlippageBps); err != nil {
		return fillModel{}, err
	}
	if out.slippageBps.GreaterThanOrEqual(decimal.NewFromInt(10000)) {
		return fillModel{}, fmt.Errorf("fill_model.market_slippage_bps: must be < 10000 (got %s: a sell at 100%%+ slippage fills at a non-positive price)", fm.MarketSlippageBps)
	}
	if out.takerBps, err = parse("taker_fee_bps", fm.TakerFeeBps); err != nil {
		return fillModel{}, err
	}
	if out.makerBps, err = parse("maker_fee_bps", fm.MakerFeeBps); err != nil {
		return fillModel{}, err
	}
	return out, nil
}

// ErrKillEpochStale is returned when a submission's kill-epoch predates the
// current one (KILL_SWITCH_ACTIVE at the OMS boundary).
var ErrKillEpochStale = errors.New("KILL_SWITCH_ACTIVE: kill-epoch stale at submission")

type positionKey struct{ strategyID, symbol string }

// OMS is the in-memory paper order manager.
type OMS struct {
	mu        sync.Mutex
	killEpoch int64
	fill      fillModel
	orders    map[string]*Order
	positions map[positionKey]*Position

	// placeProtective is the protective-placement seam; tests inject
	// failures to exercise the SL placement contingency.
	placeProtective func(*Order) error
}

// New returns an empty paper OMS. The fill model is REQUIRED at
// construction: all three bps fields must be valid non-negative decimal
// strings (no hidden defaults).
func New(fm FillModel) (*OMS, error) {
	parsed, err := parseFillModel(fm)
	if err != nil {
		return nil, err
	}
	o := &OMS{
		fill:      parsed,
		orders:    make(map[string]*Order),
		positions: make(map[positionKey]*Position),
	}
	o.placeProtective = func(*Order) error { return nil }
	return o, nil
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

// RealizedPnL returns the cumulative realized PnL (net of all fees) for
// (strategy, symbol), including flat books.
func (o *OMS) RealizedPnL(strategyID, symbol string) decimal.Decimal {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p, ok := o.positions[positionKey{strategyID, symbol}]; ok {
		return p.RealizedPnLQuote
	}
	return decimal.Zero
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

// round8 is the single normative rounding rule: half away from zero to 8
// decimal places (market-data.md §Rounding). shopspring Decimal.Round is
// half-away-from-zero; intermediate arithmetic stays unrounded and rounding
// happens once per persisted value.
func round8(d decimal.Decimal) decimal.Decimal { return d.Round(8) }

// qtyStep is one unit at the 8th decimal, the step used when rounding a
// quantity down so notional never exceeds the effective size.
var qtyStep = decimal.New(1, -8)

// qtyForNotional converts an effective quote size (the NOTIONAL cap) into a
// base quantity at the fill price: rounded per §Rounding, then stepped DOWN
// at the 8th decimal until qty × price no longer exceeds the size — rounding
// MUST NOT increase notional above the cap / clipped_size_quote
// (risk-limits.md §OMS execution rules).
func qtyForNotional(sizeQuote, fillPrice decimal.Decimal) (decimal.Decimal, error) {
	qty := round8(sizeQuote.Div(fillPrice))
	for qty.Mul(fillPrice).GreaterThan(sizeQuote) {
		qty = qty.Sub(qtyStep)
	}
	if qty.Sign() <= 0 {
		return decimal.Decimal{}, errQtyRoundsToZero
	}
	return qty, nil
}

// slippedPrice applies directional flat-bps slippage for market semantics:
// buys pay mark × (1 + slip_bps/10000), sells receive
// mark × (1 − slip_bps/10000); shorts are symmetric. The result is the
// persisted fill price, rounded per §Rounding.
func (o *OMS) slippedPrice(side Side, mark decimal.Decimal) decimal.Decimal {
	slip := o.fill.slippageBps.Shift(-4)
	if side == SideBuy {
		return round8(mark.Mul(decimal.NewFromInt(1).Add(slip)))
	}
	return round8(mark.Mul(decimal.NewFromInt(1).Sub(slip)))
}

// feeQuote is qty × price × fee_bps/10000, rounded per §Rounding. It is
// recorded separately on the fill, never baked into the fill price.
func feeQuote(qty, price, feeBps decimal.Decimal) decimal.Decimal {
	return round8(qty.Mul(price).Mul(feeBps.Shift(-4)))
}

var (
	errUnknownEntryType      = fmt.Errorf("entry type must be market or limit")
	errNonPositiveSize       = fmt.Errorf("size_quote must be strictly positive")
	errNonPositiveLimitPrice = fmt.Errorf("limit_price must be strictly positive for limit entries")
	errNonPositiveMarkPrice  = fmt.Errorf("mark_price must be strictly positive for market entries")
	errNonPositiveFillPrice  = fmt.Errorf("refusing to book a fill at a non-positive price (fail-closed)")
	errQtyRoundsToZero       = fmt.Errorf("size_quote rounds to a non-positive base quantity at 8 decimal places")
)
