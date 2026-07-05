package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// submission is one normalized order bound for the journal: entries carry
// the gate-approved parameters, flattens the reduce-only close.
type submission struct {
	strategyID string
	symbol     string // canonical
	class      string // ENTRY | PROTECTIVE
	side       string // buy | sell
	typ        string // market | limit
	origin     string // orders.origin vocabulary
	reduceOnly bool
	proposalID *string
	killEpoch  int64
	qty        decimal.Decimal // normalized base quantity
	limitPrice decimal.Decimal // zero for market semantics
	stopPrice  decimal.Decimal // SL obligation carried on the entry
	takeProfit decimal.Decimal // TP obligation carried on the entry
}

// SubmitApproved is the api.Submitter seam (called at most once per
// verdict, on the single winning approved decision): it parses the stored
// proposal/verdict payloads and submits through the write-ahead journal
// path — the gate, approval flow, and risk limits are unchanged.
func (o *OMS) SubmitApproved(meta store.VerdictMeta) error {
	ctx := context.Background()
	rawP, err := o.st.GetProposalPayload(meta.ProposalID)
	if err != nil {
		return err
	}
	var p contract.Proposal
	if err := json.Unmarshal(rawP, &p); err != nil {
		return fmt.Errorf("live: proposal %s payload: %w", meta.ProposalID, err)
	}
	rawV, err := o.st.GetVerdictByProposalID(meta.ProposalID)
	if err != nil {
		return err
	}
	var v contract.Verdict
	if err := json.Unmarshal(rawV, &v); err != nil {
		return fmt.Errorf("live: verdict for %s payload: %w", meta.ProposalID, err)
	}
	switch p.Action {
	case contract.ActionOpenLong, contract.ActionOpenShort:
		size := p.SizeQuote.Decimal()
		if v.ClippedSizeQuote != nil {
			// The clipped size is the effective NOTIONAL cap at fill price.
			size = v.ClippedSizeQuote.Decimal()
		}
		side := "buy"
		if p.Action == contract.ActionOpenShort {
			side = "sell"
		}
		sub := submission{
			strategyID: p.StrategyID, symbol: p.Symbol, class: "ENTRY",
			side: side, typ: p.Entry.Type, origin: "proposal",
			proposalID: &meta.ProposalID,
		}
		if p.Entry.LimitPrice != nil {
			sub.limitPrice = p.Entry.LimitPrice.Decimal()
		}
		if p.StopLoss != nil {
			sub.stopPrice = p.StopLoss.Decimal()
		}
		if p.TakeProfit != nil {
			sub.takeProfit = p.TakeProfit.Decimal()
		}
		return o.submitEntry(ctx, sub, size, rawP)
	case contract.ActionClose:
		if err := o.preflightGate(meta.StrategyID, rawP); err != nil {
			return err // recorded drop (RECONCILE_PENDING bookkeeping)
		}
		// The close serializes on the SAME driveMu as DriveSafetyEffects:
		// the drive's double-flatten skip snapshots live orders before it
		// flattens, so an unserialized close journaled between snapshot
		// and flatten could stack a second market sell for the same
		// (strategy, symbol) (invariant 6). Lock order stays
		// driveMu -> o.mu.
		o.driveMu.Lock()
		defer o.driveMu.Unlock()
		return o.Flatten(ctx, meta.StrategyID, p.Symbol, "proposal", &meta.ProposalID)
	default:
		return fmt.Errorf("live: action %q never submits an order", p.Action)
	}
}

// preflightGate enforces reconcile-before-trade (invariant 2): while the
// startup reconcile is incomplete or a venue reset awaits acknowledgment,
// submissions are DROPPED (never queued); proposal-origin drops are
// recorded in rejected_submissions.
func (o *OMS) preflightGate(strategyID string, payload json.RawMessage) error {
	o.mu.Lock()
	open := o.reconciled && !o.resetPending
	o.mu.Unlock()
	if open {
		return nil
	}
	if payload != nil {
		if err := o.st.AppendRejectedSubmission(store.RejectedSubmission{
			RejectionID: newUUID(), StrategyID: &strategyID,
			ReceivedAt: formatTime(o.now()), Reason: "RECONCILE_PENDING",
			PayloadJSON: string(payload),
		}); err != nil {
			return err
		}
	}
	return ErrReconcilePending
}

// submitEntry runs the submit path steps 1-2 for a gate-approved entry:
// preflight (reconcile gate, kill-epoch stamp, filters, normalization) then
// journal-and-send. sizeQuote is the effective notional cap.
func (o *OMS) submitEntry(ctx context.Context, sub submission, sizeQuote decimal.Decimal, payload json.RawMessage) error {
	if err := o.preflightGate(sub.strategyID, payload); err != nil {
		return err
	}
	// Breaker halt: ENTRY submissions stop while a breaker event binds the
	// strategy on the current UTC day (protectives and reduce-only
	// continue; §Safety-engine integration).
	halted, err := o.st.BreakerActiveToday(sub.strategyID, utcDate(o.now()))
	if err != nil {
		return err
	}
	if halted {
		return ErrBreakerActive
	}
	epoch, err := o.st.GlobalMaxKillEpoch(sub.strategyID)
	if err != nil {
		return err
	}
	// Standing-kill check (safety-wiring.md invariant 15): a fresh ENTRY
	// submission would stamp the post-kill epoch and pass the transmit
	// staleness re-check, so the standing condition rejects it here;
	// safety-origin flatten/protective submissions bypass this path and
	// rely on the transmit-loop comparison alone.
	if epoch > 0 {
		return ErrKillSwitchActive
	}
	sub.killEpoch = epoch
	venueSym, ok := o.venueOf[sub.symbol]
	if !ok {
		return fmt.Errorf("live: symbol %s is not configured", sub.symbol)
	}
	sf, err := o.symbolFiltersFor(venueSym)
	if err != nil {
		return err
	}
	if sizeQuote.Sign() <= 0 {
		return fmt.Errorf("live: size_quote must be strictly positive")
	}
	var ref decimal.Decimal // sizing reference price
	switch sub.typ {
	case "limit":
		if sub.limitPrice.Sign() <= 0 {
			return fmt.Errorf("live: limit_price must be strictly positive for limit entries")
		}
		sub.limitPrice = passivePrice(sub.side, sub.limitPrice, sf.tick)
		ref = sub.limitPrice
	case "market":
		mark, _, ok := o.marks.Mark(sub.symbol, o.now())
		if !ok {
			return fmt.Errorf("live: market entry requires a fresh mark for %s", sub.symbol)
		}
		ref = mark
	default:
		return fmt.Errorf("live: entry type must be market or limit")
	}
	if sub.qty, err = normalizeEntryQty(sizeQuote, ref, sf); err != nil {
		return err
	}
	return o.journalAndSend(ctx, sub)
}

// optDec renders a strictly positive decimal as a nullable column value.
func optDec(d decimal.Decimal) *string {
	if d.Sign() <= 0 {
		return nil
	}
	s := d.String()
	return &s
}

// journalAndSend runs steps 2-8: journal the pending_new orders row and the
// attempt-0 intent in ONE transaction BEFORE any HTTP (invariant 3), then
// claim and send.
func (o *OMS) journalAndSend(ctx context.Context, sub submission) error {
	token, err := o.newIntentToken()
	if err != nil {
		return err
	}
	now := o.now()
	orderID := newUUID()
	row := store.Order{
		OrderID: orderID, ProposalID: sub.proposalID, Origin: sub.origin,
		StrategyID: sub.strategyID, Symbol: sub.symbol, Class: sub.class,
		Side: sub.side, Type: sub.typ, ReduceOnly: sub.reduceOnly,
		QtyBase: sub.qty.String(), KillEpoch: sub.killEpoch,
		Status: "pending_new", SubmittedAt: formatTime(now), // journal time: pre-send by definition
	}
	row.LimitPrice = optDec(sub.limitPrice)
	row.StopPrice = optDec(sub.stopPrice)
	row.TakeProfit = optDec(sub.takeProfit)
	intent := store.OrderIntent{
		ClientOrderID: attemptID(token, 0), IntentToken: token, Attempt: 0,
		OrderID: orderID, StrategyID: sub.strategyID, Symbol: sub.symbol,
		VenueSymbol: o.venueOf[sub.symbol], Side: sub.side, Type: sub.typ,
		QtyBase: sub.qty.String(), LimitPrice: row.LimitPrice, StopPrice: row.StopPrice,
		Origin: sub.origin, ProposalID: sub.proposalID, KillEpoch: sub.killEpoch,
		JournaledAt: formatTime(now),
	}
	if err := o.st.InsertJournaledOrder(row, intent); err != nil {
		return err
	}
	if o.afterJournal != nil {
		o.afterJournal()
	}
	return o.sendIntent(ctx, intent)
}

// sendIntent claims and transmits attempts until one resolves. A lost claim
// (already claimed or revoked) is NOT an error: the claim holder or the
// revoker owns the intent's resolution (invariant 6).
func (o *OMS) sendIntent(ctx context.Context, intent store.OrderIntent) error {
	for {
		claimed, err := o.st.RecordIntentClaim(intent.ClientOrderID, formatTime(o.now()))
		if err != nil {
			return err
		}
		if !claimed {
			return nil
		}
		next, err := o.transmit(ctx, intent)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		intent = *next // poisoned id: steps 3+ repeat with the attempt+1 id
	}
}

// transmit runs the post-claim re-checks and the placement HTTP for one
// claimed attempt (steps 3-7). The step-3 checks re-run on EVERY loop
// iteration immediately before PlaceOrder — a throttled resend re-checks
// the kill epoch and the claim exactly like a first send. A non-nil next
// intent means this id was poisoned and the caller retries with the
// attempt+1 id.
func (o *OMS) transmit(ctx context.Context, intent store.OrderIntent) (*store.OrderIntent, error) {
	req := exchange.PlaceRequest{
		VenueSymbol:      intent.VenueSymbol,
		Side:             venueSide(intent.Side),
		Type:             venueOrderType(intent.Type),
		Qty:              intent.QtyBase,
		NewClientOrderID: intent.ClientOrderID,
	}
	if intent.LimitPrice != nil {
		req.Price = *intent.LimitPrice
		req.TimeInForce = "GTC"
	}
	if intent.StopPrice != nil {
		req.StopPrice = *intent.StopPrice
	}
	for {
		// Kill re-check immediately before every send (risk-limits.md).
		maxEpoch, err := o.st.GlobalMaxKillEpoch(intent.StrategyID)
		if err != nil {
			return nil, err
		}
		if maxEpoch > intent.KillEpoch {
			return nil, o.abandonStale(intent)
		}
		// Claim re-read: a revoked claim MUST NOT transmit (invariant 6).
		row, err := o.st.GetOrderIntent(intent.ClientOrderID)
		if err != nil {
			return nil, err
		}
		if row.ClaimRevokedAt != nil {
			return nil, nil
		}
		ack, err := o.ex.PlaceOrder(ctx, req)
		if err == nil {
			return nil, o.adoptAck(intent, ack.ExchangeOrderID, ack.Status, "", nil)
		}
		switch exchange.Classify(err) {
		case exchange.ClassDefiniteReject:
			return nil, o.rejectDefinite(intent, err)
		case exchange.ClassThrottled:
			// NOT terminal, NOT poisoned: resend the SAME attempt id after
			// the Retry-After interval (the claim stays held).
			var ve *exchange.VenueError
			if errors.As(err, &ve) && ve.RetryAfter > 0 {
				o.sleep(ve.RetryAfter)
				continue
			}
			return o.resolveAmbiguous(ctx, intent)
		default: // Ambiguous: NEVER blindly resend (invariant 5).
			return o.resolveAmbiguous(ctx, intent)
		}
	}
}

// abandonStale retires an unsent attempt whose kill epoch went stale: the
// rejected status and the intent_resolved_absent bookkeeping commit in the
// SAME transaction (the id was never sent, but it is retired anyway).
func (o *OMS) abandonStale(intent store.OrderIntent) error {
	ev := o.event("intent_resolved_absent", map[string]any{"reason": "kill_epoch_stale"})
	ev.StrategyID, ev.Symbol = &intent.StrategyID, &intent.Symbol
	ev.ClientOrderID = &intent.ClientOrderID
	if err := o.st.ApplySweep(func(tx *store.SweepTx) error {
		if _, err := tx.RecordOrderStatus(intent.OrderID, "rejected"); err != nil {
			return err
		}
		return tx.AppendOMSReconEvent(ev)
	}); err != nil {
		return err
	}
	return ErrKillEpochStale
}

// rejectDefinite terminalizes a venue-refused placement (step 5): the
// intent is terminal, no retry — the condition is real. A filter-violation
// reject additionally marks the loaded filters stale so the next
// reconcile's R1 refreshes them before their normal expiry.
func (o *OMS) rejectDefinite(intent store.OrderIntent, cause error) error {
	ev := o.event("intent_resolved_absent", venueErrDetails(cause))
	ev.StrategyID, ev.Symbol = &intent.StrategyID, &intent.Symbol
	ev.ClientOrderID = &intent.ClientOrderID
	if err := o.st.ApplySweep(func(tx *store.SweepTx) error {
		if _, err := tx.RecordOrderStatus(intent.OrderID, "rejected"); err != nil {
			return err
		}
		return tx.AppendOMSReconEvent(ev)
	}); err != nil {
		return err
	}
	if filterViolation(cause) {
		o.mu.Lock()
		o.filtersStale = true
		o.mu.Unlock()
		o.logf("live: filter-violation reject on %s; filters marked stale for the next reconcile",
			intent.Symbol)
	}
	return fmt.Errorf("%w: %v", ErrExchangeRejected, cause)
}

// filterViolation reports whether a venue reject indicates a filter
// violation (-1013 "Filter failure: ..." family): the locally cached
// exchangeInfo filters no longer match the venue's.
func filterViolation(err error) bool {
	var ve *exchange.VenueError
	if !errors.As(err, &ve) {
		return false
	}
	return ve.VenueCode == -1013 || strings.HasPrefix(ve.VenueMsg, "Filter failure")
}

// adoptAck records the venue ack (step 4) and advances the FSM from the
// reported status; eventKind (e.g. intent_resolved_present) appends when
// non-empty.
func (o *OMS) adoptAck(intent store.OrderIntent, exchangeOrderID int64, venueStatus, eventKind string, runID *string) error {
	eid := strconv.FormatInt(exchangeOrderID, 10)
	if err := o.st.RecordExchangeAck(intent.OrderID, eid); err != nil {
		return err
	}
	if mapped := localStatus(venueStatus); mapped != "" {
		if _, err := o.st.RecordOrderStatus(intent.OrderID, mapped); err != nil {
			return err
		}
	}
	if eventKind == "" {
		return nil
	}
	ev := o.event(eventKind, map[string]any{"venue_status": venueStatus})
	ev.RunID = runID
	ev.StrategyID, ev.Symbol = &intent.StrategyID, &intent.Symbol
	ev.ClientOrderID, ev.ExchangeOrderID = &intent.ClientOrderID, &eid
	return o.st.AppendOMSReconEvent(ev)
}

// resolveAmbiguous resolves an ambiguous placement by query (step 7):
// QueryOrder by the attempt id with jittered exponential backoff (500 ms
// base, <= 5 tries). Found => ack. NotFound while the venue answers => the
// id is POISONED, never resent; the attempt+1 intent is journaled and
// returned. Venue unreachable => the order stays pending_new and the
// Reconciler owns it.
func (o *OMS) resolveAmbiguous(ctx context.Context, intent store.OrderIntent) (*store.OrderIntent, error) {
	backoff := 500 * time.Millisecond
	for try := 0; try < 5; try++ {
		if try > 0 {
			o.sleep(backoff + o.jitter(backoff/2))
			backoff *= 2
		}
		state, err := o.ex.QueryOrder(ctx, intent.VenueSymbol, intent.ClientOrderID)
		if err == nil {
			return nil, o.adoptAck(intent, state.ExchangeOrderID, state.Status, "intent_resolved_present", nil)
		}
		if exchange.Classify(err) != exchange.ClassNotFound {
			continue
		}
		ev := o.event("intent_resolved_absent", map[string]any{"reason": "poisoned"})
		ev.StrategyID, ev.Symbol = &intent.StrategyID, &intent.Symbol
		ev.ClientOrderID = &intent.ClientOrderID
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return nil, err
		}
		// The kill re-check repeats before any retry (step 7).
		maxEpoch, err := o.st.GlobalMaxKillEpoch(intent.StrategyID)
		if err != nil {
			return nil, err
		}
		if maxEpoch > intent.KillEpoch {
			if _, err := o.st.RecordOrderStatus(intent.OrderID, "rejected"); err != nil {
				return nil, err
			}
			return nil, ErrKillEpochStale
		}
		if intent.Attempt >= 9 {
			if _, err := o.st.RecordOrderStatus(intent.OrderID, "rejected"); err != nil {
				return nil, err
			}
			o.logf("live: ALERT placement attempts exhausted for order %s", intent.OrderID)
			return nil, fmt.Errorf("live: placement attempts exhausted for order %s", intent.OrderID)
		}
		next := intent
		next.Attempt++
		next.ClientOrderID = attemptID(next.IntentToken, next.Attempt)
		next.JournaledAt = formatTime(o.now())
		next.ClaimedAt, next.ClaimRevokedAt = nil, nil
		if err := o.st.RecordIntentAttempt(next); err != nil {
			return nil, err
		}
		return &next, nil
	}
	return nil, ErrVenueUnreachable
}

// jitter draws a uniform duration in [0, bound) from the injected
// Config.Rand (deterministic under a seeded source); o.mu serializes the
// shared source.
func (o *OMS) jitter(bound time.Duration) time.Duration {
	if bound <= 0 {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return time.Duration(o.rng.Int63n(int64(bound)))
}

// venueSide maps the store side vocabulary to the venue's.
func venueSide(side string) string {
	if side == "buy" {
		return "BUY"
	}
	return "SELL"
}

// venueOrderType maps the store order-type vocabulary to the venue's
// (protective triggers: "stop" is a stop-market, "take_profit" its mirror).
func venueOrderType(typ string) string {
	switch typ {
	case "market":
		return "MARKET"
	case "limit":
		return "LIMIT"
	case "stop":
		return "STOP_LOSS"
	case "take_profit":
		return "TAKE_PROFIT"
	}
	return typ
}
