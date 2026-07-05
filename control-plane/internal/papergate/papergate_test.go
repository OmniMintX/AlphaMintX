package papergate

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var (
	winStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	evalNow  = winStart.Add(20 * 24 * time.Hour) // 20 days in
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// baseInput is a passing configuration: cap 2000 (min avg notional 500),
// max drawdown 10%, seed 10000, 20-day-old window.
func baseInput(fills []Fill) Input {
	return Input{
		WindowOK: true, WindowStart: winStart, Now: evalNow, Fills: fills,
		LimitsOK: true, NotionalCap: dec("2000"), MaxDrawdownPct: dec("10"),
		Seed: dec("10000"),
	}
}

// roundTrip is one full long round trip on symbol: buy qty at entry, sell
// qty at exit, fee per side.
func roundTrip(symbol, qty, entry, exit, fee string) []Fill {
	return []Fill{
		{Symbol: symbol, Side: "buy", QtyBase: dec(qty), FillPrice: dec(entry), FeeQuote: dec(fee)},
		{Symbol: symbol, Side: "sell", QtyBase: dec(qty), FillPrice: dec(exit), FeeQuote: dec(fee)},
	}
}

// winningTrips returns n identical profitable round trips: 1 BTC 1000->1010,
// zero fees — trade PnL +10, closing notional 1010.
func winningTrips(n int) []Fill {
	var fills []Fill
	for i := 0; i < n; i++ {
		fills = append(fills, roundTrip("BTC/USDT", "1", "1000", "1010", "0")...)
	}
	return fills
}

func cond(t *testing.T, rep Report, name string) Condition {
	t.Helper()
	for _, c := range rep.Conditions {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("condition %q missing from report %+v", name, rep)
	return Condition{}
}

// TestReportShape pins LC-23: five conditions in order, decimal-string
// measured/required, window_started_at set, evaluated_at RFC 3339 Z.
func TestReportShape(t *testing.T) {
	rep := Evaluate(baseInput(winningTrips(30)))
	names := []string{"min_days", "min_closed_trades", "min_avg_notional", "max_drawdown", "profit_factor"}
	if len(rep.Conditions) != 5 {
		t.Fatalf("conditions = %d, want 5", len(rep.Conditions))
	}
	for i, c := range rep.Conditions {
		if c.Name != names[i] {
			t.Errorf("conditions[%d] = %q, want %q", i, c.Name, names[i])
		}
		if c.Measured == "" || c.Required == "" {
			t.Errorf("%s: measured/required empty: %+v", c.Name, c)
		}
	}
	if rep.WindowStartedAt == nil || *rep.WindowStartedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("window_started_at = %v, want 2026-06-01T00:00:00Z", rep.WindowStartedAt)
	}
	if rep.EvaluatedAt != "2026-06-21T00:00:00Z" {
		t.Errorf("evaluated_at = %q", rep.EvaluatedAt)
	}
	if !rep.Passed {
		t.Errorf("report = %+v, want passed", rep)
	}
}

// TestFailClosedWindow pins LC-16/LC-23: no qualifying window means every
// condition is unmet with measured "0" required "0" and a null window.
func TestFailClosedWindow(t *testing.T) {
	in := baseInput(winningTrips(30))
	in.WindowOK = false
	rep := Evaluate(in)
	if rep.Passed || rep.WindowStartedAt != nil {
		t.Fatalf("report = %+v, want failed with null window", rep)
	}
	for _, c := range rep.Conditions {
		if c.Passed || c.Measured != "0" || c.Required != "0" {
			t.Errorf("%s = %+v, want failed 0/0", c.Name, c)
		}
	}
}

// TestMinDays pins LC-17/LC-23: measured/required in decimal days, the
// 14-day boundary passes, a second under fails.
func TestMinDays(t *testing.T) {
	in := baseInput(winningTrips(30))
	in.Now = winStart.Add(minWindow)
	c := cond(t, Evaluate(in), CondMinDays)
	if !c.Passed || c.Measured != "14" || c.Required != "14" {
		t.Errorf("at exactly 14 d: %+v, want pass 14/14", c)
	}

	in.Now = winStart.Add(minWindow - time.Second)
	c = cond(t, Evaluate(in), CondMinDays)
	if c.Passed {
		t.Errorf("13d23h59m59s: %+v, want fail", c)
	}
	want := decimal.NewFromInt(14*86400 - 1).Div(decimal.NewFromInt(86400)).String()
	if c.Measured != want {
		t.Errorf("measured = %q, want %q (decimal days)", c.Measured, want)
	}
}

// TestMinDaysClockRollback pins the LC-23 rollback edge: Now BEFORE the
// window start clamps measured at "0" — never a negative decimal string —
// and the condition fails.
func TestMinDaysClockRollback(t *testing.T) {
	in := baseInput(winningTrips(30))
	in.Now = winStart.Add(-time.Hour)
	c := cond(t, Evaluate(in), CondMinDays)
	if c.Passed || c.Measured != "0" || c.Required != "14" {
		t.Errorf("rolled-back clock: %+v, want fail 0/14", c)
	}
}

// TestMinClosedTrades pins LC-19: 29 trades fail, 30 pass; an open span is
// not a closed trade.
func TestMinClosedTrades(t *testing.T) {
	c := cond(t, Evaluate(baseInput(winningTrips(29))), CondMinClosedTrades)
	if c.Passed || c.Measured != "29" || c.Required != "30" {
		t.Errorf("29 trades: %+v, want fail 29/30", c)
	}

	// 30 round trips plus one dangling open entry: still exactly 30.
	fills := append(winningTrips(30),
		Fill{Symbol: "BTC/USDT", Side: "buy", QtyBase: dec("1"), FillPrice: dec("1000"), FeeQuote: dec("0")})
	c = cond(t, Evaluate(baseInput(fills)), CondMinClosedTrades)
	if !c.Passed || c.Measured != "30" {
		t.Errorf("30 trades + open span: %+v, want pass 30", c)
	}
}
