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
