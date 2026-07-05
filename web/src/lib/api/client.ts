// Typed fetch wrapper for the Phase 1 control-plane HTTP API
// (docs/specs/persistence-and-api.md §HTTP API). Base URL and the read token
// come from the environment — never hardcoded:
//   NEXT_PUBLIC_API_BASE_URL   control-plane origin ("" = same origin)
//   NEXT_PUBLIC_READ_TOKEN     READ_TOKEN, GETs only (never authorizes POSTs)
// In the same-origin deployment the empty base URL resolves against the Next
// server, which proxies /api/v1/* to the control-plane via build-time
// rewrites (CONTROLPLANE_API_BASE_URL, src/lib/config/rewrites.ts).
// NEXT_PUBLIC_API_BASE_URL remains the explicit cross-origin escape hatch —
// it requires a fronting proxy, since the control-plane serves no CORS
// headers.
// The OPERATOR_TOKEN is server-only: approvals POST to the same-origin route
// handler (app/api/strategies/[id]/approvals/route.ts), which attaches it —
// the approve credential is never inlined into a client bundle.

import type { z } from "zod";

import {
  alertsPageSchema,
  apiErrorBodySchema,
  approvalDecisionSchema,
  killClearResponseSchema,
  killResponseSchema,
  lifecycleResponseSchema,
  paperGateReportSchema,
  runDetailSchema,
  runsPageSchema,
  safetyStatusSchema,
  strategiesPageSchema,
  strategySchema,
  type AlertsPage,
  type ApiErrorBody,
  type ApprovalDecision,
  type ApprovalRequest,
  type KillClearRequest,
  type KillClearResponse,
  type KillRequest,
  type KillResponse,
  type LifecycleRequest,
  type LifecycleResponse,
  type LifecycleState,
  type PaperGateReport,
  type RunDetail,
  type RunsPage,
  type SafetyStatus,
  type StrategiesPage,
  type Strategy,
} from "./schema";
import { DEFAULT_LIMIT } from "./pagination";

// Polling interval for list/detail revalidation (SSE/websocket is deferred).
export const POLL_INTERVAL_MS = 10_000;

// Paper-gate poll interval (operator-surface.md OS-25): that GET self-charges
// the shared READ token's 60/min bucket (LC-24), so it polls at
// 6 x POLL_INTERVAL_MS (60 s) and never tighter — including on a 429.
export const PAPER_GATE_POLL_INTERVAL_MS = 6 * POLL_INTERVAL_MS;

export function apiBaseUrl(): string {
  return process.env.NEXT_PUBLIC_API_BASE_URL ?? "";
}

function readToken(): string | undefined {
  return process.env.NEXT_PUBLIC_READ_TOKEN || undefined;
}

export type QueryParams = Record<string, string | number | undefined>;

export function buildUrl(base: string, path: string, query?: QueryParams): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value !== undefined) params.set(key, String(value));
  }
  const qs = params.toString();
  return `${base}${path}${qs ? `?${qs}` : ""}`;
}

export function authHeaders(token: string | undefined): Record<string, string> {
  return token ? { authorization: `Bearer ${token}` } : {};
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

async function apiGet<T>(path: string, schema: z.ZodType<T>, query?: QueryParams): Promise<T> {
  const res = await fetch(buildUrl(apiBaseUrl(), path, query), {
    headers: authHeaders(readToken()),
    cache: "no-store",
  });
  return parseResponse(res, schema);
}

export function fetchStrategies(page = 1, limit = DEFAULT_LIMIT): Promise<StrategiesPage> {
  return apiGet("/api/v1/strategies", strategiesPageSchema, { page, limit });
}

export function fetchStrategy(strategyId: string): Promise<Strategy> {
  return apiGet(`/api/v1/strategies/${strategyId}`, strategySchema);
}

export function fetchRuns(strategyId: string, page = 1, limit = DEFAULT_LIMIT): Promise<RunsPage> {
  return apiGet(`/api/v1/strategies/${strategyId}/runs`, runsPageSchema, { page, limit });
}

export function fetchRunDetail(strategyId: string, runId: string): Promise<RunDetail> {
  return apiGet(`/api/v1/strategies/${strategyId}/runs/${runId}`, runDetailSchema);
}

// Composite safety status (operator-surface.md OS-7): lifecycle state,
// binding kills with their clears, breaker-today, watchdog liveness.
export function fetchSafety(strategyId: string): Promise<SafetyStatus> {
  return apiGet(`/api/v1/strategies/${strategyId}/safety`, safetyStatusSchema);
}

// Per-strategy safety_alerts feed (OS-15/OS-16), newest first; pages are
// per-poll snapshots under LIMIT/OFFSET.
export function fetchAlerts(strategyId: string, page = 1, limit = DEFAULT_LIMIT): Promise<AlertsPage> {
  return apiGet(`/api/v1/strategies/${strategyId}/alerts`, alertsPageSchema, { page, limit });
}

// The LC-23 paper-gate report (LC-24 read). Poll at
// PAPER_GATE_POLL_INTERVAL_MS only — this GET self-charges the rate bucket.
export function fetchPaperGate(strategyId: string): Promise<PaperGateReport> {
  return apiGet(`/api/v1/strategies/${strategyId}/paper-gate`, paperGateReportSchema);
}

// POSTs to a same-origin Next proxy route (which holds the OPERATOR_TOKEN;
// this client never sees it and attaches no auth header). Upstream errors
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
  return proxyPost(`/api/strategies/${strategyId}/approvals`, payload, approvalDecisionSchema);
}

// One lifecycle transition (OS-27). Any 422 (ILLEGAL_TRANSITION,
// PAPER_GATE_FAILED, "kill tier active") surfaces verbatim, never
// pre-suppressed — the server is the sole transition authority.
export function postLifecycle(
  strategyId: string,
  payload: LifecycleRequest,
): Promise<LifecycleResponse> {
  return proxyPost(`/api/strategies/${strategyId}/lifecycle`, payload, lifecycleResponseSchema);
}

// Strategy-tier kill (OS-28); the response acknowledges persistence only.
export function postKill(strategyId: string, payload: KillRequest): Promise<KillResponse> {
  return proxyPost(`/api/strategies/${strategyId}/kill`, payload, killResponseSchema);
}

// Strategy-tier clear (OS-29).
export function postKillClear(
  strategyId: string,
  payload: KillClearRequest,
): Promise<KillClearResponse> {
  return proxyPost(`/api/strategies/${strategyId}/kill/clear`, payload, killClearResponseSchema);
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
