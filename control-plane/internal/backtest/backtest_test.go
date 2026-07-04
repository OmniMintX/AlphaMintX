package backtest

import (
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/e2e"
)

func TestDeterministicIDStableInBacktestNamespace(t *testing.T) {
	a, b := DeterministicID("verdict/x"), DeterministicID("verdict/x")
	if a != b {
		t.Fatalf("DeterministicID not stable: %s vs %s", a, b)
	}
	if a == DeterministicID("verdict/y") {
		t.Fatal("distinct names collided")
	}
	// NAMESPACE_BACKTEST is its own namespace: the same name must not
	// collide with the e2e namespace (backtest-engine.md §Determinism).
	if DeterministicID("verdict/x") == e2e.DeterministicID("verdict/x") {
		t.Fatal("backtest and e2e namespaces collided")
	}
}

func TestLoadRunSpecValidAndHash(t *testing.T) {
	spec := testSpec(t)
	if len(spec.ConfigHash) != 64 {
		t.Errorf("ConfigHash = %q, want 64 hex chars", spec.ConfigHash)
	}
	if spec.ParsedLimits.AccountingQuote != "USDT" || spec.ParsedLimits.MaxOpenPositions != 3 {
		t.Errorf("ParsedLimits = %+v", spec.ParsedLimits)
	}
	if spec.ParsedLimits.AllocatedCapitalQuote.String() != "10000" {
		t.Errorf("allocated = %s, want 10000", spec.ParsedLimits.AllocatedCapitalQuote)
	}
	if s, err := IntervalSeconds(spec.Interval); err != nil || s != 60 {
		t.Errorf("IntervalSeconds(1m) = %d, %v", s, err)
	}
}

func TestLoadRunSpecRejections(t *testing.T) {
	tests := []struct {
		name     string
		old, new string
		wantErr  string
	}{
		{"bad strategy uuid", testStrategyID, "not-a-uuid", "strategy_id"},
		{"venue symbol", `"symbol": "BTC/USDT"`, `"symbol": "BTCUSDT"`, "canonical"},
		{"unknown interval", `"interval": "1m"`, `"interval": "7m"`, "unknown interval"},
		{"bad mask level", `"mask_level": "M0"`, `"mask_level": "M3"`, "mask_level"},
		{"M2 is a checker mode, not a run mode", `"mask_level": "M0"`, `"mask_level": "M2"`, "mask_level"},
		{"missing quote currency", `"quote_currency": "USDT"`, `"quote_currency": ""`, "quote_currency"},
		{"missing fill model field", `"taker_fee_bps": "0"`, `"taker_fee_bps": ""`, "fill_model"},
		{"max_age above interval", `"max_age_seconds": 60`, `"max_age_seconds": 61`, "max_age_seconds"},
		{"max_age zero", `"max_age_seconds": 60`, `"max_age_seconds": 0`, "max_age_seconds"},
		{"missing accounting quote", `"accounting_quote": "USDT"`, `"accounting_quote": ""`, "accounting_quote"},
		{"missing required money field", `"per_position_notional_cap_quote": "2000",`, ``, "per_position_notional_cap_quote"},
		{"unknown field", `"seed": 42`, `"seed": 42, "surprise": 1`, "surprise"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(testRunSpecJSON, tc.old, tc.new, 1)
			if raw == testRunSpecJSON {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			_, err := loadSpec(t, raw)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadRunSpec error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
