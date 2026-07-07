package papergate

import "github.com/shopspring/decimal"

// trade is one CLOSED TRADE (LC-18): a maximal span in which a symbol's
// book leaves zero and returns to zero. PnL is net of ALL span fees (entry
// fees included); ClosingNotional sums qty x price over the span's
// reducing fills only.
type trade struct {
	pnl             decimal.Decimal
	closingNotional decimal.Decimal
}

// book is one symbol's replayed signed position plus the running span
// accumulators, the SAME math as paper.applyFill (LC-18).
type book struct {
	qty      decimal.Decimal
	avg      decimal.Decimal
	pnl      decimal.Decimal // span realized deltas net of span fees
	notional decimal.Decimal // span closing notional
}

// round8 mirrors the single normative rounding rule (market-data.md
// §Rounding): half away from zero at 8 decimals, applied to each realized
// delta exactly as the paper OMS applies it.
func round8(d decimal.Decimal) decimal.Decimal { return d.Round(8) }

// replay reconstructs one signed book per symbol over the window's fills
// and folds every realized delta and fee debit into an equity curve seeded
// at seed (LC-18, LC-21). It returns the closed trades in close order and
// the maximum drawdown percentage over the curve (zero when seed <= 0:
// the caller fails the condition instead of dividing by a zero peak).
func replay(fills []Fill, seed decimal.Decimal) ([]trade, decimal.Decimal) {
	return walk(fills, seed, nil)
}

// walk is the single book-walk behind replay and ReplayCurve: the identical
// statement sequence either way, so the arena curve can never diverge from
// the gate's math. step (optional) observes the post-fill equity per fill
// in replay order.
func walk(fills []Fill, seed decimal.Decimal, step func(i int, equity decimal.Decimal)) ([]trade, decimal.Decimal) {
	books := make(map[string]*book)
	var trades []trade
	equity, peak, maxDD := seed, seed, decimal.Zero
	for i, f := range fills {
		b, ok := books[f.Symbol]
		if !ok {
			b = &book{qty: decimal.Zero, avg: decimal.Zero, pnl: decimal.Zero, notional: decimal.Zero}
			books[f.Symbol] = b
		}
		sign := decimal.NewFromInt(1)
		if f.Side != "buy" {
			sign = decimal.NewFromInt(-1)
		}
		qty := f.QtyBase
		if f.ReduceOnly {
			// Reduce-only clamp (LC-18): the flag is a sizing bound — the
			// fill can only close the book toward zero. Replay caps the
			// quantity at the open opposite-side quantity; a same-side or
			// flat book replays only the fee debit. A reduce-only fill
			// therefore never opens a phantom position and never flips.
			if b.qty.IsZero() || b.qty.Sign() == sign.Sign() {
				qty = decimal.Zero
			} else {
				qty = decimal.Min(qty, b.qty.Abs())
			}
		}
		delta := sign.Mul(qty)
		gross := decimal.Zero
		if b.qty.IsZero() || b.qty.Sign() == delta.Sign() {
			// Opening or increasing: weighted-average entry price.
			total := b.qty.Abs().Add(qty)
			if total.Sign() > 0 {
				b.avg = b.avg.Mul(b.qty.Abs()).Add(f.FillPrice.Mul(qty)).Div(total)
			}
			b.qty = b.qty.Add(delta)
			b.pnl = b.pnl.Sub(f.FeeQuote)
		} else {
			// Reducing (or flipping): realize PnL on the closed quantity;
			// only the REDUCING portion counts toward the span (LC-18).
			closed := decimal.Min(qty, b.qty.Abs())
			posSign := decimal.NewFromInt(int64(b.qty.Sign()))
			gross = round8(f.FillPrice.Sub(b.avg).Mul(closed).Mul(posSign))
			b.pnl = b.pnl.Add(gross).Sub(f.FeeQuote)
			b.notional = b.notional.Add(closed.Mul(f.FillPrice))
			flip := qty.GreaterThan(b.qty.Abs())
			b.qty = b.qty.Add(delta)
			if b.qty.IsZero() || flip {
				// The span closes at the zero crossing; a flip's
				// remainder opens the next span at the fill price with
				// fresh accumulators (the whole fee stays with the
				// closing span — fees are realized when paid).
				trades = append(trades, trade{pnl: b.pnl, closingNotional: b.notional})
				b.pnl, b.notional = decimal.Zero, decimal.Zero
				if flip {
					b.avg = f.FillPrice
				} else {
					b.avg = decimal.Zero
				}
			}
		}
		// Equity curve: realized delta and fee debit at the fill where
		// they are paid, in replay order; peak monotone from the seed.
		equity = equity.Add(gross).Sub(f.FeeQuote)
		peak = decimal.Max(peak, equity)
		if seed.Sign() > 0 {
			if dd := peak.Sub(equity).Div(peak).Mul(hundred); dd.GreaterThan(maxDD) {
				maxDD = dd
			}
		}
		if step != nil {
			step(i, equity)
		}
	}
	return trades, maxDD
}

// condMinClosedTrades is LC-19: closed trades >= 30.
func condMinClosedTrades(trades []trade) Condition {
	return Condition{
		Name:     CondMinClosedTrades,
		Passed:   len(trades) >= minTrades,
		Measured: decimal.NewFromInt(int64(len(trades))).String(),
		Required: decimal.NewFromInt(int64(minTrades)).String(),
	}
}

// condMinAvgNotional is LC-20: average closing notional >= 0.25 x cap.
// Absent limits render required "0" and fail; zero closed trades fail with
// measured "0" (no division).
func condMinAvgNotional(in Input, trades []trade) Condition {
	c := Condition{Name: CondMinAvgNotional, Measured: "0", Required: "0"}
	limitsOK := in.LimitsOK && in.NotionalCap.Sign() > 0
	if limitsOK {
		c.Required = in.NotionalCap.Mul(quarter).String()
	}
	if len(trades) == 0 {
		return c
	}
	sum := decimal.Zero
	for _, t := range trades {
		sum = sum.Add(t.closingNotional)
	}
	avg := sum.Div(decimal.NewFromInt(int64(len(trades))))
	c.Measured = avg.String()
	c.Passed = limitsOK && avg.GreaterThanOrEqual(in.NotionalCap.Mul(quarter))
	return c
}

// condMaxDrawdown is LC-21: the curve's max drawdown percentage must not
// exceed MaxDrawdownPct. Absent limits, a seed <= 0, and zero closed
// trades all fail closed (measured "0" on the zero-trade edge, LC-23).
func condMaxDrawdown(in Input, trades []trade, maxDD decimal.Decimal) Condition {
	c := Condition{Name: CondMaxDrawdown, Measured: "0", Required: "0"}
	limitsOK := in.LimitsOK && in.MaxDrawdownPct.Sign() > 0
	if limitsOK {
		c.Required = in.MaxDrawdownPct.String()
	}
	if len(trades) == 0 || in.Seed.Sign() <= 0 {
		return c
	}
	c.Measured = maxDD.String()
	c.Passed = limitsOK && maxDD.LessThanOrEqual(in.MaxDrawdownPct)
	return c
}

// condProfitFactor is LC-22 over closed-trade PnLs: gross_loss = 0 with
// gross_profit > 0 passes; both zero fails; else pass iff gp/gl >= 1.
func condProfitFactor(trades []trade) Condition {
	c := Condition{Name: CondProfitFactor, Measured: "0", Required: requiredPF.String()}
	if len(trades) == 0 {
		return c
	}
	gp, gl := decimal.Zero, decimal.Zero
	for _, t := range trades {
		if t.pnl.Sign() > 0 {
			gp = gp.Add(t.pnl)
		} else {
			gl = gl.Add(t.pnl.Neg())
		}
	}
	switch {
	case gl.IsZero() && gp.Sign() > 0:
		// No losing trades: the ratio is unbounded; measured renders the
		// gross profit itself.
		c.Measured, c.Passed = gp.String(), true
	case gl.IsZero():
		// Both zero: fails (LC-22); measured stays "0".
	default:
		pf := gp.Div(gl)
		c.Measured = pf.String()
		c.Passed = pf.GreaterThanOrEqual(requiredPF)
	}
	return c
}
