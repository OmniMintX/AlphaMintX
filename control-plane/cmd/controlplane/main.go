// Command controlplane serves the Phase-1 control-plane HTTP API when
// CONTROLPLANE_DB is set (docs/specs/persistence-and-api.md). Without a DB
// path it stays the Phase-0 demo loop: load a golden fixture proposal,
// evaluate it through the deterministic Risk Gate, and print the resulting
// RiskVerdict JSON.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/api"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/live"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/omsbridge"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/runstate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/safety"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// sweepInterval is the periodic pending-approval expiry sweep; the sweep
// also runs once at startup (restart-safe default-deny).
const sweepInterval = 30 * time.Second

func main() {
	if dbPath := os.Getenv("CONTROLPLANE_DB"); dbPath != "" {
		if err := serve(dbPath); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// serve runs the Phase-1 HTTP API until SIGINT/SIGTERM: the read + L1
// approval plane plus, when configured, the live serve-mode wiring —
// proposal ingestion gated against hydrated runtime state
// (CONTROLPLANE_RISK_LIMITS), the paper-OMS bridge acting as the Submitter
// (CONTROLPLANE_FILL_MODEL), and the BinanceFeed writer that stores marks
// and fires the OMS trigger sweep on every tick (CONTROLPLANE_SYMBOLS).
// CONTROLPLANE_OMS_MODE=live replaces the paper bridge with the live
// Binance OMS (live-oms-and-reconciler.md §Config) and registers the recon
// routes. Each piece is fail-closed when absent: no limits means no
// verdicts are produced here, no Submitter means approvals block
// SUBMITTER_UNAVAILABLE, and no feed means the gate rejects
// MARK_PRICE_UNAVAILABLE.
func serve(dbPath string) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// max_age_seconds is REQUIRED config (market-data.md §Staleness rule):
	// no hidden default, matching the e2e runspec behavior.
	v := os.Getenv("CONTROLPLANE_MARK_MAX_AGE_SECONDS")
	if v == "" {
		return errors.New("CONTROLPLANE_MARK_MAX_AGE_SECONDS is REQUIRED (max_age_seconds has no default, docs/specs/market-data.md)")
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("CONTROLPLANE_MARK_MAX_AGE_SECONDS %q: %w", v, err)
	}
	marks, err := marketdata.NewStore(time.Duration(secs) * time.Second)
	if err != nil {
		return err
	}

	agentTokens, err := parseAgentTokens(os.Getenv("CONTROLPLANE_AGENT_TOKENS"))
	if err != nil {
		return err
	}
	operatorPrincipal := os.Getenv("CONTROLPLANE_OPERATOR_PRINCIPAL")
	if operatorPrincipal == "" {
		operatorPrincipal = "operator"
	}
	cfg := api.Config{
		Store:             st,
		Marks:             marks,
		ReadToken:         os.Getenv("CONTROLPLANE_READ_TOKEN"),
		OperatorToken:     os.Getenv("CONTROLPLANE_OPERATOR_TOKEN"),
		OperatorPrincipal: operatorPrincipal,
		AgentTokens:       agentTokens,
		AdminToken:        os.Getenv("CONTROLPLANE_ADMIN_TOKEN"),
	}

	// The backup engine (ops-backup.md OB-8): dir enables the whole
	// surface, mode-independently — paper deployments wire it too.
	backupCfg, err := parseBackupConfig(
		os.Getenv("CONTROLPLANE_BACKUP_DIR"),
		os.Getenv("CONTROLPLANE_BACKUP_RETAIN"),
		os.Getenv("CONTROLPLANE_BACKUP_INTERVAL_HOURS"),
	)
	if err != nil {
		return err
	}
	var backup *backupEngine
	if backupCfg != nil {
		backup = &backupEngine{st: st, dir: backupCfg.dir, retain: backupCfg.retain}
		cfg.Backup = backup
	}

	limits, err := parseRiskLimits(os.Getenv("CONTROLPLANE_RISK_LIMITS"))
	if err != nil {
		return err
	}
	var hydrator *runstate.Hydrator
	var provider *api.LimitsProvider
	if limits != nil {
		// The provider hydrates the persisted risk_limit_changes overlay
		// here, in the server layer after store.Open (multi-tenant-rbac.md
		// §Runtime limit changes): the overlay always beats the env base.
		provider, err = api.NewLimitsProvider(st, *limits)
		if err != nil {
			return err
		}
		hydrator = &runstate.Hydrator{Store: st, Marks: marks, AllocatedCapitalQuote: limits.AllocatedCapitalQuote}
		cfg.Limits = limits
		cfg.LimitsProvider = provider
		cfg.RuntimeState = hydrator
		// The paper-gate equity curve seeds at the SAME allocated capital
		// handed to the hydrator and OMS (lifecycle-api.md LC-21).
		cfg.AllocatedCapitalQuote = limits.AllocatedCapitalQuote
		cfg.DailyLossBreached = func(strategyID string, now time.Time) (bool, error) {
			daily, err := hydrator.DailyPnL(strategyID, now)
			if err != nil {
				return false, err
			}
			// The limit comes from the provider per strategy, never a
			// startup capture.
			return daily.LessThanOrEqual(provider.Limits(strategyID).DailyLossLimitQuote.Neg()), nil
		}
	}

	// The live-OMS opt-in (live-oms-and-reconciler.md §Config): exactly one
	// of the paper bridge or the live OMS fills the Submitter seam.
	liveCfg, err := parseLiveOMS(
		os.Getenv("CONTROLPLANE_OMS_MODE"),
		os.Getenv("CONTROLPLANE_BINANCE_ENV"),
		os.Getenv("CONTROLPLANE_BINANCE_API_KEY"),
		os.Getenv("CONTROLPLANE_BINANCE_API_SECRET"),
		os.Getenv("CONTROLPLANE_LIVE_PROD_ACK"),
		os.Getenv("CONTROLPLANE_LIVE_OMS_TUNING"),
	)
	if err != nil {
		return err
	}

	symbols := splitSymbols(os.Getenv("CONTROLPLANE_SYMBOLS"))

	var bridge *omsbridge.Bridge
	var liveOMS *live.OMS
	var monitor *safety.Monitor
	switch {
	case liveCfg != nil:
		if os.Getenv("CONTROLPLANE_FILL_MODEL") != "" {
			return errors.New("CONTROLPLANE_OMS_MODE=live excludes CONTROLPLANE_FILL_MODEL (exactly one of paper bridge or live OMS)")
		}
		if limits == nil {
			return errors.New("CONTROLPLANE_OMS_MODE=live requires CONTROLPLANE_RISK_LIMITS (allocated_capital_quote seeds equity)")
		}
		if len(symbols) == 0 {
			return errors.New("CONTROLPLANE_OMS_MODE=live requires CONTROLPLANE_SYMBOLS (the OMS trades configured symbols only)")
		}
		if err := validateVenuePairing(liveCfg.env,
			os.Getenv("CONTROLPLANE_BINANCE_REST_URL"), os.Getenv("CONTROLPLANE_BINANCE_WS_URL")); err != nil {
			return err
		}
		// The breaker-monitor knobs are read only in live mode (paper
		// deployments ignore them, safety-wiring.md §Config).
		breakerActive, breakerIdle, err := parseBreakerIntervals(
			os.Getenv("CONTROLPLANE_BREAKER_INTERVAL_ACTIVE"),
			os.Getenv("CONTROLPLANE_BREAKER_INTERVAL_IDLE"))
		if err != nil {
			return err
		}
		// The watchdog escape hatch is read only in live mode too
		// (watchdog.md §Config): startup logs LOUDLY when set.
		watchdogDisabled := parseWatchdogDisabled(os.Getenv("CONTROLPLANE_WATCHDOG_DISABLED"))
		if watchdogDisabled {
			log.Printf("WARNING: CONTROLPLANE_WATCHDOG_DISABLED is set: watchdog EVALUATION is OFF — silent agents will NOT be swept or killed (heartbeats are still accepted)")
		}
		ex := exchange.NewBinance(liveCfg.env, liveCfg.apiKey, liveCfg.apiSecret, nil)
		ex.RecvWindow = time.Duration(liveCfg.tuning.RecvWindowMS) * time.Millisecond
		liveOMS, err = live.New(live.Config{
			Store:                 st,
			Exchange:              ex,
			Symbols:               symbols,
			Marks:                 marks,
			AllocatedCapitalQuote: limits.AllocatedCapitalQuote,
			VenueEnv:              string(liveCfg.env),
			Tuning:                liveCfg.tuning,
			// OnFill is the breaker monitor's Poke seam (safety-wiring.md
			// §Evaluation loop); the monitor is assigned right below,
			// before any fill can flow (Run starts later).
			OnFill: func(strategyID string) {
				if monitor != nil {
					monitor.Poke(strategyID)
				}
			},
		})
		if err != nil {
			return err
		}
		monitor, err = safety.New(safety.Config{
			Store:  st,
			PnL:    hydrator,
			Limits: provider,
			Marks:  marks,
			Driver: liveOMS,
			Recon:  liveOMS,
			// The watchdog seams (watchdog.md §Wiring seams): the live
			// OMS's CancelOpenEntries and MinFilters, wired exactly like
			// Driver/Recon.
			Entries:          liveOMS,
			Filters:          liveOMS,
			WatchdogDisabled: watchdogDisabled,
			ActiveInterval:   breakerActive,
			IdleInterval:     breakerIdle,
			StallThreshold:   time.Duration(liveCfg.tuning.SafetyEffectStallSeconds) * time.Second,
			Logf:             log.Printf,
		})
		if err != nil {
			return err
		}
		cfg.Submitter = liveOMS
		cfg.ReconStatus = liveOMS
		cfg.SafetyDriver = liveOMS
		// Heartbeat receipt lands on the Monitor in live mode (watchdog.md
		// WD-8); paper deployments leave the sink nil (WD-3).
		cfg.Heartbeats = monitor
		// The OS-12 liveness read seam (operator-surface.md §Wiring
		// seams): wired iff the Monitor runs AND the watchdog is not
		// disabled; nil renders watchdog.enabled=false with nulls.
		if !watchdogDisabled {
			cfg.Watchdog = monitor
		}
		// Lifecycle seams (lifecycle-api.md §Wiring seams): the live OMS
		// is the pause entry-canceler; exchange credentials satisfy the
		// LC-8 guard input; PaperSubmitter stays false — the live-mode
		// paper floor (LC-14a).
		cfg.EntryCanceler = liveOMS
		cfg.ExchangeKeysConfigured = true
	case os.Getenv("CONTROLPLANE_FILL_MODEL") != "":
		raw := os.Getenv("CONTROLPLANE_FILL_MODEL")
		if limits == nil {
			return errors.New("CONTROLPLANE_FILL_MODEL requires CONTROLPLANE_RISK_LIMITS (allocated_capital_quote seeds equity)")
		}
		var fm paper.FillModel
		if err := json.Unmarshal([]byte(raw), &fm); err != nil {
			return fmt.Errorf("CONTROLPLANE_FILL_MODEL: %w", err)
		}
		bridge, err = omsbridge.New(omsbridge.Config{
			Store:                 st,
			Marks:                 marks,
			FillModel:             fm,
			AllocatedCapitalQuote: limits.AllocatedCapitalQuote,
		})
		if err != nil {
			return err
		}
		cfg.Submitter = bridge
		// The paper bridge is the Submitter (LC-14a) and the LC-12a pause
		// entry-canceler.
		cfg.PaperSubmitter = true
		cfg.EntryCanceler = bridge
	}

	handler := api.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if liveOMS != nil {
		// Run drives the mandatory startup reconcile, the user-data
		// stream, and the periodic reconcile until shutdown.
		go func() {
			if err := liveOMS.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("live OMS: %v", err)
			}
		}()
		// The breaker monitor runs iff CONTROLPLANE_OMS_MODE=live and
		// stops with server shutdown (safety-wiring.md §Lifecycle).
		go monitor.Run(ctx)
	}

	if len(symbols) > 0 {
		market := marketdata.MarketSpot // v1 scope: spot markets (risk-limits.md)
		if v := os.Getenv("CONTROLPLANE_BINANCE_MARKET"); v != "" {
			market = marketdata.Market(v)
		}
		// Optional endpoint overrides (docs/specs/market-data.md §Endpoint
		// overrides): market-data-only mirrors (data-api.binance.vision /
		// data-stream.binance.vision) or testnets; empty means production.
		feed, err := marketdata.NewBinanceFeed(marketdata.BinanceConfig{
			Market:  market,
			WSURL:   os.Getenv("CONTROLPLANE_BINANCE_WS_URL"),
			RESTURL: os.Getenv("CONTROLPLANE_BINANCE_REST_URL"),
		})
		if err != nil {
			return err
		}
		var sweep func(map[string]decimal.Decimal) error
		if bridge != nil {
			sweep = bridge.Sweep
		}
		go func() {
			if err := omsbridge.RunFeedWriter(ctx, feed, symbols, marks, sweep, log.Printf); err != nil && ctx.Err() == nil {
				log.Printf("feed writer: %v", err)
			}
		}()
	}

	sweep := func() {
		expired, err := st.ExpirePendingApprovals(time.Now())
		if err != nil {
			log.Printf("pending-approval sweep: %v", err)
		}
		for _, a := range expired {
			log.Printf("pending approval expired: verdict %s -> timeout", a.VerdictID)
		}
	}
	sweep() // startup sweep: restart-safe default-deny
	go func() {
		ticker := time.NewTicker(sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()

	if backup != nil && backupCfg.interval > 0 {
		// Start-anchored periodic backup loop (ops-backup.md OB-10):
		// first run one interval after boot, cancelled with the serve
		// context; failures log LOUDLY and the loop continues at cadence;
		// an in-progress backup is skipped, never queued (OB-6a).
		go func() {
			ticker := time.NewTicker(backupCfg.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					res, err := backup.Run(ctx)
					switch {
					case errors.Is(err, store.ErrBackupInProgress):
						log.Printf("periodic backup: another backup in progress, skipping this tick")
					case err != nil:
						log.Printf("BACKUP FAILED: periodic backup: %v", err)
					default:
						log.Printf("periodic backup: artifact %s sha256 %s bytes %d duration %s",
							res.Artifact, res.SHA256, res.Bytes, res.FinishedAt.Sub(res.StartedAt))
					}
				}
			}
		}()
	}

	addr := os.Getenv("CONTROLPLANE_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("control-plane API listening on %s", addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// backupEngine adapts the store engine to api.BackupEngine (ops-backup.md
// §Wiring seams): dir and retain are captured from the OB-8 env once here;
// the api/store packages receive plain values (OB-8a).
type backupEngine struct {
	st     *store.Store
	dir    string
	retain int
}

func (b *backupEngine) Run(ctx context.Context) (store.BackupResult, error) {
	return b.st.Backup(ctx, b.dir, b.retain)
}

func (b *backupEngine) List() ([]store.BackupInfo, error) {
	return b.st.ListBackups(b.dir)
}

// parseAgentTokens parses "strategy_id=token,strategy_id=token". Values are
// secrets: never logged, in errors or otherwise.
func parseAgentTokens(v string) (map[string]string, error) {
	if v == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(v, ",") {
		strategyID, token, ok := strings.Cut(pair, "=")
		if !ok || strategyID == "" || token == "" {
			return nil, errors.New("CONTROLPLANE_AGENT_TOKENS: entries must be strategy_id=token")
		}
		out[strategyID] = token
	}
	return out, nil
}

func run() error {
	raw, err := os.ReadFile(filepath.Join(contract.FixturesDir(), "proposal_open_long.json"))
	if err != nil {
		return fmt.Errorf("load fixture: %w", err)
	}
	var p contract.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse proposal: %w", err)
	}

	limits := riskgate.RiskLimits{
		SymbolWhitelist:             []string{"BTC/USDT", "ETH/USDT"},
		MaxOpenPositions:            3,
		PerPositionNotionalCapQuote: decimal.RequireFromString("2000.00"),
		DailyLossLimitQuote:         decimal.RequireFromString("500.00"),
		MaxDrawdownPct:              decimal.NewFromInt(10),
		MaxLossAtStopQuote:          decimal.RequireFromString("50.00"),
		MinStopDistancePct:          decimal.RequireFromString("0.1"),
		MaxStopDistancePct:          decimal.NewFromInt(25),
		MaxOrdersPerMinute:          6,
		RequireStopLoss:             true,
		AllocatedCapitalQuote:       decimal.NewFromInt(10000),
		AccountingQuote:             "USDT",
		StalenessThresholdSeconds:   60,
		L1ApprovalTimeoutSeconds:    600,
	}
	state := riskgate.RuntimeState{
		Autonomy:              riskgate.AutonomyL3,
		EquityQuote:           decimal.NewFromInt(10000),
		PeakEquityQuote:       decimal.NewFromInt(10000),
		DailyRealizedPnLQuote: decimal.Zero,
		MarkPrice:             decimal.RequireFromString("64180.10"),
	}
	// The control-plane clock is authoritative for staleness; the demo
	// anchors it just after the fixture's created_at to stay deterministic.
	now := p.CreatedAt.Time().Add(2 * time.Second)

	gate := riskgate.NewService()
	verdict, err := gate.Evaluate(&p, limits, state, now)
	if err != nil {
		return fmt.Errorf("gate: %w", err)
	}
	out, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("proposal %s (%s %s %s) ->\n%s\n", p.ProposalID, p.Action, p.Symbol, p.SizeQuote, out)
	return nil
}
