package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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

// Breaker-monitor cadence defaults and bounds (safety-wiring.md §Config):
// ACTIVE defaults to 5 s within the hard [1, 10] bound (risk-limits.md:
// <= 10 s while positions are open); IDLE defaults to 60 s within
// [ACTIVE, 600]. Out of bounds or non-integer refuses to start.
const (
	defaultBreakerActiveSeconds = 5
	defaultBreakerIdleSeconds   = 60
)

// parseBreakerIntervals validates CONTROLPLANE_BREAKER_INTERVAL_ACTIVE and
// _IDLE (integer seconds, fail-closed); "" means the normative default.
// Read only in live mode — paper deployments ignore both.
func parseBreakerIntervals(activeRaw, idleRaw string) (time.Duration, time.Duration, error) {
	active := defaultBreakerActiveSeconds
	if activeRaw != "" {
		n, err := strconv.Atoi(activeRaw)
		if err != nil {
			return 0, 0, fmt.Errorf("CONTROLPLANE_BREAKER_INTERVAL_ACTIVE %q: %w", activeRaw, err)
		}
		active = n
	}
	if active < 1 || active > 10 {
		return 0, 0, fmt.Errorf("CONTROLPLANE_BREAKER_INTERVAL_ACTIVE %d: bounds [1, 10] (risk-limits.md: <= 10 s while positions are open)", active)
	}
	idle := defaultBreakerIdleSeconds
	if idleRaw != "" {
		n, err := strconv.Atoi(idleRaw)
		if err != nil {
			return 0, 0, fmt.Errorf("CONTROLPLANE_BREAKER_INTERVAL_IDLE %q: %w", idleRaw, err)
		}
		idle = n
	}
	if idle < active || idle > 600 {
		return 0, 0, fmt.Errorf("CONTROLPLANE_BREAKER_INTERVAL_IDLE %d: bounds [ACTIVE=%d, 600]", idle, active)
	}
	return time.Duration(active) * time.Second, time.Duration(idle) * time.Second, nil
}

// parseWatchdogDisabled parses CONTROLPLANE_WATCHDOG_DISABLED
// (watchdog.md §Config): "1"/"true" disables watchdog EVALUATION (the
// heartbeat endpoint still accepts beats); anything else, including
// unset, enables. Read only in live mode — paper deployments never read
// it (no monitor runs, so setting it there is a no-op).
func parseWatchdogDisabled(v string) bool {
	return v == "1" || v == "true"
}

// backupConfig carries the parsed OB-8 backup settings (ops-backup.md):
// all three variables are read in cmd wiring only (OB-8a); the api/store
// packages receive plain values.
type backupConfig struct {
	dir      string
	retain   int           // 0 = keep everything (OB-9 disabled)
	interval time.Duration // 0 = manual only (OB-10 disabled)
}

// parseBackupConfig validates the OB-8 environment; dir "" yields nil (the
// whole surface stays unregistered). The dir MUST exist and be a writable
// directory at startup (fail-fast); retain and intervalHours are optional
// ints >= 1 — 0/unset disables each, anything else refuses to start.
func parseBackupConfig(dir, retainRaw, intervalRaw string) (*backupConfig, error) {
	if dir == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("CONTROLPLANE_BACKUP_DIR %q: %w (must exist, ops-backup.md OB-8)", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("CONTROLPLANE_BACKUP_DIR %q: not a directory (ops-backup.md OB-8)", dir)
	}
	probe, err := os.CreateTemp(dir, ".backup-writable-probe-*")
	if err != nil {
		return nil, fmt.Errorf("CONTROLPLANE_BACKUP_DIR %q: not writable: %w (ops-backup.md OB-8)", dir, err)
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(probe.Name())
		return nil, fmt.Errorf("CONTROLPLANE_BACKUP_DIR %q: %w", dir, err)
	}
	if err := os.Remove(probe.Name()); err != nil {
		return nil, fmt.Errorf("CONTROLPLANE_BACKUP_DIR %q: %w", dir, err)
	}
	c := &backupConfig{dir: dir}
	if retainRaw != "" {
		n, err := strconv.Atoi(retainRaw)
		if err != nil {
			return nil, fmt.Errorf("CONTROLPLANE_BACKUP_RETAIN %q: %w", retainRaw, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("CONTROLPLANE_BACKUP_RETAIN %d: must be >= 0 (0/unset = keep everything)", n)
		}
		c.retain = n
	}
	if intervalRaw != "" {
		n, err := strconv.Atoi(intervalRaw)
		if err != nil {
			return nil, fmt.Errorf("CONTROLPLANE_BACKUP_INTERVAL_HOURS %q: %w", intervalRaw, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("CONTROLPLANE_BACKUP_INTERVAL_HOURS %d: must be >= 0 (0/unset = manual only)", n)
		}
		c.interval = time.Duration(n) * time.Hour
	}
	return c, nil
}

// parseMaxStrategiesPerTenant validates
// CONTROLPLANE_MAX_STRATEGIES_PER_TENANT (integer >= 1, fail-closed —
// 0/negative/junk refuses to start); "" yields the SP-4b default 100
// (strategy-provisioning.md SP-4b).
func parseMaxStrategiesPerTenant(raw string) (int, error) {
	if raw == "" {
		return 100, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("CONTROLPLANE_MAX_STRATEGIES_PER_TENANT %q: %w", raw, err)
	}
	if n < 1 {
		return 0, fmt.Errorf("CONTROLPLANE_MAX_STRATEGIES_PER_TENANT %d: must be >= 1 (strategy-provisioning.md SP-4b)", n)
	}
	return n, nil
}

// alertWebhookJSON is the CONTROLPLANE_ALERT_WEBHOOK JSON shape
// (alert-notifier.md AN-10); pointers distinguish absent from zero, so
// the "MUST be absent" rules are checkable.
type alertWebhookJSON struct {
	URL                 *string `json:"url"`
	AuthorizationBearer *string `json:"authorization_bearer"`
	TimeoutSeconds      *int    `json:"timeout_seconds"`
	PollSeconds         *int    `json:"poll_seconds"`
	MaxPerTick          *int    `json:"max_per_tick"`
	HeartbeatHours      *int    `json:"heartbeat_hours"`
	LogOnly             *bool   `json:"log_only"`
}

// alertConfig carries the parsed AN-10 settings; nil means the notifier
// is disabled entirely (no goroutine, no seeded watermark rows). url and
// bearer are secrets: never logged, in errors or otherwise (AN-11).
type alertConfig struct {
	url        string
	bearer     string
	timeout    time.Duration
	poll       time.Duration
	maxPerTick int
	heartbeat  time.Duration // 0 = AN-14a heartbeat disabled
	logOnly    bool
}

// parseAlertWebhook validates CONTROLPLANE_ALERT_WEBHOOK fail-fast
// (AN-10); "" yields nil (disabled). Per AN-11 no error ever wraps or
// propagates decoder/parser text (it echoes input fragments) — errors
// name the offending FIELD only.
func parseAlertWebhook(raw string) (*alertConfig, error) {
	if raw == "" {
		return nil, nil
	}
	var c alertWebhookJSON
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: invalid JSON (fields and types per docs/specs/alert-notifier.md AN-10)")
	}
	out := &alertConfig{
		timeout: 5 * time.Second, poll: 5 * time.Second,
		maxPerTick: 20, heartbeat: 24 * time.Hour,
	}
	if c.LogOnly != nil {
		out.logOnly = *c.LogOnly
	}
	if out.logOnly {
		if c.URL != nil {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url MUST be absent when log_only is true")
		}
		if c.AuthorizationBearer != nil {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: authorization_bearer MUST be absent when log_only is true")
		}
	} else {
		if c.URL == nil || *c.URL == "" {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url is REQUIRED unless log_only is true")
		}
		u, err := url.Parse(*c.URL)
		if err != nil {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url: not a parseable URL")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url: scheme must be http or https")
		}
		if u.Host == "" {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url: host is required")
		}
		if u.User != nil {
			return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: url: userinfo (user:pass@) is rejected")
		}
		out.url = *c.URL
		if c.AuthorizationBearer != nil {
			if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
				return nil, errors.New("CONTROLPLANE_ALERT_WEBHOOK: authorization_bearer over scheme http requires a loopback host (a bearer on a cleartext non-loopback hop is a config error)")
			}
			out.bearer = *c.AuthorizationBearer
		}
	}
	if c.TimeoutSeconds != nil {
		if *c.TimeoutSeconds < 1 || *c.TimeoutSeconds > 60 {
			return nil, fmt.Errorf("CONTROLPLANE_ALERT_WEBHOOK: timeout_seconds %d: bounds [1, 60]", *c.TimeoutSeconds)
		}
		out.timeout = time.Duration(*c.TimeoutSeconds) * time.Second
	}
	if c.PollSeconds != nil {
		if *c.PollSeconds < 1 || *c.PollSeconds > 300 {
			return nil, fmt.Errorf("CONTROLPLANE_ALERT_WEBHOOK: poll_seconds %d: bounds [1, 300]", *c.PollSeconds)
		}
		out.poll = time.Duration(*c.PollSeconds) * time.Second
	}
	if c.MaxPerTick != nil {
		if *c.MaxPerTick < 1 || *c.MaxPerTick > 500 {
			return nil, fmt.Errorf("CONTROLPLANE_ALERT_WEBHOOK: max_per_tick %d: bounds [1, 500]", *c.MaxPerTick)
		}
		out.maxPerTick = *c.MaxPerTick
	}
	if c.HeartbeatHours != nil {
		if *c.HeartbeatHours < 0 || *c.HeartbeatHours > 168 {
			return nil, fmt.Errorf("CONTROLPLANE_ALERT_WEBHOOK: heartbeat_hours %d: bounds [0, 168] (0 disables)", *c.HeartbeatHours)
		}
		out.heartbeat = time.Duration(*c.HeartbeatHours) * time.Hour
	}
	return out, nil
}

// isLoopbackHost reports whether the AN-10 bearer-over-http exception
// applies: 127.0.0.0/8, ::1, or the literal "localhost".
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
