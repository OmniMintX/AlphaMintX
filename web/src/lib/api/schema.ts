// Zod schemas for the Phase 1 control-plane HTTP API responses
// (docs/specs/persistence-and-api.md §HTTP API). Embedded contract payloads
// (TradeProposal, RiskVerdict) reuse the existing zod contract mirrors —
// never duplicated here.

import { z } from "zod";

import {
  analystSummarySchema,
  decimal,
  maxCodePoints,
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

// Body of POST /strategies (strategy-provisioning.md SP-2): initial state
// is draft (the default when omitted) or paper ONLY — live tiers require
// the lifecycle endpoint and its paper gate. Undefined keys are dropped by
// JSON.stringify — never sent as null.
export interface CreateStrategyRequest {
  tenant_id: string;
  name: string;
  lifecycle_state?: "draft" | "paper";
}

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

// agent_trace.schema.json $defs/trace_model_cost: the proposal model_cost
// fields PLUS the OPTIONAL per-attempt billing join key and estimated flag
// (billing-and-metering.md). The proposal-side modelCostSchema (strictObject)
// would REJECT a live mintrouter trace that carries them.
export const traceModelCostSchema = z.strictObject({
  node: maxCodePoints(64),
  model: maxCodePoints(64),
  input_tokens: z.number().int().min(0),
  output_tokens: z.number().int().min(0),
  cost_usd: decimal,
  request_id: uuid.optional(),
  estimated: z.boolean().optional(),
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
  // Counts code points (JSON Schema maxLength), not UTF-16 units: the Go
  // ingestion gate accepts 4000 code points, which can be 8000 UTF-16 units.
  debate_summary: maxCodePoints(4000),
  transcripts: z.unknown().optional(),
  proposal_id: uuid.nullable(),
  model_costs: z.array(traceModelCostSchema).max(32),
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

// Tenant-tier kill acknowledgment — persistence only, never effects.
export const tenantKillEventSchema = z.strictObject({
  event_id: uuid,
  tenant_id: z.string().min(1),
  kill_epoch: z.number().int().min(0),
  recorded_at: utcTimestamp,
  flatten: z.boolean(),
});

// Platform-tier kill acknowledgment (no scope-id field: both ids are NULL
// on the row). The POST body's ack is the operator-typed literal threaded
// verbatim — the server owns "KILL-PLATFORM", never this client.
export const platformKillEventSchema = z.strictObject({
  event_id: uuid,
  kill_epoch: z.number().int().min(0),
  recorded_at: utcTimestamp,
  flatten: z.boolean(),
});

// ---- Platform secrets (Settings) -------------------------------------------------

// Exchange environment for the platform Binance credential.
export const binanceEnvSchema = z.enum(["testnet", "prod"]);

// Stored-secret METADATA as served by GET /platform/secrets — a discriminated
// union on kind. Secrets are WRITE-ONLY: the wire carries the key's last 4
// characters and provenance, never the stored values.
export const binanceSecretItemSchema = z.strictObject({
  kind: z.literal("binance"),
  meta: z.strictObject({
    env: binanceEnvSchema,
    api_key_last4: z.string().min(1),
  }),
  updated_at: utcTimestamp,
  updated_by: z.string().min(1),
});

export const llmSecretItemSchema = z.strictObject({
  kind: z.literal("llm"),
  meta: z.strictObject({
    base_url: z.string().min(1),
    api_key_last4: z.string().min(1),
    timeout_seconds: z.number().int().min(1),
    // Absent on secrets saved before the model fields existed.
    trader_model: z.string().min(1).optional(),
    default_model: z.string().min(1).optional(),
  }),
  updated_at: utcTimestamp,
  updated_by: z.string().min(1),
});

export const platformSecretSchema = z.discriminatedUnion("kind", [
  binanceSecretItemSchema,
  llmSecretItemSchema,
]);

// GET /platform/secrets envelope; items is empty when nothing is configured.
export const platformSecretsResponseSchema = z.strictObject({
  items: z.array(platformSecretSchema),
});

// POST /platform/secrets/{binance,llm} 200 envelope: the stored item's
// metadata snapshot only — never the submitted values.
export const secretWriteResponseSchema = z.strictObject({
  secret: platformSecretSchema,
});

// ---- Market LLM analysis (Agent analysis) -----------------------------------------

// Chart dimensions accepted by POST /market/llm-analysis.
export const analysisMarketSchema = z.enum(["spot", "futures"]);
export const analysisIntervalSchema = z.enum(["15m", "1h", "4h", "1d"]);
export const analysisLocaleSchema = z.enum(["en", "vi"]);

// POST /market/llm-analysis 200 envelope: the model's text answer and which
// model answered — the provider key never crosses the web boundary.
export const marketAnalysisResponseSchema = z.strictObject({
  text: z.string().min(1),
  model: z.string().min(1),
});

// ---- Tenants / users (Admin) ------------------------------------------------------

// The normative tenant_id shape (multi-tenant-rbac.md §Tenancy rules) for
// UI-side validation. NOTE: the regex itself MATCHES "default" — the server
// reserves it separately (400 INVALID_TENANT_ID), so callers must reject
// "default" explicitly alongside this pattern.
export const TENANT_ID_PATTERN = /^[a-z0-9][a-z0-9_-]{0,31}$/;

// Tenant snapshot as served by GET/POST /tenants (and echoed inside signup).
export const tenantSchema = z.strictObject({
  tenant_id: z.string().min(1),
  name: z.string().min(1),
  created_at: utcTimestamp,
});

export const tenantsResponseSchema = z.strictObject({
  items: z.array(tenantSchema),
});

// Directory row from GET /users: tenant_id is null for platform-scoped
// users; role stays an open set — display only, never switched on
// exhaustively.
export const adminUserSchema = z.strictObject({
  user_id: z.string().min(1),
  email: z.string().min(1),
  tenant_id: z.string().nullable(),
  role: z.string().min(1),
  created_at: utcTimestamp,
  disabled: z.boolean(),
});

export const usersResponseSchema = z.strictObject({
  items: z.array(adminUserSchema),
});

// ---- API tokens (multi-tenant-rbac.md §Token lifecycle) ---------------------------

// principal is server-constrained to exactly {user, agent} — an enum (the
// binanceEnvSchema precedent), unlike role, which stays the open-set
// display-only string.
export const tokenPrincipalSchema = z.enum(["user", "agent"]);

// Token METADATA as served by GET /tokens — never the plaintext or hash.
// user tokens carry role and no strategy_id; agent tokens the inverse — the
// server enforces the pairing, this mirror only shapes the fields.
export const apiTokenSchema = z.strictObject({
  token_id: z.string().min(1),
  tenant_id: z.string().min(1),
  principal: tokenPrincipalSchema,
  role: z.string().min(1).nullable(),
  strategy_id: uuid.nullable(),
  label: z.string().min(1),
  created_by: z.string().min(1),
  created_at: utcTimestamp,
  revoked_at: utcTimestamp.nullable(),
});

export const tokensPageSchema = paginated(apiTokenSchema);

// POST /tokens 200 envelope: the stored row PLUS the plaintext `token`,
// returned exactly once — every later read is metadata only.
export const mintedTokenSchema = apiTokenSchema.extend({
  token: z.string().min(1),
});

// POST /tenants 200 envelope (env-admin ONLY): the created tenant PLUS its
// FIRST owner token — the documented mint-ceiling exception, plaintext
// returned exactly once (multi-tenant-rbac.md §Tenancy rules). Rotating
// that token SHOULD be the tenant's first act.
export const createTenantResponseSchema = tenantSchema.extend({
  owner_token: mintedTokenSchema,
});

// Body of POST /tokens: tenant_id is only meaningful for env-admin /
// platform_admin callers; user tokens carry role and no strategy_id, agent
// tokens carry strategy_id and no role. Undefined keys are dropped by
// JSON.stringify — never sent as null.
export interface MintTokenRequest {
  tenant_id?: string;
  principal: TokenPrincipal;
  role?: string;
  strategy_id?: string;
  label: string;
}

// ---- Auth (session shell) --------------------------------------------------------

// The session identity as returned by /api/auth/me and inside the login
// response: tenant_id is null (and role "platform_admin") for platform
// admins. role is an open set — display only, never switched on exhaustively.
export const sessionUserSchema = z.strictObject({
  user_id: z.string().min(1),
  email: z.string().min(1),
  tenant_id: z.string().nullable(),
  role: z.string().min(1),
});

// Body of the same-origin /api/auth/login response: {"user": ...} only — the
// session token lives in the HttpOnly cookie and never reaches this bundle.
export const loginResponseSchema = z.strictObject({
  user: sessionUserSchema,
});

// Signup echoes the created tenant snapshot alongside the owner user
// (multi-tenant-rbac.md §Password auth — verified against the live wire).
export const signupResponseSchema = z.strictObject({
  tenant: tenantSchema,
  user: sessionUserSchema,
});

export const bootstrapResponseSchema = z.strictObject({
  user: sessionUserSchema,
});

// GET /api/auth/me wraps the identity and names the session row backing it.
export const meResponseSchema = z.strictObject({
  user: sessionUserSchema,
  session_id: z.string().min(1),
});

// Logout answers an empty object.
export const logoutResponseSchema = z.strictObject({});

// ---- Billing: invoices (billing-and-metering.md §Billing) -------------------------

// YYYY-MM UTC calendar month (the control plane's periodPattern).
export const billingPeriod = z.string().regex(/^[0-9]{4}-(0[1-9]|1[0-2])$/);

// One immutable invoices row; total_usd is a decimal STRING (ADR-0003).
export const invoiceSchema = z.strictObject({
  invoice_id: uuid,
  tenant_id: z.string().min(1),
  period: billingPeriod,
  total_usd: decimal,
  line_count: z.number().int().min(0),
  generated_at: utcTimestamp,
});

export const invoicesPageSchema = paginated(invoiceSchema);

// One invoice_lines row. entry_type is an OPEN set (usage, carry_over,
// future credit_note) — a plain string, never an enum; original_period is
// non-null iff entry_type is carry_over (server-enforced pairing).
export const invoiceLineSchema = z.strictObject({
  line_id: uuid,
  invoice_id: uuid,
  strategy_id: uuid,
  model: z.string().min(1),
  entry_type: z.string().min(1),
  original_period: billingPeriod.nullable(),
  input_tokens: z.number().int().min(0),
  output_tokens: z.number().int().min(0),
  amount_usd: decimal,
});

// GET .../invoices/{invoice_id} envelope; lines is never null.
export const invoiceDetailSchema = z.strictObject({
  invoice: invoiceSchema,
  lines: z.array(invoiceLineSchema),
});

// ---- Billing: reconciliation (billing-and-metering.md §Reconciliation) ------------

// One reconciliation_runs row. status stays an open string (display only);
// the four client-cost sums are decimal STRINGS (ADR-0003) that partition
// the client set exactly (matched + orphan + estimated + unattributed ==
// invoice_total_usd, the arithmetic identity).
export const reconciliationRunSchema = z.strictObject({
  recon_id: uuid,
  tenant_id: z.string().min(1),
  period: billingPeriod,
  invoice_id: uuid,
  status: z.string().min(1),
  matched_count: z.number().int().min(0),
  discrepancy_count: z.number().int().min(0),
  matched_client_cost_usd: decimal,
  orphan_client_cost_usd: decimal,
  estimated_client_cost_usd: decimal,
  unattributed_client_cost_usd: decimal,
  invoice_total_usd: decimal,
  run_at: utcTimestamp,
});

export const reconciliationsPageSchema = paginated(reconciliationRunSchema);

// One discrepancies row: class is the OPEN set of classification names;
// details_json is the stored TEXT verbatim (the safety-alert precedent).
export const discrepancySchema = z.strictObject({
  discrepancy_id: uuid,
  recon_id: uuid,
  class: z.string().min(1),
  request_id: z.string().min(1).nullable(),
  strategy_id: uuid.nullable(),
  details_json: z.string(),
});

// GET .../reconciliations/{recon_id} envelope; discrepancies never null.
export const reconciliationDetailSchema = z.strictObject({
  run: reconciliationRunSchema,
  discrepancies: z.array(discrepancySchema),
});

// ---- Ops: backups & restore gate (ops-backup.md, deploy-and-survive.md) -----------

// POST /ops/backups/run 200 (OB-6): one verified snapshot.
export const backupRunResultSchema = z.strictObject({
  artifact: z.string().min(1),
  bytes: z.number().int().min(0),
  sha256: z.string().min(1),
  tables: z.number().int().min(0),
  rows_total: z.number().int().min(0),
  started_at: utcTimestamp,
  finished_at: utcTimestamp,
  verified: z.boolean(),
});

// One OB-7 list entry — the artifact basename only, never a path (OB-13).
export const backupItemSchema = z.strictObject({
  artifact: z.string().min(1),
  bytes: z.number().int().min(0),
  modified_at: utcTimestamp,
});

// GET /ops/backups envelope: a plain items list, NOT the page envelope.
export const backupsResponseSchema = z.strictObject({
  items: z.array(backupItemSchema),
});

// GET /ops/restore (DS-6): whether the restore gate 503-blocks writes.
export const restoreStatusSchema = z.strictObject({
  engaged: z.boolean(),
});

// POST /ops/restore/ack 200 (DS-5).
export const restoreAckResponseSchema = z.strictObject({
  cleared: z.boolean(),
});

// ---- OMS reconciliation (live-oms-and-reconciler.md §API surface) -----------------
// Live-OMS deployments only: the routes are unregistered in paper mode, so
// a GET there is a plain 404 (no error envelope).

export const omsReconRunStatusSchema = z.enum([
  "running",
  "completed",
  "failed",
  "incomplete",
]);

// The latest reconcile run, derived from the persisted run brackets. Go
// omitempty drops absent fields, so everything but status is optional —
// tenant principals see only {status, completed_at}. counters is the
// internal RunCounters struct rendered as JSON by the UI, so it stays a
// permissive record, never a mirrored shape.
export const omsReconRunSchema = z.strictObject({
  run_id: z.string().min(1).optional(),
  started_at: utcTimestamp.optional(),
  completed_at: utcTimestamp.optional(),
  status: omsReconRunStatusSchema,
  counters: z.record(z.string(), z.unknown()).optional(),
});

// One (symbol, venue_epoch, exchange_trade_id) R5 fill watermark.
export const omsReconWatermarkSchema = z.strictObject({
  symbol,
  venue_epoch: z.number().int().min(0),
  exchange_trade_id: z.number().int().min(0),
});

// GET /oms/recon/status: env classes receive the full account-level payload
// (watermarks, venue_epoch); tenant principals the restricted subset plus
// their own strategies' counts (orphans) — the env-only fields stay omitted
// (omitempty), hence optional here. last_run is a Go pointer: null before
// the first run.
export const omsReconStatusSchema = z.strictObject({
  mode: z.string().min(1),
  venue_env: z.string().min(1),
  reconciled: z.boolean(),
  last_run: omsReconRunSchema.nullable(),
  pending_intents: z.number().int().min(0),
  orphans: z.number().int().min(0).optional(),
  watermarks: z.array(omsReconWatermarkSchema).optional(),
  venue_epoch: z.number().int().min(0).optional(),
});

// ---- Alert-dispatch health (alert-notifier.md AN-17) ------------------------------
// Configured deployments only: without a notifier the route is
// unregistered, so a GET is a plain 404 (the OMS-recon precedent).

// source is an OPEN set (the alert-kind precedent, SS-25): a plain string,
// never an enum — new dispatch sources may appear beyond
// kill_breaker_events / kill_clear_events / safety_alerts / notifier.
export const notifierSourceStatusSchema = z.strictObject({
  source: z.string().min(1),
  consecutive_failed_ticks: z.number().int().nonnegative(),
  degraded: z.boolean(),
  last_degraded_at: utcTimestamp.nullable(),
});

// GET /ops/notifier-status: degraded is true when any row is; sources is
// [] never null.
export const notifierStatusSchema = z.strictObject({
  degraded: z.boolean(),
  sources: z.array(notifierSourceStatusSchema),
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
export type TraceModelCost = z.infer<typeof traceModelCostSchema>;
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
export type TenantKillEvent = z.infer<typeof tenantKillEventSchema>;
export type PlatformKillEvent = z.infer<typeof platformKillEventSchema>;
export type BinanceEnv = z.infer<typeof binanceEnvSchema>;
export type PlatformSecret = z.infer<typeof platformSecretSchema>;
export type PlatformSecretsResponse = z.infer<typeof platformSecretsResponseSchema>;
export type SecretWriteResponse = z.infer<typeof secretWriteResponseSchema>;
export type AnalysisMarket = z.infer<typeof analysisMarketSchema>;
export type AnalysisInterval = z.infer<typeof analysisIntervalSchema>;
export type AnalysisLocale = z.infer<typeof analysisLocaleSchema>;
export type MarketAnalysisResponse = z.infer<typeof marketAnalysisResponseSchema>;
export type Tenant = z.infer<typeof tenantSchema>;
export type CreateTenantResponse = z.infer<typeof createTenantResponseSchema>;
export type TenantsResponse = z.infer<typeof tenantsResponseSchema>;
export type AdminUser = z.infer<typeof adminUserSchema>;
export type UsersResponse = z.infer<typeof usersResponseSchema>;
export type TokenPrincipal = z.infer<typeof tokenPrincipalSchema>;
export type ApiToken = z.infer<typeof apiTokenSchema>;
export type TokensPage = z.infer<typeof tokensPageSchema>;
export type MintedToken = z.infer<typeof mintedTokenSchema>;
export type SessionUser = z.infer<typeof sessionUserSchema>;
export type LoginResponse = z.infer<typeof loginResponseSchema>;
export type SignupResponse = z.infer<typeof signupResponseSchema>;
export type BootstrapResponse = z.infer<typeof bootstrapResponseSchema>;
export type MeResponse = z.infer<typeof meResponseSchema>;
export type LogoutResponse = z.infer<typeof logoutResponseSchema>;
export type Invoice = z.infer<typeof invoiceSchema>;
export type InvoicesPage = z.infer<typeof invoicesPageSchema>;
export type InvoiceLine = z.infer<typeof invoiceLineSchema>;
export type InvoiceDetail = z.infer<typeof invoiceDetailSchema>;
export type ReconciliationRun = z.infer<typeof reconciliationRunSchema>;
export type ReconciliationsPage = z.infer<typeof reconciliationsPageSchema>;
export type Discrepancy = z.infer<typeof discrepancySchema>;
export type ReconciliationDetail = z.infer<typeof reconciliationDetailSchema>;
export type BackupRunResult = z.infer<typeof backupRunResultSchema>;
export type BackupItem = z.infer<typeof backupItemSchema>;
export type BackupsResponse = z.infer<typeof backupsResponseSchema>;
export type RestoreStatus = z.infer<typeof restoreStatusSchema>;
export type RestoreAckResponse = z.infer<typeof restoreAckResponseSchema>;
export type OmsReconRunStatus = z.infer<typeof omsReconRunStatusSchema>;
export type OmsReconRun = z.infer<typeof omsReconRunSchema>;
export type OmsReconWatermark = z.infer<typeof omsReconWatermarkSchema>;
export type OmsReconStatus = z.infer<typeof omsReconStatusSchema>;
export type NotifierSourceStatus = z.infer<typeof notifierSourceStatusSchema>;
export type NotifierStatus = z.infer<typeof notifierStatusSchema>;
