package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
)

// ReasonStrategyScopeMismatch is the ingestion-level auth rejection: token
// does not authenticate the proposal's strategy_id. Auth failures never
// produce verdicts (docs/specs/risk-limits.md, gate step 0).
const ReasonStrategyScopeMismatch = "STRATEGY_SCOPE_MISMATCH"

// ScenarioCloseExempt names the strategy whose runtime state is pre-seeded
// with an open BTC/USDT position and a breached daily loss, proving the exit
// exemption (gate step 3): its close proposal MUST still approve.
const ScenarioCloseExempt = "close_exempt"

// OutcomeDetail pins one scenario's result: the decision (or ingestion
// outcome), the primary (first) reason code — empty means the verdict MUST
// carry no reasons — and the clipped size for clip verdicts.
type OutcomeDetail struct {
	Outcome          string
	PrimaryReason    string
	ClippedSizeQuote string
}

func (d OutcomeDetail) String() string {
	s := d.Outcome
	if d.PrimaryReason != "" {
		s += "/" + d.PrimaryReason
	}
	if d.ClippedSizeQuote != "" {
		s += "/clipped=" + d.ClippedSizeQuote
	}
	return s
}

// ExpectedOutcome maps each interface-contract scenario to the decision (or
// ingestion outcome) AND primary reason code the run MUST produce, so a
// scenario rejecting for the wrong reason fails the run.
var ExpectedOutcome = map[string]OutcomeDetail{
	"bullish_btc_l3":      {Outcome: "approve"},
	"low_confidence_hold": {Outcome: "approve"},
	"whitelist_violation": {Outcome: "reject", PrimaryReason: contract.CodeSymbolNotWhitelisted},
	"notional_clip":       {Outcome: "clip", PrimaryReason: contract.CodeNotionalCapClipped, ClippedSizeQuote: "2000"},
	ScenarioCloseExempt:   {Outcome: "approve"},
	"stale_proposal":      {Outcome: "reject", PrimaryReason: contract.CodeProposalStale},
	"scope_mismatch":      {Outcome: "rejected_submission", PrimaryReason: ReasonStrategyScopeMismatch},
}

// Outcome is the per-strategy result of a run, in runspec order.
type Outcome struct {
	Scenario   string
	StrategyID string
	Expected   OutcomeDetail
	Got        OutcomeDetail
}

// OK reports whether the scenario produced its expected outcome.
func (o Outcome) OK() bool { return o.Expected.Outcome != "" && o.Expected == o.Got }

// runLimits is the fixed RiskLimits embedded for the run, tuned so the seven
// interface-contract scenarios land on their intended decisions.
func runLimits() riskgate.RiskLimits {
	return riskgate.RiskLimits{
		SymbolWhitelist:             []string{"BTC/USDT", "ETH/USDT"},
		MaxOpenPositions:            3,
		PerPositionNotionalCapQuote: decimal.NewFromInt(2000),
		DailyLossLimitQuote:         decimal.NewFromInt(500),
		MaxDrawdownPct:              decimal.NewFromInt(10),
		MaxLossAtStopQuote:          decimal.NewFromInt(450),
		MinStopDistancePct:          decimal.RequireFromString("0.1"),
		MaxStopDistancePct:          decimal.NewFromInt(25),
		MaxOrdersPerMinute:          60,
		RequireStopLoss:             true,
		AllocatedCapitalQuote:       decimal.NewFromInt(10000),
		AccountingQuote:             "USDT",
		StalenessThresholdSeconds:   riskgate.DefaultStalenessThresholdSeconds,
		L1ApprovalTimeoutSeconds:    600,
	}
}

// strategyState is the fixed runtime state per strategy for the run.
type strategyState struct {
	equity        decimal.Decimal
	peak          decimal.Decimal
	dailyPnL      decimal.Decimal
	openPositions int
}

// Run replays the proposal envelopes against a fresh gate and paper OMS,
// writing records to out. Proposal i (0-based line index) is evaluated at
// clock_start + i*tick_seconds + 1s — never at wall time. The stale scenario
// relies on the index-based clock: its created_at lags the loop clock.
func Run(spec *RunSpec, proposals io.Reader, out io.Writer) ([]Outcome, error) {
	marks, err := parseMarks(spec)
	if err != nil {
		return nil, err
	}
	limits := runLimits()
	gate := riskgate.NewService()
	oms := paper.New()

	tokens := make(map[string]string, len(spec.Strategies))
	states := make(map[string]*strategyState, len(spec.Strategies))
	for _, s := range spec.Strategies {
		tokens[s.StrategyID] = s.Token
		states[s.StrategyID] = &strategyState{
			equity:   decimal.NewFromInt(10000),
			peak:     decimal.NewFromInt(10000),
			dailyPnL: decimal.Zero,
		}
	}
	if err := seedCloseExempt(spec, states, oms); err != nil {
		return nil, err
	}

	got := make(map[string]OutcomeDetail, len(spec.Strategies))
	scanner := bufio.NewScanner(proposals)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	index := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("proposals line %d: %w", index+1, err)
		}
		p := &env.Proposal

		// Ingestion auth: the envelope token must be the token issued to
		// proposal.strategy_id. Mismatch or unknown strategy is rejected
		// WITHOUT a verdict (auth failures never produce verdicts).
		if expected, ok := tokens[p.StrategyID]; !ok || env.Token != expected {
			if err := writeRecord(out, rejectedSubmissionRecord{
				Kind:       "rejected_submission",
				StrategyID: p.StrategyID,
				ProposalID: p.ProposalID,
				ReasonCode: ReasonStrategyScopeMismatch,
			}); err != nil {
				return nil, err
			}
			got[p.StrategyID] = OutcomeDetail{Outcome: "rejected_submission", PrimaryReason: ReasonStrategyScopeMismatch}
			index++
			continue
		}

		now := spec.ClockStart.Time().Add(time.Duration(index*spec.TickSeconds)*time.Second + time.Second)
		mark := markAt(marks, p.Symbol, index)
		if err := evaluateOne(gate, oms, limits, states[p.StrategyID], p, mark, now, out, got); err != nil {
			return nil, err
		}
		index++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	outcomes := make([]Outcome, 0, len(spec.Strategies))
	for _, s := range spec.Strategies {
		outcomes = append(outcomes, Outcome{
			Scenario:   s.Scenario,
			StrategyID: s.StrategyID,
			Expected:   ExpectedOutcome[s.Scenario],
			Got:        got[s.StrategyID],
		})
	}
	return outcomes, nil
}
