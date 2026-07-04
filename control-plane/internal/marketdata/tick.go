// Package marketdata provides mark-price feeds (live Binance and offline
// replay) plus the last-tick Store consumed at proposal evaluation, per
// docs/specs/market-data.md. The package is fail-closed: no fresh mark
// means no price, never a guess.
package marketdata

import (
	"context"
	"time"

	"github.com/shopspring/decimal"
)

// Tick is one mark-price observation for a canonical BASE/QUOTE symbol.
type Tick struct {
	Symbol string          // canonical BASE/QUOTE, e.g. "BTC/USDT"
	Mark   decimal.Decimal // mark price (futures) or last trade (spot)
	Last   decimal.Decimal
	TS     time.Time // exchange event time (live) / tick time (replay)
}

// Feed streams ticks for subscribed canonical symbols.
type Feed interface {
	// Subscribe streams ticks for the given canonical symbols until ctx is
	// done. The channel is closed on ctx cancellation or fatal error.
	Subscribe(ctx context.Context, symbols []string) (<-chan Tick, error)
}
