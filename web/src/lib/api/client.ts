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
  apiErrorBodySchema,
  approvalDecisionSchema,
  runDetailSchema,
  runsPageSchema,
  strategiesPageSchema,
  strategySchema,
  type ApiErrorBody,
  type ApprovalDecision,
  type ApprovalRequest,
  type RunDetail,
  type RunsPage,
  type StrategiesPage,
  type Strategy,
} from "./schema";
import { DEFAULT_LIMIT } from "./pagination";

// Polling interval for list/detail revalidation (SSE/websocket is deferred).
export const POLL_INTERVAL_MS = 10_000;

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

// Records the L1 decision through the same-origin server proxy (which holds
// the OPERATOR_TOKEN; this client never sees it). A repeat decision
// (double-click, human-vs-timeout race) surfaces as ApiError status 409 with
// the recorded outcome in error.body.recorded; approved-but-blocked
// preflight results come back as a normal ApprovalDecision with
// outcome="approved_but_blocked"; a post-approval OMS submission failure
// comes back as outcome="approved" with submitted=false.
export async function postApproval(
  strategyId: string,
  payload: ApprovalRequest,
): Promise<ApprovalDecision> {
  const res = await fetch(`/api/strategies/${strategyId}/approvals`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(payload),
  });
  return parseResponse(res, approvalDecisionSchema);
}
