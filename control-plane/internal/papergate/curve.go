package papergate

import "github.com/shopspring/decimal"

// CurvePoint is one equity sample of the arena replay: the post-fill
// equity at the fill's timestamp, in replay order.
type CurvePoint struct {
	TS     string
	Equity decimal.Decimal
}

// CurveStats aggregates the replay for the Phase 28 arena surfaces. All
// decimals follow the gate's math exactly (the shared walk); the caller
// renders them as ADR-0003 strings.
type CurveStats struct {
	// RealizedPnL is the final equity minus the seed: every realized
	// delta net of every fee debit, open-position fees included.
	RealizedPnL    decimal.Decimal
	FeesPaid       decimal.Decimal
	MaxDrawdownPct decimal.Decimal
	ClosedTrades   int
	Wins           int // closed trades with pnl > 0
	Losses         int // closed trades with pnl < 0 (flat trades are neither)
	// WinRatePct is wins/closed_trades x 100; zero when no closed trades.
	WinRatePct decimal.Decimal
	// ProfitFactor is gross_profit / gross_loss over closed-trade PnLs;
	// nil when gross_loss is zero (unbounded or undefined).
	ProfitFactor *decimal.Decimal
	// LastFillAt is the newest fill's FillTS; "" when there are no fills.
	LastFillAt string
}

// ReplayCurve runs the IDENTICAL book walk as the gate's replay (LC-18)
// over the window's fills and returns the equity curve — one point per
// fill, post-update — plus the aggregate stats. Pure and deterministic;
// the gate's own numbers (closed trades, max drawdown) are byte-identical
// by construction.
func ReplayCurve(fills []Fill, seed decimal.Decimal) ([]CurvePoint, CurveStats) {
	curve := make([]CurvePoint, len(fills))
	trades, maxDD := walk(fills, seed, func(i int, equity decimal.Decimal) {
		curve[i] = CurvePoint{TS: fills[i].FillTS, Equity: equity}
	})
	stats := CurveStats{
		RealizedPnL:    decimal.Zero,
		FeesPaid:       decimal.Zero,
		MaxDrawdownPct: maxDD,
		ClosedTrades:   len(trades),
		WinRatePct:     decimal.Zero,
	}
	for _, f := range fills {
		stats.FeesPaid = stats.FeesPaid.Add(f.FeeQuote)
	}
	if len(fills) > 0 {
		stats.RealizedPnL = curve[len(curve)-1].Equity.Sub(seed)
		stats.LastFillAt = fills[len(fills)-1].FillTS
	}
	gp, gl := decimal.Zero, decimal.Zero
	for _, tr := range trades {
		switch tr.pnl.Sign() {
		case 1:
			stats.Wins++
			gp = gp.Add(tr.pnl)
		case -1:
			stats.Losses++
			gl = gl.Add(tr.pnl.Neg())
		}
	}
	if len(trades) > 0 {
		stats.WinRatePct = decimal.NewFromInt(int64(stats.Wins)).
			Div(decimal.NewFromInt(int64(len(trades)))).Mul(hundred)
	}
	if gl.Sign() > 0 {
		pf := gp.Div(gl)
		stats.ProfitFactor = &pf
	}
	return curve, stats
}
