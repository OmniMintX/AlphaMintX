package paper

import (
	"errors"
	"sort"

	"github.com/shopspring/decimal"
)

// ProcessTick runs the per-tick trigger sweep against the supplied
// per-symbol marks (one tick; every Store write MUST call it). Deterministic
// ordering (normative, market-data.md §Fill model v2): symbols in
// lexicographic order; within a symbol, queued exits (flattens awaiting a
// usable mark), then protective stops, then take-profits, then entry limits;
// ties within a class break by order_id lexicographic. Symbols with a
// non-positive mark are skipped entirely (fail-closed: queued exits stay
// queued, triggers stay armed, never a zero-price fill). The returned orders
// are the fills booked by this tick, in processing order.
func (o *OMS) ProcessTick(marks map[string]decimal.Decimal) ([]Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	symbols := make([]string, 0, len(marks))
	for sym, mark := range marks {
		if mark.Sign() > 0 {
			symbols = append(symbols, sym)
		}
	}
	sort.Strings(symbols)

	var fills []Order
	var errs []error
	for _, sym := range symbols {
		mark := marks[sym]
		// Queued exits retry first: a queued flatten closes the WHOLE
		// position as of fill time, with market semantics (directional
		// slippage + taker fee).
		for _, ord := range o.openOrdersLocked(sym, ClassProtective, "market") {
			if pos, ok := o.positions[positionKey{ord.StrategyID, ord.Symbol}]; ok && !pos.QtyBase.IsZero() {
				ord.QtyBase = pos.QtyBase.Abs()
			}
			filled, err := o.fillProtectiveLocked(ord, o.slippedPrice(ord.Side, mark), o.fill.takerBps)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if filled {
				fills = append(fills, *ord)
			}
		}
		// Protective stops are stop-market: they trigger at the FIRST mark
		// at/through stop_price and fill at the observed triggering mark ±
		// slippage — on a gap through the stop this is the gapped mark,
		// never stop_price itself. Taker fee.
		for _, ord := range o.openOrdersLocked(sym, ClassProtective, "stop") {
			if ord.Status != StatusOpen || !stopTriggered(ord.Side, mark, ord.StopPrice) {
				continue
			}
			filled, err := o.fillProtectiveLocked(ord, o.slippedPrice(ord.Side, mark), o.fill.takerBps)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if filled {
				fills = append(fills, *ord)
			}
		}
		// Take-profits have limit semantics: fill at the TP price exactly,
		// no slippage, maker fee.
		for _, ord := range o.openOrdersLocked(sym, ClassProtective, "take_profit") {
			if ord.Status != StatusOpen || !limitCrossed(ord.Side, mark, ord.LimitPrice) {
				continue
			}
			filled, err := o.fillProtectiveLocked(ord, ord.LimitPrice, o.fill.makerBps)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if filled {
				fills = append(fills, *ord)
			}
		}
		// Resting entry limits fill at the limit price exactly, no
		// slippage, maker fee, when the mark crosses.
		for _, ord := range o.openOrdersLocked(sym, ClassEntry, "limit") {
			if ord.Status != StatusOpen || !limitCrossed(ord.Side, mark, ord.LimitPrice) {
				continue
			}
			filledOrd, err := o.fillLimitEntryLocked(ord, mark, o.fill.makerBps)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			fills = append(fills, filledOrd)
		}
	}
	return fills, errors.Join(errs...)
}

// fillProtectiveLocked books a reduce-only protective fill sized
// min(order, position): it can never open or flip a position, and with
// nothing left to reduce the order is canceled instead. Once the fill
// leaves the position flat the sibling protective orders are canceled
// (when one tick crosses both a position's SL and TP, the stop fills
// pessimistically first and cancels the TP).
func (o *OMS) fillProtectiveLocked(ord *Order, price, feeBps decimal.Decimal) (bool, error) {
	pos, ok := o.positions[positionKey{ord.StrategyID, ord.Symbol}]
	if !ok || pos.QtyBase.IsZero() || closeSide(pos.QtyBase) != ord.Side {
		ord.Status = StatusCanceled
		return false, nil
	}
	ord.QtyBase = decimal.Min(ord.QtyBase, pos.QtyBase.Abs())
	if err := o.bookFillLocked(ord, price, feeQuote(ord.QtyBase, price, feeBps)); err != nil {
		return false, err
	}
	if pos.QtyBase.IsZero() {
		o.cancelOpenProtectivesLocked(ord.StrategyID, ord.Symbol)
	}
	return true, nil
}

// openOrdersLocked returns the open orders for (symbol, class, type),
// sorted by order_id lexicographic for deterministic trigger ordering.
func (o *OMS) openOrdersLocked(symbol string, class Class, typ string) []*Order {
	var out []*Order
	for _, ord := range o.orders {
		if ord.Symbol == symbol && ord.Class == class && ord.Type == typ && ord.Status == StatusOpen {
			out = append(out, ord)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// stopTriggered reports whether a protective stop triggers at mark: long
// stop (sell side): mark ≤ stop; short stop (buy side): mark ≥ stop.
func stopTriggered(side Side, mark, stop decimal.Decimal) bool {
	if side == SideSell {
		return mark.LessThanOrEqual(stop)
	}
	return mark.GreaterThanOrEqual(stop)
}

// limitCrossed reports whether a mark crosses a limit price: buy side:
// mark ≤ limit; sell side: mark ≥ limit. Entry limits use the entry side;
// take-profits use the closing side.
func limitCrossed(side Side, mark, limit decimal.Decimal) bool {
	if side == SideBuy {
		return mark.LessThanOrEqual(limit)
	}
	return mark.GreaterThanOrEqual(limit)
}
