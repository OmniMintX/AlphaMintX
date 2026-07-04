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
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/omsbridge"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/runstate"
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
// Each piece is fail-closed when absent: no limits means no verdicts are
// produced here, no fill model means approvals block SUBMITTER_UNAVAILABLE,
// and no feed means the gate rejects MARK_PRICE_UNAVAILABLE.
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
	}

	limits, err := parseRiskLimits(os.Getenv("CONTROLPLANE_RISK_LIMITS"))
	if err != nil {
		return err
	}
	var hydrator *runstate.Hydrator
	if limits != nil {
		hydrator = &runstate.Hydrator{Store: st, Marks: marks, AllocatedCapitalQuote: limits.AllocatedCapitalQuote}
		cfg.Limits = limits
		cfg.RuntimeState = hydrator
		cfg.DailyLossBreached = func(strategyID string, now time.Time) (bool, error) {
			daily, err := hydrator.DailyPnL(strategyID, now)
			if err != nil {
				return false, err
			}
			return daily.LessThanOrEqual(limits.DailyLossLimitQuote.Neg()), nil
		}
	}

	var bridge *omsbridge.Bridge
	if raw := os.Getenv("CONTROLPLANE_FILL_MODEL"); raw != "" {
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
	}

	handler := api.New(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if symbols := splitSymbols(os.Getenv("CONTROLPLANE_SYMBOLS")); len(symbols) > 0 {
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
