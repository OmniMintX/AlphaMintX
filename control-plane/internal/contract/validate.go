package contract

import (
	"unicode/utf8"

	"github.com/shopspring/decimal"
)

// Validate enforces the beyond-JSON-Schema rules of
// docs/specs/proposal-contract.md that are decidable from the proposal alone.
// Runtime-dependent rules (staleness, stop placement vs mark for market
// entries) belong to the Risk Gate. Returns all violations found; empty means
// valid.
func (p *Proposal) Validate() []Violation {
	var vs []Violation

	if p.SchemaVersion != SchemaVersion {
		vs = append(vs, violation(CodeUnsupportedSchemaVersion,
			"unsupported schema_version %q (supported: %q)", p.SchemaVersion, SchemaVersion))
	}
	for _, f := range []struct{ name, val string }{
		{"proposal_id", p.ProposalID},
		{"strategy_id", p.StrategyID},
		{"agent_trace_id", p.AgentTraceID},
	} {
		if !uuidPattern.MatchString(f.val) {
			vs = append(vs, violation(CodeSchemaInvalid, "%s %q is not a lowercase UUID", f.name, f.val))
		}
	}
	if p.CreatedAt.String() == "" {
		vs = append(vs, violation(CodeSchemaInvalid, "created_at is missing or unparseable"))
	}
	// Rule 8: canonical BASE/QUOTE uppercase; reject, never normalize.
	if !symbolPattern.MatchString(p.Symbol) {
		vs = append(vs, violation(CodeSchemaInvalid, "symbol %q is not canonical BASE/QUOTE uppercase", p.Symbol))
	}
	switch p.Action {
	case ActionOpenLong, ActionOpenShort, ActionClose, ActionHold:
	default:
		vs = append(vs, violation(CodeSchemaInvalid, "unknown action %q", p.Action))
	}
	if p.TimeInForce != "gtc" && p.TimeInForce != "ioc" {
		vs = append(vs, violation(CodeSchemaInvalid, "time_in_force %q must be gtc or ioc", p.TimeInForce))
	}
	if p.Confidence < 0 || p.Confidence > 1 {
		vs = append(vs, violation(CodeSchemaInvalid, "confidence %v out of [0,1]", p.Confidence))
	}
	// Length caps count code points (JSON Schema maxLength semantics), not bytes.
	if utf8.RuneCountInString(p.Reasoning) > 8000 {
		vs = append(vs, violation(CodeSchemaInvalid, "reasoning exceeds 8000 chars"))
	}
	if utf8.RuneCountInString(p.DebateSummary) > 4000 {
		vs = append(vs, violation(CodeSchemaInvalid, "debate_summary exceeds 4000 chars"))
	}
	vs = append(vs, validateSummaries(&p.AnalystSummaries)...)
	vs = append(vs, validateModelCosts(p.ModelCosts)...)
	vs = append(vs, p.validateOrderFields()...)
	return vs
}

func validateSummaries(s *AnalystSummaries) []Violation {
	var vs []Violation
	for _, e := range []struct {
		name string
		s    AnalystSummary
	}{{"market", s.Market}, {"news", s.News}, {"fundamental", s.Fundamental}} {
		switch e.s.Signal {
		case "bullish", "bearish", "neutral":
		default:
			vs = append(vs, violation(CodeSchemaInvalid, "analyst_summaries.%s.signal %q invalid", e.name, e.s.Signal))
		}
		if e.s.Confidence < 0 || e.s.Confidence > 1 {
			vs = append(vs, violation(CodeSchemaInvalid, "analyst_summaries.%s.confidence out of [0,1]", e.name))
		}
		if utf8.RuneCountInString(e.s.Summary) > 2000 {
			vs = append(vs, violation(CodeSchemaInvalid, "analyst_summaries.%s.summary exceeds 2000 chars", e.name))
		}
	}
	return vs
}

func validateModelCosts(costs []ModelCost) []Violation {
	var vs []Violation
	if len(costs) > 32 {
		vs = append(vs, violation(CodeSchemaInvalid, "model_costs exceeds 32 items"))
	}
	for i, c := range costs {
		if utf8.RuneCountInString(c.Node) > 64 || utf8.RuneCountInString(c.Model) > 64 {
			vs = append(vs, violation(CodeSchemaInvalid, "model_costs[%d] node/model exceeds 64 chars", i))
		}
		if c.InputTokens < 0 || c.OutputTokens < 0 {
			vs = append(vs, violation(CodeSchemaInvalid, "model_costs[%d] negative token count", i))
		}
	}
	return vs
}

// validateOrderFields covers the entry/stop_loss/take_profit/size_quote
// conditionals (contract rules 1-3 where decidable without a mark price).
func (p *Proposal) validateOrderFields() []Violation {
	var vs []Violation

	switch p.Entry.Type {
	case "limit":
		if p.Entry.LimitPrice == nil {
			vs = append(vs, violation(CodeSchemaInvalid, "entry.limit_price required for limit entries"))
		}
	case "market":
		if p.Entry.LimitPrice != nil {
			vs = append(vs, violation(CodeSchemaInvalid, "entry.limit_price forbidden for market entries"))
		}
	default:
		vs = append(vs, violation(CodeSchemaInvalid, "entry.type %q must be market or limit", p.Entry.Type))
	}

	if p.Action.IsOpen() {
		if p.StopLoss == nil {
			vs = append(vs, violation(CodeMissingStopLoss, "stop_loss required for %s", p.Action))
		}
		if p.SizeQuote.Decimal().Sign() <= 0 {
			vs = append(vs, violation(CodeInvalidSize, "size_quote must be > 0 for %s", p.Action))
		}
	} else {
		if p.StopLoss != nil {
			vs = append(vs, violation(CodeSchemaInvalid, "stop_loss forbidden for %s", p.Action))
		}
		if p.TakeProfit != nil {
			vs = append(vs, violation(CodeSchemaInvalid, "take_profit forbidden for %s", p.Action))
		}
		if p.Action == ActionHold && !p.SizeQuote.Decimal().IsZero() {
			vs = append(vs, violation(CodeInvalidSize, `size_quote must be "0" for hold`))
		}
	}

	// Rules 1-2 against a known entry price (limit orders only; market
	// entries are checked by the gate against the current mark).
	if p.Entry.Type == "limit" && p.Entry.LimitPrice != nil && p.StopLoss != nil {
		vs = append(vs, CheckStopAndTarget(p.Action, p.Entry.LimitPrice.Decimal(), p.StopLoss.Decimal(), p.TakeProfit)...)
	}
	return vs
}

// CheckStopAndTarget enforces contract rules 1-2 (stop and take-profit
// placement relative to the entry price). The entry price is the limit price
// or, for market entries, the current mark — which is why the Risk Gate calls
// this again at evaluation time.
func CheckStopAndTarget(action Action, entry, stop decimal.Decimal, takeProfit *Decimal) []Violation {
	var vs []Violation
	if entry.Sign() <= 0 {
		return append(vs, violation(CodeInvalidStopPlacement, "entry price must be > 0"))
	}
	switch action {
	case ActionOpenLong:
		if !stop.LessThan(entry) {
			vs = append(vs, violation(CodeInvalidStopPlacement,
				"stop_loss %s must be below entry %s for open_long", stop, entry))
		}
		if takeProfit != nil && !takeProfit.Decimal().GreaterThan(entry) {
			vs = append(vs, violation(CodeInvalidTakeProfit,
				"take_profit %s must be above entry %s for open_long", takeProfit, entry))
		}
	case ActionOpenShort:
		if !stop.GreaterThan(entry) {
			vs = append(vs, violation(CodeInvalidStopPlacement,
				"stop_loss %s must be above entry %s for open_short", stop, entry))
		}
		if takeProfit != nil && !takeProfit.Decimal().LessThan(entry) {
			vs = append(vs, violation(CodeInvalidTakeProfit,
				"take_profit %s must be below entry %s for open_short", takeProfit, entry))
		}
	}
	return vs
}
