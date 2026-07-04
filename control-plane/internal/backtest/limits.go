package backtest

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// limitsConfig is the runspec "limits" object: the CONTROLPLANE_RISK_LIMITS
// JSON shape (RiskLimits v1, decimal-as-string money fields, ADR-0003).
// Admin-MUST-set fields have no default and fail load when absent; the rest
// fall back to their risk-limits.md defaults.
type limitsConfig struct {
	SymbolWhitelist             []string `json:"symbol_whitelist"`
	MaxOpenPositions            *int     `json:"max_open_positions"`
	PerPositionNotionalCapQuote string   `json:"per_position_notional_cap_quote"`
	DailyLossLimitQuote         string   `json:"daily_loss_limit_quote"`
	MaxDrawdownPct              string   `json:"max_drawdown_pct"`
	MaxLossAtStopQuote          string   `json:"max_loss_at_stop_quote"`
	MinStopDistancePct          string   `json:"min_stop_distance_pct"`
	MaxStopDistancePct          string   `json:"max_stop_distance_pct"`
	MaxOrdersPerMinute          *int     `json:"max_orders_per_minute"`
	RequireStopLoss             *bool    `json:"require_stop_loss"`
	AllocatedCapitalQuote       string   `json:"allocated_capital_quote"`
	AccountingQuote             string   `json:"accounting_quote"`
	StalenessThresholdSeconds   int      `json:"staleness_threshold_seconds"`
}

// riskLimits converts the runspec limits object into the gate's RiskLimits,
// enforcing the same required/default split as the serve-mode
// CONTROLPLANE_RISK_LIMITS parser.
func (c limitsConfig) riskLimits() (riskgate.RiskLimits, error) {
	limits := riskgate.RiskLimits{
		SymbolWhitelist:           c.SymbolWhitelist,
		MaxOpenPositions:          3,
		MaxOrdersPerMinute:        6,
		RequireStopLoss:           true,
		AccountingQuote:           c.AccountingQuote,
		StalenessThresholdSeconds: riskgate.DefaultStalenessThresholdSeconds,
		L1ApprovalTimeoutSeconds:  riskgate.DefaultL1ApprovalTimeoutSeconds,
	}
	if c.MaxOpenPositions != nil {
		limits.MaxOpenPositions = *c.MaxOpenPositions
	}
	if c.MaxOrdersPerMinute != nil {
		limits.MaxOrdersPerMinute = *c.MaxOrdersPerMinute
	}
	if c.RequireStopLoss != nil {
		limits.RequireStopLoss = *c.RequireStopLoss
	}
	if c.StalenessThresholdSeconds > 0 {
		limits.StalenessThresholdSeconds = c.StalenessThresholdSeconds
	}
	if c.AccountingQuote == "" {
		return riskgate.RiskLimits{}, fmt.Errorf("limits.accounting_quote is REQUIRED")
	}
	var err error
	if limits.PerPositionNotionalCapQuote, err = requiredDec("per_position_notional_cap_quote", c.PerPositionNotionalCapQuote); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.DailyLossLimitQuote, err = requiredDec("daily_loss_limit_quote", c.DailyLossLimitQuote); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.MaxLossAtStopQuote, err = requiredDec("max_loss_at_stop_quote", c.MaxLossAtStopQuote); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.AllocatedCapitalQuote, err = requiredDec("allocated_capital_quote", c.AllocatedCapitalQuote); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.MaxDrawdownPct, err = defaultedDec("max_drawdown_pct", c.MaxDrawdownPct, "10"); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.MinStopDistancePct, err = defaultedDec("min_stop_distance_pct", c.MinStopDistancePct, "0.1"); err != nil {
		return riskgate.RiskLimits{}, err
	}
	if limits.MaxStopDistancePct, err = defaultedDec("max_stop_distance_pct", c.MaxStopDistancePct, "25"); err != nil {
		return riskgate.RiskLimits{}, err
	}
	return limits, nil
}

func requiredDec(field, v string) (decimal.Decimal, error) {
	if v == "" {
		return decimal.Decimal{}, fmt.Errorf("limits.%s is REQUIRED (Admin MUST set, docs/specs/risk-limits.md)", field)
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("limits.%s %q: %w", field, v, err)
	}
	return d, nil
}

func defaultedDec(field, v, def string) (decimal.Decimal, error) {
	if v == "" {
		v = def
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("limits.%s %q: %w", field, v, err)
	}
	return d, nil
}
