package live

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Run drives the live OMS until ctx ends: the MANDATORY startup reconcile
// (retried with backoff; the OMS stays closed until it completes), then the
// user-data stream with listen-key keepalive, silence detection, periodic
// reconcile, and reconnect + NEW key + full reconcile on any stream loss.
func (o *OMS) Run(ctx context.Context) error {
	backoff := time.Second
	for ctx.Err() == nil {
		if o.Reconciled() {
			break
		}
		if err := o.runInternal(ctx); err != nil {
			o.logf("live: startup reconcile failed: %v", err)
		}
		if !o.Reconciled() {
			o.sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
	for ctx.Err() == nil {
		reason, err := o.streamOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			o.logf("live: stream ended (%s): %v", reason, err)
		}
		ev := o.event("stream_reconnect", map[string]any{"reason": reason})
		if aerr := o.st.AppendOMSReconEvent(ev); aerr != nil {
			o.logf("live: stream_reconnect append: %v", aerr)
		}
		if rerr := o.runInternal(ctx); rerr != nil {
			o.logf("live: post-reconnect reconcile failed: %v", rerr)
		}
		o.sleep(time.Second)
	}
	return ctx.Err()
}

// streamOnce runs one stream session and returns the reconnect reason: WS
// error/close, listenKeyExpired, or SILENCE — no received frame of ANY kind
// for ws_silence_timeout_seconds.
func (o *OMS) streamOnce(ctx context.Context) (string, error) {
	key, err := o.ex.NewListenKey(ctx)
	if err != nil {
		return "listen_key_error", err
	}
	ch, err := o.ex.StreamUserData(ctx, key)
	if err != nil {
		return "stream_connect_error", err
	}
	keepalive := time.NewTicker(o.tuning.keepaliveInterval())
	defer keepalive.Stop()
	periodic := time.NewTicker(o.tuning.reconcileInterval())
	defer periodic.Stop()
	silence := time.NewTimer(o.tuning.silenceTimeout())
	defer silence.Stop()
	for {
		select {
		case <-ctx.Done():
			return "shutdown", ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return "stream_closed", nil
			}
			if !silence.Stop() {
				select {
				case <-silence.C:
				default:
				}
			}
			silence.Reset(o.tuning.silenceTimeout())
			if ev.Kind == exchange.UserEventListenKeyExpired {
				return "listen_key_expired", nil
			}
			if err := o.handleUserEvent(ev); err != nil {
				o.logf("live: user event: %v", err)
			}
		case <-keepalive.C:
			if err := o.ex.KeepAliveListenKey(ctx, key); err != nil {
				return "keepalive_failed", err
			}
		case <-silence.C:
			return "silence_timeout", nil
		case <-periodic.C:
			if err := o.runInternal(ctx); err != nil {
				o.logf("live: periodic reconcile failed: %v", err)
			}
		}
	}
}

// handleUserEvent applies one executionReport: FSM advance, and TRADE
// executions booked through the SAME deduped path as R5 (stream and
// backfill converge on the same rows, so replays are no-ops). The stream
// is a latency optimization, never a source of correctness.
func (o *OMS) handleUserEvent(ev exchange.UserEvent) error {
	if ev.Kind != exchange.UserEventExecutionReport {
		return nil
	}
	if !inNamespace(ev.ClientOrderID) {
		return nil // out-of-namespace events are ignored
	}
	var target fillTarget
	dup := false
	ord, err := o.st.GetLiveOrderByClientOrderID(ev.ClientOrderID)
	switch {
	case err == nil:
		if ord.ExchangeOrderID == nil && ev.ExchangeOrderID > 0 {
			if err := o.st.RecordExchangeAck(ord.OrderID,
				formatInt(ev.ExchangeOrderID)); err != nil {
				return err
			}
		}
		target = orderTarget(ord)
	case errors.Is(err, store.ErrNotFound):
		// In-namespace id with no local row: step-8 intent attribution;
		// fills are real and ours, the open-order handling belongs to R3.
		intent, ierr := o.st.GetOrderIntent(ev.ClientOrderID)
		if errors.Is(ierr, store.ErrNotFound) {
			return nil // unattributable: the reconcile sweep owns it
		}
		if ierr != nil {
			return ierr
		}
		target = intentTarget(intent)
		dup = true
	default:
		return err
	}
	if ev.ExecType != "TRADE" || ev.TradeID <= 0 {
		if dup {
			return nil
		}
		_, err := o.advanceStatus(ord, ev.OrderStatus, nil)
		return err
	}
	return o.bookStreamFill(ev, target, dup)
}

// bookStreamFill books one stream TRADE execution and applies the
// trade-id-discontinuity venue-reset detection: a trade id AT OR BELOW the
// current epoch's watermark that was nonetheless ABSENT locally means the
// venue restarted its trade ids (§Venue epochs).
func (o *OMS) bookStreamFill(ev exchange.UserEvent, target fillTarget, dup bool) error {
	wm, wmOK, err := o.st.FillWatermark(o.currentEpoch(), ev.VenueSymbol)
	if err != nil {
		return err
	}
	inserted, err := o.bookVenueFill(venueFill{
		target: target, venueSymbol: ev.VenueSymbol, tradeID: ev.TradeID,
		qty: ev.LastQty, price: ev.LastPrice,
		commission: ev.Commission, commissionAsset: ev.CommissionAsset,
		ts: ev.EventTime, venueOrderStatus: ev.OrderStatus,
	}, nil)
	if err != nil || !inserted {
		return err
	}
	if wmOK && ev.TradeID <= wm {
		if err := o.flagVenueReset(nil, nil); err != nil {
			return err
		}
	}
	if dup {
		return o.appendDuplicateExposure(target, ev.ClientOrderID, nil)
	}
	if o.Reconciled() {
		// Placement on fill: every entry fill event drives the protective
		// placement/resize immediately (§Protective order lifecycle).
		if err := o.driveProtectives(context.Background()); err != nil {
			o.logf("live: protective drive: %v", err)
		}
	}
	return nil
}

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }
