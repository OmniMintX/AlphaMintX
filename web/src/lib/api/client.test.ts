// Client-layer pure pieces: URL/query construction, bearer headers, the
// approval POST payload, and ApiError body parsing (409 recorded outcome).
// Plus the operator-surface additions (OS-31): safety/alerts/paper-gate GETs
// (READ token attached), lifecycle/kill/clear POSTs (same-origin, no auth
// header, exact bodies), and the OS-29 409 no-auto-retry rule.

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ApiError,
  PAPER_GATE_POLL_INTERVAL_MS,
  POLL_INTERVAL_MS,
  authHeaders,
  buildApprovalPayload,
  buildClearPayload,
  buildKillPayload,
  buildLifecyclePayload,
  buildUrl,
  clearStrategyKill,
  fetchAlerts,
  fetchLimits,
  fetchPaperGate,
  fetchSafety,
  postKill,
  postKillClear,
  postLifecycle,
  postLimits,
} from "./client";
import {
  approvalRequestSchema,
  buildLimitChanges,
  killClearRequestSchema,
  killRequestSchema,
  lifecycleRequestSchema,
} from "./schema";
import { resumeTarget } from "../view/ops";

const VERDICT_ID = "b8c9d0e1-f2a3-4b4c-8d5e-7f8a9b0c1d2e";
const STRATEGY_ID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e";

describe("buildUrl", () => {
  it("joins base, path, and query", () => {
    expect(
      buildUrl("http://cp.local", "/api/v1/strategies", { page: 2, limit: 20 }),
    ).toBe("http://cp.local/api/v1/strategies?page=2&limit=20");
  });

  it("omits undefined params and the empty query string", () => {
    expect(buildUrl("", "/api/v1/strategies", { page: undefined })).toBe("/api/v1/strategies");
    expect(buildUrl("", "/health")).toBe("/health");
  });
});

describe("authHeaders", () => {
  it("emits a bearer header only when a token is configured", () => {
    expect(authHeaders("tok-123")).toEqual({ authorization: "Bearer tok-123" });
    expect(authHeaders(undefined)).toEqual({});
  });
});

describe("buildApprovalPayload", () => {
  it("builds the exact {verdict_id, approved} body for approve and reject", () => {
    expect(buildApprovalPayload(VERDICT_ID, true)).toEqual({
      verdict_id: VERDICT_ID,
      approved: true,
    });
    expect(buildApprovalPayload(VERDICT_ID, false)).toEqual({
      verdict_id: VERDICT_ID,
      approved: false,
    });
  });

  it("round-trips through the request schema", () => {
    expect(approvalRequestSchema.parse(buildApprovalPayload(VERDICT_ID, true)).approved).toBe(true);
  });
});

describe("ApiError", () => {
  it("parses spec error bodies (code + message)", () => {
    const err = new ApiError(404, { code: "UNKNOWN_VERDICT", message: "no such verdict" });
    expect(err.status).toBe(404);
    expect(err.body?.code).toBe("UNKNOWN_VERDICT");
    expect(err.message).toBe("UNKNOWN_VERDICT: no such verdict");
  });

  it("carries the recorded outcome on a 409 conflict", () => {
    const err = new ApiError(409, {
      code: "ALREADY_DECIDED",
      message: "first decision wins",
      recorded: {
        approval_id: "d4e5f6a7-b8c9-4d0e-8f1a-3b4c5d6e7f8a",
        verdict_id: VERDICT_ID,
        proposal_id: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
        outcome: "timeout",
        preflight_reasons: null,
        decided_by: "timeout",
        decided_at: "2026-07-04T12:10:03Z",
        timeout_seconds: 600,
      },
    });
    expect(err.body?.recorded?.outcome).toBe("timeout");
  });

  it("degrades to HTTP status text for unparseable bodies", () => {
    const err = new ApiError(500, "boom");
    expect(err.body).toBeNull();
    expect(err.message).toBe("HTTP 500");
  });
});

// ---- Operator surface (operator-surface.md OS-25/OS-26/OS-29/OS-31) ------------

describe("ops payload builders", () => {
  it("builds the LC-4 lifecycle body and round-trips the schema", () => {
    expect(buildLifecyclePayload("paper", "gate passed")).toEqual({
      to: "paper",
      reason: "gate passed",
    });
    expect(lifecycleRequestSchema.parse(buildLifecyclePayload("live_l1", "go")).to).toBe("live_l1");
  });

  it("resume sends to = paused_from; paused_from killed maps to paper, never killed (OS-26)", () => {
    expect(buildLifecyclePayload(resumeTarget("live_l2")!, "resume")).toEqual({
      to: "live_l2",
      reason: "resume",
    });
    expect(buildLifecyclePayload(resumeTarget("killed")!, "resume")).toEqual({
      to: "paper",
      reason: "resume",
    });
    // Null provenance disables the button — there is no payload to build.
    expect(resumeTarget(null)).toBeNull();
  });

  it("builds explicit {flatten} and {reason, observed_epoch} bodies", () => {
    expect(killRequestSchema.parse(buildKillPayload(false))).toEqual({ flatten: false });
    expect(killClearRequestSchema.parse(buildClearPayload("resolved", 4))).toEqual({
      reason: "resolved",
      observed_epoch: 4,
    });
  });
});

describe("paper-gate poll interval (OS-25)", () => {
  it("is exactly 6 x POLL_INTERVAL_MS and never below the floor", () => {
    expect(PAPER_GATE_POLL_INTERVAL_MS).toBe(6 * POLL_INTERVAL_MS);
    expect(PAPER_GATE_POLL_INTERVAL_MS).toBeGreaterThanOrEqual(POLL_INTERVAL_MS);
  });
});

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const safetyBody = {
  strategy_id: STRATEGY_ID,
  lifecycle_state: "paper",
  paused_from: null,
  active_kill: false,
  kills: [],
  breaker: { active_today: false, event: null },
  watchdog: { enabled: false, last_heartbeat_at: null, seconds_since: null },
};

const gateBody = {
  passed: true,
  window_started_at: "2026-06-20T00:00:00Z",
  evaluated_at: "2026-07-04T12:00:00Z",
  conditions: [{ name: "min_days", passed: true, measured: "15", required: "14" }],
};

function stubFetch(...responses: Response[]) {
  const mock = vi.fn<typeof fetch>();
  for (const res of responses) mock.mockResolvedValueOnce(res);
  vi.stubGlobal("fetch", mock);
  return mock;
}

describe("ops fetchers and proxy POSTs", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
  });

  it("GETs safety/alerts/paper-gate with the READ token on the API base", async () => {
    vi.stubEnv("NEXT_PUBLIC_API_BASE_URL", "http://cp.local");
    vi.stubEnv("NEXT_PUBLIC_READ_TOKEN", "read-tok");
    const mock = stubFetch(
      jsonResponse(200, safetyBody),
      jsonResponse(200, { items: [], total: 0, page: 2, limit: 20 }),
      jsonResponse(200, gateBody),
    );

    await fetchSafety(STRATEGY_ID);
    await fetchAlerts(STRATEGY_ID, 2);
    await fetchPaperGate(STRATEGY_ID);

    expect(mock.mock.calls[0]?.[0]).toBe(`http://cp.local/api/v1/strategies/${STRATEGY_ID}/safety`);
    expect(mock.mock.calls[1]?.[0]).toBe(
      `http://cp.local/api/v1/strategies/${STRATEGY_ID}/alerts?page=2&limit=20`,
    );
    expect(mock.mock.calls[2]?.[0]).toBe(
      `http://cp.local/api/v1/strategies/${STRATEGY_ID}/paper-gate`,
    );
    for (const call of mock.mock.calls) {
      expect(call[1]).toMatchObject({
        headers: { authorization: "Bearer read-tok" },
        cache: "no-store",
      });
    }
  });

  it("POSTs lifecycle/kill/clear same-origin with exact bodies and NO auth header", async () => {
    vi.stubEnv("NEXT_PUBLIC_READ_TOKEN", "read-tok");
    const mock = stubFetch(
      jsonResponse(200, {
        strategy_id: STRATEGY_ID,
        from_state: "paused",
        to_state: "paper",
        transition_id: "d4e5f6a7-b8c9-4d0e-8f1a-3b4c5d6e7f8a",
        recorded_at: "2026-07-04T12:00:00Z",
      }),
      jsonResponse(200, {
        event_id: "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b",
        strategy_id: STRATEGY_ID,
        kill_epoch: 5,
        recorded_at: "2026-07-04T12:00:01Z",
        flatten: true,
      }),
      jsonResponse(200, {
        clear_id: "a3b4c5d6-e7f8-4a9b-8c0d-2e3f4a5b6c7d",
        scope: "strategy",
        strategy_id: STRATEGY_ID,
        cleared_epoch: 6,
        recorded_at: "2026-07-04T12:00:02Z",
        superseded_event_ids: [],
      }),
    );

    // Resume of a paused-after-kill strategy: to = "paper" (OS-26).
    await postLifecycle(STRATEGY_ID, buildLifecyclePayload(resumeTarget("killed")!, "resume"));
    await postKill(STRATEGY_ID, buildKillPayload(true));
    // observed_epoch threaded from the displayed kill epoch (OS-29).
    await postKillClear(STRATEGY_ID, buildClearPayload("resolved", 5));

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/strategies/${STRATEGY_ID}/lifecycle`);
    expect(mock.mock.calls[1]?.[0]).toBe(`/api/strategies/${STRATEGY_ID}/kill`);
    expect(mock.mock.calls[2]?.[0]).toBe(`/api/strategies/${STRATEGY_ID}/kill/clear`);
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"to":"paper","reason":"resume"}');
    expect(mock.mock.calls[1]?.[1]?.body).toBe('{"flatten":true}');
    expect(mock.mock.calls[2]?.[1]?.body).toBe('{"reason":"resolved","observed_epoch":5}');
    for (const call of mock.mock.calls) {
      // The operator credential never reaches this client (OS-32): the proxy
      // POST carries content-type only.
      expect(call[1]?.headers).toEqual({ "content-type": "application/json" });
    }
  });

  it("surfaces upstream error bodies untouched through ApiError (OS-30)", async () => {
    stubFetch(
      jsonResponse(422, { code: "ILLEGAL_TRANSITION", message: "draft cannot promote to live_l3" }),
    );
    const err = await postLifecycle(STRATEGY_ID, buildLifecyclePayload("live_l3", "r")).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).body?.code).toBe("ILLEGAL_TRANSITION");
  });

  it("clearStrategyKill on 409 CLEAR_CONFLICT re-fetches and does NOT re-POST (OS-29)", async () => {
    const mock = stubFetch(jsonResponse(409, { code: "CLEAR_CONFLICT", message: "epoch moved" }));
    const refetch = vi.fn();

    const err = await clearStrategyKill(STRATEGY_ID, "resolved", 4, refetch).catch(
      (e: unknown) => e,
    );

    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).body?.code).toBe("CLEAR_CONFLICT");
    expect(refetch).toHaveBeenCalledTimes(1);
    expect(mock).toHaveBeenCalledTimes(1); // no auto-retry with a fresh epoch
  });

  it("clearStrategyKill does not re-fetch on non-409 errors", async () => {
    stubFetch(jsonResponse(403, { code: "FORBIDDEN", message: "role lacks clear" }));
    const refetch = vi.fn();
    await expect(clearStrategyKill(STRATEGY_ID, "r", 4, refetch)).rejects.toBeInstanceOf(ApiError);
    expect(refetch).not.toHaveBeenCalled();
  });
});

// ---- Risk limits (Settings) ------------------------------------------------------

const limitsBody = {
  effective: {
    symbol_whitelist: ["BTC/USDT"],
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
  },
  changeable_fields: ["max_open_positions"],
  changes: [],
};

const limitChangeRow = {
  change_id: "d6e7f8a9-b0c1-4d2e-8f3a-5b6c7d8e9f0a",
  field: "max_open_positions",
  old_value: "3",
  new_value: "5",
  actor_id: "admin-1",
  changed_at: "2026-07-05T10:00:00Z",
};

describe("risk limits fetcher and proxy POST", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
  });

  it("GETs limits with the READ token on the API base and parses the status", async () => {
    vi.stubEnv("NEXT_PUBLIC_API_BASE_URL", "http://cp.local");
    vi.stubEnv("NEXT_PUBLIC_READ_TOKEN", "read-tok");
    const mock = stubFetch(jsonResponse(200, limitsBody));

    const status = await fetchLimits(STRATEGY_ID);

    expect(mock.mock.calls[0]?.[0]).toBe(`http://cp.local/api/v1/strategies/${STRATEGY_ID}/limits`);
    expect(mock.mock.calls[0]?.[1]).toMatchObject({
      headers: { authorization: "Bearer read-tok" },
      cache: "no-store",
    });
    expect(status.effective.daily_loss_limit_quote).toBe("250.00");
    expect(status.effective.l2_envelope).toBeNull();
  });

  it("POSTs limit changes same-origin with the exact body and NO auth header", async () => {
    vi.stubEnv("NEXT_PUBLIC_READ_TOKEN", "read-tok");
    const mock = stubFetch(jsonResponse(200, { changes: [limitChangeRow] }));

    const res = await postLimits(
      STRATEGY_ID,
      buildLimitChanges({ max_open_positions: 5, per_position_notional_cap_quote: "1500.00" }),
    );

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/strategies/${STRATEGY_ID}/limits`);
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"changes":{"max_open_positions":5,"per_position_notional_cap_quote":"1500.00"}}',
    );
    // The operator credential never reaches this client: content-type only.
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.changes[0]?.new_value).toBe("5");
  });

  it("surfaces upstream error bodies untouched through ApiError", async () => {
    stubFetch(jsonResponse(403, { code: "FORBIDDEN", message: "role lacks limit changes" }));
    const err = await postLimits(STRATEGY_ID, buildLimitChanges({ max_open_positions: 5 })).catch(
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
    expect((err as ApiError).body?.code).toBe("FORBIDDEN");
  });
});
