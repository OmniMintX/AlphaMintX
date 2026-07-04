package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// DebateRound mirrors agent_trace.schema.json $defs/debate_round.
type DebateRound struct {
	RoundIndex   int     `json:"round_index"`
	BullArgument string  `json:"bull_argument"`
	BullScore    float64 `json:"bull_score"`
	BearArgument string  `json:"bear_argument"`
	BearScore    float64 `json:"bear_score"`
}

// BudgetState mirrors agent_trace.schema.json#/properties/budget_state.
// Informational only: it never writes the token_budget_ledger.
type BudgetState struct {
	UTCDate     string           `json:"utc_date"`
	TokensUsed  int              `json:"tokens_used"`
	CostUSDUsed contract.Decimal `json:"cost_usd_used"`
}

// TraceModelCost mirrors agent_trace.schema.json $defs/trace_model_cost:
// the proposal model_cost fields plus the OPTIONAL per-attempt billing join
// key and estimated flag (docs/specs/billing-and-metering.md §Join key).
// The shared contract.ModelCost stays proposal-shaped and NEVER carries the
// new fields. Both additions are pointers with omitempty so a pre-upgrade
// envelope re-marshals byte-identical (hash stability: no spurious
// IDEMPOTENCY_CONFLICT on checkpoint re-drives of old runs).
type TraceModelCost struct {
	Node         string           `json:"node"`
	Model        string           `json:"model"`
	InputTokens  int              `json:"input_tokens"`
	OutputTokens int              `json:"output_tokens"`
	CostUSD      contract.Decimal `json:"cost_usd"`
	RequestID    *string          `json:"request_id,omitempty"`
	Estimated    *bool            `json:"estimated,omitempty"`
}

// TraceEnvelope mirrors contracts/agent_trace.schema.json (AgentTrace v1).
type TraceEnvelope struct {
	SchemaVersion      string                    `json:"schema_version"`
	StrategyID         string                    `json:"strategy_id"`
	RunID              string                    `json:"run_id"`
	TickNumber         int                       `json:"tick_number"`
	StartedAt          contract.UTCTime          `json:"started_at"`
	CompletedAt        contract.UTCTime          `json:"completed_at"`
	AnalystSummaries   contract.AnalystSummaries `json:"analyst_summaries"`
	DebateRounds       []DebateRound             `json:"debate_rounds"`
	DebateSummary      string                    `json:"debate_summary"`
	Transcripts        map[string]string         `json:"transcripts,omitempty"`
	ProposalID         *string                   `json:"proposal_id"` // null ONLY when the proposal POST failed
	ModelCosts         []TraceModelCost          `json:"model_costs"`
	EstimatedCostNodes []string                  `json:"estimated_cost_nodes,omitempty"`
	BudgetState        BudgetState               `json:"budget_state"`
}

// InsertTrace ingests a trace envelope: trace insert, model_costs fan-out,
// token_budget_ledger increment, and runs.completed_at all in ONE
// transaction. Idempotent by run_id: a duplicate with the same canonical
// payload hash is a no-op (false, nil) skipping all writes atomically; a
// different hash returns ErrIdempotencyConflict. The run row must already
// exist (proposals arrive before traces) or ErrNotFound is returned.
func (s *Store) InsertTrace(env *TraceEnvelope, now time.Time) (bool, error) {
	payload, hash, err := canonicalJSON(env)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)

	var storedHash string
	err = tx.QueryRow(`SELECT payload_sha256 FROM agent_traces WHERE run_id = ?`, env.RunID).Scan(&storedHash)
	switch {
	case err == nil:
		if storedHash != hash {
			return false, ErrIdempotencyConflict
		}
		return false, nil
	case err != sql.ErrNoRows:
		return false, err
	}

	var runStrategy string
	err = tx.QueryRow(`SELECT strategy_id FROM runs WHERE run_id = ?`, env.RunID).Scan(&runStrategy)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("run %s: %w", env.RunID, ErrNotFound)
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.Exec(`INSERT INTO agent_traces
		(trace_id, run_id, strategy_id, proposal_id, started_at, completed_at, payload_json, payload_sha256)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), env.RunID, env.StrategyID, env.ProposalID,
		env.StartedAt.String(), env.CompletedAt.String(), string(payload), hash); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE runs SET completed_at = ? WHERE run_id = ?`,
		env.CompletedAt.String(), env.RunID); err != nil {
		return false, err
	}

	recordedAt := formatTime(now)
	tokens := 0
	cost := decimal.Zero
	for _, mc := range env.ModelCosts {
		if err := insertModelCost(tx, env, mc, recordedAt); err != nil {
			return false, err
		}
		tokens += mc.InputTokens + mc.OutputTokens
		cost = cost.Add(mc.CostUSD.Decimal())
	}

	// Ledger day: the UTC day of started_at (llm-routing §4), never the
	// informational budget_state; increments come from the ingested costs.
	utcDate := env.StartedAt.Time().UTC().Format("2006-01-02")
	var haveTokens int
	var haveCost string
	err = tx.QueryRow(`SELECT tokens_used, cost_usd_used FROM token_budget_ledger
		WHERE strategy_id = ? AND utc_date = ?`, env.StrategyID, utcDate).Scan(&haveTokens, &haveCost)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO token_budget_ledger
			(strategy_id, utc_date, tokens_used, cost_usd_used, updated_at)
			VALUES (?, ?, ?, ?, ?)`,
			env.StrategyID, utcDate, tokens, cost.String(), recordedAt); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		prev, err := decimal.NewFromString(haveCost)
		if err != nil {
			return false, fmt.Errorf("ledger cost_usd_used %q: %w", haveCost, err)
		}
		if _, err := tx.Exec(`UPDATE token_budget_ledger
			SET tokens_used = ?, cost_usd_used = ?, updated_at = ?
			WHERE strategy_id = ? AND utc_date = ?`,
			haveTokens+tokens, prev.Add(cost).String(), recordedAt, env.StrategyID, utcDate); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// insertModelCost fans one trace entry out to model_costs, copying the
// entry's request_id and estimated flag (an ABSENT estimated field means
// is_estimated = 0 — never inferred from estimated_cost_nodes[]). On a
// request_id UNIQUE-index conflict (agent defect, UUID collision, or a
// squatted id) the row is stored with request_id NULL: a join-key defect
// MUST NOT drop cost or fail the trace (billing-and-metering.md §Ingest
// fan-out) — the resulting gateway orphan surfaces at reconciliation.
func insertModelCost(tx *sql.Tx, env *TraceEnvelope, mc TraceModelCost, recordedAt string) error {
	isEstimated := mc.Estimated != nil && *mc.Estimated
	const insertSQL = `INSERT INTO model_costs
		(cost_id, run_id, strategy_id, node, model, input_tokens, output_tokens,
		 cost_usd, recorded_at, request_id, is_estimated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := tx.Exec(insertSQL, uuid.NewString(), env.RunID, env.StrategyID, mc.Node, mc.Model,
		mc.InputTokens, mc.OutputTokens, mc.CostUSD.String(), recordedAt, mc.RequestID, isEstimated)
	// Null-and-retry ONLY on the request_id partial index (the driver
	// message names the failing column set); any other unique violation
	// (e.g. cost_id) returns as-is.
	if err != nil && mc.RequestID != nil && isUniqueConstraint(err) &&
		strings.Contains(err.Error(), "model_costs.request_id") {
		_, err = tx.Exec(insertSQL, uuid.NewString(), env.RunID, env.StrategyID, mc.Node, mc.Model,
			mc.InputTokens, mc.OutputTokens, mc.CostUSD.String(), recordedAt, nil, isEstimated)
	}
	return err
}
