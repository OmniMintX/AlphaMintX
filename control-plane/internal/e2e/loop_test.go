package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

const (
	testStrategy1 = "e2e00000-0000-4000-8000-0000000000a1"
	testStrategy2 = "e2e00000-0000-4000-8000-0000000000a2"
	testStrategy3 = "e2e00000-0000-4000-8000-0000000000a3"
	testUnknown   = "e2e00000-0000-4000-8000-0000000000ff"
)

func mustDec(t *testing.T, s string) contract.Decimal {
	t.Helper()
	d, err := contract.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return d
}

func decPtr(t *testing.T, s string) *contract.Decimal {
	t.Helper()
	d := mustDec(t, s)
	return &d
}

func mustTime(t *testing.T, s string) contract.UTCTime {
	t.Helper()
	u, err := contract.ParseUTCTime(s)
	if err != nil {
		t.Fatalf("ParseUTCTime(%q): %v", s, err)
	}
	return u
}

func summary() contract.AnalystSummary {
	return contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "s"}
}

// openProposal: open_long BTC/USDT 1000 at limit 100, stop 98 (2% distance,
// worst_case 20), tp 105 — approved and filled at the tick-0 mark 100.
func openProposal(t *testing.T, strategyID, proposalID, createdAt string) *contract.Proposal {
	t.Helper()
	return &contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    proposalID,
		StrategyID:    strategyID,
		AgentTraceID:  "c3d4e5f6-a7b8-4c9d-8e0f-2a3b4c5d6e7f",
		CreatedAt:     mustTime(t, createdAt),
		Symbol:        "BTC/USDT",
		Action:        contract.ActionOpenLong,
		SizeQuote:     mustDec(t, "1000"),
		Entry:         contract.Entry{Type: "limit", LimitPrice: decPtr(t, "100")},
		StopLoss:      decPtr(t, "98"),
		TakeProfit:    decPtr(t, "105"),
		TimeInForce:   "gtc",
		Confidence:    0.7,
		Reasoning:     "test",
		AnalystSummaries: contract.AnalystSummaries{
			Market: summary(), News: summary(), Fundamental: summary(),
		},
		DebateSummary: "d",
		ModelCosts:    []contract.ModelCost{},
	}
}

// closeProposal: close BTC/USDT, no stop/tp — flattens the pre-seeded
// close_exempt position (seedCloseExempt seeds BTC/USDT).
func closeProposal(t *testing.T, strategyID, proposalID, createdAt string) *contract.Proposal {
	t.Helper()
	p := openProposal(t, strategyID, proposalID, createdAt)
	p.Symbol = "BTC/USDT"
	p.Action = contract.ActionClose
	p.SizeQuote = mustDec(t, "0")
	p.Entry = contract.Entry{Type: "market"}
	p.StopLoss = nil
	p.TakeProfit = nil
	return p
}

func testSpec(t *testing.T) *RunSpec {
	t.Helper()
	return &RunSpec{
		ClockStart:    mustTime(t, "2026-07-04T12:00:00Z"),
		TickSeconds:   60,
		Seed:          42,
		QuoteCurrency: "USDT",
		Strategies: []Strategy{
			{StrategyID: testStrategy1, Token: "e2e-token-1", Scenario: "bullish_btc_l3"},
			{StrategyID: testStrategy2, Token: "e2e-token-2", Scenario: ScenarioCloseExempt},
			{StrategyID: testStrategy3, Token: "e2e-token-3", Scenario: "scope_mismatch"},
		},
		Marks: map[string][]string{
			"BTC/USDT": {"100", "100", "100", "100"},
			"ETH/USDT": {"3400", "3400", "3400", "3400"},
		},
	}
}

// testEnvelopes: line 0 approves and fills, line 1 is the exempt close, line
// 2 carries strategy 1's token with strategy 3's proposal (scope mismatch),
// line 3 is an unknown strategy_id.
func testEnvelopes(t *testing.T) string {
	t.Helper()
	envs := []Envelope{
		{Token: "e2e-token-1", Proposal: *openProposal(t, testStrategy1, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c01", "2026-07-04T12:00:00Z")},
		{Token: "e2e-token-2", Proposal: *closeProposal(t, testStrategy2, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c02", "2026-07-04T12:01:00Z")},
		{Token: "e2e-token-1", Proposal: *openProposal(t, testStrategy3, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c03", "2026-07-04T12:02:00Z")},
		{Token: "e2e-token-9", Proposal: *openProposal(t, testUnknown, "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c04", "2026-07-04T12:03:00Z")},
	}
	var b strings.Builder
	for _, env := range envs {
		line, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func runOnce(t *testing.T) ([]Outcome, []byte) {
	t.Helper()
	var out bytes.Buffer
	outcomes, err := Run(testSpec(t), strings.NewReader(testEnvelopes(t)), &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return outcomes, out.Bytes()
}

// decodeRecords parses records.jsonl into generic maps, asserting LF framing
// and a trailing newline.
func decodeRecords(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("records output must end with a trailing LF")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	records := make([]map[string]any, 0, len(lines))
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("records line %d: %v", i+1, err)
		}
		records = append(records, rec)
	}
	return records
}

func TestRunIsByteDeterministic(t *testing.T) {
	outcomes, first := runOnce(t)
	_, second := runOnce(t)
	if !bytes.Equal(first, second) {
		t.Fatalf("two runs over identical inputs differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	for _, o := range outcomes {
		if !o.OK() {
			t.Errorf("scenario %s: got %v, want %v", o.Scenario, o.Got, o.Expected)
		}
	}
}

// TestExpectedOutcomePinsReasonCodes guards the per-scenario reason-code
// pinning: a scenario landing on the right decision for the WRONG reason
// (e.g. whitelist_violation rejecting MARK_PRICE_UNAVAILABLE instead of
// SYMBOL_NOT_WHITELISTED) must fail the run.
func TestExpectedOutcomePinsReasonCodes(t *testing.T) {
	want := map[string]OutcomeDetail{
		"bullish_btc_l3":      {Outcome: "approve"},
		"low_confidence_hold": {Outcome: "approve"},
		"whitelist_violation": {Outcome: "reject", PrimaryReason: contract.CodeSymbolNotWhitelisted},
		"notional_clip":       {Outcome: "clip", PrimaryReason: contract.CodeNotionalCapClipped, ClippedSizeQuote: "2000"},
		ScenarioCloseExempt:   {Outcome: "approve"},
		"stale_proposal":      {Outcome: "reject", PrimaryReason: contract.CodeProposalStale},
		"scope_mismatch":      {Outcome: "rejected_submission", PrimaryReason: ReasonStrategyScopeMismatch},
	}
	for scenario, expected := range want {
		if got := ExpectedOutcome[scenario]; got != expected {
			t.Errorf("ExpectedOutcome[%s] = %v, want %v", scenario, got, expected)
		}
	}
	// An approve with a stray reason, or a reject for another reason, must
	// not satisfy the expectation.
	if (Outcome{Expected: want["bullish_btc_l3"], Got: OutcomeDetail{Outcome: "approve", PrimaryReason: contract.CodeLowConfidence}}).OK() {
		t.Error("approve with an unexpected reason code must not pass")
	}
	if (Outcome{Expected: want["whitelist_violation"], Got: OutcomeDetail{Outcome: "reject", PrimaryReason: contract.CodeMarkPriceUnavailable}}).OK() {
		t.Error("reject for the wrong reason code must not pass")
	}
	if (Outcome{Expected: want["notional_clip"], Got: OutcomeDetail{Outcome: "clip", PrimaryReason: contract.CodeNotionalCapClipped, ClippedSizeQuote: "1500"}}).OK() {
		t.Error("clip to the wrong size must not pass")
	}
}

// TestTokenScopeMismatchProducesNoVerdict is the Phase-0 exit-criterion test:
// a proposal for a token-scoped strategy the token does not own is rejected
// at ingestion with STRATEGY_SCOPE_MISMATCH and NO verdict is produced (auth
// failures never produce verdicts). Unknown strategy_ids behave the same.
func TestTokenScopeMismatchProducesNoVerdict(t *testing.T) {
	_, raw := runOnce(t)
	records := decodeRecords(t, raw)

	rejected := map[string]bool{}
	for _, rec := range records {
		switch rec["kind"] {
		case "rejected_submission":
			if rec["reason_code"] != ReasonStrategyScopeMismatch {
				t.Errorf("rejected_submission reason_code = %v, want %s", rec["reason_code"], ReasonStrategyScopeMismatch)
			}
			rejected[rec["strategy_id"].(string)] = true
		case "proposal":
			p := rec["proposal"].(map[string]any)
			if id := p["strategy_id"].(string); id == testStrategy3 || id == testUnknown {
				t.Errorf("scope-mismatched strategy %s produced a proposal record", id)
			}
		case "verdict":
			v := rec["verdict"].(map[string]any)
			if id := v["proposal_id"].(string); id == "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c03" || id == "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c04" {
				t.Errorf("scope-mismatched proposal %s produced a verdict (auth failures must not)", id)
			}
		}
	}
	if !rejected[testStrategy3] {
		t.Errorf("token-scope mismatch for %s did not produce a rejected_submission record", testStrategy3)
	}
	if !rejected[testUnknown] {
		t.Errorf("unknown strategy_id %s did not produce a rejected_submission record", testUnknown)
	}
}

// TestCloseExemptionApprovesDespiteBreachedDailyLoss proves gate step 3: the
// close_exempt strategy is pre-seeded with daily_realized_pnl -600 (beyond
// the 500 daily loss limit) and an open BTC/USDT position, yet its close
// approves and flattens reduce-only.
func TestCloseExemptionApprovesDespiteBreachedDailyLoss(t *testing.T) {
	_, raw := runOnce(t)
	records := decodeRecords(t, raw)

	var verdict, order map[string]any
	for i, rec := range records {
		if rec["kind"] != "verdict" {
			continue
		}
		v := rec["verdict"].(map[string]any)
		if v["proposal_id"] != "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c02" {
			continue
		}
		verdict = v
		if i+1 < len(records) && records[i+1]["kind"] == "order" {
			order = records[i+1]
		}
	}
	if verdict == nil {
		t.Fatalf("no verdict record for the close_exempt proposal")
	}
	if verdict["decision"] != "approve" {
		t.Fatalf("close_exempt decision = %v, want approve", verdict["decision"])
	}
	if reasons := verdict["reasons"].([]any); len(reasons) != 0 {
		t.Errorf("close_exempt approve carries reasons %v, want none", reasons)
	}
	snap := verdict["limits_snapshot"].(map[string]any)
	if snap["daily_realized_pnl_quote"] != "-600" {
		t.Errorf("snapshot daily_realized_pnl_quote = %v, want -600 (limit is 500)", snap["daily_realized_pnl_quote"])
	}
	if order == nil {
		t.Fatalf("no order record after the close_exempt verdict")
	}
	if order["reduce_only"] != true || order["status"] != "filled" {
		t.Errorf("close order = %v, want reduce-only filled flatten", order)
	}
}
