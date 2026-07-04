// Rewrite-rule construction for the same-origin deployment: an unset/blank
// CONTROLPLANE_API_BASE_URL yields no rewrites (zero-behavior-change default);
// a configured origin yields exactly one /api/v1/* proxy rule.

import { describe, expect, it } from "vitest";

import { controlPlaneRewrites } from "./rewrites";

describe("controlPlaneRewrites", () => {
  it("returns no rules when the base URL is undefined", () => {
    expect(controlPlaneRewrites(undefined)).toEqual([]);
  });

  it("returns no rules for an empty string", () => {
    expect(controlPlaneRewrites("")).toEqual([]);
  });

  it("returns no rules for whitespace-only input", () => {
    expect(controlPlaneRewrites("   ")).toEqual([]);
  });

  it("builds exactly one /api/v1 rule for a configured origin", () => {
    expect(controlPlaneRewrites("http://127.0.0.1:8080")).toEqual([
      {
        source: "/api/v1/:path*",
        destination: "http://127.0.0.1:8080/api/v1/:path*",
      },
    ]);
  });

  it("strips trailing slashes from the origin", () => {
    expect(controlPlaneRewrites("http://cp:8080///")).toEqual([
      {
        source: "/api/v1/:path*",
        destination: "http://cp:8080/api/v1/:path*",
      },
    ]);
  });
});
