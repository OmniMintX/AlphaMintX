package papergate

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
)

// stamped returns fills with deterministic ascending FillTS values.
func stamped(fills []Fill) []Fill {
	out := make([]Fill, len(fills))
	for i, f := range fills {
		f.FillTS = fmt.Sprintf("2026-06-%02dT00:00:00Z", i+1)
		out[i] = f
	}
	return out
}

// TestReplayCurveMatchesReplay pins the shared-walk invariant: ReplayCurve
// and the gate's replay produce identical closed trades and max drawdown,
// the curve has one post-fill point per fill, and the final equity equals
// seed + realized PnL.
func TestReplayCurveMatchesReplay(t *testing.T) {
	fills := stamped(append(winningTrips(3), roundTrip("ETH/USDT", "1", "2000", "1500", "5")...))
	seed := dec("10000")

	trades, maxDD := replay(fills, seed)
	curve, stats := ReplayCurve(fills, seed)

	if stats.ClosedTrades != len(trades) {
		t.Errorf("ClosedTrades = %d, want %d", stats.ClosedTrades, len(trades))
	}
	if !stats.MaxDrawdownPct.Equal(maxDD) {
		t.Errorf("MaxDrawdownPct = %s, want %s", stats.MaxDrawdownPct, maxDD)
	}
	if len(curve) != len(fills) {
		t.Fatalf("curve length = %d, want %d", len(curve), len(fills))
	}
	for i, p := range curve {
		if p.TS != fills[i].FillTS {
			t.Errorf("curve[%d].TS = %q, want %q", i, p.TS, fills[i].FillTS)
		}
	}
	final := curve[len(curve)-1].Equity
	if !final.Equal(seed.Add(stats.RealizedPnL)) {
		t.Errorf("final equity = %s, want seed+pnl %s", final, seed.Add(stats.RealizedPnL))
	}
}

// TestReplayCurveStats pins the aggregate math: pnl net of fees, fee sum,
// win/loss split, win rate, profit factor, and last fill timestamp.
func TestReplayCurveStats(t *testing.T) {
	// +10 BTC win (fee 1 per side => trade pnl +8), -10 ETH loss (no fee).
	fills := stamped(append(roundTrip("BTC/USDT", "1", "1000", "1010", "1"),
		roundTrip("ETH/USDT", "1", "1000", "990", "0")...))
	_, stats := ReplayCurve(fills, dec("10000"))

	if !stats.RealizedPnL.Equal(dec("-2")) {
		t.Errorf("RealizedPnL = %s, want -2", stats.RealizedPnL)
	}
	if !stats.FeesPaid.Equal(dec("2")) {
		t.Errorf("FeesPaid = %s, want 2", stats.FeesPaid)
	}
	if stats.ClosedTrades != 2 || stats.Wins != 1 || stats.Losses != 1 {
		t.Errorf("trades/wins/losses = %d/%d/%d, want 2/1/1", stats.ClosedTrades, stats.Wins, stats.Losses)
	}
	if !stats.WinRatePct.Equal(dec("50")) {
		t.Errorf("WinRatePct = %s, want 50", stats.WinRatePct)
	}
	if stats.ProfitFactor == nil || !stats.ProfitFactor.Equal(dec("0.8")) {
		t.Errorf("ProfitFactor = %v, want 0.8", stats.ProfitFactor)
	}
	if stats.LastFillAt != fills[len(fills)-1].FillTS {
		t.Errorf("LastFillAt = %q, want %q", stats.LastFillAt, fills[len(fills)-1].FillTS)
	}
}

// TestReplayCurveEdges pins the no-fill and no-loss edges: an empty window
// yields an empty curve and zero stats with a nil profit factor; all
// winners keep the factor nil (unbounded, LC-22 analogue) with a 100% win
// rate; a flat trade counts as neither win nor loss.
func TestReplayCurveEdges(t *testing.T) {
	curve, stats := ReplayCurve(nil, dec("10000"))
	if len(curve) != 0 || stats.ClosedTrades != 0 || stats.ProfitFactor != nil ||
		!stats.RealizedPnL.IsZero() || !stats.WinRatePct.IsZero() || stats.LastFillAt != "" {
		t.Errorf("empty replay = %d points, stats %+v; want all-zero", len(curve), stats)
	}

	_, stats = ReplayCurve(stamped(winningTrips(2)), dec("10000"))
	if stats.ProfitFactor != nil {
		t.Errorf("all-winners ProfitFactor = %s, want nil", stats.ProfitFactor)
	}
	if !stats.WinRatePct.Equal(dec("100")) || stats.Wins != 2 || stats.Losses != 0 {
		t.Errorf("all-winners = %+v, want 100%% win rate", stats)
	}

	_, stats = ReplayCurve(stamped(roundTrip("BTC/USDT", "1", "1000", "1000", "0")), dec("10000"))
	if stats.ClosedTrades != 1 || stats.Wins != 0 || stats.Losses != 0 || !stats.WinRatePct.IsZero() {
		t.Errorf("flat trade = %+v, want 1 closed, 0 wins, 0 losses", stats)
	}
}

// TestReplayCurveEquityProgression pins the per-point equity: each sample
// is the running equity AFTER that fill's realized delta and fee debit.
func TestReplayCurveEquityProgression(t *testing.T) {
	fills := stamped([]Fill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("1")},
		{Symbol: "BTC/USDT", Side: "sell", QtyBase: dec("1"), FillPrice: dec("1010"), FeeQuote: dec("1")},
	})
	curve, _ := ReplayCurve(fills, dec("10000"))
	want := []decimal.Decimal{dec("9999"), dec("10008")}
	for i, p := range curve {
		if !p.Equity.Equal(want[i]) {
			t.Errorf("curve[%d].Equity = %s, want %s", i, p.Equity, want[i])
		}
	}
}
