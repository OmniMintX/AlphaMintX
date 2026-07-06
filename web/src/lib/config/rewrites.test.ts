// The build-time /api/v1 rewrite is retired (session edition): a cookieless
// passthrough would bypass the /api/cp session proxy, so the superseded
// helper must yield no rules no matter what origin is configured.

import { describe, expect, it } from "vitest";

import { controlPlaneRewrites } from "./rewrites";

describe("controlPlaneRewrites (superseded by the /api/cp session proxy)", () => {
  it("returns no rules when the base URL is undefined", () => {
    expect(controlPlaneRewrites(undefined)).toEqual([]);
  });

  it("returns no rules even for a configured origin", () => {
    expect(controlPlaneRewrites("http://127.0.0.1:8080")).toEqual([]);
    expect(controlPlaneRewrites("http://cp:8080///")).toEqual([]);
  });
});
