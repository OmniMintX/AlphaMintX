package live

import (
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// bookState is one (strategy, symbol) position book in decimal form.
type bookState struct {
	qty, entry, fees, realized decimal.Decimal
}

// applyFill updates the signed position, cumulative fees, and realized PnL
// for one booked fill — a verbatim port of the paper OMS's applyFill so
// live fills flow through the IDENTICAL accounting math (invariant 10):
// fee-exclusive weighted-average entry, realized PnL net of ALL fees, fees
// restarting when a position reopens from flat.
func applyFill(b *bookState, side string, qty, price, fee decimal.Decimal) {
	if b.qty.IsZero() {
		// Fresh lifecycle: fees_quote restarts with the opening fill.
		b.fees = decimal.Zero
	}
	sign := decimal.NewFromInt(-1)
	if side == "buy" {
		sign = decimal.NewFromInt(1)
	}
	delta := sign.Mul(qty)
	if b.qty.Sign() == 0 || b.qty.Sign() == delta.Sign() {
		// Opening or increasing: weighted-average entry price.
		total := b.qty.Abs().Add(qty)
		b.entry = b.entry.Mul(b.qty.Abs()).Add(price.Mul(qty)).Div(total)
	} else {
		// Reducing (or flipping): realize PnL on the closed quantity,
		// rounded per §Rounding (half away from zero, signed).
		closed := decimal.Min(qty, b.qty.Abs())
		posSign := decimal.NewFromInt(int64(b.qty.Sign()))
		gross := round8(price.Sub(b.entry).Mul(closed).Mul(posSign))
		b.realized = b.realized.Add(gross)
		if qty.GreaterThan(b.qty.Abs()) {
			// Flip: the remainder opens a new lot at the fill price.
			b.entry = price
		}
	}
	b.fees = b.fees.Add(fee)
	b.realized = b.realized.Sub(fee)
	b.qty = b.qty.Add(delta)
	if b.qty.IsZero() {
		b.entry = decimal.Zero
	}
}

// loadBook reads the (strategy, symbol) position inside the fill
// transaction; a missing row is the zero book.
func loadBook(tx *store.SweepTx, strategyID, symbol string) (bookState, error) {
	row, ok, err := tx.GetPosition(strategyID, symbol)
	if err != nil {
		return bookState{}, err
	}
	b := bookState{qty: decimal.Zero, entry: decimal.Zero, fees: decimal.Zero, realized: decimal.Zero}
	if !ok {
		return b, nil
	}
	if b.qty, err = parseDec("positions.qty_base", row.QtyBase); err != nil {
		return bookState{}, err
	}
	if b.entry, err = parseDec("positions.entry_price", row.EntryPrice); err != nil {
		return bookState{}, err
	}
	if b.fees, err = parseDec("positions.fees_quote", row.FeesQuote); err != nil {
		return bookState{}, err
	}
	if b.realized, err = parseDec("positions.realized_pnl_quote", row.RealizedPnLQuote); err != nil {
		return bookState{}, err
	}
	return b, nil
}

// persistBook upserts the book and advances the strategy's realized-equity
// snapshot with the normative rollover math (identical to omsbridge:
// missing row seeds equity/peak at the allocated capital; a row from an
// earlier UTC day restarts the daily figure; peak is monotone).
func (o *OMS) persistBook(tx *store.SweepTx, strategyID, symbol string, b bookState, delta decimal.Decimal, now time.Time) error {
	if err := tx.UpsertPosition(store.Position{
		StrategyID:       strategyID,
		Symbol:           symbol,
		QtyBase:          b.qty.String(),
		EntryPrice:       b.entry.String(),
		FeesQuote:        b.fees.String(),
		RealizedPnLQuote: b.realized.String(),
		UpdatedAt:        formatTime(now),
	}); err != nil {
		return err
	}
	equity, peak, daily := o.allocated, o.allocated, decimal.Zero
	row, ok, err := tx.GetStrategyState(strategyID)
	if err != nil {
		return err
	}
	if ok {
		if equity, err = parseDec("strategy_state.equity_quote", row.EquityQuote); err != nil {
			return err
		}
		if peak, err = parseDec("strategy_state.peak_equity_quote", row.PeakEquityQuote); err != nil {
			return err
		}
		if row.UTCDate == utcDate(now) {
			if daily, err = parseDec("strategy_state.daily_realized_pnl_quote", row.DailyRealizedPnLQuote); err != nil {
				return err
			}
		}
	}
	equity = equity.Add(delta)
	daily = daily.Add(delta)
	peak = decimal.Max(peak, equity)
	return tx.UpsertStrategyState(store.StrategyState{
		StrategyID:            strategyID,
		EquityQuote:           equity.String(),
		PeakEquityQuote:       peak.String(),
		DailyRealizedPnLQuote: daily.String(),
		UTCDate:               utcDate(now),
		UpdatedAt:             formatTime(now),
	})
}

// splitSymbol splits a canonical BASE/QUOTE symbol.
func splitSymbol(symbol string) (base, quote string, err error) {
	base, quote, ok := strings.Cut(symbol, "/")
	if !ok || base == "" || quote == "" {
		return "", "", fmt.Errorf("live: invalid canonical symbol %q", symbol)
	}
	return base, quote, nil
}

// convertedFee is the outcome of one commission-to-quote conversion.
type convertedFee struct {
	fee      decimal.Decimal
	deferred bool // no fresh mark: fee accounting persists via pending_fill_fees
	anomaly  bool // non-base/quote commission asset converted at the mark
}

// feeToQuote converts a venue commission to fee_quote (Reconciler R5):
// quote-asset commission verbatim; base-asset commission at that trade's
// price; anything else at the current mark when fresh (anomaly, alerted) or
// DEFERRED when not — no fee is ever silently zero.
func (o *OMS) feeToQuote(symbol, commission, commissionAsset string, price decimal.Decimal) (convertedFee, error) {
	if commission == "" {
		return convertedFee{fee: decimal.Zero}, nil
	}
	c, err := parseDec("commission", commission)
	if err != nil {
		return convertedFee{}, err
	}
	if c.IsZero() {
		return convertedFee{fee: decimal.Zero}, nil
	}
	base, quote, err := splitSymbol(symbol)
	if err != nil {
		return convertedFee{}, err
	}
	switch commissionAsset {
	case quote:
		return convertedFee{fee: c}, nil
	case base:
		return convertedFee{fee: round8(c.Mul(price))}, nil
	}
	if mark, _, ok := o.marks.Mark(commissionAsset+"/"+quote, o.now()); ok {
		return convertedFee{fee: round8(c.Mul(mark)), anomaly: true}, nil
	}
	return convertedFee{deferred: true, anomaly: true}, nil
}

// fillTarget is the attributed destination of one venue fill: a local order
// (normal path) or an intent-attributed order (poisoned-late, step 8).
type fillTarget struct {
	orderID    string
	strategyID string
	symbol     string // canonical
	side       string // buy | sell
	qtyBase    string // the order's requested quantity ("" when unknown)
	status     string // current persisted status ("" when unknown)
	// protectiveKinds are the entry's protective obligations ("sl"/"tp"):
	// set only for ENTRY-class order targets, never intent-attributed
	// duplicates (§Protective order lifecycle: placement on fill).
	protectiveKinds []string
}

func orderTarget(ord store.LiveOrder) fillTarget {
	t := fillTarget{
		orderID: ord.OrderID, strategyID: ord.StrategyID, symbol: ord.Symbol,
		side: ord.Side, qtyBase: ord.QtyBase, status: ord.Status,
	}
	if ord.Class == "ENTRY" {
		t.protectiveKinds = protectiveKindsOf(ord)
	}
	return t
}

func intentTarget(i store.OrderIntent) fillTarget {
	return fillTarget{
		orderID: i.OrderID, strategyID: i.StrategyID, symbol: i.Symbol,
		side: i.Side, qtyBase: i.QtyBase,
	}
}

// venueFill is one venue execution to book (stream and backfill converge
// here; values are verbatim venue decimal strings).
type venueFill struct {
	target          fillTarget
	venueSymbol     string
	tradeID         int64
	qty, price      string
	commission      string
	commissionAsset string
	ts              time.Time
	// venueOrderStatus advances the FSM when the venue reported one
	// (stream executionReport); "" derives filled/partially_filled from the
	// cumulative booked quantity (backfill).
	venueOrderStatus string
}

// bookVenueFill books one venue fill through the single deduped,
// one-transaction path (invariant 8): fill INSERT keyed by (venue_epoch,
// venue_symbol, exchange_trade_id), VWAP/filled_at bookkeeping, monotone
// FSM advance, and the identical-to-paper accounting application. Replays
// return (false, nil). fill_after_terminal is appended when a late fill
// lands on a terminal order (cancel-then-fill race: status is KEPT).
func (o *OMS) bookVenueFill(f venueFill, runID *string) (bool, error) {
	qty, err := parseDec("fill qty", f.qty)
	if err != nil {
		return false, err
	}
	price, err := parseDec("fill price", f.price)
	if err != nil {
		return false, err
	}
	conv, err := o.feeToQuote(f.target.symbol, f.commission, f.commissionAsset, price)
	if err != nil {
		return false, err
	}
	fee := conv.fee
	if conv.deferred {
		fee = decimal.Zero
	}
	status := localStatus(f.venueOrderStatus)
	if status == "" {
		status, err = o.deriveFillStatus(f.target, qty)
		if err != nil {
			return false, err
		}
	}
	now := o.now()
	fillID := newUUID()
	row := store.VenueFill{
		Fill: store.Fill{
			FillID: fillID, OrderID: f.target.orderID, QtyBase: f.qty,
			FillPrice: f.price, FeeQuote: fee.String(), FillTS: formatTime(f.ts),
		},
		VenueSymbol: f.venueSymbol, ExchangeTradeID: f.tradeID, VenueEpoch: o.currentEpoch(),
	}
	var flat bool
	inserted, err := o.st.RecordVenueFill(row, status, func(tx *store.SweepTx) error {
		book, err := loadBook(tx, f.target.strategyID, f.target.symbol)
		if err != nil {
			return err
		}
		before := book.realized
		applyFill(&book, f.target.side, qty, price, fee)
		if err := o.persistBook(tx, f.target.strategyID, f.target.symbol, book, book.realized.Sub(before), now); err != nil {
			return err
		}
		flat = book.qty.IsZero()
		// Every entry fill (each partial included) arms a fresh restart-safe
		// deadline row in the SAME transaction as its booking (§Protective
		// order lifecycle: due_at = the triggering fill's time + deadline).
		for _, kind := range f.target.protectiveKinds {
			if err := tx.InsertProtectiveObligation(store.ProtectiveObligation{
				ObligationID: newUUID(), EntryOrderID: f.target.orderID,
				StrategyID: f.target.strategyID, Kind: kind,
				DueAt:     formatTime(f.ts.Add(o.tuning.slDeadline())),
				CreatedAt: formatTime(now),
			}); err != nil {
				return err
			}
		}
		if conv.deferred {
			return tx.InsertPendingFillFee(store.PendingFillFee{
				FillID: fillID, Commission: f.commission,
				CommissionAsset: f.commissionAsset, RecordedAt: formatTime(now),
			})
		}
		return nil
	})
	if err != nil || !inserted {
		return false, err
	}
	if flat {
		// Stops-after-flatten: the fill that leaves the book flat triggers
		// the cancel of the position's resting protectives (a stop fill
		// likewise cancels its sibling TP once the position is closed).
		o.cancelRestingProtectives(f.target.strategyID, f.target.symbol, f.target.orderID)
	}
	o.notifyFill(f.target.strategyID)
	return true, o.appendFillEvents(f, conv, runID)
}

// notifyFill invokes the optional Config.OnFill hook after a booked fill
// (the breaker monitor's Poke seam, safety-wiring.md §Evaluation loop): a
// panicking hook is recovered and logged so it can never take down the
// booking path.
func (o *OMS) notifyFill(strategyID string) {
	if o.onFill == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			o.logf("live: OnFill hook panic: %v", r)
		}
	}()
	o.onFill(strategyID)
}

// deriveFillStatus computes the FSM status a backfilled fill implies:
// filled once the cumulative booked quantity (this fill included) covers
// the order's requested quantity, else partially_filled.
func (o *OMS) deriveFillStatus(t fillTarget, qty decimal.Decimal) (string, error) {
	if t.qtyBase == "" {
		return "partially_filled", nil
	}
	orig, err := parseDec("orders.qty_base", t.qtyBase)
	if err != nil {
		return "", err
	}
	booked, err := o.bookedQty(t.orderID)
	if err != nil {
		return "", err
	}
	if booked.Add(qty).GreaterThanOrEqual(orig) {
		return "filled", nil
	}
	return "partially_filled", nil
}

// bookedQty is the derived executed quantity: SUM(fills.qty_base).
func (o *OMS) bookedQty(orderID string) (decimal.Decimal, error) {
	fills, err := o.st.ListFillsByOrder(orderID)
	if err != nil {
		return decimal.Decimal{}, err
	}
	sum := decimal.Zero
	for _, f := range fills {
		q, err := parseDec("fills.qty_base", f.QtyBase)
		if err != nil {
			return decimal.Decimal{}, err
		}
		sum = sum.Add(q)
	}
	return sum, nil
}

// appendFillEvents appends the observational rows a booked fill implies:
// fill_after_terminal (cancel-then-fill race) and commission_asset_anomaly
// (non-base/quote commission, operator alert).
func (o *OMS) appendFillEvents(f venueFill, conv convertedFee, runID *string) error {
	if f.target.status != "" && isTerminal(f.target.status) {
		ev := o.event("fill_after_terminal", map[string]any{
			"kept_status": f.target.status, "qty": f.qty, "price": f.price,
		})
		ev.RunID, ev.StrategyID, ev.Symbol = runID, &f.target.strategyID, &f.target.symbol
		ev.ExchangeTradeID = &f.tradeID
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return err
		}
	}
	if conv.anomaly {
		details := map[string]any{"commission_asset": f.commissionAsset, "deferred": conv.deferred}
		ev := o.event("commission_asset_anomaly", details)
		ev.RunID, ev.StrategyID, ev.Symbol = runID, &f.target.strategyID, &f.target.symbol
		ev.ExchangeTradeID = &f.tradeID
		o.logf("live: ALERT commission asset anomaly on %s trade %d (asset %s, deferred %v)",
			f.venueSymbol, f.tradeID, f.commissionAsset, conv.deferred)
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return err
		}
	}
	return nil
}
