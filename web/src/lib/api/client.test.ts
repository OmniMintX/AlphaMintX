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
  bootstrap,
  buildApprovalPayload,
  buildClearPayload,
  buildKillPayload,
  buildLifecyclePayload,
  buildUrl,
  clearStrategyKill,
  createTenant,
  fetchAlerts,
  fetchLimits,
  fetchMe,
  fetchPaperGate,
  fetchPlatformSecrets,
  fetchSafety,
  fetchTenants,
  fetchUsers,
  login,
  logout,
  postKill,
  postKillClear,
  postLifecycle,
  postLimits,
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

  it("createTenant POSTs {name} and parses the created tenant directly (no envelope)", async () => {
    const mock = stubFetch(jsonResponse(200, tenant));

    const res = await createTenant("Acme Trading");

    expect(mock.mock.calls[0]?.[0]).toBe("/api/cp/tenants");
    expect(mock.mock.calls[0]?.[1]?.body).toBe('{"name":"Acme Trading"}');
    expect(res.tenant_id).toBe("t-1");
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
