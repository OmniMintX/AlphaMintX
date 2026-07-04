package omsbridge

import (
	"context"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
)

// RunFeedWriter is the serve-mode market-data writer goroutine: it drains a
// Feed subscription into the last-tick Store and fires the OMS trigger
// sweep after EVERY Store write (market-data.md §Fill model v2 — trigger
// checks are tick-granular; a connected-but-silent feed keeps nothing
// alive, the Store staleness rule still applies at read time). sweep may be
// nil in deployments with no OMS wired; sweep errors are logged and never
// stop the writer (a bad tick must not tear down the feed). Returns when
// ctx is done or the feed channel closes.
func RunFeedWriter(ctx context.Context, feed marketdata.Feed, symbols []string, marks *marketdata.Store, sweep func(map[string]decimal.Decimal) error, logf func(format string, args ...any)) error {
	ch, err := feed.Subscribe(ctx, symbols)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case tick, ok := <-ch:
			if !ok {
				return nil
			}
			marks.Put(tick)
			if sweep == nil {
				continue
			}
			if err := sweep(map[string]decimal.Decimal{tick.Symbol: tick.Mark}); err != nil && logf != nil {
				logf("omsbridge: sweep %s: %v", tick.Symbol, err)
			}
		}
	}
}
