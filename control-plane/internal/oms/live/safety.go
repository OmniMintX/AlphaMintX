package live

import (
	"context"
	"errors"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// CancelOpenEntries cancels every non-terminal ENTRY-class order — the
// kill/breaker/watchdog cancel sweep (risk-limits.md: ENTRY only,
// PROTECTIVE stops are never touched while a position is open). An empty
// strategyID sweeps ALL strategies. NotFound is success (already gone); a
// claimed-but-unsent pending_new intent has its claim REVOKED first so the
// send cannot follow the cancel (§In-flight exclusion); an ambiguous cancel
// is left for the next pass — the caller's persisted kill_breaker_events
// intent makes the sweep resumable.
func (o *OMS) CancelOpenEntries(ctx context.Context, strategyID string) error {
	orders, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		return err
	}
	for _, ord := range orders {
		if ord.Class != "ENTRY" || (strategyID != "" && ord.StrategyID != strategyID) {
			continue
		}
		if ord.ClientOrderID == nil {
			continue
		}
		if ord.Status == "pending_new" {
			if err := o.st.RecordIntentClaimRevoked(*ord.ClientOrderID, formatTime(o.now())); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		venueSym, ok := o.venueOf[ord.Symbol]
		if !ok {
			continue // unconfigured symbol: nothing to cancel at the venue
		}
		if _, cerr := o.ex.CancelOrder(ctx, venueSym, *ord.ClientOrderID); cerr != nil &&
			exchange.Classify(cerr) != exchange.ClassNotFound {
			o.logf("live: entry cancel %s ambiguous (next reconcile pass retries): %v",
				*ord.ClientOrderID, cerr)
			continue
		}
		if _, err := o.st.RecordOrderStatus(ord.OrderID, "canceled"); err != nil {
			return err
		}
	}
	return nil
}

// FlattenAll market-closes every open position of the strategy through the
// FULL journal path (§Safety-engine integration): origin is 'kill',
// 'breaker', or 'watchdog' per the calling engine.
func (o *OMS) FlattenAll(ctx context.Context, strategyID, origin string) error {
	positions, err := o.st.ListPositions(strategyID)
	if err != nil {
		return err
	}
	for _, p := range positions {
		qty, err := parseDec("positions.qty_base", p.QtyBase)
		if err != nil {
			return err
		}
		if qty.IsZero() {
			continue
		}
		if err := o.Flatten(ctx, strategyID, p.Symbol, origin, nil); err != nil {
			return err
		}
	}
	return nil
}
