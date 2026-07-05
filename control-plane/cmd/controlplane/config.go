package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/live"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// limitsConfig is the CONTROLPLANE_RISK_LIMITS JSON shape: RiskLimits v1
// (docs/specs/risk-limits.md) with decimal-as-string money fields
// (ADR-0003). Admin-MUST-set fields have no default and fail startup when
// absent; the remaining fields fall back to their spec defaults.
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
	L1ApprovalTimeoutSeconds    int      `json:"l1_approval_timeout_seconds"`
	L2MaxSizeQuote              string   `json:"l2_max_size_quote"`
	L2AllowedSymbols            []string `json:"l2_allowed_symbols"`
}

// parseRiskLimits parses the CONTROLPLANE_RISK_LIMITS JSON; "" yields nil
// (proposal ingestion disabled — no limits, no gate).
func parseRiskLimits(raw string) (*riskgate.RiskLimits, error) {
	if raw == "" {
		return nil, nil
	}
	var c limitsConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("CONTROLPLANE_RISK_LIMITS: %w", err)
	}
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
	if c.L1ApprovalTimeoutSeconds > 0 {
		limits.L1ApprovalTimeoutSeconds = c.L1ApprovalTimeoutSeconds
	}
	if c.AccountingQuote == "" {
		return nil, fmt.Errorf("CONTROLPLANE_RISK_LIMITS: accounting_quote is REQUIRED (Admin MUST set)")
	}
	var err error
	// Admin-MUST-set money fields (no defaults).
	if limits.PerPositionNotionalCapQuote, err = requiredDec("per_position_notional_cap_quote", c.PerPositionNotionalCapQuote); err != nil {
		return nil, err
	}
	if limits.DailyLossLimitQuote, err = requiredDec("daily_loss_limit_quote", c.DailyLossLimitQuote); err != nil {
		return nil, err
	}
	if limits.MaxLossAtStopQuote, err = requiredDec("max_loss_at_stop_quote", c.MaxLossAtStopQuote); err != nil {
		return nil, err
	}
	if limits.AllocatedCapitalQuote, err = requiredDec("allocated_capital_quote", c.AllocatedCapitalQuote); err != nil {
		return nil, err
	}
	// Defaulted numeric fields (risk-limits.md field table).
	if limits.MaxDrawdownPct, err = defaultedDec("max_drawdown_pct", c.MaxDrawdownPct, "10"); err != nil {
		return nil, err
	}
	if limits.MinStopDistancePct, err = defaultedDec("min_stop_distance_pct", c.MinStopDistancePct, "0.1"); err != nil {
		return nil, err
	}
	if limits.MaxStopDistancePct, err = defaultedDec("max_stop_distance_pct", c.MaxStopDistancePct, "25"); err != nil {
		return nil, err
	}
	if c.L2MaxSizeQuote != "" {
		maxSize, err := requiredDec("l2_max_size_quote", c.L2MaxSizeQuote)
		if err != nil {
			return nil, err
		}
		limits.L2Envelope = &riskgate.L2Envelope{MaxSizeQuote: maxSize, AllowedSymbols: c.L2AllowedSymbols}
	}
	return &limits, nil
}

func requiredDec(field, v string) (decimal.Decimal, error) {
	if v == "" {
		return decimal.Decimal{}, fmt.Errorf("CONTROLPLANE_RISK_LIMITS: %s is REQUIRED (Admin MUST set, docs/specs/risk-limits.md)", field)
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("CONTROLPLANE_RISK_LIMITS: %s %q: %w", field, v, err)
	}
	return d, nil
}

func defaultedDec(field, v, def string) (decimal.Decimal, error) {
	if v == "" {
		v = def
	}
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("CONTROLPLANE_RISK_LIMITS: %s %q: %w", field, v, err)
	}
	return d, nil
}

// prodAckLiteral is the exact CONTROLPLANE_LIVE_PROD_ACK value required for
// CONTROLPLANE_BINANCE_ENV=prod (live-oms-and-reconciler.md §Config,
// invariant 15: three explicit settings before real funds).
const prodAckLiteral = "I-UNDERSTAND-THIS-TRADES-REAL-FUNDS"

// liveOMSConfig carries the parsed live-OMS opt-in (spec §Config); nil
// means paper mode (the default). The credentials are secrets: never
// logged, in errors or otherwise.
type liveOMSConfig struct {
	env       exchange.Env
	apiKey    string
	apiSecret string
	tuning    live.Tuning
}

// parseLiveOMS validates the live-OMS env settings. mode "" or "paper"
// yields nil (paper is the default and behaviorally unchanged); "live"
// requires BOTH API credentials; env "prod" additionally requires the exact
// ack literal. Any other mode or env value refuses to start.
func parseLiveOMS(mode, env, apiKey, apiSecret, prodAck, tuningRaw string) (*liveOMSConfig, error) {
	switch mode {
	case "", "paper":
		return nil, nil
	case "live":
	default:
		return nil, fmt.Errorf("CONTROLPLANE_OMS_MODE %q: must be \"paper\" or \"live\"", mode)
	}
	c := &liveOMSConfig{env: exchange.EnvTestnet}
	switch env {
	case "", string(exchange.EnvTestnet):
	case string(exchange.EnvProd):
		if prodAck != prodAckLiteral {
			return nil, errors.New("CONTROLPLANE_BINANCE_ENV=prod requires CONTROLPLANE_LIVE_PROD_ACK to equal " + prodAckLiteral)
		}
		c.env = exchange.EnvProd
	default:
		return nil, fmt.Errorf("CONTROLPLANE_BINANCE_ENV %q: must be \"testnet\" or \"prod\"", env)
	}
	if apiKey == "" || apiSecret == "" {
		return nil, errors.New("CONTROLPLANE_OMS_MODE=live requires CONTROLPLANE_BINANCE_API_KEY and CONTROLPLANE_BINANCE_API_SECRET")
	}
	c.apiKey, c.apiSecret = apiKey, apiSecret
	tuning, err := live.ParseTuning(tuningRaw)
	if err != nil {
		return nil, err
	}
	c.tuning = tuning
	return c, nil
}

// validateVenuePairing enforces the normative venue pairing (spec §Config):
// CONTROLPLANE_BINANCE_ENV=prod REQUIRES prod market data — a testnet
// market-data endpoint override refuses to start. Testnet trading may (and
// is recommended to) use prod market data.
func validateVenuePairing(env exchange.Env, restURL, wsURL string) error {
	if env != exchange.EnvProd {
		return nil
	}
	for _, u := range []string{restURL, wsURL} {
		if strings.Contains(u, "testnet") {
			return errors.New("CONTROLPLANE_BINANCE_ENV=prod requires prod market data: remove the testnet CONTROLPLANE_BINANCE_REST_URL/_WS_URL override (venue pairing, docs/specs/live-oms-and-reconciler.md §Config)")
		}
	}
	return nil
}

// splitSymbols parses the CONTROLPLANE_SYMBOLS comma list of canonical
// BASE/QUOTE symbols; "" yields nil (no market-data feed).
func splitSymbols(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
