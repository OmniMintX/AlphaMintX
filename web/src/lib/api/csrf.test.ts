// Cross-site rejection model: safe methods pass, Origin (then Referer) must
// match EITHER x-forwarded-host (first value) or Host — proxies differ on
// which carries the public name — and a request with neither header is
// allowed (non-browser clients).

import { describe, expect, it } from "vitest";

import { type CrossSiteHeaders, crossSiteHeadersFrom, crossSiteRejection } from "./csrf";

function headers(overrides: Partial<CrossSiteHeaders> = {}): CrossSiteHeaders {
  return { origin: null, referer: null, host: "app.example.com", forwardedHost: null, ...overrides };
}

describe("crossSiteRejection: safe methods", () => {
  it("always allows GET/HEAD/OPTIONS, even with a foreign origin", () => {
    const foreign = headers({ origin: "https://evil.example" });
    expect(crossSiteRejection("GET", foreign)).toBeNull();
    expect(crossSiteRejection("HEAD", foreign)).toBeNull();
    expect(crossSiteRejection("OPTIONS", foreign)).toBeNull();
    expect(crossSiteRejection("get", foreign)).toBeNull();
  });
});

describe("crossSiteRejection: Origin checks on POST", () => {
  it("allows a same-origin request (scheme ignored)", () => {
    expect(crossSiteRejection("POST", headers({ origin: "https://app.example.com" }))).toBeNull();
    expect(crossSiteRejection("POST", headers({ origin: "http://app.example.com" }))).toBeNull();
  });

  it("allows a same-origin request with an explicit port on both sides", () => {
    const h = headers({ origin: "http://localhost:3000", host: "localhost:3000" });
    expect(crossSiteRejection("POST", h)).toBeNull();
  });

  it("allows an origin matching x-forwarded-host when Host is internal (proxy rewrites Host)", () => {
    const h = headers({
      origin: "https://app.example.com",
      host: "127.0.0.1:3000",
      forwardedHost: "app.example.com",
    });
    expect(crossSiteRejection("POST", h)).toBeNull();
  });

  it("allows an origin matching Host when x-forwarded-host is internal (proxy preserves Host)", () => {
    const h = headers({
      origin: "https://app.example.com",
      host: "app.example.com",
      forwardedHost: "127.0.0.1:3000",
    });
    expect(crossSiteRejection("POST", h)).toBeNull();
  });

  it("rejects an origin matching neither x-forwarded-host nor Host", () => {
    const h = headers({
      origin: "https://evil.example",
      host: "127.0.0.1:3000",
      forwardedHost: "app.example.com",
    });
    expect(crossSiteRejection("POST", h)).toMatch(/does not match/);
  });

  it("takes the first value of a comma-separated x-forwarded-host", () => {
    const h = headers({
      origin: "https://app.example.com",
      host: "127.0.0.1:3000",
      forwardedHost: "app.example.com, cdn.internal",
    });
    expect(crossSiteRejection("POST", h)).toBeNull();
  });

  it("rejects a cross-origin request", () => {
    expect(crossSiteRejection("POST", headers({ origin: "https://evil.example" }))).toMatch(
      /does not match/,
    );
  });

  it("rejects a port mismatch (host:port compared, not just hostname)", () => {
    const h = headers({ origin: "http://app.example.com:8443" });
    expect(crossSiteRejection("POST", h)).toMatch(/does not match/);
  });

  it('rejects the literal Origin "null" and other unparseable origins', () => {
    expect(crossSiteRejection("POST", headers({ origin: "null" }))).toMatch(/unparseable/);
    expect(crossSiteRejection("POST", headers({ origin: "not a url" }))).toMatch(/unparseable/);
  });
});

describe("crossSiteRejection: Referer fallback and absence", () => {
  it("falls back to Referer when Origin is absent: match allows, mismatch rejects", () => {
    expect(
      crossSiteRejection("POST", headers({ referer: "https://app.example.com/login" })),
    ).toBeNull();
    expect(crossSiteRejection("POST", headers({ referer: "https://evil.example/" }))).toMatch(
      /does not match/,
    );
  });

  it("checks Origin, not Referer, when both are present", () => {
    const h = headers({ origin: "https://evil.example", referer: "https://app.example.com/" });
    expect(crossSiteRejection("POST", h)).toMatch(/origin host/);
  });

  it("allows when both Origin and Referer are absent (curl, smoke tests)", () => {
    expect(crossSiteRejection("POST", headers())).toBeNull();
  });
});

describe("crossSiteHeadersFrom", () => {
  it("extracts the four relevant headers from a Headers object", () => {
    const h = crossSiteHeadersFrom(
      new Headers({
        origin: "https://app.example.com",
        referer: "https://app.example.com/settings",
        host: "127.0.0.1:3000",
        "x-forwarded-host": "app.example.com",
      }),
    );
    expect(h).toEqual({
      origin: "https://app.example.com",
      referer: "https://app.example.com/settings",
      host: "127.0.0.1:3000",
      forwardedHost: "app.example.com",
    });
  });

  it("yields nulls for missing headers", () => {
    expect(crossSiteHeadersFrom(new Headers())).toEqual({
      origin: null,
      referer: null,
      host: null,
      forwardedHost: null,
    });
  });
});
