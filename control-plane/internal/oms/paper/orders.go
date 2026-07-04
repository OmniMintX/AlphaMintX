package paper

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// EntryRequest describes a gate-approved ENTRY submission.
type EntryRequest struct {
	StrategyID string
	Symbol     string
	Side       Side
	Type       string // market | limit
	LimitPrice decimal.Decimal
	SizeQuote  decimal.Decimal
	MarkPrice  decimal.Decimal
	// StopPrice is the protective SL the OMS must place after the entry
	// fills (invariant 2: exchange-resident, never LLM-managed).
	StopPrice decimal.Decimal
	// KillEpoch observed on the gate verdict; stale epochs are rejected.
	KillEpoch int64
}

// SubmitEntry submits an ENTRY order. Market entries fill immediately at the
// supplied mark; the protective stop is then placed reduce-only. If
// protective placement fails, the filled quantity is flattened with a
// reduce-only market order so a position is never left naked.
//
// TODO(Phase 1): implement the sl_placement_deadline (default 30 s)
// retry-with-backoff before falling back to the reduce-only flatten, and SL
// quantity tracking across partial fills (risk-limits.md, OMS rules).
func (o *OMS) SubmitEntry(req EntryRequest) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if req.KillEpoch < o.killEpoch {
		return Order{}, ErrKillEpochStale
	}
	// Zero guards: qty sizing divides by the entry price; a non-positive
	// size or price is an error, never a decimal division-by-zero panic.
	if req.SizeQuote.Sign() <= 0 {
		return Order{}, errNonPositiveSize
	}

	ord := &Order{
		ID:         newOrderID(),
		StrategyID: req.StrategyID,
		Symbol:     req.Symbol,
		Class:      ClassEntry,
		Side:       req.Side,
		Type:       req.Type,
		KillEpoch:  req.KillEpoch,
		Status:     StatusOpen,
	}
	switch req.Type {
	case "limit":
		if req.LimitPrice.Sign() <= 0 {
			return Order{}, errNonPositiveLimitPrice
		}
		// Resting limit entries have no automated fill model in Phase 0
		// (TODO(Phase 1)); the protective-stop obligation is carried on the
		// resting order and placed on fill (FillLimitEntry) so a fill can
		// never create a naked position (invariant 2).
		ord.LimitPrice = req.LimitPrice
		ord.StopPrice = req.StopPrice
		ord.QtyBase = req.SizeQuote.Div(req.LimitPrice)
		o.orders[ord.ID] = ord
		return *ord, nil
	case "market":
		if req.MarkPrice.Sign() <= 0 {
			return Order{}, errNonPositiveMarkPrice
		}
		ord.QtyBase = req.SizeQuote.Div(req.MarkPrice)
		ord.FillPrice = req.MarkPrice
		ord.Status = StatusFilled
		o.orders[ord.ID] = ord
		o.applyFill(req.StrategyID, req.Symbol, req.Side, ord.QtyBase, req.MarkPrice)
	default:
		return Order{}, errUnknownEntryType
	}

	if err := o.placeStopLocked(req, ord.QtyBase); err != nil {
		// SL placement contingency: close the filled quantity reduce-only.
		o.flattenLocked(req.StrategyID, req.Symbol, req.MarkPrice)
		return *ord, fmt.Errorf("protective SL placement failed, position flattened reduce-only: %w", err)
	}
	return *ord, nil
}

// FillLimitEntry fills a resting limit ENTRY order at its limit price and
// places the protective stop carried on the order, with the same
// SL-placement contingency as market entries: a fill never leaves the
// position naked (invariant 2). Phase 0 has no market-driven fill model;
// this is the explicit fill path a Phase-1 model will call.
func (o *OMS) FillLimitEntry(orderID string) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ord, ok := o.orders[orderID]
	if !ok || ord.Class != ClassEntry || ord.Type != "limit" || ord.Status != StatusOpen {
		return Order{}, fmt.Errorf("no open limit ENTRY order %q", orderID)
	}
	ord.FillPrice = ord.LimitPrice
	ord.Status = StatusFilled
	o.applyFill(ord.StrategyID, ord.Symbol, ord.Side, ord.QtyBase, ord.LimitPrice)

	stopReq := EntryRequest{
		StrategyID: ord.StrategyID,
		Symbol:     ord.Symbol,
		Side:       ord.Side,
		StopPrice:  ord.StopPrice,
		KillEpoch:  ord.KillEpoch,
	}
	if err := o.placeStopLocked(stopReq, ord.QtyBase); err != nil {
		// SL placement contingency: close the filled quantity reduce-only.
		o.flattenLocked(ord.StrategyID, ord.Symbol, ord.LimitPrice)
		return *ord, fmt.Errorf("protective SL placement failed, position flattened reduce-only: %w", err)
	}
	return *ord, nil
}

func (o *OMS) placeStopLocked(req EntryRequest, qty decimal.Decimal) error {
	stop := &Order{
		ID:         newOrderID(),
		StrategyID: req.StrategyID,
		Symbol:     req.Symbol,
		Class:      ClassProtective,
		Side:       closeSide(sideSign(req.Side)),
		Type:       "stop",
		ReduceOnly: true,
		QtyBase:    qty,
		StopPrice:  req.StopPrice,
		KillEpoch:  req.KillEpoch,
		Status:     StatusOpen,
	}
	if err := o.placeProtective(stop); err != nil {
		return err
	}
	o.orders[stop.ID] = stop
	return nil
}

// Kill records the new kill epoch and cancels ENTRY orders ONLY: protective
// stops are never canceled while a position remains open unless the action
// flattens it (risk-limits.md, order classes).
func (o *OMS) Kill(epoch int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if epoch > o.killEpoch {
		o.killEpoch = epoch
	}
	for _, ord := range o.orders {
		if ord.Class == ClassEntry && ord.Status == StatusOpen {
			ord.Status = StatusCanceled
		}
	}
}

// Flatten closes the (strategy, symbol) position with a reduce-only market
// order at the supplied mark. Safety-path: exempt from rate limits and the
// kill-epoch re-check. SL/TP are canceled only AFTER the flatten fill
// confirms (stops-after-flatten ordering).
func (o *OMS) Flatten(strategyID, symbol string, mark decimal.Decimal) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.flattenLocked(strategyID, symbol, mark)
}

func (o *OMS) flattenLocked(strategyID, symbol string, mark decimal.Decimal) (Order, error) {
	pos, ok := o.positions[positionKey{strategyID, symbol}]
	if !ok || pos.QtyBase.IsZero() {
		return Order{}, fmt.Errorf("no open position for %s %s", strategyID, symbol)
	}
	side := closeSide(pos.QtyBase)
	// Reduce-only sizing: min(order, position); can never open or flip.
	qty := pos.QtyBase.Abs()
	ord := &Order{
		ID:         newOrderID(),
		StrategyID: strategyID,
		Symbol:     symbol,
		Class:      ClassProtective,
		Side:       side,
		Type:       "market",
		ReduceOnly: true,
		QtyBase:    qty,
		FillPrice:  mark,
		Status:     StatusFilled,
	}
	o.orders[ord.ID] = ord
	o.applyFill(strategyID, symbol, side, qty, mark)
	for _, other := range o.orders {
		if other.StrategyID == strategyID && other.Symbol == symbol &&
			other.Class == ClassProtective && other.Status == StatusOpen {
			other.Status = StatusCanceled
		}
	}
	return *ord, nil
}

// applyFill updates the signed position for a fill.
func (o *OMS) applyFill(strategyID, symbol string, side Side, qty, price decimal.Decimal) {
	key := positionKey{strategyID, symbol}
	pos, ok := o.positions[key]
	if !ok {
		pos = &Position{Symbol: symbol, QtyBase: decimal.Zero, EntryPrice: decimal.Zero}
		o.positions[key] = pos
	}
	delta := sideSign(side).Mul(qty)
	newQty := pos.QtyBase.Add(delta)
	if pos.QtyBase.Sign() == 0 || pos.QtyBase.Sign() == delta.Sign() {
		// Opening or increasing: weighted-average entry price.
		total := pos.QtyBase.Abs().Add(qty)
		pos.EntryPrice = pos.EntryPrice.Mul(pos.QtyBase.Abs()).Add(price.Mul(qty)).Div(total)
	}
	pos.QtyBase = newQty
	if pos.QtyBase.IsZero() {
		pos.EntryPrice = decimal.Zero
	}
}
