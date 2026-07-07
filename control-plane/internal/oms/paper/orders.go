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
	// SizeQuote is the effective size: clipped_size_quote when the verdict
	// clipped, else the proposal size_quote. It is a NOTIONAL cap at the
	// fill price, not a pre-slippage figure.
	SizeQuote decimal.Decimal
	MarkPrice decimal.Decimal
	// StopPrice is the protective SL the OMS must place after the entry
	// fills (invariant 2: exchange-resident, never LLM-managed).
	StopPrice decimal.Decimal
	// TakeProfit is the optional protective TP (limit semantics), placed
	// alongside the SL after the entry fills.
	TakeProfit decimal.Decimal
	// KillEpoch observed on the gate verdict; stale epochs are rejected.
	KillEpoch int64
}

// SubmitEntry submits an ENTRY order (fill model v2). Market entries fill
// immediately at mark ± directional slippage with the taker fee. Limit
// entries that are marketable at placement (buy: mark ≤ limit; sell:
// mark ≥ limit) fill immediately AT THE LIMIT PRICE with the TAKER fee — a
// crossing limit executes as a taker on a real venue; otherwise they rest
// and later fill at the limit with the maker fee via ProcessTick. After any
// entry fill the protective SL/TP are placed reduce-only; if protective
// placement fails, the filled quantity is flattened with a reduce-only
// market order so a position is never left naked.
//
// TODO(Phase 1): implement the sl_placement_deadline (default 30 s)
// retry-with-backoff before falling back to the reduce-only flatten, and SL
// quantity tracking across partial fills (risk-limits.md, OMS rules).
func (o *OMS) SubmitEntry(req EntryRequest) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if req.KillEpoch < o.killEpochs[req.StrategyID] {
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
		// Marketable and resting limit entries both fill at the limit price
		// exactly, so the base quantity is fixed at submission. The SL/TP
		// obligations are carried on the order and placed on fill so a fill
		// can never create a naked position (invariant 2).
		qty, err := qtyForNotional(req.SizeQuote, req.LimitPrice)
		if err != nil {
			return Order{}, err
		}
		ord.LimitPrice = req.LimitPrice
		ord.StopPrice = req.StopPrice
		ord.TakeProfit = req.TakeProfit
		ord.QtyBase = qty
		o.orders[ord.ID] = ord
		if req.MarkPrice.Sign() > 0 && limitCrossed(req.Side, req.MarkPrice, req.LimitPrice) {
			return o.fillLimitEntryLocked(ord, req.MarkPrice, o.fill.takerBps)
		}
		return *ord, nil
	case "market":
		if req.MarkPrice.Sign() <= 0 {
			return Order{}, errNonPositiveMarkPrice
		}
		price := o.slippedPrice(req.Side, req.MarkPrice)
		qty, err := qtyForNotional(req.SizeQuote, price)
		if err != nil {
			return Order{}, err
		}
		ord.QtyBase = qty
		o.orders[ord.ID] = ord
		if err := o.bookFillLocked(ord, price, feeQuote(qty, price, o.fill.takerBps)); err != nil {
			ord.Status = StatusCanceled
			return Order{}, err
		}
		if err := o.placeProtectivesLocked(ord, req.StopPrice, req.TakeProfit); err != nil {
			// SL placement contingency: close the filled quantity reduce-only.
			o.flattenLocked(req.StrategyID, req.Symbol, req.MarkPrice)
			return *ord, fmt.Errorf("protective SL placement failed, position flattened reduce-only: %w", err)
		}
		return *ord, nil
	default:
		return Order{}, errUnknownEntryType
	}
}

// FillLimitEntry fills a resting limit ENTRY order at its limit price with
// the MAKER fee and places the protective SL/TP carried on the order, with
// the same SL-placement contingency as market entries: a fill never leaves
// the position naked (invariant 2). ProcessTick is the market-driven fill
// path; this explicit path remains for callers that detect the cross
// themselves (the limit price doubles as the contingency-flatten mark, as
// no fresher mark is supplied here).
func (o *OMS) FillLimitEntry(orderID string) (Order, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ord, ok := o.orders[orderID]
	if !ok || ord.Class != ClassEntry || ord.Type != "limit" || ord.Status != StatusOpen {
		return Order{}, fmt.Errorf("no open limit ENTRY order %q", orderID)
	}
	return o.fillLimitEntryLocked(ord, ord.LimitPrice, o.fill.makerBps)
}

// fillLimitEntryLocked books a limit ENTRY fill at the limit price exactly
// (no slippage) with the given fee schedule, then places the protective
// SL/TP carried on the order. contingencyMark is the mark used for the
// reduce-only contingency flatten if protective placement fails.
func (o *OMS) fillLimitEntryLocked(ord *Order, contingencyMark, feeBps decimal.Decimal) (Order, error) {
	if err := o.bookFillLocked(ord, ord.LimitPrice, feeQuote(ord.QtyBase, ord.LimitPrice, feeBps)); err != nil {
		return Order{}, err
	}
	if err := o.placeProtectivesLocked(ord, ord.StopPrice, ord.TakeProfit); err != nil {
		// SL placement contingency: close the filled quantity reduce-only.
		o.flattenLocked(ord.StrategyID, ord.Symbol, contingencyMark)
		return *ord, fmt.Errorf("protective SL placement failed, position flattened reduce-only: %w", err)
	}
	return *ord, nil
}

// bookFillLocked books a fill on ord at price, with the fee recorded
// separately (fills are fee-EXCLUSIVE). This is the single fill-booking
// path: the fail-closed guard refusing ANY fill at a non-positive price
// lives here, on the OMS fill path itself, not only in the gate
// (market-data.md §Exits fail-closed).
func (o *OMS) bookFillLocked(ord *Order, price, fee decimal.Decimal) error {
	if price.Sign() <= 0 {
		return errNonPositiveFillPrice
	}
	ord.FillPrice = price
	ord.FeeQuote = fee
	ord.Status = StatusFilled
	o.applyFill(ord.StrategyID, ord.Symbol, ord.Side, ord.QtyBase, price, fee)
	return nil
}

// placeProtectivesLocked places the reduce-only protective stop
// (stop-market) and optional take-profit (limit semantics) covering exactly
// the filled quantity — never the pre-clip request (market-data.md §Fill
// arithmetic, step 6).
func (o *OMS) placeProtectivesLocked(entry *Order, stopPrice, takeProfit decimal.Decimal) error {
	side := closeSide(sideSign(entry.Side))
	if stopPrice.Sign() > 0 {
		stop := &Order{
			ID:         newOrderID(),
			StrategyID: entry.StrategyID,
			Symbol:     entry.Symbol,
			Class:      ClassProtective,
			Side:       side,
			Type:       "stop",
			ReduceOnly: true,
			QtyBase:    entry.QtyBase,
			StopPrice:  stopPrice,
			KillEpoch:  entry.KillEpoch,
			Status:     StatusOpen,
		}
		if err := o.placeProtective(stop); err != nil {
			return err
		}
		o.orders[stop.ID] = stop
	}
	if takeProfit.Sign() > 0 {
		tp := &Order{
			ID:         newOrderID(),
			StrategyID: entry.StrategyID,
			Symbol:     entry.Symbol,
			Class:      ClassProtective,
			Side:       side,
			Type:       "take_profit",
			ReduceOnly: true,
			QtyBase:    entry.QtyBase,
			LimitPrice: takeProfit,
			KillEpoch:  entry.KillEpoch,
			Status:     StatusOpen,
		}
		if err := o.placeProtective(tp); err != nil {
			return err
		}
		o.orders[tp.ID] = tp
	}
	return nil
}

// Kill records the strategy's new kill epoch and cancels ITS ENTRY orders
// ONLY: other strategies' epochs and resting entries are untouched (kill
// epochs are per-strategy), and protective stops are never canceled while a
// position remains open unless the action flattens it (risk-limits.md,
// order classes).
func (o *OMS) Kill(strategyID string, epoch int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if epoch > o.killEpochs[strategyID] {
		o.killEpochs[strategyID] = epoch
	}
	for _, ord := range o.orders {
		if ord.StrategyID == strategyID && ord.Class == ClassEntry && ord.Status == StatusOpen {
			ord.Status = StatusCanceled
		}
	}
}

// CancelOpenEntries cancels the strategy's resting un-filled ENTRY orders
// only — reduce-only protectives untouched, positions still managed (the
// EffectCancelEntryOrders contract, lifecycle-api.md LC-12a). The kill
// epoch is unchanged: this is the pause effect, not a kill.
func (o *OMS) CancelOpenEntries(strategyID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ord := range o.orders {
		if ord.StrategyID == strategyID && ord.Class == ClassEntry && ord.Status == StatusOpen {
			ord.Status = StatusCanceled
		}
	}
}

// Flatten closes the (strategy, symbol) position with a reduce-only market
// order at the supplied mark ± directional slippage, taker fee. Safety-path:
// exempt from rate limits and the kill-epoch re-check. SL/TP are canceled
// only AFTER the flatten fill confirms (stops-after-flatten ordering). With
// no usable mark (≤ 0) the exit is QUEUED, never filled: the returned order
// stays open, the position stays open, the stops stay armed, and the flatten
// retries on the next fresh tick via ProcessTick (market-data.md §Exits
// fail-closed).
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
		Status:     StatusOpen,
	}
	o.orders[ord.ID] = ord
	if mark.Sign() <= 0 {
		// Queued exit: no usable mark — never a zero-price fill.
		return *ord, nil
	}
	price := o.slippedPrice(side, mark)
	if err := o.bookFillLocked(ord, price, feeQuote(qty, price, o.fill.takerBps)); err != nil {
		return *ord, err
	}
	o.cancelOpenProtectivesLocked(strategyID, symbol)
	return *ord, nil
}

// cancelOpenProtectivesLocked cancels the open PROTECTIVE orders for
// (strategy, symbol); called only AFTER the fill that left the position flat
// confirms (stops-after-flatten ordering; a stop and TP crossed by one tick:
// the stop fills and the sibling TP is canceled once the position is closed).
func (o *OMS) cancelOpenProtectivesLocked(strategyID, symbol string) {
	for _, other := range o.orders {
		if other.StrategyID == strategyID && other.Symbol == symbol &&
			other.Class == ClassProtective && other.Status == StatusOpen {
			other.Status = StatusCanceled
		}
	}
}

// applyFill updates the signed position, cumulative fees, and realized PnL
// for a booked fill (price already validated > 0 by bookFillLocked). Fees
// are realized when paid, so realized PnL after a full round trip is
// exit proceeds − entry cost − Σ fees, with no fee counted twice.
func (o *OMS) applyFill(strategyID, symbol string, side Side, qty, price, fee decimal.Decimal) {
	key := positionKey{strategyID, symbol}
	pos, ok := o.positions[key]
	if !ok {
		pos = &Position{Symbol: symbol, QtyBase: decimal.Zero, EntryPrice: decimal.Zero}
		o.positions[key] = pos
	}
	if pos.QtyBase.IsZero() {
		// Fresh lifecycle: fees_quote restarts with the opening fill.
		pos.FeesQuote = decimal.Zero
	}
	delta := sideSign(side).Mul(qty)
	if pos.QtyBase.Sign() == 0 || pos.QtyBase.Sign() == delta.Sign() {
		// Opening or increasing: weighted-average entry price.
		total := pos.QtyBase.Abs().Add(qty)
		pos.EntryPrice = pos.EntryPrice.Mul(pos.QtyBase.Abs()).Add(price.Mul(qty)).Div(total)
	} else {
		// Reducing (or flipping): realize PnL on the closed quantity,
		// rounded per §Rounding (Round is half-away-from-zero, signed).
		closed := decimal.Min(qty, pos.QtyBase.Abs())
		sign := decimal.NewFromInt(int64(pos.QtyBase.Sign()))
		gross := round8(price.Sub(pos.EntryPrice).Mul(closed).Mul(sign))
		pos.RealizedPnLQuote = pos.RealizedPnLQuote.Add(gross)
		if qty.GreaterThan(pos.QtyBase.Abs()) {
			// Flip: the remainder opens a new lot at the fill price.
			pos.EntryPrice = price
		}
	}
	pos.FeesQuote = pos.FeesQuote.Add(fee)
	pos.RealizedPnLQuote = pos.RealizedPnLQuote.Sub(fee)
	pos.QtyBase = pos.QtyBase.Add(delta)
	if pos.QtyBase.IsZero() {
		pos.EntryPrice = decimal.Zero
	}
}
