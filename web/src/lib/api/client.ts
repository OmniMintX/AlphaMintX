// Typed fetch wrapper for the control-plane HTTP API, session edition: every
// read and mutation goes through a same-origin Next route. Data traffic hits
// /api/cp/<path> (the catch-all session proxy, app/api/cp/[...path]/route.ts,
// which attaches the HttpOnly amx_session cookie's bearer server-side) and
// auth flows hit /api/auth/*. No token or base URL is ever inlined into this
// bundle — the browser never sees a credential.

import type { z } from "zod";

import {
  alertsPageSchema,
  apiErrorBodySchema,
  approvalDecisionSchema,
  bootstrapResponseSchema,
  killClearResponseSchema,
  killResponseSchema,
  lifecycleResponseSchema,
  limitChangeResponseSchema,
  limitsStatusSchema,
  loginResponseSchema,
  logoutResponseSchema,
  marketAnalysisResponseSchema,
  paperGateReportSchema,
  platformSecretsResponseSchema,
  runDetailSchema,
  runsPageSchema,
  safetyStatusSchema,
  secretWriteResponseSchema,
  meResponseSchema,
  signupResponseSchema,
  strategiesPageSchema,
  strategySchema,
  tenantSchema,
  tenantsResponseSchema,
  usersResponseSchema,
  type AlertsPage,
  type AnalysisInterval,
  type AnalysisLocale,
  type AnalysisMarket,
  type ApiErrorBody,
  type ApprovalDecision,
  type ApprovalRequest,
  type BinanceEnv,
  type BootstrapResponse,
  type KillClearRequest,
  type KillClearResponse,
  type KillRequest,
  type KillResponse,
  type LifecycleRequest,
  type LifecycleResponse,
  type LifecycleState,
  type LimitChangeRequest,
  type LimitChangeResponse,
  type LimitsStatus,
  type LoginResponse,
  type LogoutResponse,
  type MarketAnalysisResponse,
  type PaperGateReport,
  type PlatformSecretsResponse,
  type RunDetail,
  type RunsPage,
  type SafetyStatus,
  type SecretWriteResponse,
  type SessionUser,
  type SignupResponse,
  type StrategiesPage,
  type Strategy,
  type Tenant,
  type TenantsResponse,
  type UsersResponse,
} from "./schema";
import { DEFAULT_LIMIT } from "./pagination";

// Polling interval for list/detail revalidation (SSE/websocket is deferred).
export const POLL_INTERVAL_MS = 10_000;

// Paper-gate poll interval (operator-surface.md OS-25): that GET self-charges
// the shared READ token's 60/min bucket (LC-24), so it polls at
// 6 x POLL_INTERVAL_MS (60 s) and never tighter — including on a 429.
export const PAPER_GATE_POLL_INTERVAL_MS = 6 * POLL_INTERVAL_MS;

// Same-origin base of the catch-all session proxy: /api/cp/<cp-path> maps to
// `${CP}/api/v1/<cp-path>` server-side.
export const CP_PROXY_BASE = "/api/cp";

export type QueryParams = Record<string, string | number | undefined>;

export function buildUrl(base: string, path: string, query?: QueryParams): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value !== undefined) params.set(key, String(value));
  }
  const qs = params.toString();
  return `${base}${path}${qs ? `?${qs}` : ""}`;
}

// Body of POST .../approvals: {verdict_id, approved: bool}.
export function buildApprovalPayload(verdictId: string, approved: boolean): ApprovalRequest {
  return { verdict_id: verdictId, approved };
}

// Body of POST .../lifecycle (LC-4): {to, reason}, reason always non-empty
// (empty reason is 400 SCHEMA_INVALID — the UI never sends without one).
export function buildLifecyclePayload(to: LifecycleState, reason: string): LifecycleRequest {
  return { to, reason };
}

// Body of POST .../kill: {flatten} — always explicit; the wire default is
// false and no client flattens by omission (safety-wiring.md §Flatten choice).
export function buildKillPayload(flatten: boolean): KillRequest {
  return { flatten };
}

// Body of POST .../kill/clear (LC-30): observed_epoch is the CAS token read
// from the displayed standing kill (OS-29), threaded verbatim.
export function buildClearPayload(reason: string, observedEpoch: number): KillClearRequest {
  return { reason, observed_epoch: observedEpoch };
}

export class ApiError extends Error {
  readonly status: number;
  readonly body: ApiErrorBody | null;

  constructor(status: number, rawBody: unknown) {
    const parsed = apiErrorBodySchema.safeParse(rawBody);
    const body = parsed.success ? parsed.data : null;
    super(body ? `${body.code}: ${body.message}` : `HTTP ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

async function parseResponse<T>(res: Response, schema: z.ZodType<T>): Promise<T> {
  const text = await res.text();
  let json: unknown = null;
  if (text) {
    try {
      json = JSON.parse(text);
    } catch {
      json = null;
    }
  }
  if (!res.ok) throw new ApiError(res.status, json);
  return schema.parse(json);
}

// Same-origin GET through the session proxy: the cookie rides along
// automatically and no auth header is ever attached here.
async function apiGet<T>(path: string, schema: z.ZodType<T>, query?: QueryParams): Promise<T> {
  const res = await fetch(buildUrl(CP_PROXY_BASE, path, query), { cache: "no-store" });
  return parseResponse(res, schema);
}

export function fetchStrategies(page = 1, limit = DEFAULT_LIMIT): Promise<StrategiesPage> {
  return apiGet("/strategies", strategiesPageSchema, { page, limit });
}

export function fetchStrategy(strategyId: string): Promise<Strategy> {
  return apiGet(`/strategies/${strategyId}`, strategySchema);
}

export function fetchRuns(strategyId: string, page = 1, limit = DEFAULT_LIMIT): Promise<RunsPage> {
  return apiGet(`/strategies/${strategyId}/runs`, runsPageSchema, { page, limit });
}

export function fetchRunDetail(strategyId: string, runId: string): Promise<RunDetail> {
  return apiGet(`/strategies/${strategyId}/runs/${runId}`, runDetailSchema);
}

// Composite safety status (operator-surface.md OS-7): lifecycle state,
// binding kills with their clears, breaker-today, watchdog liveness.
export function fetchSafety(strategyId: string): Promise<SafetyStatus> {
  return apiGet(`/strategies/${strategyId}/safety`, safetyStatusSchema);
}

// Per-strategy safety_alerts feed (OS-15/OS-16), newest first; pages are
// per-poll snapshots under LIMIT/OFFSET.
export function fetchAlerts(strategyId: string, page = 1, limit = DEFAULT_LIMIT): Promise<AlertsPage> {
  return apiGet(`/strategies/${strategyId}/alerts`, alertsPageSchema, { page, limit });
}

// The LC-23 paper-gate report (LC-24 read). Poll at
// PAPER_GATE_POLL_INTERVAL_MS only — this GET self-charges the rate bucket.
export function fetchPaperGate(strategyId: string): Promise<PaperGateReport> {
  return apiGet(`/strategies/${strategyId}/paper-gate`, paperGateReportSchema);
}

// DB-backed risk limits: effective values, changeable fields, and the
// change audit trail.
export function fetchLimits(strategyId: string): Promise<LimitsStatus> {
  return apiGet(`/strategies/${strategyId}/limits`, limitsStatusSchema);
}

// POSTs to a same-origin Next route (the session proxy attaches the cookie's
// bearer server-side; this client attaches no auth header). Upstream errors
// pass through verbatim and surface as ApiError (OS-30).
async function proxyPost<T>(path: string, payload: unknown, schema: z.ZodType<T>): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(payload),
  });
  return parseResponse(res, schema);
}

// Records the L1 decision through the same-origin server proxy. A repeat
// decision (double-click, human-vs-timeout race) surfaces as ApiError status
// 409 with the recorded outcome in error.body.recorded; approved-but-blocked
// preflight results come back as a normal ApprovalDecision with
// outcome="approved_but_blocked"; a post-approval OMS submission failure
// comes back as outcome="approved" with submitted=false.
export function postApproval(
  strategyId: string,
  payload: ApprovalRequest,
): Promise<ApprovalDecision> {
  return proxyPost(`/api/cp/strategies/${strategyId}/approvals`, payload, approvalDecisionSchema);
}

// One lifecycle transition (OS-27). Any 422 (ILLEGAL_TRANSITION,
// PAPER_GATE_FAILED, "kill tier active") surfaces verbatim, never
// pre-suppressed — the server is the sole transition authority.
export function postLifecycle(
  strategyId: string,
  payload: LifecycleRequest,
): Promise<LifecycleResponse> {
  return proxyPost(`/api/cp/strategies/${strategyId}/lifecycle`, payload, lifecycleResponseSchema);
}

// Strategy-tier kill (OS-28); the response acknowledges persistence only.
export function postKill(strategyId: string, payload: KillRequest): Promise<KillResponse> {
  return proxyPost(`/api/cp/strategies/${strategyId}/kill`, payload, killResponseSchema);
}

// Runtime risk-limit changes through the same-origin proxy; the response
// carries the audit rows recorded by this request. The control plane
// resolves the caller's role — a 403 surfaces verbatim as ApiError.
export function postLimits(
  strategyId: string,
  payload: LimitChangeRequest,
): Promise<LimitChangeResponse> {
  return proxyPost(`/api/cp/strategies/${strategyId}/limits`, payload, limitChangeResponseSchema);
}

// One-shot LLM read of the chart's indicator snapshot (Agent analysis). The
// provider key stays in the control-plane vault — only the model's text
// answer comes back. A missing LLM config surfaces as ApiError 404
// NOT_CONFIGURED; provider failures as 502 LLM_UPSTREAM.
export function requestMarketAnalysis(
  symbol: string,
  market: AnalysisMarket,
  interval: AnalysisInterval,
  locale: AnalysisLocale,
  summary: string,
): Promise<MarketAnalysisResponse> {
  return proxyPost(
    "/api/cp/market/llm-analysis",
    { symbol, market, interval, locale, summary },
    marketAnalysisResponseSchema,
  );
}

// Strategy-tier clear (OS-29).
export function postKillClear(
  strategyId: string,
  payload: KillClearRequest,
): Promise<KillClearResponse> {
  return proxyPost(`/api/cp/strategies/${strategyId}/kill/clear`, payload, killClearResponseSchema);
}

// Clears the displayed standing strategy kill (OS-29): observed_epoch is the
// kill_epoch read from the safety card. On a 409 CLEAR_CONFLICT the safety
// card is re-fetched and the conflict re-thrown for the operator — NEVER an
// auto-retry with the fresh epoch (the CAS exists so a human re-observes).
export async function clearStrategyKill(
  strategyId: string,
  reason: string,
  observedEpoch: number,
  refetchSafety: () => void,
): Promise<KillClearResponse> {
  try {
    return await postKillClear(strategyId, buildClearPayload(reason, observedEpoch));
  } catch (err) {
    if (err instanceof ApiError && err.status === 409) refetchSafety();
    throw err;
  }
}

// ---- Platform settings & admin (platform_admin only) ------------------------------
// All six go through the same /api/cp session proxy; a non-admin session
// surfaces the upstream 403 verbatim as ApiError, and an unprovisioned vault
// key surfaces 503 VAULT_UNAVAILABLE.

// Stored-secret METADATA only (kind, last4, updated_at/by) — the values are
// write-only and never come back over the wire.
export function fetchPlatformSecrets(): Promise<PlatformSecretsResponse> {
  return apiGet("/platform/secrets", platformSecretsResponseSchema);
}

// Stores the platform Binance credential; the response echoes the metadata
// snapshot only, never the submitted key/secret.
export function setBinanceSecret(
  env: BinanceEnv,
  apiKey: string,
  apiSecret: string,
): Promise<SecretWriteResponse> {
  return proxyPost(
    "/api/cp/platform/secrets/binance",
    { env, api_key: apiKey, api_secret: apiSecret },
    secretWriteResponseSchema,
  );
}

// Stores the platform LLM provider credential (same write-only semantics).
export function setLlmSecret(
  baseUrl: string,
  apiKey: string,
  timeoutSeconds: number,
  traderModel: string,
  defaultModel: string,
): Promise<SecretWriteResponse> {
  return proxyPost(
    "/api/cp/platform/secrets/llm",
    {
      base_url: baseUrl,
      api_key: apiKey,
      timeout_seconds: timeoutSeconds,
      trader_model: traderModel,
      default_model: defaultModel,
    },
    secretWriteResponseSchema,
  );
}

export function fetchTenants(): Promise<TenantsResponse> {
  return apiGet("/tenants", tenantsResponseSchema);
}

// POST /tenants answers the created tenant snapshot directly (no envelope).
export function createTenant(name: string): Promise<Tenant> {
  return proxyPost("/api/cp/tenants", { name }, tenantSchema);
}

export function fetchUsers(): Promise<UsersResponse> {
  return apiGet("/users", usersResponseSchema);
}

// ---- Auth (session shell) --------------------------------------------------------
// All auth flows hit the same-origin /api/auth/* routes. The session token is
// moved into the HttpOnly amx_session cookie server-side — the login response
// this client sees carries {"user": ...} only. Upstream errors (401
// INVALID_CREDENTIALS, 409 EMAIL_TAKEN / CONFLICT) surface verbatim as
// ApiError.

export function login(email: string, password: string): Promise<LoginResponse> {
  return proxyPost("/api/auth/login", { email, password }, loginResponseSchema);
}

export function logout(): Promise<LogoutResponse> {
  return proxyPost("/api/auth/logout", {}, logoutResponseSchema);
}

export function signup(
  tenantName: string,
  email: string,
  password: string,
): Promise<SignupResponse> {
  return proxyPost(
    "/api/auth/signup",
    { tenant_name: tenantName, email, password },
    signupResponseSchema,
  );
}

export function bootstrap(email: string, password: string): Promise<BootstrapResponse> {
  return proxyPost("/api/auth/bootstrap", { email, password }, bootstrapResponseSchema);
}

// The current session identity; a missing/revoked session is a 401 ApiError.
// The wire wraps the user next to its session_id — callers want the identity.
export async function fetchMe(): Promise<SessionUser> {
  const res = await fetch("/api/auth/me", { cache: "no-store" });
  return (await parseResponse(res, meResponseSchema)).user;
}
