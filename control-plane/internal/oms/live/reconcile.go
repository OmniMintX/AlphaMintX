package live

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// RunCounters are the run_completed details (Reconciler R7), exported for
// the recon-status API payload (§API surface).
type RunCounters struct {
	IntentsResolved int `json:"intents_resolved"`
	OrphansAdopted  int `json:"orphans_adopted"`
	OrphansCanceled int `json:"orphans_canceled"`
	FillsBackfilled int `json:"fills_backfilled"`
	Mismatches      int `json:"mismatches"`
}

// reconRun is one run's working state; run_id is the run's own UUID (NOT a
// runs-table foreign key).
type reconRun struct {
	id       string
	counters RunCounters
	// seen indexes R3's venue open orders by clientOrderID (R4 exclusion,
	// R6 cum-qty audit).
	seen map[string]exchange.OrderState
	// foreign dedupes foreign_order_ignored sightings per order id per run.
	foreign map[int64]bool
	// dupExposed suppresses repeated cum_qty_mismatch for orders already
	// flagged duplicate_exposure (R6 suppression rule).
	dupExposed map[string]bool
}

// TriggerRun runs R1-R7 synchronously (the POST /api/v1/oms/recon/run
// seam): ErrReconRunning when a run is in progress (internal triggers
// coalesce instead). acceptVenueReset acknowledges a detected venue reset
// by INSERTing the next venue_epochs row — the epoch transition — after
// which a startup-grade reconcile runs against the new venue world.
func (o *OMS) TriggerRun(ctx context.Context, acceptVenueReset bool) error {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return ErrReconRunning
	}
	o.running = true
	if acceptVenueReset && o.resetPending {
		next := store.VenueEpoch{
			VenueEpoch:  o.venueEpoch.VenueEpoch + 1,
			StartedAt:   formatTime(o.now()),
			Reason:      "venue_reset_accepted",
			DetailsJSON: fmt.Sprintf(`{"previous_epoch":%d}`, o.venueEpoch.VenueEpoch),
		}
		if err := o.st.InsertVenueEpoch(next); err != nil {
			o.running = false
			o.mu.Unlock()
			return err
		}
		o.venueEpoch = next
		o.resetPending = false
		o.reconciled = false // startup-grade: reconcile-before-trade again
	}
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()
	return o.reconcile(ctx)
}

// runInternal coalesces internal triggers (startup, periodic, stream
// reconnect) into an in-progress run: it never surfaces RECON_RUNNING.
func (o *OMS) runInternal(ctx context.Context) error {
	o.mu.Lock()
	if o.running {
		o.mu.Unlock()
		return nil
	}
	o.running = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.running = false
		o.mu.Unlock()
	}()
	return o.reconcile(ctx)
}

// reconcile executes one full R1-R7 run, bracketed by run_started and
// run_completed|run_failed (invariant 16).
func (o *OMS) reconcile(ctx context.Context) error {
	run := &reconRun{
		id:         newUUID(),
		seen:       make(map[string]exchange.OrderState),
		foreign:    make(map[int64]bool),
		dupExposed: make(map[string]bool),
	}
	o.mu.Lock()
	startup := !o.reconciled
	o.mu.Unlock()
	started := o.event("run_started", nil)
	started.RunID = &run.id
	if err := o.st.AppendOMSReconEvent(started); err != nil {
		return err
	}
	if err := o.runSteps(ctx, run, startup); err != nil {
		failed := o.event("run_failed", venueErrDetails(err))
		failed.RunID = &run.id
		if aerr := o.st.AppendOMSReconEvent(failed); aerr != nil {
			return aerr
		}
		if startup {
			o.noteStartupFailure(run.id)
		}
		return err
	}
	done := o.event("run_completed", map[string]any{
		"intents_resolved": run.counters.IntentsResolved,
		"orphans_adopted":  run.counters.OrphansAdopted,
		"orphans_canceled": run.counters.OrphansCanceled,
		"fills_backfilled": run.counters.FillsBackfilled,
		"mismatches":       run.counters.Mismatches,
	})
	done.RunID = &run.id
	if err := o.st.AppendOMSReconEvent(done); err != nil {
		return err
	}
	o.mu.Lock()
	o.reconFails = 0
	if !o.resetPending {
		o.reconciled = true
		o.lastReconcileEnd = o.now()
	}
	o.mu.Unlock()
	if o.Reconciled() {
		// R7: the OMS now accepts submissions, then immediately re-drives
		// pending safety effects BEFORE the protective drive
		// (safety-wiring.md §DriveSafetyEffects cadence) and finally the
		// protective obligations (startup re-arm; periodic runs double as
		// the deadline/retry cadence).
		if err := o.DriveSafetyEffects(ctx); err != nil {
			o.logf("live: safety drive: %v", err)
		}
		if err := o.driveProtectives(ctx); err != nil {
			o.logf("live: protective drive: %v", err)
		}
	}
	return nil
}

// noteStartupFailure counts consecutive startup-reconcile failures and, at
// the alert threshold, appends recon_blocked_safety + operator alert: the
// OMS KEEPS failing closed (the bounded exception to exit exemption).
func (o *OMS) noteStartupFailure(runID string) {
	o.mu.Lock()
	o.reconFails++
	fails := o.reconFails
	threshold := o.tuning.ReconFailureAlertThreshold
	o.mu.Unlock()
	if fails < threshold {
		return
	}
	ev := o.event("recon_blocked_safety", map[string]any{"consecutive_failures": fails})
	ev.RunID = &runID
	o.logf("live: ALERT startup reconcile failed %d consecutive attempts; OMS stays closed", fails)
	if err := o.st.AppendOMSReconEvent(ev); err != nil {
		o.logf("live: recon_blocked_safety append: %v", err)
	}
}

// runSteps executes R1-R6 (R7 bracketing lives in reconcile).
func (o *OMS) runSteps(ctx context.Context, run *reconRun, startup bool) error {
	// R1 — filters: refresh iff due OR flagged stale by a venue
	// filter-violation reject. STARTUP failure aborts the run (fail
	// closed, never trade unfiltered); a PERIODIC transient failure keeps
	// serving unexpired filters (expiry itself fails closed at preflight).
	o.mu.Lock()
	due := o.filtersDueLocked() || o.filtersStale
	o.mu.Unlock()
	if due {
		if err := o.loadFilters(ctx); err != nil {
			if startup {
				return fmt.Errorf("R1 filters: %w", err)
			}
			o.logf("live: R1 periodic filter refresh failed: %v", err)
		}
	}
	if err := o.resolveIntents(ctx, run); err != nil { // R2
		return err
	}
	if err := o.sweepOpenOrders(ctx, run); err != nil { // R3
		return err
	}
	if err := o.absenceCheck(ctx, run); err != nil { // R4
		return err
	}
	if !o.resetPendingNow() {
		for _, sym := range o.symbols { // R5
			if err := o.backfillSymbol(ctx, run, sym); err != nil {
				return err
			}
		}
		if err := o.retryFeeConversions(run); err != nil { // R5 deferred fees
			return err
		}
		if err := o.cumQtyAudit(run); err != nil { // R6
			return err
		}
		if err := o.balanceSanity(ctx, run); err != nil { // R6 balance sanity
			return err
		}
	}
	return nil
}

func (o *OMS) resetPendingNow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.resetPending
}

// resolveIntents is R2: every pending_new order's LATEST attempt is either
// unclaimed (crash before send) or has its claim REVOKED first — in-flight
// exclusion is transactional, not clock-based — then resolved by query.
func (o *OMS) resolveIntents(ctx context.Context, run *reconRun) error {
	intents, err := o.st.ListPendingNewIntents()
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if intent.ClaimedAt != nil && intent.ClaimRevokedAt == nil {
			if err := o.st.RecordIntentClaimRevoked(intent.ClientOrderID, formatTime(o.now())); err != nil {
				return err
			}
		}
		state, qerr := o.ex.QueryOrder(ctx, intent.VenueSymbol, intent.ClientOrderID)
		switch {
		case qerr == nil: // found => adopt
			if err := o.adoptAck(intent, state.ExchangeOrderID, state.Status,
				"intent_resolved_present", &run.id); err != nil {
				return err
			}
			run.counters.IntentsResolved++
			run.counters.OrphansAdopted++
		case exchange.Classify(qerr) == exchange.ClassNotFound: // never-acked absence
			if err := o.resolveAbsent(intent, "never_acked", &run.id); err != nil {
				return err
			}
			run.counters.IntentsResolved++
		default: // ambiguous: leave for the next run
		}
	}
	return nil
}

// resolveAbsent retires one intent as a never-acked absence: rejected
// status and the intent_resolved_absent event in ONE transaction.
func (o *OMS) resolveAbsent(intent store.OrderIntent, reason string, runID *string) error {
	ev := o.event("intent_resolved_absent", map[string]any{"reason": reason})
	ev.RunID = runID
	ev.StrategyID, ev.Symbol = &intent.StrategyID, &intent.Symbol
	ev.ClientOrderID = &intent.ClientOrderID
	return o.st.ApplySweep(func(tx *store.SweepTx) error {
		if _, err := tx.RecordOrderStatus(intent.OrderID, "rejected"); err != nil {
			return err
		}
		return tx.AppendOMSReconEvent(ev)
	})
}

// sweepOpenOrders is R3: classify every venue open order per symbol.
func (o *OMS) sweepOpenOrders(ctx context.Context, run *reconRun) error {
	for _, sym := range o.symbols {
		open, err := o.ex.OpenOrders(ctx, o.venueOf[sym])
		if err != nil {
			o.logf("live: R3 OpenOrders(%s) failed: %v", o.venueOf[sym], err)
			continue // transient: the next run retries
		}
		for _, vo := range open {
			if err := o.classifyVenueOpen(ctx, run, sym, vo); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *OMS) classifyVenueOpen(ctx context.Context, run *reconRun, canonical string, vo exchange.OrderState) error {
	if !inNamespace(vo.ClientOrderID) {
		// Out-of-namespace: NEVER cancel or adopt (the account may be
		// shared); one sighting event per order id per run.
		if run.foreign[vo.ExchangeOrderID] {
			return nil
		}
		run.foreign[vo.ExchangeOrderID] = true
		eid := strconv.FormatInt(vo.ExchangeOrderID, 10)
		ev := o.event("foreign_order_ignored", map[string]any{"venue_type": vo.Type})
		ev.RunID, ev.Symbol, ev.ExchangeOrderID = &run.id, &canonical, &eid
		return o.st.AppendOMSReconEvent(ev)
	}
	run.seen[vo.ClientOrderID] = vo
	ord, err := o.st.GetLiveOrderByClientOrderID(vo.ClientOrderID)
	switch {
	case err == nil && !isTerminal(ord.Status):
		// Matches a live local order: sync.
		if ord.ExchangeOrderID == nil {
			if err := o.st.RecordExchangeAck(ord.OrderID,
				strconv.FormatInt(vo.ExchangeOrderID, 10)); err != nil {
				return err
			}
		}
		_, err := o.advanceStatus(ord, vo.Status, &run.id)
		return err
	case err == nil:
		// Terminal locally yet open at the venue: an intent-attributed
		// duplicate (poisoned-late or a late send that slipped a crash
		// window after claim revocation). Cancel REGARDLESS of shape.
		return o.cancelVenueOrphan(ctx, run, canonical, vo,
			o.orphanReason(vo.ClientOrderID), &ord.StrategyID)
	case errors.Is(err, store.ErrNotFound):
		intent, ierr := o.st.GetOrderIntent(vo.ClientOrderID)
		if ierr == nil {
			return o.cancelVenueOrphan(ctx, run, canonical, vo,
				o.orphanReason(vo.ClientOrderID), &intent.StrategyID)
		}
		if !errors.Is(ierr, store.ErrNotFound) {
			return ierr
		}
		// Intent-less in-namespace id: an UNATTRIBUTABLE orphan (possible
		// only if the DB regressed).
		if protectiveShaped(vo.Type) {
			// Invariant 11: never cancel a protective; not adopted (no
			// strategy attribution exists).
			eid := strconv.FormatInt(vo.ExchangeOrderID, 10)
			ev := o.event("orphan_protective_left", map[string]any{"venue_type": vo.Type})
			ev.RunID, ev.Symbol = &run.id, &canonical
			ev.ClientOrderID, ev.ExchangeOrderID = &vo.ClientOrderID, &eid
			o.logf("live: ALERT unattributable protective-shaped orphan %s left open on %s",
				vo.ClientOrderID, vo.VenueSymbol)
			return o.st.AppendOMSReconEvent(ev)
		}
		return o.cancelVenueOrphan(ctx, run, canonical, vo, "unattributable", nil)
	default:
		return err
	}
}

// orphanReason distinguishes a late send that slipped out after claim
// revocation (late_send_detected) from a poisoned id that materialized
// late (poisoned_late), via the journal row.
func (o *OMS) orphanReason(clientOrderID string) string {
	intent, err := o.st.GetOrderIntent(clientOrderID)
	if err == nil && intent.ClaimRevokedAt != nil {
		return "late_send_detected"
	}
	return "poisoned_late"
}

// cancelVenueOrphan journals the orphan_canceled event BEFORE the cancel
// executes (journal-then-act, invariant 16); NotFound on cancel is success
// (already gone), other failures retry on the next run.
func (o *OMS) cancelVenueOrphan(ctx context.Context, run *reconRun, canonical string, vo exchange.OrderState, reason string, strategyID *string) error {
	eid := strconv.FormatInt(vo.ExchangeOrderID, 10)
	ev := o.event("orphan_canceled", map[string]any{"reason": reason, "venue_type": vo.Type})
	ev.RunID, ev.StrategyID, ev.Symbol = &run.id, strategyID, &canonical
	ev.ClientOrderID, ev.ExchangeOrderID = &vo.ClientOrderID, &eid
	if err := o.st.AppendOMSReconEvent(ev); err != nil {
		return err
	}
	if _, err := o.ex.CancelOrder(ctx, vo.VenueSymbol, vo.ClientOrderID); err != nil &&
		exchange.Classify(err) != exchange.ClassNotFound {
		o.logf("live: orphan cancel %s failed (next run retries): %v", vo.ClientOrderID, err)
		return nil
	}
	run.counters.OrphansCanceled++
	return nil
}

// absenceCheck is R4: every local rank 1-2 order absent from R3's sweep is
// queried; terminal at venue terminalizes locally; NotFound splits on ack
// history — never-acked rejects, previously-acked is the venue-reset alarm.
func (o *OMS) absenceCheck(ctx context.Context, run *reconRun) error {
	orders, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		return err
	}
	for _, ord := range orders {
		if ord.Status == "pending_new" || ord.ClientOrderID == nil {
			continue // R2 owns pending_new
		}
		if _, ok := run.seen[*ord.ClientOrderID]; ok {
			continue
		}
		venueSym, ok := o.venueOf[ord.Symbol]
		if !ok {
			continue // unconfigured symbol: nothing to query
		}
		state, qerr := o.ex.QueryOrder(ctx, venueSym, *ord.ClientOrderID)
		switch {
		case qerr == nil:
			mapped := localStatus(state.Status)
			if _, err := o.advanceStatus(ord, state.Status, &run.id); err != nil {
				return err
			}
			if mapped != "" && isTerminal(mapped) {
				eid := strconv.FormatInt(state.ExchangeOrderID, 10)
				ev := o.event("order_terminalized", map[string]any{"venue_status": state.Status})
				ev.RunID, ev.StrategyID, ev.Symbol = &run.id, &ord.StrategyID, &ord.Symbol
				ev.ClientOrderID, ev.ExchangeOrderID = ord.ClientOrderID, &eid
				if err := o.st.AppendOMSReconEvent(ev); err != nil {
					return err
				}
				// executedQty > 0: R5 backfills this symbol in this run.
			}
		case exchange.Classify(qerr) == exchange.ClassNotFound:
			if ord.ExchangeOrderID == nil {
				// Never acked: the venue never had it.
				intent := store.OrderIntent{
					OrderID: ord.OrderID, StrategyID: ord.StrategyID,
					Symbol: ord.Symbol, ClientOrderID: *ord.ClientOrderID,
				}
				if err := o.resolveAbsent(intent, "never_acked", &run.id); err != nil {
					return err
				}
				continue
			}
			// Previously ACKED yet NotFound: the venue-reset alarm — never
			// a quiet reject.
			if err := o.flagVenueReset(&run.id, &ord); err != nil {
				return err
			}
		default: // ambiguous: leave for the next run
		}
	}
	return nil
}

// flagVenueReset appends venue_reset plus its companion safety_alerts
// row in ONE transaction (alert-notifier.md AN-1a: sends refused until a
// human acknowledges is the most page-worthy condition — the alert makes
// it reach the notifier), marks the order expired with the reason in the
// event details, and transitions to RECONCILE_PENDING: ALL sends are
// refused until an operator acknowledges (§Venue epochs).
func (o *OMS) flagVenueReset(runID *string, ord *store.LiveOrder) error {
	details := map[string]any{"reason": "previously_acked_not_found"}
	ev := o.event("venue_reset", details)
	ev.RunID = runID
	if ord != nil {
		ev.StrategyID, ev.Symbol = &ord.StrategyID, &ord.Symbol
		ev.ClientOrderID, ev.ExchangeOrderID = ord.ClientOrderID, ord.ExchangeOrderID
	}
	if err := o.st.AppendOMSReconEventWithAlert(ev, store.SafetyAlert{
		AlertID: newUUID(), Kind: "venue_reset", StrategyID: ev.StrategyID,
		RefID: &ev.EventID, DetailsJSON: ev.DetailsJSON, RecordedAt: ev.RecordedAt,
	}); err != nil {
		return err
	}
	if ord != nil {
		if _, err := o.st.RecordOrderStatus(ord.OrderID, "expired"); err != nil {
			return err
		}
	}
	o.logf("live: ALERT venue reset detected; sends refused until operator acknowledgment")
	o.mu.Lock()
	o.resetPending = true
	o.mu.Unlock()
	return nil
}

// backfillSymbol is R5 for one symbol: page MyTrades from the persisted
// watermark W = MAX(fills.exchange_trade_id) of the CURRENT epoch (cold
// start: first page by startTime = the epoch's started_at, then the fromId
// handoff), attribute each trade, and book it through the deduped path.
func (o *OMS) backfillSymbol(ctx context.Context, run *reconRun, canonical string) error {
	venueSym := o.venueOf[canonical]
	o.mu.Lock()
	epoch := o.venueEpoch
	o.mu.Unlock()
	wm, ok, err := o.st.FillWatermark(epoch.VenueEpoch, venueSym)
	if err != nil {
		return err
	}
	fromID := int64(0)
	var startTime time.Time
	if ok {
		fromID = wm + 1
	} else if startTime, err = time.Parse(time.RFC3339, epoch.StartedAt); err != nil {
		return fmt.Errorf("live: venue_epochs.started_at %q: %w", epoch.StartedAt, err)
	}
	const pageLimit = 1000
	for {
		trades, terr := o.ex.MyTrades(ctx, venueSym, fromID, startTime, pageLimit)
		if terr != nil {
			o.logf("live: R5 MyTrades(%s) failed (next run retries): %v", venueSym, terr)
			return nil
		}
		if len(trades) == 0 {
			return nil
		}
		for _, t := range trades {
			if err := o.backfillTrade(run, canonical, venueSym, t); err != nil {
				return err
			}
		}
		// Paging handoff: every page after the first switches to fromId.
		fromID = trades[len(trades)-1].ID + 1
		startTime = time.Time{}
		if len(trades) < pageLimit {
			return nil
		}
	}
}

// backfillTrade attributes and books one trade: via clientOrderId/orderId
// to a local order, else via the order_intents journal (poisoned-late
// fills are real and OURS: booked to the intent's strategy, flagged
// duplicate_exposure). Out-of-namespace trades are skipped (not ours).
func (o *OMS) backfillTrade(run *reconRun, canonical, venueSym string, t exchange.Trade) error {
	var target fillTarget
	dup := false
	switch {
	case t.ClientOrderID != "":
		if !inNamespace(t.ClientOrderID) {
			return nil // not ours
		}
		ord, err := o.st.GetLiveOrderByClientOrderID(t.ClientOrderID)
		switch {
		case err == nil:
			target = orderTarget(ord)
		case errors.Is(err, store.ErrNotFound):
			intent, ierr := o.st.GetOrderIntent(t.ClientOrderID)
			if errors.Is(ierr, store.ErrNotFound) {
				return nil // in-namespace but unattributable: DB regression
			}
			if ierr != nil {
				return ierr
			}
			target = intentTarget(intent)
			dup = true
		default:
			return err
		}
	default:
		ord, err := o.st.GetLiveOrderByExchangeOrderID(canonical,
			strconv.FormatInt(t.ExchangeOrderID, 10))
		if errors.Is(err, store.ErrNotFound) {
			return nil // no local ack for this venue order: not ours
		}
		if err != nil {
			return err
		}
		target = orderTarget(ord)
	}
	inserted, err := o.bookVenueFill(venueFill{
		target: target, venueSymbol: venueSym, tradeID: t.ID,
		qty: t.Qty, price: t.Price,
		commission: t.Commission, commissionAsset: t.CommissionAsset,
		ts: t.Time,
	}, &run.id)
	if err != nil || !inserted {
		return err
	}
	run.counters.FillsBackfilled++
	ev := o.event("fill_backfilled", map[string]any{"qty": t.Qty, "price": t.Price})
	ev.RunID, ev.StrategyID, ev.Symbol = &run.id, &target.strategyID, &target.symbol
	ev.ExchangeTradeID = &t.ID
	if t.ClientOrderID != "" {
		ev.ClientOrderID = &t.ClientOrderID
	}
	if err := o.st.AppendOMSReconEvent(ev); err != nil {
		return err
	}
	if dup && !run.dupExposed[target.orderID] {
		run.dupExposed[target.orderID] = true
		return o.appendDuplicateExposure(target, t.ClientOrderID, &run.id)
	}
	return nil
}

// appendDuplicateExposure flags booked fills of a poisoned-late order: the
// fills are real and ours; risk reduction is an OPERATOR action (the OMS
// never un-books a venue fill).
func (o *OMS) appendDuplicateExposure(target fillTarget, clientOrderID string, runID *string) error {
	ev := o.event("duplicate_exposure", map[string]any{"order_id": target.orderID})
	ev.RunID, ev.StrategyID, ev.Symbol = runID, &target.strategyID, &target.symbol
	if clientOrderID != "" {
		ev.ClientOrderID = &clientOrderID
	}
	o.logf("live: ALERT duplicate exposure booked for strategy %s on %s (order %s)",
		target.strategyID, target.symbol, target.orderID)
	return o.st.AppendOMSReconEvent(ev)
}

// cumQtyAudit is R6: per venue order seen in R3, venue cumulative
// executedQty vs the derived SUM(fills.qty_base); a mismatch AFTER R5 is
// evented and alerted (venue is truth — the gap closes next run or flags a
// dedup defect); orders already flagged duplicate_exposure are suppressed.
func (o *OMS) cumQtyAudit(run *reconRun) error {
	for clientID, vo := range run.seen {
		ord, err := o.st.GetLiveOrderByClientOrderID(clientID)
		if errors.Is(err, store.ErrNotFound) {
			continue // intent-attributed duplicates audit via their events
		}
		if err != nil {
			return err
		}
		if run.dupExposed[ord.OrderID] {
			continue
		}
		venueCum, err := parseDec("venue executedQty", zeroIfEmpty(vo.ExecutedQty))
		if err != nil {
			return err
		}
		local, err := o.bookedQty(ord.OrderID)
		if err != nil {
			return err
		}
		if venueCum.Equal(local) {
			continue
		}
		run.counters.Mismatches++
		ev := o.event("cum_qty_mismatch", map[string]any{
			"venue_executed_qty": venueCum.String(), "local_fill_sum": local.String(),
		})
		ev.RunID, ev.StrategyID, ev.Symbol = &run.id, &ord.StrategyID, &ord.Symbol
		ev.ClientOrderID = ord.ClientOrderID
		o.logf("live: ALERT cum-qty mismatch on %s (venue %s, local %s)",
			clientID, venueCum, local)
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return err
		}
	}
	return nil
}

func zeroIfEmpty(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

// retryFeeConversions retries every deferred fee conversion against fresh
// marks (R5): converted_at and the fee-dependent accounting (fees_quote,
// realized PnL, strategy_state) commit in ONE transaction — the fill row
// and the position quantity were already booked at deferral time. A fee
// whose mark is still missing waits for the next run; no fee is ever
// silently zero (invariant 10).
func (o *OMS) retryFeeConversions(run *reconRun) error {
	pending, err := o.st.ListUnconvertedPendingFillFees()
	if err != nil {
		return err
	}
	for _, pf := range pending {
		ord, err := o.st.GetLiveOrderForFill(pf.FillID)
		if err != nil {
			return err
		}
		_, quote, err := splitSymbol(ord.Symbol)
		if err != nil {
			return err
		}
		commission, err := parseDec("pending_fill_fees.commission", pf.Commission)
		if err != nil {
			return err
		}
		mark, _, ok := o.marks.Mark(pf.CommissionAsset+"/"+quote, o.now())
		if !ok {
			continue // still no fresh mark: the next run retries
		}
		fee := round8(commission.Mul(mark))
		now := o.now()
		if err := o.st.ApplySweep(func(tx *store.SweepTx) error {
			if err := tx.RecordFeeConverted(pf.FillID, formatTime(now)); err != nil {
				return err
			}
			book, err := loadBook(tx, ord.StrategyID, ord.Symbol)
			if err != nil {
				return err
			}
			book.fees = book.fees.Add(fee)
			book.realized = book.realized.Sub(fee)
			return o.persistBook(tx, ord.StrategyID, ord.Symbol, book, fee.Neg(), now)
		}); err != nil {
			return err
		}
		ev := o.event("fee_conversion_applied", map[string]any{
			"commission": pf.Commission, "commission_asset": pf.CommissionAsset,
			"fee_quote": fee.String(),
		})
		ev.RunID, ev.StrategyID, ev.Symbol = &run.id, &ord.StrategyID, &ord.Symbol
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return err
		}
	}
	return nil
}

// balanceSanity is the R6 cross-check of venue balances against local
// positions: per configured symbol's BASE asset, free+locked must cover the
// summed long positions (positions are base-denominated and stay
// fill-derived, invariant 1). Drift appends balance_drift — WARNING only:
// spot balances are account-global and may legitimately include foreign
// activity.
func (o *OMS) balanceSanity(ctx context.Context, run *reconRun) error {
	balances, err := o.ex.Balances(ctx)
	if err != nil {
		o.logf("live: R6 Balances failed (next run retries): %v", err)
		return nil
	}
	venueTotal := make(map[string]decimal.Decimal, len(balances))
	for _, b := range balances {
		free, err := parseDec("balance.free", zeroIfEmpty(b.Free))
		if err != nil {
			return err
		}
		locked, err := parseDec("balance.locked", zeroIfEmpty(b.Locked))
		if err != nil {
			return err
		}
		venueTotal[b.Asset] = free.Add(locked)
	}
	local := make(map[string]decimal.Decimal)
	for page := 1; ; page++ {
		strategies, total, err := o.st.ListStrategies(page, store.MaxPageLimit)
		if err != nil {
			return err
		}
		for _, s := range strategies {
			positions, err := o.st.ListPositions(s.StrategyID)
			if err != nil {
				return err
			}
			for _, p := range positions {
				if _, ok := o.venueOf[p.Symbol]; !ok {
					continue
				}
				qty, err := parseDec("positions.qty_base", p.QtyBase)
				if err != nil {
					return err
				}
				if qty.Sign() <= 0 {
					continue // spot: only long inventory maps to base balance
				}
				base, _, err := splitSymbol(p.Symbol)
				if err != nil {
					return err
				}
				local[base] = local[base].Add(qty)
			}
		}
		if page*store.MaxPageLimit >= total || len(strategies) == 0 {
			break
		}
	}
	for asset, want := range local {
		have := venueTotal[asset]
		if have.GreaterThanOrEqual(want) {
			continue
		}
		ev := o.event("balance_drift", map[string]any{
			"asset": asset, "venue_total": have.String(), "local_positions": want.String(),
		})
		ev.RunID = &run.id
		o.logf("live: WARNING balance drift on %s (venue %s < local positions %s)",
			asset, have, want)
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return err
		}
	}
	return nil
}
