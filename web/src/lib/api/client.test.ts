// Client-layer pure pieces: URL/query construction, bearer headers, the
// approval POST payload, and ApiError body parsing (409 recorded outcome).

import { describe, expect, it } from "vitest";

import { ApiError, authHeaders, buildApprovalPayload, buildUrl } from "./client";
import { approvalRequestSchema } from "./schema";

const VERDICT_ID = "b8c9d0e1-f2a3-4b4c-8d5e-7f8a9b0c1d2e";

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
