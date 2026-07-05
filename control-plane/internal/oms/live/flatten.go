package live

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
)

// Flatten market-closes the (strategy, symbol) position through the FULL
// journal path (safety-engine integration). Binance spot has NO reduceOnly
// flag: reduce_only=1 is a LOCAL intent marker enforced by sizing. A LONG
// close (sell) is bounded by the venue free BASE balance — flatten qty =
// min(local fill-derived position, venue free), quantized DOWN to
// stepSize; a short venue balance flattens what is available
// (flatten_short_balance + alert, never overselling). A SHORT close (buy)
// buys back the full local position: the base balance never bounds a buy.
// Dust below minQty, or below the venue minNotional at a fresh mark,
// cannot be flattened on spot (flatten_dust + alert).
func (o *OMS) Flatten(ctx context.Context, strategyID, symbol, origin string, proposalID *string) error {
	_, err := o.flattenWithOutcome(ctx, strategyID, symbol, origin, proposalID)
	return err
}

// flattenOutcome classifies the path one flatten call took, so the safety
// driver labels terminal residue from the call's OWN outcome instead of
// diffing audit tables (safety-wiring.md §Driver step 4 carve-outs).
type flattenOutcome int

const (
	// flattenNoop: an error return — nothing was journaled or evented.
	flattenNoop flattenOutcome = iota
	// flattenJournaled: a reduce-only market close (full or venue-bounded)
	// was journaled and sent; its completion is owned by the
	// journal/Reconciler/fill machinery.
	flattenJournaled
	// flattenDust: the remainder is below minQty/minNotional — the
	// flatten_dust journal path, NO order sent.
	flattenDust
	// flattenShortBalanceBounded: a sell-side venue-balance shortfall was
	// alerted (flatten_short_balance) and what remains after the bound is
	// dust — NO order sent.
	flattenShortBalanceBounded
)

// flattenWithOutcome is Flatten exposing which internal path ran.
func (o *OMS) flattenWithOutcome(ctx context.Context, strategyID, symbol, origin string, proposalID *string) (flattenOutcome, error) {
	if err := o.preflightGate(strategyID, nil); err != nil {
		return flattenNoop, err
	}
	venueSym, ok := o.venueOf[symbol]
	if !ok {
		return flattenNoop, fmt.Errorf("live: symbol %s is not configured", symbol)
	}
	sf, err := o.symbolFiltersFor(venueSym)
	if err != nil {
		return flattenNoop, err
	}
	pos, err := o.positionQty(strategyID, symbol)
	if err != nil {
		return flattenNoop, err
	}
	if pos.IsZero() {
		return flattenNoop, fmt.Errorf("live: no open position for %s %s", strategyID, symbol)
	}
	side := "sell"
	if pos.Sign() < 0 {
		side = "buy"
	}
	want := pos.Abs()
	avail := want
	shortBalance := false
	if side == "sell" {
		base, _, err := splitSymbol(symbol)
		if err != nil {
			return flattenNoop, err
		}
		free, err := o.freeBalance(ctx, base)
		if err != nil {
			return flattenNoop, err
		}
		avail = decimal.Min(want, free)
		if free.LessThan(want) {
			shortBalance = true
			ev := o.event("flatten_short_balance", map[string]any{
				"local_position": want.String(), "venue_free": free.String(),
			})
			ev.StrategyID, ev.Symbol = &strategyID, &symbol
			o.logf("live: ALERT flatten short balance for %s %s (local %s, venue free %s)",
				strategyID, symbol, want, free)
			if err := o.st.AppendOMSReconEvent(ev); err != nil {
				return flattenNoop, err
			}
		}
	}
	qty := floorToStep(avail, sf.step)
	dust := qty.Sign() <= 0 || qty.LessThan(sf.minQty)
	if !dust && sf.minNotional.Sign() > 0 {
		// The market flatten must also clear the venue notional minimum;
		// the fresh mark is the reference (no fresh mark: send anyway and
		// let the venue arbitrate).
		if mark, _, ok := o.marks.Mark(symbol, o.now()); ok && qty.Mul(mark).LessThan(sf.minNotional) {
			dust = true
		}
	}
	if dust {
		ev := o.event("flatten_dust", map[string]any{
			"remaining": avail.String(), "min_qty": sf.minQty.String(),
			"min_notional": sf.minNotional.String(),
		})
		ev.StrategyID, ev.Symbol = &strategyID, &symbol
		o.logf("live: ALERT flatten dust for %s %s (remaining %s below minQty %s or minNotional %s)",
			strategyID, symbol, avail, sf.minQty, sf.minNotional)
		if err := o.st.AppendOMSReconEvent(ev); err != nil {
			return flattenNoop, err
		}
		if shortBalance {
			return flattenShortBalanceBounded, nil
		}
		return flattenDust, nil
	}
	epoch, err := o.st.GlobalMaxKillEpoch(strategyID)
	if err != nil {
		return flattenNoop, err
	}
	if err := o.journalAndSend(ctx, submission{
		strategyID: strategyID, symbol: symbol, class: "PROTECTIVE",
		side: side, typ: "market", origin: origin, reduceOnly: true,
		proposalID: proposalID, killEpoch: epoch, qty: qty,
	}); err != nil {
		return flattenNoop, err
	}
	return flattenJournaled, nil
}

// freeBalance returns one asset's venue free balance (zero when absent).
func (o *OMS) freeBalance(ctx context.Context, asset string) (decimal.Decimal, error) {
	balances, err := o.ex.Balances(ctx)
	if err != nil {
		return decimal.Decimal{}, err
	}
	for _, b := range balances {
		if b.Asset == asset {
			return parseDec("balance.free", b.Free)
		}
	}
	return decimal.Zero, nil
}
