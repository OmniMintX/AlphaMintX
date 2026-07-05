package papergate

import "testing"

// TestThirtyTinyTradesFailAvgNotional pins LC-20: thirty $1 round trips
// pass the trade count but fail min_avg_notional against 0.25 x 2000.
func TestThirtyTinyTradesFailAvgNotional(t *testing.T) {
	var fills []Fill
	for i := 0; i < 30; i++ {
		fills = append(fills, roundTrip("BTC/USDT", "0.001", "1000", "1000", "0")...)
	}
	rep := Evaluate(baseInput(fills))
	if rep.Passed {
		t.Fatal("report passed; want min_avg_notional to fail")
	}
	if c := cond(t, rep, CondMinClosedTrades); !c.Passed {
		t.Errorf("min_closed_trades = %+v, want pass", c)
	}
	c := cond(t, rep, CondMinAvgNotional)
	if c.Passed || c.Measured != "1" || c.Required != "500" {
		t.Errorf("min_avg_notional = %+v, want fail 1/500", c)
	}
}

// TestDrawdownBreachFails pins LC-21: a 15% drawdown against a 10% limit
// fails with the measured percentage rendered.
func TestDrawdownBreachFails(t *testing.T) {
	fills := append(winningTrips(30), roundTrip("ETH/USDT", "1", "2000", "500", "0")...)
	c := cond(t, Evaluate(baseInput(fills)), CondMaxDrawdown)
	if c.Passed || c.Required != "10" {
		t.Errorf("max_drawdown = %+v, want fail against 10", c)
	}
	// Peak includes the 300 profit of the 30 winning trips: equity peaks
	// at 10300, drops 1500 on the loser => 1500/10300 x 100.
	want := dec("1500").Div(dec("10300")).Mul(dec("100")).String()
	if c.Measured != want {
		t.Errorf("measured = %q, want %q", c.Measured, want)
	}
}

// TestDrawdownEdges pins LC-21/LC-23: a non-positive seed fails; zero
// closed trades render measured "0" and fail even with fills in flight.
func TestDrawdownEdges(t *testing.T) {
	in := baseInput(winningTrips(30))
	in.Seed = dec("0")
	if c := cond(t, Evaluate(in), CondMaxDrawdown); c.Passed || c.Measured != "0" {
		t.Errorf("zero seed: %+v, want fail measured 0", c)
	}

	openOnly := baseInput([]Fill{{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("5")}})
	rep := Evaluate(openOnly)
	for _, name := range []string{CondMinAvgNotional, CondMaxDrawdown, CondProfitFactor} {
		if c := cond(t, rep, name); c.Passed || c.Measured != "0" {
			t.Errorf("%s with zero closed trades = %+v, want fail measured 0", name, c)
		}
	}
}

// TestProfitFactorEdges pins LC-22: gl=0/gp>0 passes; both-zero fails;
// otherwise the gp/gl ratio decides at 1.0.
func TestProfitFactorEdges(t *testing.T) {
	// All winners: gl = 0, gp = 300 — pass, measured renders gp.
	c := cond(t, Evaluate(baseInput(winningTrips(30))), CondProfitFactor)
	if !c.Passed || c.Measured != "300" || c.Required != "1" {
		t.Errorf("gl=0 gp>0: %+v, want pass 300/1", c)
	}

	// Thirty flat trades: both zero — fail.
	var flat []Fill
	for i := 0; i < 30; i++ {
		flat = append(flat, roundTrip("BTC/USDT", "1", "1000", "1000", "0")...)
	}
	if c := cond(t, Evaluate(baseInput(flat)), CondProfitFactor); c.Passed || c.Measured != "0" {
		t.Errorf("both zero: %+v, want fail measured 0", c)
	}

	// One +10 win and one -5 loss: pf 2 passes.
	fills := append(winningTrips(1), roundTrip("ETH/USDT", "1", "1000", "995", "0")...)
	if c := cond(t, Evaluate(baseInput(fills)), CondProfitFactor); !c.Passed || c.Measured != "2" {
		t.Errorf("pf 2: %+v, want pass measured 2", c)
	}

	// One +5 win and one -10 loss: pf 0.5 fails.
	fills = append(roundTrip("BTC/USDT", "1", "1000", "1005", "0"), roundTrip("ETH/USDT", "1", "1000", "990", "0")...)
	if c := cond(t, Evaluate(baseInput(fills)), CondProfitFactor); c.Passed || c.Measured != "0.5" {
		t.Errorf("pf 0.5: %+v, want fail measured 0.5", c)
	}
}

// TestReplayMath pins LC-18: weighted-average increases, fee-inclusive
// trade PnL, short spans, and the sign-flip split (only the reducing
// portion counts toward the closing span).
func TestReplayMath(t *testing.T) {
	// Weighted average: 1@1000 + 1@1100, close 2@1080 with 6 total fees.
	fills := []Fill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("2")},
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1100"), FeeQuote: dec("2")},
		{Symbol: "BTC/USDT", Side: "sell", QtyBase: dec("2"), FillPrice: dec("1080"), FeeQuote: dec("2")},
	}
	trades, _ := replay(fills, dec("10000"))
	if len(trades) != 1 || !trades[0].pnl.Equal(dec("54")) || !trades[0].closingNotional.Equal(dec("2160")) {
		t.Fatalf("weighted-average trade = %+v, want pnl 54 notional 2160", trades)
	}

	// Short round trip: sell 1@1000, cover 1@990 => +10.
	trades, _ = replay([]Fill{
		{Symbol: "BTC/USDT", Side: "sell", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("990"), FeeQuote: dec("0")},
	}, dec("10000"))
	if len(trades) != 1 || !trades[0].pnl.Equal(dec("10")) {
		t.Fatalf("short trade = %+v, want pnl 10", trades)
	}

	// Flip: long 1@1000; sell 3@1010 (reduce 1, open short 2); cover
	// 2@1005. Span 1: +10 on notional 1010; span 2: +10 on 2010.
	trades, _ = replay([]Fill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "sell", QtyBase: dec("3"), FillPrice: dec("1010"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("2"), FillPrice: dec("1005"), FeeQuote: dec("0")},
	}, dec("10000"))
	if len(trades) != 2 {
		t.Fatalf("flip trades = %+v, want 2 spans", trades)
	}
	if !trades[0].pnl.Equal(dec("10")) || !trades[0].closingNotional.Equal(dec("1010")) {
		t.Errorf("flip span 1 = %+v, want pnl 10 notional 1010", trades[0])
	}
	if !trades[1].pnl.Equal(dec("10")) || !trades[1].closingNotional.Equal(dec("2010")) {
		t.Errorf("flip span 2 = %+v, want pnl 10 notional 2010", trades[1])
	}
}

// TestReduceOnlyClamp pins the LC-18 reduce-only clamp: a reduce-only
// fill is capped at the open opposite-side quantity — it closes the span
// without flipping, and the NEXT same-symbol entry opens a fresh book
// instead of covering a phantom short.
func TestReduceOnlyClamp(t *testing.T) {
	trades, _ := replay([]Fill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "sell", ReduceOnly: true, QtyBase: dec("2"), FillPrice: dec("1010"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("990"), FeeQuote: dec("0")},
	}, dec("10000"))
	if len(trades) != 1 {
		t.Fatalf("trades = %+v, want exactly 1 (no phantom short span)", trades)
	}
	if !trades[0].pnl.Equal(dec("10")) || !trades[0].closingNotional.Equal(dec("1010")) {
		t.Errorf("clamped trade = %+v, want pnl 10 notional 1010 (1 of the 2 sold)", trades[0])
	}
}

// TestReduceOnlyFlatAndSameSide pins the LC-18 clamp edges: a reduce-only
// fill against a flat or same-side book replays only its fee debit — no
// position, no trade, the fee still hits the equity curve.
func TestReduceOnlyFlatAndSameSide(t *testing.T) {
	// Flat book: the reduce-only sell must NOT open a short, so the later
	// buy opens a dangling long — zero closed trades; the 100 fee alone
	// draws the 10000 seed down 1%.
	trades, maxDD := replay([]Fill{
		{Symbol: "BTC/USDT", Side: "sell", ReduceOnly: true, QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("100")},
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("990"), FeeQuote: dec("0")},
	}, dec("10000"))
	if len(trades) != 0 {
		t.Errorf("flat-book trades = %+v, want none", trades)
	}
	if !maxDD.Equal(dec("1")) {
		t.Errorf("maxDD = %s, want 1 (fee-only debit)", maxDD)
	}

	// Same-side book: a reduce-only buy over a long never increases it.
	trades, _ = replay([]Fill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "buy", ReduceOnly: true, QtyBase: dec("1"), FillPrice: dec("1100"), FeeQuote: dec("0")},
		{Symbol: "BTC/USDT", Side: "sell", QtyBase: dec("1"), FillPrice: dec("1050"), FeeQuote: dec("0")},
	}, dec("10000"))
	if len(trades) != 1 || !trades[0].pnl.Equal(dec("50")) {
		t.Fatalf("same-side trades = %+v, want one +50 trade on the 1@1000 book", trades)
	}
}

// TestLimitsAbsent pins LC-23: with no limits provider the limit-bound
// conditions render required "0" and fail while the rest still compute.
func TestLimitsAbsent(t *testing.T) {
	in := baseInput(winningTrips(30))
	in.LimitsOK = false
	rep := Evaluate(in)
	for _, name := range []string{CondMinAvgNotional, CondMaxDrawdown} {
		if c := cond(t, rep, name); c.Passed || c.Required != "0" {
			t.Errorf("%s without limits = %+v, want fail required 0", name, c)
		}
	}
	if c := cond(t, rep, CondMinClosedTrades); !c.Passed {
		t.Errorf("min_closed_trades = %+v, want pass (limit-independent)", c)
	}
	if c := cond(t, rep, CondProfitFactor); !c.Passed {
		t.Errorf("profit_factor = %+v, want pass (limit-independent)", c)
	}
}
