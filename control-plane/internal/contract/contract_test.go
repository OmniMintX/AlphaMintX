package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(FixturesDir(), name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// assertSemanticEqual compares two JSON documents after decoding, so field
// order is irrelevant but every field, string form of every decimal, and
// every timestamp must survive the round-trip.
func assertSemanticEqual(t *testing.T, original, remarshaled []byte) {
	t.Helper()
	var want, got any
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if err := json.Unmarshal(remarshaled, &got); err != nil {
		t.Fatalf("decode remarshaled: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\noriginal:    %s\nremarshaled: %s", original, remarshaled)
	}
}

func TestProposalFixturesRoundTrip(t *testing.T) {
	for _, name := range []string{"proposal_open_long.json", "proposal_hold.json", "proposal_decimal_edges.json"} {
		t.Run(name, func(t *testing.T) {
			raw := readFixture(t, name)
			var p Proposal
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if vs := p.Validate(); len(vs) != 0 {
				t.Fatalf("golden fixture must validate, got violations: %+v", vs)
			}
			out, err := json.Marshal(&p)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			assertSemanticEqual(t, raw, out)
		})
	}
}

func TestVerdictFixtureRoundTrip(t *testing.T) {
	raw := readFixture(t, "verdict_reject_daily_loss.json")
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Decision != DecisionReject {
		t.Errorf("decision = %q, want reject", v.Decision)
	}
	if len(v.Reasons) != 1 || v.Reasons[0].Code != CodeDailyLossLimitBreached {
		t.Errorf("reasons = %+v, want single DAILY_LOSS_LIMIT_BREACHED", v.Reasons)
	}
	if got := v.LimitsSnapshot.DailyRealizedPnlQuote.String(); got != "-512.40" {
		t.Errorf("daily_realized_pnl_quote = %q, want string form preserved", got)
	}
	out, err := json.Marshal(&v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSemanticEqual(t, raw, out)
}

// The decimal-edges fixture (SS-26) pins the boundary string forms: 34-char
// maximum, minimal positive value, integers with no fractional part, and "0".
func TestDecimalEdgesFixtureStringForms(t *testing.T) {
	raw := readFixture(t, "proposal_decimal_edges.json")
	var p Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := p.SizeQuote.String(); len(got) != 34 || got != "10000.0000000000000000000000000001" {
		t.Errorf("size_quote = %q, want 34-char maximum form preserved", got)
	}
	if p.StopLoss == nil || p.StopLoss.String() != "0.00000001" {
		t.Errorf("stop_loss = %v, want minimal positive form preserved", p.StopLoss)
	}
	if p.TakeProfit == nil || p.TakeProfit.String() != "9999999999999999999999999999999999" {
		t.Errorf("take_profit = %v, want 34-digit integer form preserved", p.TakeProfit)
	}
	if len(p.ModelCosts) != 1 || p.ModelCosts[0].CostUSD.String() != "0" {
		t.Errorf("model_costs = %+v, want single entry with cost_usd \"0\" preserved", p.ModelCosts)
	}
}

func TestVerdictClipFixtureRoundTrip(t *testing.T) {
	raw := readFixture(t, "verdict_clip.json")
	var v Verdict
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Decision != DecisionClip {
		t.Errorf("decision = %q, want clip", v.Decision)
	}
	if v.ClippedSizeQuote == nil || v.ClippedSizeQuote.String() != "1200.00" {
		t.Errorf("clipped_size_quote = %v, want string form 1200.00 preserved", v.ClippedSizeQuote)
	}
	if len(v.Reasons) != 1 || v.Reasons[0].Code != CodeNotionalCapClipped {
		t.Errorf("reasons = %+v, want single NOTIONAL_CAP_CLIPPED", v.Reasons)
	}
	out, err := json.Marshal(&v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertSemanticEqual(t, raw, out)
}

// The invalid golden fixture violates exactly one rule: the stop_loss
// conditional for open_long (proposal-contract.md, Golden fixtures).
func TestInvalidNoStopLossFixture(t *testing.T) {
	raw := readFixture(t, "proposal_invalid_no_sl.json")
	var p Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	vs := p.Validate()
	if len(vs) != 1 {
		t.Fatalf("want exactly 1 violation, got %d: %+v", len(vs), vs)
	}
	if vs[0].Code != CodeMissingStopLoss {
		t.Errorf("violation code = %q, want %q", vs[0].Code, CodeMissingStopLoss)
	}
}

// The numeric-size fixture pins ADR-0003: money fields are decimal strings,
// never JSON numbers; decoding itself must fail on the JSON number.
func TestInvalidNumericSizeFixture(t *testing.T) {
	raw := readFixture(t, "proposal_invalid_numeric_size.json")
	var p Proposal
	err := json.Unmarshal(raw, &p)
	if err == nil {
		t.Fatal("unmarshal accepted size_quote as a JSON number")
	}
	if !strings.Contains(err.Error(), "decimal field must be a JSON string") {
		t.Errorf("error = %q, want decimal-must-be-string parse failure", err)
	}
}

// Length caps count Unicode code points (JSON Schema maxLength semantics),
// not bytes: text at the cap must validate even when every rune is multi-byte.
func TestValidateLengthCapsCountRunes(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		char  string
		set   func(p *Proposal, s string)
	}{
		{"reasoning", 8000, "ệ", func(p *Proposal, s string) { p.Reasoning = s }},
		{"reasoning astral", 8000, "😀", func(p *Proposal, s string) { p.Reasoning = s }},
		{"debate summary", 4000, "ệ", func(p *Proposal, s string) { p.DebateSummary = s }},
		{"analyst summary", 2000, "ệ", func(p *Proposal, s string) { p.AnalystSummaries.Market.Summary = s }},
		{"model cost node", 64, "ệ", func(p *Proposal, s string) { p.ModelCosts[0].Node = s }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := readFixture(t, "proposal_open_long.json")
			var p Proposal
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tc.set(&p, strings.Repeat(tc.char, tc.limit))
			if vs := p.Validate(); len(vs) != 0 {
				t.Errorf("%d runes at the cap must validate, got violations: %+v", tc.limit, vs)
			}
			tc.set(&p, strings.Repeat(tc.char, tc.limit+1))
			vs := p.Validate()
			if len(vs) != 1 || vs[0].Code != CodeSchemaInvalid {
				t.Errorf("%d runes want single SCHEMA_INVALID violation, got %+v", tc.limit+1, vs)
			}
		})
	}
}

func TestDecimalStringForm(t *testing.T) {
	for _, bad := range []string{"", ".5", "05", "1e5", "+1", "-3", "1.", "00.5"} {
		if _, err := ParseDecimal(bad); err == nil {
			t.Errorf("ParseDecimal(%q) accepted invalid form", bad)
		}
	}
	d, err := ParseDecimal("1500.00")
	if err != nil {
		t.Fatalf("ParseDecimal: %v", err)
	}
	if d.String() != "1500.00" {
		t.Errorf("string form not preserved: %q", d.String())
	}
	if !d.Decimal().Equal(decimal.NewFromInt(1500)) {
		t.Errorf("decimal value mismatch: %s", d.Decimal())
	}
	if _, err := ParseSignedDecimal("-512.40"); err != nil {
		t.Errorf("ParseSignedDecimal rejected valid signed form: %v", err)
	}
	if _, err := ParseDecimal("-512.40"); err == nil {
		t.Error("ParseDecimal accepted negative value")
	}
}

func TestUTCTimeStringForm(t *testing.T) {
	for _, bad := range []string{"", "2026-07-04T12:00:00+05:00", "2026-07-04 12:00:00Z"} {
		if _, err := ParseUTCTime(bad); err == nil {
			t.Errorf("ParseUTCTime(%q) accepted invalid form", bad)
		}
	}
	u, err := ParseUTCTime("2026-07-04T12:00:00.5Z")
	if err != nil {
		t.Fatalf("ParseUTCTime: %v", err)
	}
	if u.String() != "2026-07-04T12:00:00.5Z" {
		t.Errorf("raw form not preserved: %q", u.String())
	}
}
