// Zod schemas for the Phase 1 control-plane HTTP API responses
// (docs/specs/persistence-and-api.md §HTTP API). Embedded contract payloads
// (TradeProposal, RiskVerdict) reuse the existing zod contract mirrors —
// never duplicated here.

import { z } from "zod";

import {
  analystSummarySchema,
  decimal,
  modelCostSchema,
  riskVerdictSchema,
  symbol,
  tradeProposalSchema,
  utcTimestamp,
  uuid,
} from "../contract/schema";

// ---- Strategies -----------------------------------------------------------

export const lifecycleStateSchema = z.enum([
  "draft",
  "paper",
  "live_l1",
  "live_l2",
  "live_l3",
  "paused",
  "killed",
]);

export const strategySchema = z.strictObject({
  strategy_id: uuid,
  tenant_id: z.string().min(1),
  name: z.string().min(1),
  lifecycle_state: lifecycleStateSchema,
  created_at: utcTimestamp,
  updated_at: utcTimestamp,
});

// ---- Pagination: {items, total, page, limit} (page 1-based) ----------------

export function paginated<T extends z.ZodType>(item: T) {
  return z.strictObject({
    items: z.array(item),
    total: z.number().int().min(0),
    page: z.number().int().min(1),
    limit: z.number().int().min(1).max(100),
  });
}

export const strategiesPageSchema = paginated(strategySchema);

// ---- Runs -------------------------------------------------------------------

export const runSummarySchema = z.strictObject({
  run_id: uuid,
  strategy_id: uuid,
  tick_number: z.number().int().min(0),
  created_at: utcTimestamp,
  completed_at: utcTimestamp.nullable(),
});

export const runsPageSchema = paginated(runSummarySchema);

// ---- Trace envelope (persistence-and-api.md §Trace ingestion) ---------------

export const debateRoundSchema = z.strictObject({
  round_index: z.number().int().min(0),
  bull_argument: z.string(),
  bull_score: z.number(),
  bear_argument: z.string(),
  bear_score: z.number(),
});

export const budgetStateSchema = z.strictObject({
  utc_date: z.string().regex(/^[0-9]{4}-[0-9]{2}-[0-9]{2}$/),
  tokens_used: z.number().int().min(0),
  cost_usd_used: decimal,
});

export const agentTraceSchema = z.strictObject({
  strategy_id: uuid,
  run_id: uuid,
  tick_number: z.number().int().min(0),
  started_at: utcTimestamp,
  completed_at: utcTimestamp,
  analyst_summaries: z.strictObject({
    market: analystSummarySchema,
    news: analystSummarySchema,
    fundamental: analystSummarySchema,
  }),
  debate_rounds: z.array(debateRoundSchema),
  debate_summary: z.string().max(4000),
  transcripts: z.unknown().optional(),
  proposal_id: uuid.nullable(),
  model_costs: z.array(modelCostSchema).max(32),
  estimated_cost_nodes: z.array(z.string()).optional(),
  budget_state: budgetStateSchema,
});

// ---- Orders / fills ----------------------------------------------------------

export const orderSchema = z.strictObject({
  order_id: z.string().min(1),
  proposal_id: uuid.nullable(),
  origin: z.enum(["proposal", "breaker", "kill", "watchdog", "sl_contingency"]),
  strategy_id: uuid,
  symbol,
  class: z.enum(["ENTRY", "PROTECTIVE"]),
  side: z.string().min(1),
  type: z.string().min(1),
  reduce_only: z.boolean(),
  qty_base: decimal,
  limit_price: decimal.nullable(),
  stop_price: decimal.nullable(),
  take_profit: decimal.nullable(),
  fill_price: decimal.nullable(),
  kill_epoch: z.number().int().min(0),
  status: z.string().min(1),
  submitted_at: utcTimestamp,
  filled_at: utcTimestamp.nullable(),
});

export const fillSchema = z.strictObject({
  fill_id: z.string().min(1),
  order_id: z.string().min(1),
  qty_base: decimal,
  fill_price: decimal,
  fee_quote: decimal,
  fill_ts: utcTimestamp,
});

// ---- Approvals (L0/L1 execution semantics) -----------------------------------

export const approvalOutcomeSchema = z.enum([
  "approved",
  "approved_but_blocked",
  "rejected",
  "timeout",
]);

export const approvalDecisionSchema = z
  .strictObject({
    approval_id: uuid,
    verdict_id: uuid,
    proposal_id: uuid,
    outcome: approvalOutcomeSchema,
    // JSON array; non-null iff approved_but_blocked (DDL comment).
    preflight_reasons: z.array(z.string()).nullish(),
    decided_by: z.string().min(1),
    decided_at: utcTimestamp,
    timeout_seconds: z.number().int().min(0),
    // OMS submission status, present only on the immediate POST response
    // when a submission was attempted (outcome=approved with a Submitter
    // wired). submitted=false means the OMS rejected the approved decision
    // (persisted control-plane-side as SUBMIT_FAILED).
    submitted: z.boolean().optional(),
    submit_error_code: z.string().optional(),
  })
  .superRefine((a, ctx) => {
    const blocked = a.outcome === "approved_but_blocked";
    if (blocked && (a.preflight_reasons == null || a.preflight_reasons.length === 0)) {
      ctx.addIssue({
        code: "custom",
        path: ["preflight_reasons"],
        message: 'preflight_reasons is required when outcome is "approved_but_blocked"',
      });
    }
    if (!blocked && a.preflight_reasons != null) {
      ctx.addIssue({
        code: "custom",
        path: ["preflight_reasons"],
        message: `preflight_reasons is forbidden when outcome is "${a.outcome}"`,
      });
    }
  });

export const pendingApprovalSchema = z.strictObject({
  verdict_id: uuid,
  strategy_id: uuid,
  created_at: utcTimestamp,
  deadline_at: utcTimestamp,
});

// Body of POST /api/v1/strategies/{id}/approvals (operator token only).
export const approvalRequestSchema = z.strictObject({
  verdict_id: uuid,
  approved: z.boolean(),
});

// ---- Run detail (contract payloads embedded verbatim) -------------------------

export const runDetailSchema = z.strictObject({
  run: runSummarySchema,
  proposal: tradeProposalSchema.nullable(),
  verdict: riskVerdictSchema.nullable(),
  trace: agentTraceSchema.nullable(),
  orders: z.array(orderSchema),
  fills: z.array(fillSchema),
  approvals: z.array(approvalDecisionSchema),
  pending_approval: pendingApprovalSchema.nullable(),
});

// ---- Safety status (operator-surface.md OS-7/OS-31) ---------------------------

// scope is DERIVED server-side from id NULL-ness (OS-8): Phase-1 global rows
// report "platform".
export const killScopeSchema = z.enum(["strategy", "tenant", "platform"]);

// The newest clear COVERING a kill row (OS-9); null while the kill stands.
export const killClearedSchema = z.strictObject({
  clear_id: uuid,
  actor_id: z.string().min(1),
  reason: z.string().min(1),
  recorded_at: utcTimestamp,
  cleared_epoch: z.number().int().min(0),
});

// One kill binding the strategy (OS-8a wire DTO). flatten is a plain
// boolean: a NULL pre-flatten-era column renders false server-side (OS-8).
export const boundKillSchema = z.strictObject({
  event_id: uuid,
  scope: killScopeSchema,
  kill_epoch: z.number().int().min(0),
  flatten: z.boolean(),
  actor_id: z.string().min(1),
  recorded_at: utcTimestamp,
  cleared: killClearedSchema.nullable(),
});

// Newest breaker row on today's UTC date (OS-11); trigger_ref is the stored
// TEXT verbatim (the monitor's {daily_pnl, limit, evaluated_at} sample) or null.
export const breakerEventSchema = z.strictObject({
  event_id: uuid,
  recorded_at: utcTimestamp,
  trigger_ref: z.string().nullable(),
});

export const safetyStatusSchema = z.strictObject({
  strategy_id: uuid,
  lifecycle_state: lifecycleStateSchema,
  // Non-null iff paused with known provenance (OS-7); drives the resume verb.
  paused_from: lifecycleStateSchema.nullable(),
  // The LC-28 acting predicate verbatim — never re-derived client-side.
  active_kill: z.boolean(),
  kills: z.array(boundKillSchema),
  breaker: z.strictObject({
    active_today: z.boolean(),
    event: breakerEventSchema.nullable(),
  }),
  watchdog: z.strictObject({
    enabled: z.boolean(),
    last_heartbeat_at: utcTimestamp.nullable(),
    seconds_since: z.number().int().min(0).nullable(),
  }),
});

// ---- Safety alerts (operator-surface.md OS-18) ---------------------------------

// kind is the OPEN set (SS-25): a plain string, never an enum; details_json
// is the stored TEXT verbatim, never re-shaped server-side.
export const safetyAlertSchema = z.strictObject({
  alert_id: uuid,
  kind: z.string().min(1),
  strategy_id: uuid.nullable(),
  ref_id: z.string().nullable(),
  details_json: z.string(),
  recorded_at: utcTimestamp,
});

export const alertsPageSchema = paginated(safetyAlertSchema);

// ---- Paper-gate report (lifecycle-api.md LC-23) ---------------------------------

export const paperGateConditionSchema = z.strictObject({
  name: z.string().min(1),
  passed: z.boolean(),
  measured: decimal,
  required: decimal,
});

export const paperGateReportSchema = z.strictObject({
  passed: z.boolean(),
  window_started_at: utcTimestamp.nullable(),
  evaluated_at: utcTimestamp,
  conditions: z.array(paperGateConditionSchema),
});

// ---- Risk limits (Settings) ------------------------------------------------------

// L2 envelope carve-out; null when the strategy has no envelope configured.
export const l2EnvelopeSchema = z.strictObject({
  max_size_quote: z.string(),
  allowed_symbols: z.array(z.string()),
});

// Effective (DB-backed) risk limits as served by GET .../limits. Decimals
// stay strings end-to-end — never parsed to floats.
export const riskLimitsSchema = z.strictObject({
  symbol_whitelist: z.array(z.string()),
  max_open_positions: z.number().int().min(0),
  per_position_notional_cap_quote: decimal,
  daily_loss_limit_quote: decimal,
  max_drawdown_pct: decimal,
  max_loss_at_stop_quote: decimal,
  min_stop_distance_pct: decimal,
  max_stop_distance_pct: decimal,
  max_orders_per_minute: z.number().int().min(0),
  require_stop_loss: z.boolean(),
  allocated_capital_quote: decimal,
  accounting_quote: z.string().min(1),
  staleness_threshold_seconds: z.number().int().min(0),
  l1_approval_timeout_seconds: z.number().int().min(0),
  l2_envelope: l2EnvelopeSchema.nullable(),
});

// One audit row: old_value is null when the field had no prior override.
export const limitChangeRowSchema = z.strictObject({
  change_id: z.string().min(1),
  field: z.string().min(1),
  old_value: z.string().nullable(),
  new_value: z.string(),
  actor_id: z.string().min(1),
  changed_at: utcTimestamp,
});

// GET .../limits envelope: effective limits, the server's list of
// runtime-changeable fields, and the change audit trail.
export const limitsStatusSchema = z.strictObject({
  effective: riskLimitsSchema,
  changeable_fields: z.array(z.string()),
  changes: z.array(limitChangeRowSchema),
});

// POST .../limits 200 envelope: the audit rows recorded by this request.
export const limitChangeResponseSchema = z.strictObject({
  changes: z.array(limitChangeRowSchema),
});

// The five runtime-changeable fields. Ints are JSON numbers; quote caps are
// decimal STRINGS on the wire.
export interface LimitChangesInput {
  max_open_positions?: number;
  max_orders_per_minute?: number;
  per_position_notional_cap_quote?: string;
  daily_loss_limit_quote?: string;
  max_loss_at_stop_quote?: string;
}

export interface LimitChangeRequest {
  changes: Record<string, number | string>;
}

// Body of POST .../limits: {changes: {<field>: <value>}} with ONLY the
// fields the operator actually entered — undefined keys are dropped.
export function buildLimitChanges(input: LimitChangesInput): LimitChangeRequest {
  const changes: Record<string, number | string> = {};
  for (const [key, value] of Object.entries(input)) {
    if (value !== undefined) changes[key] = value;
  }
  return { changes };
}

// ---- Lifecycle / kill / clear bodies and responses ------------------------------

// Body of POST .../lifecycle (LC-4). to: "killed" is never offered by the
// panel (LC-5: kills flow through the kill endpoint).
export const lifecycleRequestSchema = z.strictObject({
  to: lifecycleStateSchema,
  reason: z.string().min(1),
});

// LC-13 success envelope.
export const lifecycleResponseSchema = z.strictObject({
  strategy_id: uuid,
  from_state: lifecycleStateSchema,
  to_state: lifecycleStateSchema,
  transition_id: uuid,
  recorded_at: utcTimestamp,
});

// Body of POST .../kill (safety-wiring.md §Kill endpoints; wire default false).
export const killRequestSchema = z.strictObject({
  flatten: z.boolean(),
});

// Strategy-tier kill acknowledgment — persistence only, never effects.
export const killResponseSchema = z.strictObject({
  event_id: uuid,
  strategy_id: uuid,
  kill_epoch: z.number().int().min(0),
  recorded_at: utcTimestamp,
  flatten: z.boolean(),
});

// Body of POST .../kill/clear (LC-30): observed_epoch is the CAS token —
// the displayed standing kill's epoch, never a guess (OS-29).
export const killClearRequestSchema = z.strictObject({
  reason: z.string().min(1),
  observed_epoch: z.number().int().min(0),
});

// LC-33 clear envelope; scope-id fields render only on their tier (omitempty).
export const killClearResponseSchema = z.strictObject({
  clear_id: uuid,
  scope: killScopeSchema,
  strategy_id: uuid.optional(),
  tenant_id: z.string().min(1).optional(),
  cleared_epoch: z.number().int().min(0),
  recorded_at: utcTimestamp,
  superseded_event_ids: z.array(uuid),
});

// ---- Errors --------------------------------------------------------------------

// Error codes named by the spec; servers may add more, so the schema accepts
// any SCREAMING_SNAKE code and this list is for display/switching only.
export const KNOWN_API_ERROR_CODES = [
  "UNKNOWN_VERDICT",
  "NOT_PENDING",
  "ALREADY_DECIDED",
  "IDEMPOTENCY_CONFLICT",
  "STRATEGY_SCOPE_MISMATCH",
  "PROPOSAL_STALE",
] as const;

export const apiErrorBodySchema = z.object({
  code: z.string().max(64).regex(/^[A-Z][A-Z0-9_]*$/),
  message: z.string(),
  // 409 on an already-decided approval carries the recorded outcome.
  recorded: approvalDecisionSchema.optional(),
});

// ---- Inferred types --------------------------------------------------------------

export type LifecycleState = z.infer<typeof lifecycleStateSchema>;
export type Strategy = z.infer<typeof strategySchema>;
export type StrategiesPage = z.infer<typeof strategiesPageSchema>;
export type RunSummary = z.infer<typeof runSummarySchema>;
export type RunsPage = z.infer<typeof runsPageSchema>;
export type DebateRound = z.infer<typeof debateRoundSchema>;
export type AgentTrace = z.infer<typeof agentTraceSchema>;
export type Order = z.infer<typeof orderSchema>;
export type Fill = z.infer<typeof fillSchema>;
export type ApprovalOutcome = z.infer<typeof approvalOutcomeSchema>;
export type ApprovalDecision = z.infer<typeof approvalDecisionSchema>;
export type PendingApproval = z.infer<typeof pendingApprovalSchema>;
export type ApprovalRequest = z.infer<typeof approvalRequestSchema>;
export type RunDetail = z.infer<typeof runDetailSchema>;
export type ApiErrorBody = z.infer<typeof apiErrorBodySchema>;
export type KillScope = z.infer<typeof killScopeSchema>;
export type KillCleared = z.infer<typeof killClearedSchema>;
export type BoundKill = z.infer<typeof boundKillSchema>;
export type SafetyStatus = z.infer<typeof safetyStatusSchema>;
export type SafetyAlert = z.infer<typeof safetyAlertSchema>;
export type AlertsPage = z.infer<typeof alertsPageSchema>;
export type PaperGateCondition = z.infer<typeof paperGateConditionSchema>;
export type PaperGateReport = z.infer<typeof paperGateReportSchema>;
export type L2Envelope = z.infer<typeof l2EnvelopeSchema>;
export type RiskLimits = z.infer<typeof riskLimitsSchema>;
export type LimitChangeRow = z.infer<typeof limitChangeRowSchema>;
export type LimitsStatus = z.infer<typeof limitsStatusSchema>;
export type LimitChangeResponse = z.infer<typeof limitChangeResponseSchema>;
export type LifecycleRequest = z.infer<typeof lifecycleRequestSchema>;
export type LifecycleResponse = z.infer<typeof lifecycleResponseSchema>;
export type KillRequest = z.infer<typeof killRequestSchema>;
export type KillResponse = z.infer<typeof killResponseSchema>;
export type KillClearRequest = z.infer<typeof killClearRequestSchema>;
export type KillClearResponse = z.infer<typeof killClearResponseSchema>;
