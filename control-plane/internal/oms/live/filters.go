package live

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

// symbolFilters are one venue symbol's parsed trading filters
// (PRICE_FILTER tickSize, LOT_SIZE stepSize/minQty/maxQty, min notional).
type symbolFilters struct {
	tick, step, minQty, maxQty, minNotional decimal.Decimal
}

// loadFilters fetches and parses exchangeInfo for every configured symbol
// (Reconciler R1); on success the refresh clock restarts and any
// filter-violation staleness flag clears.
func (o *OMS) loadFilters(ctx context.Context) error {
	venueSymbols := make([]string, 0, len(o.symbols))
	for _, sym := range o.symbols {
		venueSymbols = append(venueSymbols, o.venueOf[sym])
	}
	raw, err := o.ex.ExchangeInfo(ctx, venueSymbols)
	if err != nil {
		return err
	}
	parsed := make(map[string]symbolFilters, len(raw))
	for venue, f := range raw {
		sf, err := parseFilters(venue, f)
		if err != nil {
			return err
		}
		parsed[venue] = sf
	}
	now := o.now()
	o.mu.Lock()
	o.filters = parsed
	o.filtersAt = now
	o.filtersStale = false
	o.mu.Unlock()
	return nil
}

func parseFilters(venue string, f exchange.SymbolFilters) (symbolFilters, error) {
	var sf symbolFilters
	for _, field := range []struct {
		name string
		raw  string
		dst  *decimal.Decimal
	}{
		{"tickSize", f.TickSize, &sf.tick},
		{"stepSize", f.StepSize, &sf.step},
		{"minQty", f.MinQty, &sf.minQty},
		{"maxQty", f.MaxQty, &sf.maxQty},
		{"minNotional", f.MinNotional, &sf.minNotional},
	} {
		d, err := parseDec("exchangeInfo "+venue+" "+field.name, field.raw)
		if err != nil {
			return symbolFilters{}, err
		}
		*field.dst = d
	}
	if sf.tick.Sign() <= 0 || sf.step.Sign() <= 0 {
		return symbolFilters{}, fmt.Errorf("live: exchangeInfo %s: tickSize and stepSize must be > 0", venue)
	}
	return sf, nil
}

// filtersDue reports whether the loaded filters are absent or expired;
// callers hold o.mu.
func (o *OMS) filtersDueLocked() bool {
	return o.filters == nil || o.now().Sub(o.filtersAt) >= o.tuning.filterRefresh()
}

// symbolFiltersFor returns the fresh filters for one venue symbol, or
// ErrFilterUnavailable (fail closed: never trade unfiltered).
func (o *OMS) symbolFiltersFor(venueSymbol string) (symbolFilters, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.filtersDueLocked() {
		return symbolFilters{}, ErrFilterUnavailable
	}
	sf, ok := o.filters[venueSymbol]
	if !ok {
		return symbolFilters{}, ErrFilterUnavailable
	}
	return sf, nil
}

// MinFilters is the safety.FiltersProvider seam (watchdog.md WD-20): one
// canonical symbol's venue minimums for the watchdog's dust carve-out.
// ok=false for an unconfigured symbol or when filters are unloaded or
// expired (the ErrFilterUnavailable condition) — the watchdog EXCLUDES
// the position, failing toward PROTECTED.
func (o *OMS) MinFilters(symbol string) (minQty, minNotional decimal.Decimal, ok bool) {
	venueSymbol, found := o.venueOf[symbol]
	if !found {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	sf, err := o.symbolFiltersFor(venueSymbol)
	if err != nil {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	return sf.minQty, sf.minNotional, true
}

// floorToStep rounds d DOWN to a multiple of step (step > 0).
func floorToStep(d, step decimal.Decimal) decimal.Decimal {
	return d.Div(step).Floor().Mul(step)
}

// ceilToStep rounds d UP to a multiple of step (step > 0).
func ceilToStep(d, step decimal.Decimal) decimal.Decimal {
	return d.Div(step).Ceil().Mul(step)
}

// passivePrice rounds a limit price to the nearest tick toward the passive
// side: buy limits DOWN, sell limits UP (spec §Filters).
func passivePrice(side string, price, tick decimal.Decimal) decimal.Decimal {
	if side == "buy" {
		return floorToStep(price, tick)
	}
	return ceilToStep(price, tick)
}

// normalizeEntryQty sizes an entry from its effective quote cap at the
// reference price: qty = cap/price rounded DOWN to stepSize, then shaved
// one step if rounding pushed qty*price above the cap (cap preservation).
// Post-rounding minima reject BELOW_MIN_NOTIONAL; maxQty rejects
// FILTER_REJECTED.
func normalizeEntryQty(sizeQuote, price decimal.Decimal, f symbolFilters) (decimal.Decimal, error) {
	if price.Sign() <= 0 {
		return decimal.Decimal{}, fmt.Errorf("live: non-positive reference price for sizing")
	}
	qty := floorToStep(sizeQuote.Div(price), f.step)
	if qty.Mul(price).GreaterThan(sizeQuote) {
		qty = qty.Sub(f.step)
	}
	if qty.Sign() <= 0 || qty.LessThan(f.minQty) || qty.Mul(price).LessThan(f.minNotional) {
		return decimal.Decimal{}, ErrBelowMinNotional
	}
	if f.maxQty.Sign() > 0 && qty.GreaterThan(f.maxQty) {
		return decimal.Decimal{}, ErrFilterRejected
	}
	return qty, nil
}
