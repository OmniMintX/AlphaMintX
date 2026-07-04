// Package e2e implements the deterministic Phase-0 paper loop: it replays a
// proposals.jsonl produced by the agent-plane emitter against the real Risk
// Gate and paper OMS under a fixed runspec clock, writing byte-deterministic
// records.jsonl. No wall clock, no random ids: two runs over the same inputs
// are byte-identical.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
)

// NamespaceE2E = uuid5(NAMESPACE_URL, "https://alphamintx.dev/e2e"); shared
// with the Python emitter so both planes derive the same deterministic ids.
var NamespaceE2E = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://alphamintx.dev/e2e"))

// DeterministicID derives a version-5 UUID in the shared e2e namespace
// (uuid.NewSHA1 produces version-5 UUIDs, accepted by the contracts' uuid
// pattern, which is version-agnostic lowercase hex).
func DeterministicID(name string) string {
	return uuid.NewSHA1(NamespaceE2E, []byte(name)).String()
}

// Strategy is one runspec strategy entry: the paper token authenticating the
// strategy and the scenario it exercises.
type Strategy struct {
	StrategyID string `json:"strategy_id"`
	Token      string `json:"token"`
	Scenario   string `json:"scenario"`
}

// RunSpec is e2e/runspec.json: the single source of truth for a run.
// FillModel and MaxAgeSeconds are REQUIRED (docs/specs/market-data.md
// §Determinism): reproducibility requires the fill and staleness parameters
// explicit in the runspec, never hidden defaults.
type RunSpec struct {
	ClockStart    contract.UTCTime    `json:"clock_start"`
	TickSeconds   int                 `json:"tick_seconds"`
	Seed          int                 `json:"seed"`
	QuoteCurrency string              `json:"quote_currency"`
	FillModel     paper.FillModel     `json:"fill_model"`
	MaxAgeSeconds int                 `json:"max_age_seconds"`
	Strategies    []Strategy          `json:"strategies"`
	Marks         map[string][]string `json:"marks"`
}

// LoadRunSpec reads and validates a runspec file.
func LoadRunSpec(path string) (*RunSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec RunSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parse runspec %s: %w", path, err)
	}
	if spec.TickSeconds <= 0 {
		return nil, fmt.Errorf("runspec %s: tick_seconds must be > 0", path)
	}
	if spec.FillModel.MarketSlippageBps == "" || spec.FillModel.TakerFeeBps == "" || spec.FillModel.MakerFeeBps == "" {
		return nil, fmt.Errorf("runspec %s: fill_model with market_slippage_bps, taker_fee_bps and maker_fee_bps is REQUIRED (no hidden defaults)", path)
	}
	if spec.MaxAgeSeconds <= 0 {
		return nil, fmt.Errorf("runspec %s: max_age_seconds must be > 0 (REQUIRED, no hidden defaults)", path)
	}
	return &spec, nil
}

// parseMarks converts the runspec mark series into decimals once, up front.
func parseMarks(spec *RunSpec) (map[string][]decimal.Decimal, error) {
	marks := make(map[string][]decimal.Decimal, len(spec.Marks))
	for symbol, series := range spec.Marks {
		out := make([]decimal.Decimal, 0, len(series))
		for i, s := range series {
			d, err := decimal.NewFromString(s)
			if err != nil {
				return nil, fmt.Errorf("marks[%s][%d]: %w", symbol, i, err)
			}
			out = append(out, d)
		}
		marks[symbol] = out
	}
	return marks, nil
}

// Envelope is one proposals.jsonl line: the paper auth token plus the
// schema-valid TradeProposal.
type Envelope struct {
	Token    string            `json:"token"`
	Proposal contract.Proposal `json:"proposal"`
}

// Record kinds written to records.jsonl. Field order of the structs below is
// the wire order (encoding/json marshals struct fields in declaration order).
// They are exported for reuse by the backtest engine (internal/backtest),
// which shares this record contract byte-for-byte.

// ProposalRecord is the "proposal" record line.
type ProposalRecord struct {
	Kind     string             `json:"kind"`
	Proposal *contract.Proposal `json:"proposal"`
}

// VerdictRecord is the "verdict" record line.
type VerdictRecord struct {
	Kind    string            `json:"kind"`
	Verdict *contract.Verdict `json:"verdict"`
}

// OrderRecord is the "order" record line.
type OrderRecord struct {
	Kind       string `json:"kind"`
	OrderID    string `json:"order_id"`
	ProposalID string `json:"proposal_id"`
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
	Class      string `json:"class"`
	Side       string `json:"side"`
	Type       string `json:"type"`
	ReduceOnly bool   `json:"reduce_only"`
	QtyBase    string `json:"qty_base"`
	LimitPrice string `json:"limit_price,omitempty"`
	FillPrice  string `json:"fill_price,omitempty"`
	FeeQuote   string `json:"fee_quote,omitempty"`
	Status     string `json:"status"`
}

// PositionRecord is the "position" record line.
type PositionRecord struct {
	Kind       string `json:"kind"`
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
	QtyBase    string `json:"qty_base"`
	EntryPrice string `json:"entry_price"`
}

// RejectedSubmissionRecord is the "rejected_submission" record line.
type RejectedSubmissionRecord struct {
	Kind       string `json:"kind"`
	StrategyID string `json:"strategy_id"`
	ProposalID string `json:"proposal_id"`
	ReasonCode string `json:"reason_code"`
}
