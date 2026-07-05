package live

import (
	"context"
	"errors"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// protectiveKindsOf lists the protective obligations an ENTRY order carries
// ("sl" from orders.stop_price, "tp" from orders.take_profit).
func protectiveKindsOf(ord store.LiveOrder) []string {
	var kinds []string
	if ord.StopPrice != nil {
		kinds = append(kinds, "sl")
	}
	if ord.TakeProfit != nil {
		kinds = append(kinds, "tp")
	}
	return kinds
}

// protectiveType maps an obligation kind to the local order type (mapped to
// the venue's STOP_LOSS / TAKE_PROFIT by venueOrderType).
func protectiveType(kind string) string {
	if kind == "sl" {
		return "stop"
	}
	return "take_profit"
}

// protectiveTrigger rounds a trigger price to the nearest tick toward
// trigger safety — the direction that fires EARLIER (§Filters: stops toward
// trigger safety): a sell SL triggers on a falling price so it rounds UP; a
// sell TP triggers on a rising price so it rounds DOWN; buys mirror.
func protectiveTrigger(kind, side string, price, tick decimal.Decimal) decimal.Decimal {
	if (kind == "sl" && side == "sell") || (kind == "tp" && side == "buy") {
		return ceilToStep(price, tick)
	}
	return floorToStep(price, tick)
}

// positionQty is the fill-derived signed base position for
// (strategy, symbol); zero when no row exists.
func (o *OMS) positionQty(strategyID, symbol string) (decimal.Decimal, error) {
	positions, err := o.st.ListPositions(strategyID)
	if err != nil {
		return decimal.Decimal{}, err
	}
	for _, row := range positions {
		if row.Symbol == symbol {
			return parseDec("positions.qty_base", row.QtyBase)
		}
	}
	return decimal.Zero, nil
}

// driveProtectives converges every filled entry toward protected-or-flat
// (invariant 12): unmet obligations retry placement, cumulative-quantity
// growth cancel+replaces the resting protective, satisfied coverage
// resolves the timer rows, and a breached 'sl' deadline contingency-
// flattens. It runs after every completed reconcile (startup re-arm
// included — never before, §Reconciler reconcile-before-trade) and after
// every stream fill; that cadence is the retry backoff.
func (o *OMS) driveProtectives(ctx context.Context) error {
	o.driveMu.Lock()
	defer o.driveMu.Unlock()
	entries, err := o.st.ListFilledProtectiveEntries()
	if err != nil || len(entries) == 0 {
		return err
	}
	obls, err := o.st.ListOpenProtectiveObligations()
	if err != nil {
		return err
	}
	open := make(map[string][]store.ProtectiveObligation)
	for _, ob := range obls {
		key := ob.EntryOrderID + "/" + ob.Kind
		open[key] = append(open[key], ob)
	}
	live, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := o.driveEntry(ctx, entry, open, live); err != nil {
			return err
		}
	}
	return nil
}

// driveEntry reconciles ONE filled entry's protective coverage.
func (o *OMS) driveEntry(ctx context.Context, entry store.LiveOrder, open map[string][]store.ProtectiveObligation, live []store.LiveOrder) error {
	pos, err := o.positionQty(entry.StrategyID, entry.Symbol)
	if err != nil {
		return err
	}
	// An in-flight reduce-only market flatten is already closing the
	// position: nothing to protect until it resolves.
	if findLiveProtective(live, entry.StrategyID, entry.Symbol, "market") != nil {
		return nil
	}
	if pos.IsZero() {
		// Flat: nothing left to protect. A protective still resting here
		// means a crash separated the closing fill's booking from
		// stops-after-flatten — cancel it now (the drive is the
		// restart-safe retry); then the timer rows resolve.
		if findLiveProtective(live, entry.StrategyID, entry.Symbol, "stop") != nil ||
			findLiveProtective(live, entry.StrategyID, entry.Symbol, "take_profit") != nil {
			o.cancelRestingProtectives(entry.StrategyID, entry.Symbol, "")
		}
		for _, kind := range protectiveKindsOf(entry) {
			if err := o.satisfyObligations(open[entry.OrderID+"/"+kind]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, kind := range protectiveKindsOf(entry) {
		if err := o.driveKind(ctx, entry, kind, open[entry.OrderID+"/"+kind], live, pos); err != nil {
			return err
		}
	}
	return nil
}

// findLiveProtective returns the first non-terminal PROTECTIVE order of the
// given local type for (strategy, symbol), or nil.
func findLiveProtective(live []store.LiveOrder, strategyID, symbol, typ string) *store.LiveOrder {
	for i := range live {
		ord := &live[i]
		if ord.Class == "PROTECTIVE" && ord.StrategyID == strategyID &&
			ord.Symbol == symbol && ord.Type == typ {
			return ord
		}
	}
	return nil
}

// satisfyObligations resolves the timer rows once coverage holds (or the
// position is flat); satisfied rows are never reopened.
func (o *OMS) satisfyObligations(group []store.ProtectiveObligation) error {
	ts := formatTime(o.now())
	for _, ob := range group {
		if err := o.st.RecordProtectiveSatisfied(ob.ObligationID, ts); err != nil {
			return err
		}
	}
	return nil
}

// driveKind places, resizes, or resolves ONE (entry, kind) obligation set.
func (o *OMS) driveKind(ctx context.Context, entry store.LiveOrder, kind string, group []store.ProtectiveObligation, live []store.LiveOrder, pos decimal.Decimal) error {
	filled, err := o.bookedQty(entry.OrderID)
	if err != nil {
		return err
	}
	resting := findLiveProtective(live, entry.StrategyID, entry.Symbol, protectiveType(kind))
	if resting != nil && resting.Status == "pending_new" {
		return nil // unresolved send: the Reconciler owns the intent
	}
	sf, err := o.symbolFiltersFor(o.venueOf[entry.Symbol])
	if err != nil {
		return o.handleDeadline(ctx, entry, kind, group, err)
	}
	// The protective quantity tracks the entry's cumulative filled
	// quantity, capped at the live position (reduce-only sizing,
	// risk-limits.md) and quantized DOWN to stepSize.
	target := floorToStep(decimal.Min(filled, pos.Abs()), sf.step)
	if resting != nil {
		restingQty, err := parseDec("orders.qty_base", resting.QtyBase)
		if err != nil {
			return err
		}
		if restingQty.GreaterThanOrEqual(target) {
			// ACKED at (or above) the correct cumulative size: satisfied.
			return o.satisfyObligations(group)
		}
	}
	if target.Sign() <= 0 || target.LessThan(sf.minQty) {
		// Dust below minQty cannot rest on spot: placement cannot succeed.
		return o.handleDeadline(ctx, entry, kind, group, ErrBelowMinNotional)
	}
	if len(group) == 0 {
		// Startup re-arm: filled-but-unprotected quantity with no live
		// timer (derived from orders joined to fills) gets a FRESH
		// deadline row before the placement attempt.
		ob := store.ProtectiveObligation{
			ObligationID: newUUID(), EntryOrderID: entry.OrderID,
			StrategyID: entry.StrategyID, Kind: kind,
			DueAt:     formatTime(o.now().Add(o.tuning.slDeadline())),
			CreatedAt: formatTime(o.now()),
		}
		if err := o.st.InsertProtectiveObligation(ob); err != nil {
			return err
		}
		group = []store.ProtectiveObligation{ob}
	}
	if resting != nil {
		if err := o.resizeProtective(ctx, entry, *resting, target); err != nil {
			return o.handleDeadline(ctx, entry, kind, group, err)
		}
	}
	if err := o.placeProtective(ctx, entry, kind, target, sf); err != nil {
		return o.handleDeadline(ctx, entry, kind, group, err)
	}
	return o.satisfyObligations(group)
}

// resizeProtective cancel+replaces a resting protective whose quantity fell
// behind the entry's cumulative fills (there is no modify in v1). The
// protective_resized event journals BEFORE the destructive cancel
// (invariant 16); the caller places the replacement.
func (o *OMS) resizeProtective(ctx context.Context, entry store.LiveOrder, resting store.LiveOrder, target decimal.Decimal) error {
	ev := o.event("protective_resized", map[string]any{
		"old_qty": resting.QtyBase, "new_qty": target.String(),
	})
	ev.StrategyID, ev.Symbol = &entry.StrategyID, &entry.Symbol
	ev.ClientOrderID = resting.ClientOrderID
	if err := o.st.AppendOMSReconEvent(ev); err != nil {
		return err
	}
	if resting.ClientOrderID != nil {
		if _, err := o.ex.CancelOrder(ctx, o.venueOf[entry.Symbol], *resting.ClientOrderID); err != nil &&
			exchange.Classify(err) != exchange.ClassNotFound {
			return err // ambiguous: the next drive retries
		}
	}
	_, err := o.st.RecordOrderStatus(resting.OrderID, "canceled")
	return err
}

// placeProtective journals and sends one protective order through the SAME
// write-ahead journal path as proposal orders (§Protective order
// lifecycle): class='PROTECTIVE', reduce_only=1, the entry's
// origin/proposal linkage, trigger price rounded toward trigger safety. A
// nil return means the venue ACKED the order at the requested size.
func (o *OMS) placeProtective(ctx context.Context, entry store.LiveOrder, kind string, qty decimal.Decimal, sf symbolFilters) error {
	// Reconcile gate (invariant 2): a protective never sends while the
	// startup reconcile is incomplete or a venue reset awaits
	// acknowledgment; the deadline policy owns the retry.
	if err := o.preflightGate(entry.StrategyID, nil); err != nil {
		return err
	}
	side := "sell"
	if entry.Side == "sell" {
		side = "buy"
	}
	raw := entry.StopPrice
	if kind == "tp" {
		raw = entry.TakeProfit
	}
	if raw == nil {
		return fmt.Errorf("live: entry %s carries no %s trigger price", entry.OrderID, kind)
	}
	trigger, err := parseDec("orders."+kind+" trigger", *raw)
	if err != nil {
		return err
	}
	epoch, err := o.st.GlobalMaxKillEpoch(entry.StrategyID)
	if err != nil {
		return err
	}
	return o.journalAndSend(ctx, submission{
		strategyID: entry.StrategyID, symbol: entry.Symbol, class: "PROTECTIVE",
		side: side, typ: protectiveType(kind), origin: entry.Origin, reduceOnly: true,
		proposalID: entry.ProposalID, killEpoch: epoch,
		qty: qty, stopPrice: protectiveTrigger(kind, side, trigger, sf.tick),
	})
}

// handleDeadline runs the deadline policy after a failed placement attempt:
// while the deadline runs the next drive retries; a breached 'sl'
// obligation contingency-flattens the filled quantity (event journaled
// BEFORE the flatten, strategy-tier alert); a breached 'tp' NEVER flattens
// — it keeps retrying under tp_deadline_missed alerts, the SL still
// protects the position.
func (o *OMS) handleDeadline(ctx context.Context, entry store.LiveOrder, kind string, group []store.ProtectiveObligation, cause error) error {
	now := formatTime(o.now())
	breached := false
	for _, ob := range group {
		if ob.DueAt <= now {
			breached = true
			break
		}
	}
	if !breached {
		return nil
	}
	if kind == "tp" {
		ev := o.event("tp_deadline_missed", venueErrDetails(cause))
		ev.StrategyID, ev.Symbol = &entry.StrategyID, &entry.Symbol
		o.logf("live: ALERT TP placement deadline missed for %s %s: %v",
			entry.StrategyID, entry.Symbol, cause)
		return o.st.AppendOMSReconEvent(ev)
	}
	ev := o.event("sl_deadline_contingency", venueErrDetails(cause))
	ev.StrategyID, ev.Symbol = &entry.StrategyID, &entry.Symbol
	o.logf("live: ALERT SL placement deadline breached for %s %s: contingency flatten (%v)",
		entry.StrategyID, entry.Symbol, cause)
	if err := o.st.AppendOMSReconEvent(ev); err != nil {
		return err
	}
	if err := o.Flatten(ctx, entry.StrategyID, entry.Symbol, "sl_contingency", nil); err != nil {
		o.logf("live: contingency flatten for %s %s failed (next drive retries): %v",
			entry.StrategyID, entry.Symbol, err)
	}
	return nil
}

// cancelRestingProtectives cancels every resting PROTECTIVE order of the
// now-flat (strategy, symbol) position, excluding the order whose fill just
// closed it (stops-after-flatten: the flatten-fill BOOKING path is the
// trigger; SL/TP are canceled only AFTER the closing fill confirms). Each
// cancellation journals orphan_canceled (reason stops_after_flatten)
// BEFORE the destructive cancel (journal-then-act, invariant 16). NotFound
// is success; other cancel failures wait for the next drive or reconcile
// pass.
func (o *OMS) cancelRestingProtectives(strategyID, symbol, excludeOrderID string) {
	ctx := context.Background()
	orders, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		o.logf("live: stops-after-flatten list: %v", err)
		return
	}
	for _, ord := range orders {
		if ord.Class != "PROTECTIVE" || ord.StrategyID != strategyID ||
			ord.Symbol != symbol || ord.OrderID == excludeOrderID || ord.ClientOrderID == nil {
			continue
		}
		if ord.Status == "pending_new" {
			// Revoke the claim so an unsent protective can never transmit
			// after its position closed (§In-flight exclusion).
			if err := o.st.RecordIntentClaimRevoked(*ord.ClientOrderID, formatTime(o.now())); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				o.logf("live: stops-after-flatten revoke %s: %v", *ord.ClientOrderID, err)
				continue
			}
		}
		ev := o.event("orphan_canceled", map[string]any{
			"reason": "stops_after_flatten", "venue_type": venueOrderType(ord.Type),
		})
		ev.StrategyID, ev.Symbol = &ord.StrategyID, &ord.Symbol
		ev.ClientOrderID = ord.ClientOrderID
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			o.logf("live: stops-after-flatten journal %s: %v", *ord.ClientOrderID, err)
			continue // journal-then-act: never cancel unjournaled
		}
		if _, cerr := o.ex.CancelOrder(ctx, o.venueOf[symbol], *ord.ClientOrderID); cerr != nil &&
			exchange.Classify(cerr) != exchange.ClassNotFound {
			o.logf("live: stops-after-flatten cancel %s failed (next run retries): %v",
				*ord.ClientOrderID, cerr)
			continue
		}
		if _, err := o.st.RecordOrderStatus(ord.OrderID, "canceled"); err != nil {
			o.logf("live: stops-after-flatten status %s: %v", ord.OrderID, err)
		}
	}
}
