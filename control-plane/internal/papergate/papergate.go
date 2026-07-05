// Package papergate evaluates the enforced paper-gate of
// docs/specs/lifecycle-api.md (LC-15..LC-23): a pure, deterministic replay
// of a strategy's paper-window fills into closed trades, an equity curve,
// and the five-condition report behind paper -> live_* promotion. The
// package holds no state and reads nothing: the caller supplies the window,
// the ordered fills, and the CURRENT effective limits; the gate fails
// closed on a missing window, missing limits, or a non-positive equity
// seed (invariant 4 — no role can waive it).
package papergate

import (
	"time"

	"github.com/shopspring/decimal"
)

// Condition names, exactly the LC-23 report set.
const (
	CondMinDays         = "min_days"
	CondMinClosedTrades = "min_closed_trades"
	CondMinAvgNotional  = "min_avg_notional"
	CondMaxDrawdown     = "max_drawdown"
	CondProfitFactor    = "profit_factor"
)

// Fill is one paper-window fill joined to its order (LC-18), in replay
// order — the caller supplies them ordered by (fill_ts, fills.rowid).
type Fill struct {
	Symbol string
	Side   string // "buy" | "sell"
	// ReduceOnly is the joined orders row's reduce_only flag: the replay
	// clamps such fills at the open opposite-side quantity — a reduce-only
	// fill never opens a book and never flips it (LC-18).
	ReduceOnly bool
	QtyBase    decimal.Decimal
	FillPrice  decimal.Decimal
	FeeQuote   decimal.Decimal
}

// Input carries everything one evaluation needs; nothing is read from
// request fields or cached state (LC-15).
type Input struct {
	// WindowOK is the LC-16 window verdict: false means the gate fails
	// closed (no qualifying entry into paper, or none since the newest
	// binding kill) and Fills is ignored.
	WindowOK    bool
	WindowStart time.Time
	Now         time.Time
	Fills       []Fill
	// LimitsOK marks the effective limits as present (a wired provider);
	// false renders required "0" and fails the limit-bound conditions.
	LimitsOK bool
	// NotionalCap is PerPositionNotionalCapQuote; min_avg_notional
	// requires 0.25 x cap (LC-20, v1 pinned default).
	NotionalCap decimal.Decimal
	// MaxDrawdownPct bounds the replay's max drawdown (LC-21).
	MaxDrawdownPct decimal.Decimal
	// Seed is AllocatedCapitalQuote, the equity-curve seed (LC-21);
	// a seed <= 0 fails max_drawdown.
	Seed decimal.Decimal
}

// Condition is one row of the LC-23 report; Measured and Required are
// decimal strings, never null.
type Condition struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Measured string `json:"measured"`
	Required string `json:"required"`
}

// Report is the full LC-23 condition report — always all five conditions,
// never first-failure.
type Report struct {
	Passed          bool        `json:"passed"`
	WindowStartedAt *string     `json:"window_started_at"`
	EvaluatedAt     string      `json:"evaluated_at"`
	Conditions      []Condition `json:"conditions"`
}

// minWindow is the LC-17 minimum paper-window age.
const minWindow = 14 * 24 * time.Hour

var (
	hundred     = decimal.NewFromInt(100)
	secsPerDay  = decimal.NewFromInt(86400)
	quarter     = decimal.RequireFromString("0.25")
	requiredPF  = decimal.NewFromInt(1)
	minTrades   = 30
	fourteenStr = "14"
)

// Evaluate runs the gate over in and returns the full report. It never
// errs: malformed inputs are impossible by construction (decimals are
// parsed by the caller) and every fail-closed edge is a failed condition,
// not an error (LC-23).
func Evaluate(in Input) Report {
	rep := Report{EvaluatedAt: formatTime(in.Now)}
	if !in.WindowOK {
		// Fail-closed window: every condition unmet, "0"/"0" (LC-23),
		// window_started_at null.
		for _, name := range []string{CondMinDays, CondMinClosedTrades,
			CondMinAvgNotional, CondMaxDrawdown, CondProfitFactor} {
			rep.Conditions = append(rep.Conditions, Condition{Name: name, Measured: "0", Required: "0"})
		}
		return rep
	}
	started := formatTime(in.WindowStart)
	rep.WindowStartedAt = &started

	trades, maxDD := replay(in.Fills, in.Seed)
	rep.Conditions = []Condition{
		condMinDays(in),
		condMinClosedTrades(trades),
		condMinAvgNotional(in, trades),
		condMaxDrawdown(in, trades, maxDD),
		condProfitFactor(trades),
	}
	rep.Passed = true
	for _, c := range rep.Conditions {
		rep.Passed = rep.Passed && c.Passed
	}
	return rep
}

// condMinDays renders measured/required in DECIMAL DAYS (LC-23): elapsed
// seconds / 86400; the pass comparison is on the timestamps themselves
// (LC-17, second precision). A clock rollback (Now before the window
// start) clamps measured at 0 — never a negative decimal string.
func condMinDays(in Input) Condition {
	elapsed := in.Now.Sub(in.WindowStart)
	if elapsed < 0 {
		elapsed = 0
	}
	measured := decimal.NewFromInt(int64(elapsed / time.Second)).Div(secsPerDay)
	return Condition{
		Name:     CondMinDays,
		Passed:   elapsed >= minWindow,
		Measured: measured.String(),
		Required: fourteenStr,
	}
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
