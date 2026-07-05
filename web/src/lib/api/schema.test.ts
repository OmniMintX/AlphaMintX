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
  approvalDecisionSchema,
  approvalRequestSchema,
  paperGateReportSchema,
  runDetailSchema,
  runsPageSchema,
  safetyAlertSchema,
  safetyStatusSchema,
  strategiesPageSchema,
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
