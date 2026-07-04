// Command controlplane is the Phase-0 demo loop: load a golden fixture
// proposal, evaluate it through the deterministic Risk Gate, and print the
// resulting RiskVerdict JSON.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
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
