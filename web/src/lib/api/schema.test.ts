// API-response schema tests: the pagination envelope {items,total,page,limit},
// the run-detail embedding (contract payloads verbatim, via the golden
// fixtures), the trace envelope, approvals (incl. approved_but_blocked), and
// the error shape with spec codes (UNKNOWN_VERDICT, NOT_PENDING, ...).

import { describe, expect, it } from "vitest";

import proposalOpenLong from "../../../../contracts/fixtures/proposal_open_long.json";
import verdictRejectDailyLoss from "../../../../contracts/fixtures/verdict_reject_daily_loss.json";
import {
  agentTraceSchema,
  alertsPageSchema,
  apiErrorBodySchema,
  apiTokenSchema,
  approvalDecisionSchema,
  approvalRequestSchema,
  backupRunResultSchema,
  backupsResponseSchema,
  buildLimitChanges,
  discrepancySchema,
  invoiceDetailSchema,
  invoiceLineSchema,
  invoiceSchema,
  invoicesPageSchema,
  killClearResponseSchema,
  limitChangeResponseSchema,
  limitsStatusSchema,
  mintedTokenSchema,
  paperGateReportSchema,
  platformKillEventSchema,
  platformSecretSchema,
  platformSecretsResponseSchema,
  reconciliationDetailSchema,
  reconciliationsPageSchema,
  restoreAckResponseSchema,
  restoreStatusSchema,
  runDetailSchema,
  runsPageSchema,
  safetyAlertSchema,
  safetyStatusSchema,
  secretWriteResponseSchema,
  strategiesPageSchema,
  tenantKillEventSchema,
  tenantSchema,
  tenantsResponseSchema,
  tokensPageSchema,
  usersResponseSchema,
} from "./schema";

const STRATEGY_ID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e";
const RUN_ID = "c3d4e5f6-a7b8-4c9d-8e0f-2a3b4c5d6e7f"; // == agent_trace_id
const VERDICT_ID = "b8c9d0e1-f2a3-4b4c-8d5e-7f8a9b0c1d2e";
const PROPOSAL_ID = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d";

const strategy = {
  strategy_id: STRATEGY_ID,
  tenant_id: "tenant-1",
  name: "BTC momentum",
  lifecycle_state: "live_l1",
  created_at: "2026-07-01T00:00:00Z",
  updated_at: "2026-07-04T12:00:00Z",
};

const run = {
  run_id: RUN_ID,
  strategy_id: STRATEGY_ID,
  tick_number: 42,
  created_at: "2026-07-04T12:00:00Z",
  completed_at: "2026-07-04T12:00:05Z",
};

const trace = {
  strategy_id: STRATEGY_ID,
  run_id: RUN_ID,
  tick_number: 42,
  started_at: "2026-07-04T12:00:00Z",
  completed_at: "2026-07-04T12:00:05Z",
  analyst_summaries: proposalOpenLong.analyst_summaries,
  debate_rounds: [
    {
      round_index: 0,
      bull_argument: "Momentum breakout with volume confirmation.",
      bull_score: 0.7,
      bear_argument: "Macro tightening risk, thin liquidity.",
      bear_score: 0.4,
    },
  ],
  debate_summary: proposalOpenLong.debate_summary,
  proposal_id: PROPOSAL_ID,
  model_costs: proposalOpenLong.model_costs,
  estimated_cost_nodes: ["news_analyst"],
  budget_state: { utc_date: "2026-07-04", tokens_used: 14754, cost_usd_used: "0.030790" },
};

const approvedButBlocked = {
  approval_id: "d4e5f6a7-b8c9-4d0e-8f1a-3b4c5d6e7f8a",
  verdict_id: VERDICT_ID,
  proposal_id: PROPOSAL_ID,
  outcome: "approved_but_blocked",
  preflight_reasons: ["MARK_STALE: mark age 45s exceeds max_age_seconds 30"],
  decided_by: "operator-1",
  decided_at: "2026-07-04T12:05:00Z",
  timeout_seconds: 600,
};

describe("pagination envelope {items,total,page,limit}", () => {
  it("parses a strategies page", () => {
    const page = strategiesPageSchema.parse({ items: [strategy], total: 1, page: 1, limit: 20 });
    expect(page.items[0]?.lifecycle_state).toBe("live_l1");
  });

  it("parses a runs page", () => {
    const page = runsPageSchema.parse({ items: [run], total: 5, page: 1, limit: 20 });
    expect(page.items[0]?.tick_number).toBe(42);
  });

  it("rejects a 0-based page and an over-max limit", () => {
    expect(strategiesPageSchema.safeParse({ items: [], total: 0, page: 0, limit: 20 }).success).toBe(false);
    expect(strategiesPageSchema.safeParse({ items: [], total: 0, page: 1, limit: 101 }).success).toBe(false);
  });

  it("rejects unknown envelope fields", () => {
    expect(
      strategiesPageSchema.safeParse({ items: [], total: 0, page: 1, limit: 20, next: 2 }).success,
    ).toBe(false);
  });
});

describe("trace envelope", () => {
  it("parses a full trace with estimated_cost_nodes", () => {
    const parsed = agentTraceSchema.parse(trace);
    expect(parsed.debate_rounds[0]?.bull_score).toBe(0.7);
    expect(parsed.estimated_cost_nodes).toEqual(["news_analyst"]);
  });

  it("accepts proposal_id null (proposal POST failed after retries)", () => {
    expect(agentTraceSchema.safeParse({ ...trace, proposal_id: null }).success).toBe(true);
  });

  it("rejects a debate round missing bear_score", () => {
    const { bear_score: _bearScore, ...partial } = trace.debate_rounds[0]!;
    expect(agentTraceSchema.safeParse({ ...trace, debate_rounds: [partial] }).success).toBe(false);
  });
});

describe("run detail (contract payloads verbatim)", () => {
  const detail = {
    run,
    proposal: proposalOpenLong,
    verdict: verdictRejectDailyLoss,
    trace,
    orders: [
      {
        order_id: "ord-1",
        proposal_id: PROPOSAL_ID,
        origin: "proposal",
        strategy_id: STRATEGY_ID,
        symbol: "BTC/USDT",
        class: "ENTRY",
        side: "buy",
        type: "limit",
        reduce_only: false,
        qty_base: "0.0234",
        limit_price: "64250.50",
        stop_price: null,
        take_profit: "66820.52",
        fill_price: "64250.50",
        kill_epoch: 0,
        status: "filled",
        submitted_at: "2026-07-04T12:00:06Z",
        filled_at: "2026-07-04T12:00:07Z",
      },
    ],
    fills: [
      {
        fill_id: "fill-1",
        order_id: "ord-1",
        qty_base: "0.0234",
        fill_price: "64250.50",
        fee_quote: "1.50",
        fill_ts: "2026-07-04T12:00:07Z",
      },
    ],
    approvals: [approvedButBlocked],
    pending_approval: null,
  };

  it("parses a full run detail incl. fee_quote and approved_but_blocked", () => {
    const parsed = runDetailSchema.parse(detail);
    expect(parsed.proposal?.proposal_id).toBe(PROPOSAL_ID);
    expect(parsed.verdict?.decision).toBe("reject");
    expect(parsed.fills[0]?.fee_quote).toBe("1.50");
    expect(parsed.approvals[0]?.outcome).toBe("approved_but_blocked");
  });

  it("parses a run detail with everything absent but the run", () => {
    const parsed = runDetailSchema.parse({
      run: { ...run, completed_at: null },
      proposal: null,
      verdict: null,
      trace: null,
      orders: [],
      fills: [],
      approvals: [],
      pending_approval: {
        verdict_id: VERDICT_ID,
        strategy_id: STRATEGY_ID,
        created_at: "2026-07-04T12:00:03Z",
        deadline_at: "2026-07-04T12:10:03Z",
      },
    });
    expect(parsed.pending_approval?.deadline_at).toBe("2026-07-04T12:10:03Z");
  });

  it("rejects an embedded proposal that violates the contract", () => {
    const invalid = { ...detail, proposal: { ...proposalOpenLong, stop_loss: undefined } };
    expect(runDetailSchema.safeParse(invalid).success).toBe(false);
  });
});

describe("approvals", () => {
  it("parses approved_but_blocked with preflight_reasons", () => {
    const parsed = approvalDecisionSchema.parse(approvedButBlocked);
    expect(parsed.preflight_reasons).toHaveLength(1);
  });

  it("requires preflight_reasons iff approved_but_blocked", () => {
    const missing = approvalDecisionSchema.safeParse({
      ...approvedButBlocked,
      preflight_reasons: null,
    });
    expect(missing.success).toBe(false);
    if (!missing.success) {
      expect(missing.error.issues[0]?.path).toEqual(["preflight_reasons"]);
    }
    const forbidden = approvalDecisionSchema.safeParse({
      ...approvedButBlocked,
      outcome: "approved",
    });
    expect(forbidden.success).toBe(false);
  });

  it("parses a timeout decision (decided_by timeout, no preflight reasons)", () => {
    const parsed = approvalDecisionSchema.parse({
      ...approvedButBlocked,
      outcome: "timeout",
      preflight_reasons: null,
      decided_by: "timeout",
    });
    expect(parsed.outcome).toBe("timeout");
  });

  it("rejects an unknown outcome", () => {
    expect(
      approvalDecisionSchema.safeParse({ ...approvedButBlocked, outcome: "maybe" }).success,
    ).toBe(false);
  });

  it("parses the OMS submission status on the immediate POST response", () => {
    const approved = {
      ...approvedButBlocked,
      outcome: "approved",
      preflight_reasons: undefined,
    };
    const ok = approvalDecisionSchema.parse({ ...approved, submitted: true });
    expect(ok.submitted).toBe(true);
    const failed = approvalDecisionSchema.parse({
      ...approved,
      submitted: false,
      submit_error_code: "SUBMIT_FAILED",
    });
    expect(failed.submitted).toBe(false);
    expect(failed.submit_error_code).toBe("SUBMIT_FAILED");
    // Stored approvals (GET run detail) carry no submission status.
    expect(approvalDecisionSchema.parse(approved).submitted).toBeUndefined();
  });

  it("validates the POST body {verdict_id, approved}", () => {
    expect(approvalRequestSchema.parse({ verdict_id: VERDICT_ID, approved: true })).toEqual({
      verdict_id: VERDICT_ID,
      approved: true,
    });
    expect(approvalRequestSchema.safeParse({ verdict_id: "nope", approved: true }).success).toBe(false);
    expect(approvalRequestSchema.safeParse({ verdict_id: VERDICT_ID }).success).toBe(false);
  });
});

describe("error shape", () => {
  it("parses spec error codes", () => {
    for (const code of ["UNKNOWN_VERDICT", "NOT_PENDING", "IDEMPOTENCY_CONFLICT", "STRATEGY_SCOPE_MISMATCH"]) {
      expect(apiErrorBodySchema.parse({ code, message: "m" }).code).toBe(code);
    }
  });

  it("parses a 409 body carrying the recorded outcome", () => {
    const body = apiErrorBodySchema.parse({
      code: "ALREADY_DECIDED",
      message: "verdict already decided",
      recorded: approvedButBlocked,
    });
    expect(body.recorded?.outcome).toBe("approved_but_blocked");
  });

  it("rejects a non-SCREAMING_SNAKE code", () => {
    expect(apiErrorBodySchema.safeParse({ code: "not pending", message: "m" }).success).toBe(false);
  });
});

// ---- Operator surface (operator-surface.md OS-7/OS-18/OS-31) -------------------

const unclearedKill = {
  event_id: "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b",
  scope: "strategy",
  kill_epoch: 4,
  flatten: true,
  actor_id: "admin-1",
  recorded_at: "2026-07-04T12:00:00Z",
  cleared: null,
};

const clearedKill = {
  event_id: "f2a3b4c5-d6e7-4f8a-9b0c-1d2e3f4a5b6c",
  scope: "tenant",
  kill_epoch: 2,
  flatten: false,
  actor_id: "watchdog",
  recorded_at: "2026-07-03T09:00:00Z",
  cleared: {
    clear_id: "a3b4c5d6-e7f8-4a9b-8c0d-2e3f4a5b6c7d",
    actor_id: "admin-1",
    reason: "resolved",
    recorded_at: "2026-07-03T10:00:00Z",
    cleared_epoch: 3,
  },
};

const safetyStatus = {
  strategy_id: STRATEGY_ID,
  lifecycle_state: "paused",
  paused_from: "live_l1",
  active_kill: true,
  kills: [unclearedKill, clearedKill],
  breaker: {
    active_today: true,
    event: {
      event_id: "b4c5d6e7-f8a9-4b0c-8d1e-3f4a5b6c7d8e",
      recorded_at: "2026-07-04T11:00:00Z",
      trigger_ref: '{"daily_pnl":"-120.5","limit":"-100","evaluated_at":"2026-07-04T11:00:00Z"}',
    },
  },
  watchdog: { enabled: true, last_heartbeat_at: "2026-07-04T12:00:00Z", seconds_since: 12 },
};

describe("safetyStatusSchema (OS-7)", () => {
  it("parses the composite with kills, breaker, and watchdog", () => {
    const parsed = safetyStatusSchema.parse(safetyStatus);
    expect(parsed.active_kill).toBe(true);
    expect(parsed.kills[0]?.cleared).toBeNull();
    expect(parsed.kills[1]?.cleared?.cleared_epoch).toBe(3);
  });

  it("accepts nullable paused_from, breaker event, and watchdog nulls", () => {
    const parsed = safetyStatusSchema.parse({
      ...safetyStatus,
      paused_from: null,
      active_kill: false,
      kills: [],
      breaker: { active_today: false, event: null },
      watchdog: { enabled: true, last_heartbeat_at: null, seconds_since: null },
    });
    expect(parsed.paused_from).toBeNull();
    expect(parsed.watchdog.last_heartbeat_at).toBeNull();
  });

  it("rejects unknown keys (strictObject pinned)", () => {
    expect(safetyStatusSchema.safeParse({ ...safetyStatus, extra: 1 }).success).toBe(false);
    expect(
      safetyStatusSchema.safeParse({
        ...safetyStatus,
        kills: [{ ...unclearedKill, extra: 1 }],
      }).success,
    ).toBe(false);
  });

  it("rejects an unknown kill scope", () => {
    expect(
      safetyStatusSchema.safeParse({
        ...safetyStatus,
        kills: [{ ...unclearedKill, scope: "galaxy" }],
      }).success,
    ).toBe(false);
  });
});

describe("safetyAlertSchema / alertsPageSchema (OS-18)", () => {
  const alert = {
    alert_id: "c5d6e7f8-a9b0-4c1d-8e2f-4a5b6c7d8e9f",
    kind: "breaker_daily_loss",
    strategy_id: STRATEGY_ID,
    ref_id: "ref-1",
    details_json: '{"daily_pnl":"-120.5"}',
    recorded_at: "2026-07-04T11:00:00Z",
  };

  it("accepts an unknown kind as a plain string (open set) and nullable fields", () => {
    const parsed = safetyAlertSchema.parse({
      ...alert,
      kind: "some_future_kind",
      strategy_id: null,
      ref_id: null,
    });
    expect(parsed.kind).toBe("some_future_kind");
    expect(parsed.strategy_id).toBeNull();
  });

  it("parses the pagination envelope and rejects unknown keys", () => {
    const page = alertsPageSchema.parse({ items: [alert], total: 1, page: 1, limit: 20 });
    expect(page.items[0]?.kind).toBe("breaker_daily_loss");
    expect(safetyAlertSchema.safeParse({ ...alert, extra: 1 }).success).toBe(false);
  });
});

describe("paperGateReportSchema (LC-23)", () => {
  const report = {
    passed: false,
    window_started_at: "2026-06-20T00:00:00Z",
    evaluated_at: "2026-07-04T12:00:00Z",
    conditions: [
      { name: "min_days", passed: true, measured: "14", required: "14" },
      { name: "max_drawdown", passed: false, measured: "0.21", required: "0.15" },
    ],
  };

  it("accepts the report with decimal-string measured/required", () => {
    const parsed = paperGateReportSchema.parse(report);
    expect(parsed.conditions[1]?.measured).toBe("0.21");
  });

  it("accepts a null window_started_at", () => {
    expect(paperGateReportSchema.parse({ ...report, window_started_at: null }).window_started_at).toBeNull();
  });

  it("rejects unknown keys and non-decimal measurements", () => {
    expect(paperGateReportSchema.safeParse({ ...report, extra: 1 }).success).toBe(false);
    expect(
      paperGateReportSchema.safeParse({
        ...report,
        conditions: [{ name: "x", passed: true, measured: "not-a-number", required: "1" }],
      }).success,
    ).toBe(false);
  });
});

// ---- Risk limits (Settings) ------------------------------------------------------

const effectiveLimits = {
  symbol_whitelist: ["BTC/USDT", "ETH/USDT"],
  max_open_positions: 3,
  per_position_notional_cap_quote: "1500.00",
  daily_loss_limit_quote: "250.00",
  max_drawdown_pct: "0.15",
  max_loss_at_stop_quote: "75.00",
  min_stop_distance_pct: "0.005",
  max_stop_distance_pct: "0.05",
  max_orders_per_minute: 10,
  require_stop_loss: true,
  allocated_capital_quote: "10000.00",
  accounting_quote: "USDT",
  staleness_threshold_seconds: 30,
  l1_approval_timeout_seconds: 600,
  l2_envelope: null,
};

const limitChangeRow = {
  change_id: "d6e7f8a9-b0c1-4d2e-8f3a-5b6c7d8e9f0a",
  field: "max_open_positions",
  old_value: "3",
  new_value: "5",
  actor_id: "admin-1",
  changed_at: "2026-07-05T10:00:00Z",
};

const limitsStatus = {
  effective: effectiveLimits,
  changeable_fields: [
    "max_open_positions",
    "max_orders_per_minute",
    "per_position_notional_cap_quote",
    "daily_loss_limit_quote",
    "max_loss_at_stop_quote",
  ],
  changes: [limitChangeRow],
};

describe("limitsStatusSchema", () => {
  it("parses a full status with a null l2_envelope", () => {
    const parsed = limitsStatusSchema.parse(limitsStatus);
    expect(parsed.effective.per_position_notional_cap_quote).toBe("1500.00");
    expect(parsed.effective.l2_envelope).toBeNull();
    expect(parsed.changeable_fields).toHaveLength(5);
    expect(parsed.changes[0]?.new_value).toBe("5");
  });

  it("parses a non-null l2_envelope", () => {
    const parsed = limitsStatusSchema.parse({
      ...limitsStatus,
      effective: {
        ...effectiveLimits,
        l2_envelope: { max_size_quote: "500.00", allowed_symbols: ["BTC/USDT"] },
      },
    });
    expect(parsed.effective.l2_envelope?.max_size_quote).toBe("500.00");
  });

  it("accepts a null old_value (no prior override)", () => {
    const parsed = limitsStatusSchema.parse({
      ...limitsStatus,
      changes: [{ ...limitChangeRow, old_value: null }],
    });
    expect(parsed.changes[0]?.old_value).toBeNull();
  });

  it("rejects unknown keys (strictObject pinned)", () => {
    expect(limitsStatusSchema.safeParse({ ...limitsStatus, extra: 1 }).success).toBe(false);
    expect(
      limitsStatusSchema.safeParse({
        ...limitsStatus,
        effective: { ...effectiveLimits, extra: 1 },
      }).success,
    ).toBe(false);
    expect(
      limitsStatusSchema.safeParse({
        ...limitsStatus,
        changes: [{ ...limitChangeRow, extra: 1 }],
      }).success,
    ).toBe(false);
  });

  it("rejects a non-decimal quote cap and a non-int position count", () => {
    expect(
      limitsStatusSchema.safeParse({
        ...limitsStatus,
        effective: { ...effectiveLimits, daily_loss_limit_quote: "not-a-number" },
      }).success,
    ).toBe(false);
    expect(
      limitsStatusSchema.safeParse({
        ...limitsStatus,
        effective: { ...effectiveLimits, max_open_positions: 2.5 },
      }).success,
    ).toBe(false);
  });
});

describe("limitChangeResponseSchema", () => {
  it("parses the POST 200 envelope of audit rows", () => {
    const parsed = limitChangeResponseSchema.parse({ changes: [limitChangeRow] });
    expect(parsed.changes[0]?.change_id).toBe(limitChangeRow.change_id);
  });

  it("rejects unknown envelope fields", () => {
    expect(
      limitChangeResponseSchema.safeParse({ changes: [], extra: 1 }).success,
    ).toBe(false);
  });
});

describe("buildLimitChanges", () => {
  it("includes only defined keys, keeping int vs string types", () => {
    expect(
      buildLimitChanges({
        max_open_positions: 5,
        per_position_notional_cap_quote: "1500.00",
      }),
    ).toEqual({
      changes: { max_open_positions: 5, per_position_notional_cap_quote: "1500.00" },
    });
    const built = buildLimitChanges({ max_orders_per_minute: 12, daily_loss_limit_quote: "300" });
    expect(typeof built.changes.max_orders_per_minute).toBe("number");
    expect(typeof built.changes.daily_loss_limit_quote).toBe("string");
  });

  it("drops undefined keys and builds an empty body from an empty input", () => {
    expect(buildLimitChanges({ max_open_positions: undefined })).toEqual({ changes: {} });
    expect(buildLimitChanges({})).toEqual({ changes: {} });
  });
});

// ---- Platform secrets & admin directory (Settings / Admin) ------------------------

const binanceSecretItem = {
  kind: "binance",
  meta: { env: "testnet", api_key_last4: "wfK4" },
  updated_at: "2026-07-05T09:00:00Z",
  updated_by: "root@example.com",
};

const llmSecretItem = {
  kind: "llm",
  meta: { base_url: "https://api.openai.com/v1", api_key_last4: "ab12", timeout_seconds: 30 },
  updated_at: "2026-07-05T09:01:00Z",
  updated_by: "root@example.com",
};

describe("platformSecretSchema (discriminated union on kind)", () => {
  it("parses the binance meta variant", () => {
    const parsed = platformSecretSchema.parse(binanceSecretItem);
    expect(parsed.kind).toBe("binance");
    if (parsed.kind === "binance") {
      expect(parsed.meta.env).toBe("testnet");
      expect(parsed.meta.api_key_last4).toBe("wfK4");
    }
  });

  it("parses the llm meta variant", () => {
    const parsed = platformSecretSchema.parse(llmSecretItem);
    expect(parsed.kind).toBe("llm");
    if (parsed.kind === "llm") {
      expect(parsed.meta.base_url).toBe("https://api.openai.com/v1");
      expect(parsed.meta.timeout_seconds).toBe(30);
    }
  });

  it("rejects a kind/meta mismatch (each variant is strict)", () => {
    expect(
      platformSecretSchema.safeParse({ ...binanceSecretItem, meta: llmSecretItem.meta }).success,
    ).toBe(false);
    expect(
      platformSecretSchema.safeParse({ ...llmSecretItem, meta: binanceSecretItem.meta }).success,
    ).toBe(false);
  });

  it("rejects unknown keys on the item and inside meta (write-only: values never ride the wire)", () => {
    expect(platformSecretSchema.safeParse({ ...binanceSecretItem, api_key: "leak" }).success).toBe(
      false,
    );
    expect(
      platformSecretSchema.safeParse({
        ...binanceSecretItem,
        meta: { ...binanceSecretItem.meta, api_key: "leak" },
      }).success,
    ).toBe(false);
  });

  it("rejects an unknown env", () => {
    expect(
      platformSecretSchema.safeParse({
        ...binanceSecretItem,
        meta: { ...binanceSecretItem.meta, env: "mainnet" },
      }).success,
    ).toBe(false);
  });
});

describe("platform secrets envelopes", () => {
  it("parses an empty items list (nothing configured)", () => {
    expect(platformSecretsResponseSchema.parse({ items: [] }).items).toEqual([]);
  });

  it("parses a mixed list and the write echo envelope", () => {
    const list = platformSecretsResponseSchema.parse({
      items: [binanceSecretItem, llmSecretItem],
    });
    expect(list.items).toHaveLength(2);
    expect(secretWriteResponseSchema.parse({ secret: llmSecretItem }).secret.kind).toBe("llm");
  });

  it("rejects unknown envelope keys", () => {
    expect(platformSecretsResponseSchema.safeParse({ items: [], extra: 1 }).success).toBe(false);
    expect(
      secretWriteResponseSchema.safeParse({ secret: binanceSecretItem, extra: 1 }).success,
    ).toBe(false);
  });
});

// ---- API tokens (multi-tenant-rbac.md §Token lifecycle) ----------------------------

const userToken = {
  token_id: "tok-1",
  tenant_id: "t-1",
  principal: "user",
  role: "operator",
  strategy_id: null,
  label: "ops laptop",
  created_by: "u-1",
  created_at: "2026-07-05T09:00:00Z",
  revoked_at: null,
};

const agentToken = {
  ...userToken,
  token_id: "tok-2",
  principal: "agent",
  role: null,
  strategy_id: STRATEGY_ID,
  label: "runner",
};

describe("apiTokenSchema / tokensPageSchema", () => {
  it("parses user (role, no strategy) and agent (strategy, no role) variants", () => {
    expect(apiTokenSchema.parse(userToken).role).toBe("operator");
    expect(apiTokenSchema.parse(userToken).strategy_id).toBeNull();
    expect(apiTokenSchema.parse(agentToken).role).toBeNull();
    expect(apiTokenSchema.parse(agentToken).strategy_id).toBe(STRATEGY_ID);
  });

  it("accepts a revoked row and an unknown role as a plain string (open set)", () => {
    expect(
      apiTokenSchema.parse({ ...userToken, revoked_at: "2026-07-05T10:00:00Z" }).revoked_at,
    ).toBe("2026-07-05T10:00:00Z");
    expect(apiTokenSchema.parse({ ...userToken, role: "some_future_role" }).role).toBe(
      "some_future_role",
    );
  });

  it("rejects an unknown principal (server-constrained enum, unlike role)", () => {
    expect(apiTokenSchema.safeParse({ ...userToken, principal: "service" }).success).toBe(false);
  });

  it("parses the pagination envelope", () => {
    const page = tokensPageSchema.parse({
      items: [userToken, agentToken],
      total: 2,
      page: 1,
      limit: 20,
    });
    expect(page.items[1]?.principal).toBe("agent");
  });

  it("rejects unknown keys (strictObject pinned; plaintext/hash never ride list reads)", () => {
    expect(apiTokenSchema.safeParse({ ...userToken, extra: 1 }).success).toBe(false);
    expect(apiTokenSchema.safeParse({ ...userToken, token: "leak" }).success).toBe(false);
    expect(apiTokenSchema.safeParse({ ...userToken, token_hash: "leak" }).success).toBe(false);
  });
});

describe("mintedTokenSchema", () => {
  it("parses the mint echo: the token row PLUS the plaintext, returned exactly once", () => {
    const parsed = mintedTokenSchema.parse({ ...agentToken, token: "amx_plain_2" });
    expect(parsed.token).toBe("amx_plain_2");
    expect(parsed.strategy_id).toBe(STRATEGY_ID);
  });

  it("rejects a missing or empty plaintext token and unknown keys", () => {
    expect(mintedTokenSchema.safeParse(userToken).success).toBe(false);
    expect(mintedTokenSchema.safeParse({ ...userToken, token: "" }).success).toBe(false);
    expect(
      mintedTokenSchema.safeParse({ ...userToken, token: "amx_plain_1", extra: 1 }).success,
    ).toBe(false);
  });
});

describe("tenant / user directory schemas", () => {
  const tenant = { tenant_id: "t-1", name: "Acme Trading", created_at: "2026-07-01T00:00:00Z" };
  const user = {
    user_id: "u-1",
    email: "op@example.com",
    tenant_id: "t-1",
    role: "owner",
    created_at: "2026-07-01T00:00:00Z",
    disabled: false,
  };

  it("parses the tenants envelope and rejects unknown tenant keys", () => {
    expect(tenantsResponseSchema.parse({ items: [tenant] }).items[0]?.name).toBe("Acme Trading");
    expect(tenantSchema.safeParse({ ...tenant, extra: 1 }).success).toBe(false);
  });

  it("parses a platform-scoped user (tenant_id null) with an open-set role", () => {
    const parsed = usersResponseSchema.parse({
      items: [{ ...user, tenant_id: null, role: "platform_admin", disabled: true }],
    });
    expect(parsed.items[0]?.tenant_id).toBeNull();
    expect(parsed.items[0]?.role).toBe("platform_admin");
    expect(parsed.items[0]?.disabled).toBe(true);
  });

  it("rejects unknown user keys and a missing disabled flag", () => {
    expect(usersResponseSchema.safeParse({ items: [{ ...user, extra: 1 }] }).success).toBe(false);
    const { disabled: _disabled, ...partial } = user;
    expect(usersResponseSchema.safeParse({ items: [partial] }).success).toBe(false);
  });
});

// ---- Billing: invoices & reconciliation (billing-and-metering.md) -------------------

const INVOICE_ID = "e7f8a9b0-c1d2-4e3f-8a4b-6c7d8e9f0a1b";
const RECON_ID = "a9b0c1d2-e3f4-4a5b-8c6d-8e9f0a1b2c3d";
const REQUEST_ID = "c1d2e3f4-a5b6-4c7d-8e8f-0a1b2c3d4e5f";

const invoice = {
  invoice_id: INVOICE_ID,
  tenant_id: "tenant-1",
  period: "2026-06",
  total_usd: "12.345678",
  line_count: 2,
  generated_at: "2026-07-01T00:00:05Z",
};

const usageLine = {
  line_id: "f8a9b0c1-d2e3-4f4a-8b5c-7d8e9f0a1b2c",
  invoice_id: INVOICE_ID,
  strategy_id: STRATEGY_ID,
  model: "gpt-4o",
  entry_type: "usage",
  original_period: null,
  input_tokens: 120000,
  output_tokens: 24000,
  amount_usd: "10.000000",
};

const carryOverLine = {
  ...usageLine,
  line_id: "d2e3f4a5-b6c7-4d8e-8f9a-1b2c3d4e5f6a",
  entry_type: "carry_over",
  original_period: "2026-05",
  amount_usd: "2.345678",
};

describe("invoice schemas", () => {
  it("parses the pagination envelope and the detail with lines", () => {
    const page = invoicesPageSchema.parse({ items: [invoice], total: 1, page: 1, limit: 20 });
    expect(page.items[0]?.total_usd).toBe("12.345678");
    const detail = invoiceDetailSchema.parse({ invoice, lines: [usageLine, carryOverLine] });
    expect(detail.lines[0]?.original_period).toBeNull();
    expect(detail.lines[1]?.original_period).toBe("2026-05");
  });

  it("accepts an unknown entry_type as a plain string (open set)", () => {
    expect(invoiceLineSchema.parse({ ...usageLine, entry_type: "credit_note" }).entry_type).toBe(
      "credit_note",
    );
  });

  it("rejects a non-YYYY-MM period and a non-decimal total", () => {
    expect(invoiceSchema.safeParse({ ...invoice, period: "2026-13" }).success).toBe(false);
    expect(invoiceSchema.safeParse({ ...invoice, period: "2026-06-01" }).success).toBe(false);
    expect(invoiceSchema.safeParse({ ...invoice, total_usd: "not-a-number" }).success).toBe(false);
  });

  it("rejects unknown keys (strictObject pinned)", () => {
    expect(invoiceSchema.safeParse({ ...invoice, extra: 1 }).success).toBe(false);
    expect(invoiceLineSchema.safeParse({ ...usageLine, extra: 1 }).success).toBe(false);
    expect(
      invoiceDetailSchema.safeParse({ invoice, lines: [usageLine], extra: 1 }).success,
    ).toBe(false);
  });
});

const reconRun = {
  recon_id: RECON_ID,
  tenant_id: "tenant-1",
  period: "2026-06",
  invoice_id: INVOICE_ID,
  status: "fail",
  matched_count: 41,
  discrepancy_count: 2,
  matched_client_cost_usd: "10.5",
  orphan_client_cost_usd: "0.25",
  estimated_client_cost_usd: "1.5",
  unattributed_client_cost_usd: "0.095678",
  invoice_total_usd: "12.345678",
  run_at: "2026-07-02T00:00:00Z",
};

const discrepancy = {
  discrepancy_id: "b0c1d2e3-f4a5-4b6c-8d7e-9f0a1b2c3d4e",
  recon_id: RECON_ID,
  class: "mismatch_tokens",
  request_id: REQUEST_ID,
  strategy_id: STRATEGY_ID,
  details_json: '{"client_input_tokens":10,"gateway_input_tokens":12}',
};

describe("reconciliation schemas", () => {
  it("parses the pagination envelope with decimal-string cost sums (ADR-0003)", () => {
    const page = reconciliationsPageSchema.parse({
      items: [reconRun],
      total: 1,
      page: 1,
      limit: 20,
    });
    expect(page.items[0]?.matched_client_cost_usd).toBe("10.5");
    expect(page.items[0]?.status).toBe("fail");
  });

  it("parses the detail incl. null request_id/strategy_id and an open-set class", () => {
    const detail = reconciliationDetailSchema.parse({
      run: reconRun,
      discrepancies: [
        discrepancy,
        { ...discrepancy, class: "some_future_class", request_id: null, strategy_id: null },
      ],
    });
    expect(detail.discrepancies[0]?.request_id).toBe(REQUEST_ID);
    expect(detail.discrepancies[1]?.request_id).toBeNull();
    expect(detail.discrepancies[1]?.strategy_id).toBeNull();
  });

  it("rejects a non-decimal cost sum and unknown keys", () => {
    expect(
      reconciliationsPageSchema.safeParse({
        items: [{ ...reconRun, orphan_client_cost_usd: "oops" }],
        total: 1,
        page: 1,
        limit: 20,
      }).success,
    ).toBe(false);
    expect(discrepancySchema.safeParse({ ...discrepancy, extra: 1 }).success).toBe(false);
    expect(
      reconciliationDetailSchema.safeParse({ run: reconRun, discrepancies: [], extra: 1 }).success,
    ).toBe(false);
  });
});

// ---- Tenant / platform kill & clear (safety-wiring.md §Kill endpoints) --------------

const tenantKillEvent = {
  event_id: "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b",
  tenant_id: "tenant-1",
  kill_epoch: 3,
  recorded_at: "2026-07-05T12:00:00Z",
  flatten: true,
};

const clearResponse = {
  clear_id: "a3b4c5d6-e7f8-4a9b-8c0d-2e3f4a5b6c7d",
  scope: "tenant",
  tenant_id: "tenant-1",
  cleared_epoch: 4,
  recorded_at: "2026-07-05T12:01:00Z",
  superseded_event_ids: ["e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b"],
};

describe("tenant / platform kill events and the LC-33 clear envelope", () => {
  it("parses the tenant kill acknowledgment", () => {
    const parsed = tenantKillEventSchema.parse(tenantKillEvent);
    expect(parsed.kill_epoch).toBe(3);
    expect(parsed.flatten).toBe(true);
  });

  it("parses the platform kill acknowledgment (no scope-id field)", () => {
    const parsed = platformKillEventSchema.parse({
      event_id: "f2a3b4c5-d6e7-4f8a-9b0c-1d2e3f4a5b6c",
      kill_epoch: 7,
      recorded_at: "2026-07-05T12:00:00Z",
      flatten: false,
    });
    expect(parsed.kill_epoch).toBe(7);
    expect(platformKillEventSchema.safeParse(tenantKillEvent).success).toBe(false);
  });

  it("parses the clear envelope with tenant_id (tenant tier) and with neither id (platform)", () => {
    const tenantClear = killClearResponseSchema.parse(clearResponse);
    expect(tenantClear.tenant_id).toBe("tenant-1");
    expect(tenantClear.strategy_id).toBeUndefined();
    const { tenant_id: _tenantId, ...platformClear } = clearResponse;
    const parsed = killClearResponseSchema.parse({ ...platformClear, scope: "platform" });
    expect(parsed.tenant_id).toBeUndefined();
    expect(parsed.superseded_event_ids).toHaveLength(1);
  });

  it("parses the clear envelope with strategy_id (strategy tier)", () => {
    const { tenant_id: _tenantId, ...rest } = clearResponse;
    const parsed = killClearResponseSchema.parse({
      ...rest,
      scope: "strategy",
      strategy_id: STRATEGY_ID,
    });
    expect(parsed.strategy_id).toBe(STRATEGY_ID);
  });

  it("rejects unknown keys", () => {
    expect(tenantKillEventSchema.safeParse({ ...tenantKillEvent, extra: 1 }).success).toBe(false);
    expect(killClearResponseSchema.safeParse({ ...clearResponse, extra: 1 }).success).toBe(false);
  });
});

// ---- Ops: backups & restore gate (ops-backup.md, deploy-and-survive.md) -------------

const backupRun = {
  artifact: "controlplane-20260706T020000Z.sqlite.gz",
  bytes: 1048576,
  sha256: "b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
  tables: 24,
  rows_total: 15320,
  started_at: "2026-07-06T02:00:00Z",
  finished_at: "2026-07-06T02:00:03Z",
  verified: true,
};

const backupItem = {
  artifact: "controlplane-20260706T020000Z.sqlite.gz",
  bytes: 1048576,
  modified_at: "2026-07-06T02:00:03Z",
};

describe("backup and restore-gate schemas", () => {
  it("parses the OB-6 run result and the OB-7 list (plain items, no page envelope)", () => {
    expect(backupRunResultSchema.parse(backupRun).verified).toBe(true);
    const list = backupsResponseSchema.parse({ items: [backupItem] });
    expect(list.items[0]?.bytes).toBe(1048576);
    expect(backupsResponseSchema.parse({ items: [] }).items).toEqual([]);
  });

  it("rejects a page-envelope shape on the backup list", () => {
    expect(
      backupsResponseSchema.safeParse({ items: [backupItem], total: 1, page: 1, limit: 20 })
        .success,
    ).toBe(false);
  });

  it("parses the DS-6 status and the DS-5 ack bodies", () => {
    expect(restoreStatusSchema.parse({ engaged: true }).engaged).toBe(true);
    expect(restoreStatusSchema.parse({ engaged: false }).engaged).toBe(false);
    expect(restoreAckResponseSchema.parse({ cleared: true }).cleared).toBe(true);
  });

  it("rejects unknown keys (strictObject pinned)", () => {
    expect(backupRunResultSchema.safeParse({ ...backupRun, path: "/leak" }).success).toBe(false);
    expect(restoreStatusSchema.safeParse({ engaged: true, extra: 1 }).success).toBe(false);
  });
});
