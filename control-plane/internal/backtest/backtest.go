// Package backtest implements the Phase 2 backtest engine of
// docs/specs/backtest-engine.md: it replays a historical kline dataset plus
// an agent-plane proposals.jsonl through the IDENTICAL Risk Gate + paper OMS
// path as the e2e harness (internal/e2e is the pattern), under a virtual
// candle clock with sub-tick pumping. Same dataset (sha256) + same seed ⇒
// byte-identical records. No wall clock, no random ids, no network.
package backtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// NamespaceBacktest = uuid5(NAMESPACE_URL, "https://alphamintx.dev/backtest");
// shared with the Python emitter so both planes derive the same deterministic
// ids (backtest-engine.md §Determinism).
var NamespaceBacktest = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://alphamintx.dev/backtest"))

// DeterministicID derives a version-5 UUID in the shared backtest namespace.
func DeterministicID(name string) string {
	return uuid.NewSHA1(NamespaceBacktest, []byte(name)).String()
}

// intervalSeconds is the closed set of legal kline intervals and their
// candle durations in seconds.
var intervalSeconds = map[string]int64{
	"1m": 60, "3m": 180, "5m": 300, "15m": 900, "30m": 1800,
	"1h": 3600, "2h": 7200, "4h": 14400, "6h": 21600, "8h": 28800,
	"12h": 43200, "1d": 86400,
}

// IntervalSeconds returns the candle duration in seconds for a legal
// interval, or an error for an unknown one.
func IntervalSeconds(interval string) (int64, error) {
	s, ok := intervalSeconds[interval]
	if !ok {
		return 0, fmt.Errorf("backtest: unknown interval %q", interval)
	}
	return s, nil
}

// maskLevels are the EMITTER mask modes (backtest-engine.md §Lookahead):
// M2 is a checker mode over an existing emit, never an emitter mode, so an
// M2 run row cannot exist.
var maskLevels = map[string]bool{"M0": true, "M1": true}

var (
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// The contract's canonical BASE/QUOTE bounds (contract/proposal.go,
	// mirrored by the Python SymbolStr) — one rule on both planes.
	symbolPattern = regexp.MustCompile(`^[A-Z0-9]{2,15}/[A-Z0-9]{2,10}$`)
)

// RunSpec is the backtest runspec file: the single source of truth for one
// replay. Like the e2e runspec, fill_model and max_age_seconds are REQUIRED
// (no hidden defaults), and limits carries the full RiskLimits the gate
// evaluates (CONTROLPLANE_RISK_LIMITS JSON shape).
type RunSpec struct {
	StrategyID    string          `json:"strategy_id"`
	Symbol        string          `json:"symbol"`
	Interval      string          `json:"interval"`
	MaskLevel     string          `json:"mask_level"`
	Seed          int64           `json:"seed"`
	QuoteCurrency string          `json:"quote_currency"`
	FillModel     paper.FillModel `json:"fill_model"`
	MaxAgeSeconds int64           `json:"max_age_seconds"`
	Limits        limitsConfig    `json:"limits"`

	// ConfigHash is the sha256 of the runspec file bytes (recorded in
	// backtest_runs and folded into the backtest_id).
	ConfigHash string `json:"-"`
	// ParsedLimits is the validated RiskLimits form of Limits.
	ParsedLimits riskgate.RiskLimits `json:"-"`
}

// LoadRunSpec reads and validates a backtest runspec file, recording the
// sha256 of its exact bytes as the config hash.
func LoadRunSpec(path string) (*RunSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec RunSpec
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse runspec %s: %w", path, err)
	}
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("runspec %s: %w", path, err)
	}
	sum := sha256.Sum256(b)
	spec.ConfigHash = hex.EncodeToString(sum[:])
	return &spec, nil
}

func (s *RunSpec) validate() error {
	if !uuidPattern.MatchString(s.StrategyID) {
		return fmt.Errorf("strategy_id %q is not a lowercase-hex uuid", s.StrategyID)
	}
	if !symbolPattern.MatchString(s.Symbol) {
		return fmt.Errorf("symbol %q is not canonical BASE/QUOTE", s.Symbol)
	}
	ivl, err := IntervalSeconds(s.Interval)
	if err != nil {
		return err
	}
	if !maskLevels[s.MaskLevel] {
		return fmt.Errorf("mask_level %q not in {M0, M1}", s.MaskLevel)
	}
	if s.QuoteCurrency == "" {
		return fmt.Errorf("quote_currency is REQUIRED")
	}
	if s.FillModel.MarketSlippageBps == "" || s.FillModel.TakerFeeBps == "" || s.FillModel.MakerFeeBps == "" {
		return fmt.Errorf("fill_model with market_slippage_bps, taker_fee_bps and maker_fee_bps is REQUIRED (no hidden defaults)")
	}
	// The healthy-feed mark age at decision time is 1s by construction and a
	// gapped candle leaves the last mark interval+1s old, so the staleness
	// bound must sit in [1, interval] for the fail-closed gap rejects
	// (MARK_PRICE_UNAVAILABLE) to remain reachable.
	if s.MaxAgeSeconds < 1 || s.MaxAgeSeconds > ivl {
		return fmt.Errorf("max_age_seconds %d must be in [1, %d] (interval %s)", s.MaxAgeSeconds, ivl, s.Interval)
	}
	limits, err := s.Limits.riskLimits()
	if err != nil {
		return err
	}
	s.ParsedLimits = limits
	return nil
}
