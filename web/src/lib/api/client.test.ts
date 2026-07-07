// Client-layer pure pieces: URL/query construction, the approval POST
// payload, and ApiError body parsing (409 recorded outcome). Plus the
// session-shell wiring: every GET/POST goes same-origin through /api/cp
// (cookie-authenticated server-side — NO auth header or token in this
// bundle), the auth helpers hit /api/auth/*, and the OS-29 409
// no-auto-retry rule still holds.

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ApiError,
  CP_PROXY_BASE,
  PAPER_GATE_POLL_INTERVAL_MS,
  POLL_INTERVAL_MS,
  ackRestore,
  bootstrap,
  buildApprovalPayload,
  buildClearPayload,
  buildKillPayload,
  buildLifecyclePayload,
  buildUrl,
  clearPlatformKill,
  clearStrategyKill,
  clearTenantKill,
  closeBillingPeriod,
  createStrategy,
  createTenant,
  fetchAlerts,
  fetchBackups,
  fetchGlobalAlerts,
  fetchInvoiceDetail,
  fetchInvoices,
  fetchLeaderboard,
  fetchLimits,
  fetchMe,
  fetchOmsReconStatus,
  fetchPaperGate,
  fetchPerformance,
  fetchPlatformSecrets,
  fetchReconciliationDetail,
  fetchReconciliations,
  fetchRestoreStatus,
  fetchSafety,
  fetchTenants,
  fetchTokens,
  fetchUsers,
  killPlatform,
  killTenant,
  login,
  logout,
  mintToken,
  postKill,
  postKillClear,
  postLifecycle,
  postLimits,
  revokeToken,
  runBackup,
  runBillingReconcile,
  runOmsRecon,
  setBinanceSecret,
  setLlmSecret,
  signup,
} from "./client";
import {
  approvalRequestSchema,
  buildLimitChanges,
  killClearRequestSchema,
  killRequestSchema,
  lifecycleRequestSchema,
} from "./schema";
import {
  NO_STORE,
  forwardAuthPost,
  forwardWithSession,
  jsonError,
  unconfigured,
} from "./session";
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

describe("CP_PROXY_BASE", () => {
  it("is the same-origin session proxy base", () => {
    expect(CP_PROXY_BASE).toBe("/api/cp");
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

const performanceBody = {
  strategy_id: STRATEGY_ID,
  window_started_at: "2026-06-20T00:00:00Z",
  evaluated_at: "2026-07-04T12:00:00Z",
  seed: "10000",
  model: "gpt-4o",
  equity_curve: [{ ts: "2026-07-04T12:00:00Z", equity: "10012.3" }],
  stats: {
    realized_pnl: "12.3",
    return_pct: "0.123",
    max_drawdown_pct: "1.2",
    closed_trades: 3,
    wins: 2,
    losses: 1,
    win_rate_pct: "66.67",
    profit_factor: "2.5",
    fees_paid: "0.4",
    last_fill_at: "2026-07-04T12:00:00Z",
  },
};

const leaderboardBody = {
  evaluated_at: "2026-07-04T12:00:00Z",
  items: [
    {
      rank: 1,
      strategy_id: STRATEGY_ID,
      name: "BTC momentum",
      tenant_id: "tenant-1",
      lifecycle_state: "paper",
      model: "gpt-4o",
      seed: "10000",
      equity: "10012.3",
      realized_pnl: "12.3",
      return_pct: "0.123",
      max_drawdown_pct: "1.2",
      closed_trades: 3,
      win_rate_pct: "66.67",
      profit_factor: "2.5",
      last_fill_at: "2026-07-04T12:00:00Z",
    },
  ],
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

  it("GETs safety/alerts/paper-gate same-origin through /api/cp with no auth header", async () => {
    const mock = stubFetch(
      jsonResponse(200, safetyBody),
      jsonResponse(200, { items: [], total: 0, page: 2, limit: 20 }),
      jsonResponse(200, gateBody),
    );

    await fetchSafety(STRATEGY_ID);
    await fetchAlerts(STRATEGY_ID, 2);
    await fetchPaperGate(STRATEGY_ID);

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/safety`);
    expect(mock.mock.calls[1]?.[0]).toBe(
      `/api/cp/strategies/${STRATEGY_ID}/alerts?page=2&limit=20`,
    );
    expect(mock.mock.calls[2]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/paper-gate`);
    for (const call of mock.mock.calls) {
      // The session cookie rides along automatically; no token in the bundle.
      expect(call[1]).toEqual({ cache: "no-store" });
    }
  });

  it("GETs arena leaderboard/performance same-origin through /api/cp", async () => {
    const mock = stubFetch(
      jsonResponse(200, leaderboardBody),
      jsonResponse(200, performanceBody),
      jsonResponse(200, performanceBody),
    );

    const board = await fetchLeaderboard();
    const perf = await fetchPerformance(STRATEGY_ID, 500);
    await fetchPerformance(STRATEGY_ID);

    expect(board.items[0]?.return_pct).toBe("0.123");
    expect(perf.equity_curve[0]?.equity).toBe("10012.3");
    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/arena/leaderboard");
    expect(mock.mock.calls[1]?.[0]).toBe(
      `/api/cp/strategies/${STRATEGY_ID}/performance?max_points=500`,
    );
    // max_points omitted -> no query string, never "undefined".
    expect(mock.mock.calls[2]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/performance`);
    for (const call of mock.mock.calls) {
      expect(call[1]).toEqual({ cache: "no-store" });
    }
  });

  it("POSTs lifecycle/kill/clear same-origin with exact bodies and NO auth header", async () => {
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

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/lifecycle`);
    expect(mock.mock.calls[1]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/kill`);
    expect(mock.mock.calls[2]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/kill/clear`);
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"to":"paper","reason":"resume"}');
    expect(mock.mock.calls[1]?.[1]?.body).toBe('{"flatten":true}');
    expect(mock.mock.calls[2]?.[1]?.body).toBe('{"reason":"resolved","observed_epoch":5}');
    for (const call of mock.mock.calls) {
      // No credential ever reaches this client (OS-32): the proxy POST
      // carries content-type only.
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

  it("GETs limits same-origin through /api/cp and parses the status", async () => {
    const mock = stubFetch(jsonResponse(200, limitsBody));

    const status = await fetchLimits(STRATEGY_ID);

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/limits`);
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    expect(status.effective.daily_loss_limit_quote).toBe("250.00");
    expect(status.effective.l2_envelope).toBeNull();
  });

  it("POSTs limit changes same-origin with the exact body and NO auth header", async () => {
    const mock = stubFetch(jsonResponse(200, { changes: [limitChangeRow] }));

    const res = await postLimits(
      STRATEGY_ID,
      buildLimitChanges({ max_open_positions: 5, per_position_notional_cap_quote: "1500.00" }),
    );

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/cp/strategies/${STRATEGY_ID}/limits`);
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"changes":{"max_open_positions":5,"per_position_notional_cap_quote":"1500.00"}}',
    );
    // No credential ever reaches this client: content-type only.
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

// ---- Platform settings & admin (platform_admin only) --------------------------------

const binanceSecret = {
  kind: "binance",
  meta: { env: "testnet", api_key_last4: "wfK4" },
  updated_at: "2026-07-05T09:00:00Z",
  updated_by: "root@example.com",
};

const llmSecret = {
  kind: "llm",
  meta: { base_url: "https://api.openai.com/v1", api_key_last4: "ab12", timeout_seconds: 30 },
  updated_at: "2026-07-05T09:01:00Z",
  updated_by: "root@example.com",
};

const tenant = { tenant_id: "t-1", name: "Acme Trading", created_at: "2026-07-01T00:00:00Z" };

const adminUser = {
  user_id: "u-1",
  email: "op@example.com",
  tenant_id: "t-1",
  role: "owner",
  created_at: "2026-07-01T00:00:00Z",
  disabled: false,
};

describe("platform settings and admin helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("GETs /platform/secrets same-origin and parses both meta variants", async () => {
    const mock = stubFetch(jsonResponse(200, { items: [binanceSecret, llmSecret] }));

    const res = await fetchPlatformSecrets();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/platform/secrets");
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    const [binance, llm] = res.items;
    expect(binance?.kind).toBe("binance");
    if (binance?.kind === "binance") expect(binance.meta.env).toBe("testnet");
    expect(llm?.kind).toBe("llm");
    if (llm?.kind === "llm") expect(llm.meta.timeout_seconds).toBe(30);
  });

  it("setBinanceSecret POSTs the exact write-only body and parses the metadata echo", async () => {
    const mock = stubFetch(jsonResponse(200, { secret: binanceSecret }));

    const res = await setBinanceSecret("testnet", "AKfull-key", "s3cr3t");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/platform/secrets/binance");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"env":"testnet","api_key":"AKfull-key","api_secret":"s3cr3t"}',
    );
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.secret.kind).toBe("binance");
  });

  it("setLlmSecret POSTs {base_url, api_key, timeout_seconds, models} exactly", async () => {
    const mock = stubFetch(jsonResponse(200, { secret: llmSecret }));

    const res = await setLlmSecret(
      "https://api.openai.com/v1",
      "sk-x",
      30,
      "gpt-4o",
      "gpt-4o-mini",
    );

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/platform/secrets/llm");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"base_url":"https://api.openai.com/v1","api_key":"sk-x","timeout_seconds":30,' +
        '"trader_model":"gpt-4o","default_model":"gpt-4o-mini"}',
    );
    expect(res.secret.kind).toBe("llm");
  });

  it("parses llm meta that includes the model fields", async () => {
    const withModels = {
      ...llmSecret,
      meta: { ...llmSecret.meta, trader_model: "gpt-4o", default_model: "gpt-4o-mini" },
    };
    stubFetch(jsonResponse(200, { items: [withModels] }));

    const res = await fetchPlatformSecrets();

    const [llm] = res.items;
    expect(llm?.kind).toBe("llm");
    if (llm?.kind === "llm") expect(llm.meta.trader_model).toBe("gpt-4o");
  });

  it("GETs /tenants and /users same-origin with no auth header", async () => {
    const mock = stubFetch(
      jsonResponse(200, { items: [tenant] }),
      jsonResponse(200, { items: [adminUser] }),
    );

    const tenants = await fetchTenants();
    const users = await fetchUsers();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tenants");
    expect(mock.mock.calls[1]?.[0]).toBe("/api/cp/users");
    for (const call of mock.mock.calls) {
      expect(call[1]).toEqual({ cache: "no-store" });
    }
    expect(tenants.items[0]?.name).toBe("Acme Trading");
    expect(users.items[0]?.disabled).toBe(false);
  });

  it("createTenant POSTs {tenant_id, name} and parses the tenant plus owner_token", async () => {
    const ownerToken = {
      token_id: "tok-owner-1",
      tenant_id: "t-1",
      principal: "user",
      role: "owner",
      strategy_id: null,
      label: "initial-owner",
      created_by: "env-admin",
      created_at: "2026-07-01T00:00:00Z",
      revoked_at: null,
      token: "amx_plain_owner",
    };
    const mock = stubFetch(jsonResponse(200, { ...tenant, owner_token: ownerToken }));

    const res = await createTenant("t-1", "Acme Trading");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tenants");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"tenant_id":"t-1","name":"Acme Trading"}');
    expect(res.tenant_id).toBe("t-1");
    // The plaintext owner token rides along exactly once.
    expect(res.owner_token.token).toBe("amx_plain_owner");
  });

  it("createTenant surfaces INVALID_TENANT_ID and TENANT_EXISTS verbatim as ApiError", async () => {
    stubFetch(
      jsonResponse(400, { code: "INVALID_TENANT_ID", message: '"default" is reserved' }),
      jsonResponse(409, { code: "TENANT_EXISTS", message: "tenant_id already taken" }),
    );

    const invalid = await createTenant("default", "Nope").catch((e: unknown) => e);
    expect(invalid).toBeInstanceOf(ApiError);
    expect((invalid as ApiError).status).toBe(400);
    expect((invalid as ApiError).body?.code).toBe("INVALID_TENANT_ID");

    const taken = await createTenant("t-1", "Again").catch((e: unknown) => e);
    expect(taken).toBeInstanceOf(ApiError);
    expect((taken as ApiError).status).toBe(409);
    expect((taken as ApiError).body?.code).toBe("TENANT_EXISTS");
  });

  it("createStrategy POSTs the exact body and parses the Strategy row directly", async () => {
    const strategyRow = {
      strategy_id: STRATEGY_ID,
      tenant_id: "t-1",
      name: "mean-revert-1",
      lifecycle_state: "paper",
      created_at: "2026-07-06T10:00:00Z",
      updated_at: "2026-07-06T10:00:00Z",
    };
    const mock = stubFetch(jsonResponse(200, strategyRow), jsonResponse(200, strategyRow));

    const res = await createStrategy({
      tenant_id: "t-1",
      name: "mean-revert-1",
      lifecycle_state: "paper",
    });
    // Undefined lifecycle_state is dropped by JSON.stringify — never sent as null.
    await createStrategy({ tenant_id: "t-1", name: "mean-revert-1" });

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/strategies");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"tenant_id":"t-1","name":"mean-revert-1","lifecycle_state":"paper"}',
    );
    expect(mock.mock.calls[1]?.[1]?.body).toBe('{"tenant_id":"t-1","name":"mean-revert-1"}');
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.lifecycle_state).toBe("paper");
  });

  it("createStrategy surfaces 409 STRATEGY_NAME_TAKEN verbatim as ApiError", async () => {
    stubFetch(jsonResponse(409, { code: "STRATEGY_NAME_TAKEN", message: "name in use" }));
    const err = await createStrategy({ tenant_id: "t-1", name: "dup" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).body?.code).toBe("STRATEGY_NAME_TAKEN");
  });

  it("surfaces 403 and 503 VAULT_UNAVAILABLE verbatim as ApiError", async () => {
    stubFetch(
      jsonResponse(403, { code: "FORBIDDEN", message: "platform_admin required" }),
      jsonResponse(503, { code: "VAULT_UNAVAILABLE", message: "vault key not provisioned" }),
    );

    const forbidden = await fetchPlatformSecrets().catch((e: unknown) => e);
    expect(forbidden).toBeInstanceOf(ApiError);
    expect((forbidden as ApiError).status).toBe(403);
    expect((forbidden as ApiError).body?.code).toBe("FORBIDDEN");

    const vault = await setBinanceSecret("prod", "k", "s").catch((e: unknown) => e);
    expect(vault).toBeInstanceOf(ApiError);
    expect((vault as ApiError).status).toBe(503);
    expect((vault as ApiError).body?.code).toBe("VAULT_UNAVAILABLE");
  });
});

// ---- Global alerts & API tokens ------------------------------------------------------

const globalAlert = {
  alert_id: "c5d6e7f8-a9b0-4c1d-8e2f-4a5b6c7d8e9f",
  kind: "watchdog_stall",
  strategy_id: null,
  ref_id: null,
  details_json: "{}",
  recorded_at: "2026-07-05T11:00:00Z",
};

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

describe("global alerts and token helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("fetchGlobalAlerts GETs /alerts same-origin and omits an empty kind entirely", async () => {
    const mock = stubFetch(
      jsonResponse(200, { items: [globalAlert], total: 1, page: 1, limit: 20 }),
      jsonResponse(200, { items: [], total: 0, page: 2, limit: 20 }),
    );

    const page = await fetchGlobalAlerts();
    await fetchGlobalAlerts(2, 20, "breaker_daily_loss");

    // No kind param at all when the filter is empty — never kind=.
    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/alerts?page=1&limit=20");
    expect(mock.mock.calls[1]?.[0]).toBe(
      "/api/cp/alerts?page=2&limit=20&kind=breaker_daily_loss",
    );
    for (const call of mock.mock.calls) {
      expect(call[1]).toEqual({ cache: "no-store" });
    }
    expect(page.items[0]?.strategy_id).toBeNull();
  });

  it("fetchGlobalAlerts surfaces the tenant-principal 403 verbatim (UI gates, not the client)", async () => {
    stubFetch(jsonResponse(403, { code: "FORBIDDEN", message: "env-class read" }));
    const err = await fetchGlobalAlerts().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(403);
    expect((err as ApiError).body?.code).toBe("FORBIDDEN");
  });

  it("fetchTokens GETs the metadata page (never plaintext)", async () => {
    const mock = stubFetch(
      jsonResponse(200, { items: [userToken, agentToken], total: 2, page: 1, limit: 20 }),
    );

    const page = await fetchTokens();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tokens?page=1&limit=20");
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    expect(page.items[0]?.principal).toBe("user");
    expect(page.items[1]?.strategy_id).toBe(STRATEGY_ID);
  });

  it("mintToken POSTs the exact user-token body and parses the plaintext-once echo", async () => {
    const mock = stubFetch(jsonResponse(200, { ...userToken, token: "amx_plain_1" }));

    const res = await mintToken({ principal: "user", role: "operator", label: "ops laptop" });

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tokens");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"principal":"user","role":"operator","label":"ops laptop"}',
    );
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.token).toBe("amx_plain_1");
    expect(res.role).toBe("operator");
    expect(res.strategy_id).toBeNull();
  });

  it("mintToken agent variant carries tenant_id + strategy_id and no role", async () => {
    const mock = stubFetch(jsonResponse(200, { ...agentToken, token: "amx_plain_2" }));

    const res = await mintToken({
      tenant_id: "t-1",
      principal: "agent",
      strategy_id: STRATEGY_ID,
      label: "runner",
    });

    // Undefined keys (role) are dropped by JSON.stringify — never sent as null.
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      `{"tenant_id":"t-1","principal":"agent","strategy_id":"${STRATEGY_ID}","label":"runner"}`,
    );
    expect(res.role).toBeNull();
    expect(res.strategy_id).toBe(STRATEGY_ID);
  });

  it("revokeToken POSTs {token_id}/revoke with an empty object body and parses the revoked row", async () => {
    const mock = stubFetch(
      jsonResponse(200, { ...userToken, revoked_at: "2026-07-05T10:00:00Z" }),
    );

    const res = await revokeToken("tok-1");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tokens/tok-1/revoke");
    expect(mock.mock.calls[0]?.[1]?.body).toBe("{}");
    expect(res.revoked_at).toBe("2026-07-05T10:00:00Z");
  });
});

// ---- Auth helpers (session shell) --------------------------------------------------

const SESSION_USER = {
  user_id: "u-1",
  email: "op@example.com",
  tenant_id: "t-1",
  role: "owner",
};

describe("auth helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("login POSTs /api/auth/login with the exact body and parses {user}", async () => {
    const mock = stubFetch(jsonResponse(200, { user: SESSION_USER }));

    const res = await login("op@example.com", "hunter22");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/auth/login");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"email":"op@example.com","password":"hunter22"}',
    );
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.user.email).toBe("op@example.com");
  });

  it("login surfaces a 401 INVALID_CREDENTIALS verbatim as ApiError", async () => {
    stubFetch(jsonResponse(401, { code: "INVALID_CREDENTIALS", message: "bad email or password" }));
    const err = await login("op@example.com", "wrong").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
    expect((err as ApiError).body?.code).toBe("INVALID_CREDENTIALS");
  });

  it("logout POSTs /api/auth/logout with an empty object body", async () => {
    const mock = stubFetch(jsonResponse(200, {}));

    await logout();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/auth/logout");
    expect(mock.mock.calls[0]?.[1]?.body).toBe("{}");
  });

  it("signup POSTs /api/auth/signup with the tenant_name body", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        tenant: { tenant_id: "t-1", name: "Acme Trading", created_at: "2026-07-06T10:00:00Z" },
        user: { user_id: "u-1", email: "op@example.com", tenant_id: "t-1", role: "owner" },
      }),
    );

    const res = await signup("Acme Trading", "op@example.com", "hunter22");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/auth/signup");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"tenant_name":"Acme Trading","email":"op@example.com","password":"hunter22"}',
    );
    expect(res.tenant.tenant_id).toBe("t-1");
    expect(res.user.role).toBe("owner");
  });

  it("bootstrap POSTs /api/auth/bootstrap and parses the admin response", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        user: { user_id: "u-0", email: "root@example.com", tenant_id: null, role: "platform_admin" },
      }),
    );

    const res = await bootstrap("root@example.com", "hunter22");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/auth/bootstrap");
    expect(mock.mock.calls[0]?.[1]?.body).toBe(
      '{"email":"root@example.com","password":"hunter22"}',
    );
    expect(res.user.role).toBe("platform_admin");
  });

  it("fetchMe GETs /api/auth/me uncached and 401s as ApiError on no session", async () => {
    const mock = stubFetch(
      jsonResponse(200, { user: SESSION_USER, session_id: "sess-1" }),
      jsonResponse(401, { code: "UNAUTHENTICATED", message: "no session" }),
    );

    const me = await fetchMe();
    expect(mock.mock.calls[0]?.[0]).toBe("/api/auth/me");
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    expect(me.role).toBe("owner");

    const err = await fetchMe().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(401);
  });
});


// ---- Billing & platform ops (Phase 5) ------------------------------------------------

const INVOICE_ID = "e7f8a9b0-c1d2-4e3f-8a4b-6c7d8e9f0a1b";
const RECON_ID = "a9b0c1d2-e3f4-4a5b-8c6d-8e9f0a1b2c3d";

const invoiceBody = {
  invoice_id: INVOICE_ID,
  tenant_id: "t-1",
  period: "2026-06",
  total_usd: "12.345678",
  line_count: 1,
  generated_at: "2026-07-01T00:00:05Z",
};

const invoiceLineBody = {
  line_id: "f8a9b0c1-d2e3-4f4a-8b5c-7d8e9f0a1b2c",
  invoice_id: INVOICE_ID,
  strategy_id: STRATEGY_ID,
  model: "gpt-4o",
  entry_type: "usage",
  original_period: null,
  input_tokens: 120000,
  output_tokens: 24000,
  amount_usd: "12.345678",
};

const reconRunBody = {
  recon_id: RECON_ID,
  tenant_id: "t-1",
  period: "2026-06",
  invoice_id: INVOICE_ID,
  status: "pass",
  matched_count: 41,
  discrepancy_count: 1,
  matched_client_cost_usd: "12.345678",
  orphan_client_cost_usd: "0",
  estimated_client_cost_usd: "0",
  unattributed_client_cost_usd: "0",
  invoice_total_usd: "12.345678",
  run_at: "2026-07-02T00:00:00Z",
};

const discrepancyBody = {
  discrepancy_id: "b0c1d2e3-f4a5-4b6c-8d7e-9f0a1b2c3d4e",
  recon_id: RECON_ID,
  class: "unattributed",
  request_id: null,
  strategy_id: null,
  details_json: "{}",
};

describe("billing fetchers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("GETs the invoice and reconciliation pages same-origin through /api/cp", async () => {
    const mock = stubFetch(
      jsonResponse(200, { items: [invoiceBody], total: 1, page: 1, limit: 20 }),
      jsonResponse(200, { items: [reconRunBody], total: 1, page: 2, limit: 50 }),
    );

    const invoices = await fetchInvoices(1);
    const recons = await fetchReconciliations(2, 50);

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/billing/invoices?page=1&limit=20");
    expect(mock.mock.calls[1]?.[0]).toBe("/api/cp/billing/reconciliations?page=2&limit=50");
    for (const call of mock.mock.calls) {
      expect(call[1]).toEqual({ cache: "no-store" });
    }
    expect(invoices.items[0]?.total_usd).toBe("12.345678");
    expect(recons.items[0]?.status).toBe("pass");
  });

  it("GETs the invoice and reconciliation details (nulls in discrepancies pass)", async () => {
    const mock = stubFetch(
      jsonResponse(200, { invoice: invoiceBody, lines: [invoiceLineBody] }),
      jsonResponse(200, { run: reconRunBody, discrepancies: [discrepancyBody] }),
    );

    const invoice = await fetchInvoiceDetail(INVOICE_ID);
    const recon = await fetchReconciliationDetail(RECON_ID);

    expect(mock.mock.calls[0]?.[0]).toBe(`/api/cp/billing/invoices/${INVOICE_ID}`);
    expect(mock.mock.calls[1]?.[0]).toBe(`/api/cp/billing/reconciliations/${RECON_ID}`);
    expect(invoice.lines[0]?.original_period).toBeNull();
    expect(recon.discrepancies[0]?.request_id).toBeNull();
    expect(recon.discrepancies[0]?.strategy_id).toBeNull();
  });

  it("surfaces the no-oracle 404 verbatim as ApiError", async () => {
    stubFetch(jsonResponse(404, { code: "UNKNOWN_INVOICE", message: "unknown invoice" }));
    const err = await fetchInvoiceDetail(INVOICE_ID).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).body?.code).toBe("UNKNOWN_INVOICE");
  });

  it("closeBillingPeriod POSTs {tenant_id, period} and parses the invoice with lines", async () => {
    const mock = stubFetch(jsonResponse(200, { invoice: invoiceBody, lines: [invoiceLineBody] }));

    const res = await closeBillingPeriod("t-1", "2026-06");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/billing/periods/close");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"tenant_id":"t-1","period":"2026-06"}');
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.invoice.total_usd).toBe("12.345678");
    expect(res.lines[0]?.entry_type).toBe("usage");
  });

  it("closeBillingPeriod surfaces INVALID_PERIOD / PERIOD_CLOSED verbatim as ApiError", async () => {
    stubFetch(
      jsonResponse(400, { code: "INVALID_PERIOD", message: "period is still running" }),
      jsonResponse(409, { code: "PERIOD_CLOSED", message: "period already closed" }),
    );

    const running = await closeBillingPeriod("t-1", "2026-07").catch((e: unknown) => e);
    expect(running).toBeInstanceOf(ApiError);
    expect((running as ApiError).status).toBe(400);
    expect((running as ApiError).body?.code).toBe("INVALID_PERIOD");

    const closed = await closeBillingPeriod("t-1", "2026-06").catch((e: unknown) => e);
    expect(closed).toBeInstanceOf(ApiError);
    expect((closed as ApiError).status).toBe(409);
    expect((closed as ApiError).body?.code).toBe("PERIOD_CLOSED");
  });

  it("runBillingReconcile POSTs the same body and parses the run with discrepancies", async () => {
    const mock = stubFetch(
      jsonResponse(200, { run: reconRunBody, discrepancies: [discrepancyBody] }),
    );

    const res = await runBillingReconcile("t-1", "2026-06");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/billing/reconcile");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"tenant_id":"t-1","period":"2026-06"}');
    expect(res.run.status).toBe("pass");
    expect(res.discrepancies[0]?.class).toBe("unattributed");
  });

  it("runBillingReconcile surfaces 404 UNKNOWN_TENANT verbatim as ApiError", async () => {
    stubFetch(jsonResponse(404, { code: "UNKNOWN_TENANT", message: "no such tenant" }));
    const err = await runBillingReconcile("ghost", "2026-06").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).body?.code).toBe("UNKNOWN_TENANT");
  });
});

// ---- OMS reconciliation (live-oms-and-reconciler.md §API surface) -------------------

describe("oms recon helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const reconRunFull = {
    run_id: "run-20260706T020000Z",
    started_at: "2026-07-06T02:00:00Z",
    completed_at: "2026-07-06T02:00:04Z",
    status: "completed",
    counters: { orders_fetched: 12 },
  };

  it("fetchOmsReconStatus GETs same-origin and parses the env-admin full payload", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        mode: "live",
        venue_env: "testnet",
        reconciled: true,
        last_run: reconRunFull,
        pending_intents: 0,
        orphans: 1,
        watermarks: [{ symbol: "BTC/USDT", venue_epoch: 2, exchange_trade_id: 987654 }],
        venue_epoch: 2,
      }),
    );

    const status = await fetchOmsReconStatus();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/oms/recon/status");
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    expect(status.reconciled).toBe(true);
    expect(status.watermarks?.[0]?.venue_epoch).toBe(2);
  });

  it("fetchOmsReconStatus parses the restricted tenant payload (omitempty absences)", async () => {
    stubFetch(
      jsonResponse(200, {
        mode: "live",
        venue_env: "testnet",
        reconciled: false,
        last_run: null,
        pending_intents: 3,
      }),
    );

    const status = await fetchOmsReconStatus();

    expect(status.last_run).toBeNull();
    expect(status.orphans).toBeUndefined();
    expect(status.watermarks).toBeUndefined();
  });

  it("runOmsRecon POSTs {accept_venue_reset} and parses the completed run", async () => {
    const mock = stubFetch(jsonResponse(200, reconRunFull));

    const run = await runOmsRecon(true);

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/oms/recon/run");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"accept_venue_reset":true}');
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(run.status).toBe("completed");
  });

  it("runOmsRecon surfaces 409 RECON_RUNNING verbatim as ApiError", async () => {
    stubFetch(jsonResponse(409, { code: "RECON_RUNNING", message: "a reconcile run is in progress" }));
    const err = await runOmsRecon(false).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).body?.code).toBe("RECON_RUNNING");
  });

  it("a paper deployment answers a plain 404 with a null body", async () => {
    stubFetch(new Response("not found", { status: 404 }));
    const err = await fetchOmsReconStatus().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).body).toBeNull();
  });
});

describe("tenant / platform kill and clear helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("killTenant/clearTenantKill POST the exact bodies to the tenant routes", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        event_id: "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b",
        tenant_id: "t-1",
        kill_epoch: 3,
        recorded_at: "2026-07-05T12:00:00Z",
        flatten: true,
      }),
      jsonResponse(200, {
        clear_id: "a3b4c5d6-e7f8-4a9b-8c0d-2e3f4a5b6c7d",
        scope: "tenant",
        tenant_id: "t-1",
        cleared_epoch: 4,
        recorded_at: "2026-07-05T12:01:00Z",
        superseded_event_ids: ["e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b"],
      }),
    );

    const kill = await killTenant("t-1", true);
    const clear = await clearTenantKill("t-1", "resolved", 3);

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tenants/t-1/kill");
    expect(mock.mock.calls[1]?.[0]).toBe("/api/cp/tenants/t-1/kill/clear");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"flatten":true}');
    expect(mock.mock.calls[1]?.[1]?.body).toBe('{"reason":"resolved","observed_epoch":3}');
    for (const call of mock.mock.calls) {
      expect(call[1]?.headers).toEqual({ "content-type": "application/json" });
    }
    expect(kill.kill_epoch).toBe(3);
    expect(clear.tenant_id).toBe("t-1");
    expect(clear.strategy_id).toBeUndefined();
  });

  it("killPlatform/clearPlatformKill thread the operator-typed ack verbatim", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        event_id: "f2a3b4c5-d6e7-4f8a-9b0c-1d2e3f4a5b6c",
        kill_epoch: 7,
        recorded_at: "2026-07-05T12:00:00Z",
        flatten: false,
      }),
      jsonResponse(200, {
        clear_id: "b4c5d6e7-f8a9-4b0c-8d1e-3f4a5b6c7d8e",
        scope: "platform",
        cleared_epoch: 8,
        recorded_at: "2026-07-05T12:01:00Z",
        superseded_event_ids: [],
      }),
    );

    const kill = await killPlatform("KILL-PLATFORM", false);
    const clear = await clearPlatformKill("CLEAR-PLATFORM", "drill over", 7);

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/platform/kill");
    expect(mock.mock.calls[1]?.[0]).toBe("/api/cp/platform/kill/clear");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"ack":"KILL-PLATFORM","flatten":false}');
    expect(mock.mock.calls[1]?.[1]?.body).toBe(
      '{"reason":"drill over","observed_epoch":7,"ack":"CLEAR-PLATFORM"}',
    );
    expect(kill.kill_epoch).toBe(7);
    expect(clear.tenant_id).toBeUndefined();
    expect(clear.superseded_event_ids).toEqual([]);
  });

  it("surfaces a wrong-ack 400 verbatim — the server owns the literal, not this client", async () => {
    stubFetch(
      jsonResponse(400, {
        code: "PLATFORM_KILL_ACK_REQUIRED",
        message: 'platform kill requires the acknowledgment "ack": "KILL-PLATFORM"',
      }),
    );
    const err = await killPlatform("kill-platform", false).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(400);
    expect((err as ApiError).body?.code).toBe("PLATFORM_KILL_ACK_REQUIRED");
  });
});

describe("backup and restore-gate helpers", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  const backupRunBody = {
    artifact: "controlplane-20260706T020000Z.sqlite.gz",
    bytes: 1048576,
    sha256: "b5bb9d8014a0f9b1d61e21e796d78dccdf1352f23cd32812f4850b878ae4944c",
    tables: 24,
    rows_total: 15320,
    started_at: "2026-07-06T02:00:00Z",
    finished_at: "2026-07-06T02:00:03Z",
    verified: true,
  };

  it("runBackup POSTs an empty JSON body and parses the OB-6 result", async () => {
    const mock = stubFetch(jsonResponse(200, backupRunBody));

    const res = await runBackup();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/ops/backups/run");
    expect(mock.mock.calls[0]?.[1]?.body).toBe("{}");
    expect(mock.mock.calls[0]?.[1]?.headers).toEqual({ "content-type": "application/json" });
    expect(res.verified).toBe(true);
  });

  it("fetchBackups/fetchRestoreStatus GET same-origin (plain items, no page envelope)", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        items: [{ artifact: backupRunBody.artifact, bytes: 1048576, modified_at: "2026-07-06T02:00:03Z" }],
      }),
      jsonResponse(200, { engaged: true }),
    );

    const backups = await fetchBackups();
    const restore = await fetchRestoreStatus();

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/ops/backups");
    expect(mock.mock.calls[1]?.[0]).toBe("/api/cp/ops/restore");
    for (const call of mock.mock.calls) {
      expect(call[1]).toEqual({ cache: "no-store" });
    }
    expect(backups.items[0]?.artifact).toBe(backupRunBody.artifact);
    expect(restore.engaged).toBe(true);
  });

  it("ackRestore POSTs an empty JSON body and 409s verbatim when not engaged", async () => {
    const mock = stubFetch(
      jsonResponse(200, { cleared: true }),
      jsonResponse(409, { code: "RESTORE_GATE_NOT_ENGAGED", message: "restore gate is not engaged" }),
    );

    const res = await ackRestore();
    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/ops/restore/ack");
    expect(mock.mock.calls[0]?.[1]?.body).toBe("{}");
    expect(res.cleared).toBe(true);

    const err = await ackRestore().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(409);
    expect((err as ApiError).body?.code).toBe("RESTORE_GATE_NOT_ENGAGED");
  });
});

// ---- Session-proxy response headers -------------------------------------------------
// Every server-side session-proxy response must carry Cache-Control: no-store —
// Next.js adds no Cache-Control to route-handler responses, so a shared cache
// could otherwise store authenticated bodies (RFC 9111 §3.5 heuristic caching).

describe("session-proxy responses are never cacheable", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.unstubAllEnvs();
  });

  const cookieRequest = (url: string, init: RequestInit = {}) =>
    new Request(url, { ...init, headers: { cookie: "amx_session=tok-1", ...init.headers } });

  it("jsonError and unconfigured stamp no-store", () => {
    expect(jsonError(401, "UNAUTHENTICATED", "no session").headers.get("cache-control")).toBe(
      NO_STORE,
    );
    expect(unconfigured().headers.get("cache-control")).toBe(NO_STORE);
  });

  it("forwardWithSession stamps no-store on upstream passthrough, success and error", async () => {
    vi.stubEnv("CONTROLPLANE_API_BASE_URL", "http://cp.local");
    stubFetch(
      jsonResponse(200, { items: [] }),
      jsonResponse(401, { code: "UNAUTHENTICATED", message: "session revoked" }),
    );

    const ok = await forwardWithSession(
      cookieRequest("http://web.local/api/cp/strategies"),
      "/strategies",
      "GET",
    );
    expect(ok.status).toBe(200);
    expect(ok.headers.get("cache-control")).toBe(NO_STORE);

    const denied = await forwardWithSession(
      cookieRequest("http://web.local/api/cp/strategies"),
      "/strategies",
      "GET",
    );
    expect(denied.status).toBe(401);
    expect(denied.headers.get("cache-control")).toBe(NO_STORE);
  });

  it("forwardWithSession 401s with no-store when there is no session cookie", async () => {
    const res = await forwardWithSession(
      new Request("http://web.local/api/cp/strategies"),
      "/strategies",
      "GET",
    );
    expect(res.status).toBe(401);
    expect(res.headers.get("cache-control")).toBe(NO_STORE);
  });

  it("forwardAuthPost stamps no-store on the anonymous auth passthrough", async () => {
    vi.stubEnv("CONTROLPLANE_API_BASE_URL", "http://cp.local");
    stubFetch(jsonResponse(200, { user: SESSION_USER }));

    const res = await forwardAuthPost(
      new Request("http://web.local/api/auth/signup", { method: "POST", body: "{}" }),
      "/auth/signup",
    );
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe(NO_STORE);
  });
});
