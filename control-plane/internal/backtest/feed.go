package backtest

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
)

// The backtest deliberately does NOT implement the marketdata.Feed channel
// shape: close_time(t) == open_time(t+1), so a time-threshold drain (the e2e
// tickPump) would pull candle t+1's open sub-tick into candle t's decision.
// Instead the replay loop pumps each candle's sub-ticks synchronously before
// its decision tick (backtest-engine.md §Clock model).

// SubTick is one intra-candle mark write: a marketdata.Tick plus the OHLC
// leg it represents (the leg names the deterministic tick-fill order ids).
type SubTick struct {
	Leg  string // "open" | "low" | "high" | "close"
	Tick marketdata.Tick
}

// SubTicks expands one candle into the four pinned mark writes of
// backtest-engine.md §Clock model. Order: bullish (close >= open)
// open -> low -> high -> close; bearish open -> high -> low -> close.
// Timestamps advance monotonically within the candle: open_time,
// open_time + ¼d, open_time + ½d, close_time (= open_time + d).
func SubTicks(symbol string, k Kline, intervalMS int64) ([4]SubTick, error) {
	var out [4]SubTick
	prices := make(map[string]decimal.Decimal, 4)
	for _, f := range []struct{ name, v string }{
		{"open", k.Open}, {"high", k.High}, {"low", k.Low}, {"close", k.Close},
	} {
		d, err := decimal.NewFromString(f.v)
		if err != nil {
			return out, fmt.Errorf("kline %d %s %q: %w", k.OpenTime, f.name, f.v, err)
		}
		prices[f.name] = d
	}
	o, h, l, c := prices["open"], prices["high"], prices["low"], prices["close"]
	if l.GreaterThan(o) || l.GreaterThan(c) || h.LessThan(o) || h.LessThan(c) {
		return out, fmt.Errorf("kline %d: OHLC violates low <= open,close <= high (o=%s h=%s l=%s c=%s)",
			k.OpenTime, k.Open, k.High, k.Low, k.Close)
	}

	legs := [4]string{"open", "low", "high", "close"}
	if c.LessThan(o) { // bearish
		legs = [4]string{"open", "high", "low", "close"}
	}
	offsets := [4]int64{0, intervalMS / 4, intervalMS / 2, intervalMS}
	for i, leg := range legs {
		out[i] = SubTick{
			Leg: leg,
			Tick: marketdata.Tick{
				Symbol: symbol,
				Mark:   prices[leg],
				TS:     time.UnixMilli(k.OpenTime + offsets[i]).UTC(),
			},
		}
	}
	return out, nil
}
