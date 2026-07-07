package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// maxTranscriptBytes bounds the serialized transcripts map (trace envelope
// table: <= 256 KiB serialized).
const maxTranscriptBytes = 256 << 10

var (
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	utcDatePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// handlePostTrace ingests the agent-plane trace envelope
// (persistence-and-api.md §Trace ingestion). Idempotent by run_id: same
// canonical payload => 200 no-op; different payload => 409.
func (s *Server) handlePostTrace(w http.ResponseWriter, r *http.Request) {
	var env store.TraceEnvelope
	if !decodeStrict(w, r, &env) {
		return
	}
	// The middleware already matched the token scope to the path {id}; the
	// body strategy_id must agree with both, exactly as for proposals.
	if env.StrategyID != r.PathValue("id") {
		writeError(w, http.StatusForbidden, codeStrategyScopeMismatch,
			"body strategy_id outside the token scope")
		return
	}
	if msg := validateTrace(&env); msg != "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, msg)
		return
	}
	// The run must exist (proposals arrive before traces) AND belong to the
	// caller's strategy; a foreign run is reported as unknown.
	owner, err := s.cfg.Store.RunStrategy(env.RunID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && owner != env.StrategyID) {
		writeError(w, http.StatusNotFound, codeUnknownRun, "unknown run")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}

	_, err = s.cfg.Store.InsertTrace(&env, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, codeIdempotencyConflict,
			"run_id already has a trace with a different payload")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeUnknownRun, "unknown run")
	case err != nil:
		s.writeInternal(w, r, err)
	default:
		// Fresh and duplicate ingests both answer 200 {run_id}: the agent
		// client treats exactly 200 as success (persistence-and-api.md HTTP
		// API table), mirroring the proposals envelope precedent.
		writeJSON(w, http.StatusOK, map[string]string{"run_id": env.RunID})
	}
}

// validateTrace enforces contracts/agent_trace.schema.json (plus its
// beyond-schema transcript bound); "" means valid. Length caps count code
// points (JSON Schema maxLength semantics), not bytes.
func validateTrace(env *store.TraceEnvelope) string {
	if env.SchemaVersion != contract.SchemaVersion {
		return fmt.Sprintf("unsupported schema_version %q (supported: %q)", env.SchemaVersion, contract.SchemaVersion)
	}
	if !uuidPattern.MatchString(env.StrategyID) {
		return "strategy_id is not a lowercase UUID"
	}
	if !uuidPattern.MatchString(env.RunID) {
		return "run_id is not a lowercase UUID"
	}
	if env.TickNumber < 0 {
		return "tick_number must be >= 0"
	}
	if env.StartedAt.String() == "" || env.CompletedAt.String() == "" {
		return "started_at and completed_at are required"
	}
	if msg := validateTraceSummaries(&env.AnalystSummaries); msg != "" {
		return msg
	}
	for i, d := range env.DebateRounds {
		switch {
		case d.RoundIndex < 0:
			return fmt.Sprintf("debate_rounds[%d].round_index must be >= 0", i)
		case utf8.RuneCountInString(d.BullArgument) > 4000 || utf8.RuneCountInString(d.BearArgument) > 4000:
			return fmt.Sprintf("debate_rounds[%d] argument exceeds 4000 chars", i)
		case d.BullScore < 0 || d.BullScore > 1 || d.BearScore < 0 || d.BearScore > 1:
			return fmt.Sprintf("debate_rounds[%d] score out of [0,1]", i)
		}
	}
	if utf8.RuneCountInString(env.DebateSummary) > 4000 {
		return "debate_summary exceeds 4000 chars"
	}
	if env.Transcripts != nil {
		b, err := json.Marshal(env.Transcripts)
		if err != nil {
			return "transcripts is not serializable"
		}
		if len(b) > maxTranscriptBytes {
			return "transcripts exceed 256 KiB serialized"
		}
	}
	if env.ProposalID != nil && !uuidPattern.MatchString(*env.ProposalID) {
		return "proposal_id is not a lowercase UUID"
	}
	if len(env.ModelCosts) > 32 {
		return "model_costs exceeds 32 items"
	}
	for i, c := range env.ModelCosts {
		if utf8.RuneCountInString(c.Node) > 64 || utf8.RuneCountInString(c.Model) > 64 {
			return fmt.Sprintf("model_costs[%d] node/model exceeds 64 chars", i)
		}
		if c.InputTokens < 0 || c.OutputTokens < 0 {
			return fmt.Sprintf("model_costs[%d] negative token count", i)
		}
		if c.CostUSD.String() == "" {
			return fmt.Sprintf("model_costs[%d].cost_usd is required", i)
		}
		if c.RequestID != nil && !uuidPattern.MatchString(*c.RequestID) {
			return fmt.Sprintf("model_costs[%d].request_id is not a lowercase UUID", i)
		}
	}
	if len(env.EstimatedCostNodes) > 32 {
		return "estimated_cost_nodes exceeds 32 items"
	}
	for i, n := range env.EstimatedCostNodes {
		if utf8.RuneCountInString(n) > 64 {
			return fmt.Sprintf("estimated_cost_nodes[%d] exceeds 64 chars", i)
		}
	}
	if !utcDatePattern.MatchString(env.BudgetState.UTCDate) {
		return "budget_state.utc_date is not a YYYY-MM-DD date"
	}
	if env.BudgetState.TokensUsed < 0 {
		return "budget_state.tokens_used must be >= 0"
	}
	if env.BudgetState.CostUSDUsed.String() == "" {
		return "budget_state.cost_usd_used is required"
	}
	return ""
}

func validateTraceSummaries(s *contract.AnalystSummaries) string {
	for _, e := range []struct {
		name string
		s    contract.AnalystSummary
	}{{"market", s.Market}, {"news", s.News}, {"fundamental", s.Fundamental}} {
		switch e.s.Signal {
		case "bullish", "bearish", "neutral":
		default:
			return fmt.Sprintf("analyst_summaries.%s.signal %q invalid", e.name, e.s.Signal)
		}
		if e.s.Confidence < 0 || e.s.Confidence > 1 {
			return fmt.Sprintf("analyst_summaries.%s.confidence out of [0,1]", e.name)
		}
		if utf8.RuneCountInString(e.s.Summary) > 2000 {
			return fmt.Sprintf("analyst_summaries.%s.summary exceeds 2000 chars", e.name)
		}
	}
	return ""
}
