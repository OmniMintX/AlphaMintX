// Orderbook/trades view-model: cumulative ladder totals with a shared
// cross-side bar scale, spread readout, display formatters, and the
// fetchers' tuple mapping + newest-first ordering.

import { afterEach, describe, expect, it, vi } from "vitest";

import { fetchDepth, fetchRecentTrades, type DepthSnapshot } from "./binance";
import { buildLadder, fmtQty, fmtTradeTime } from "./depth";

const depth: DepthSnapshot = {
  bids: [
    { price: "100", qty: "1" },
    { price: "99", qty: "2" },
    { price: "98", qty: "3" },
  ],
  asks: [
    { price: "101", qty: "2" },
    { price: "102", qty: "2" },
  ],
};

describe("buildLadder", () => {
  it("cumulates each side best-first and scales pct to the max across BOTH sides", () => {
    const ladder = buildLadder(depth, 20);

    expect(ladder.bids.map((r) => r.total)).toEqual([1, 3, 6]);
    expect(ladder.asks.map((r) => r.total)).toEqual([2, 4]);
    // maxTotal = 6 (bid side) — ask bars are scaled against it too.
    expect(ladder.bids.map((r) => r.pct)).toEqual([(1 / 6) * 100, 50, 100]);
    expect(ladder.asks.map((r) => r.pct)).toEqual([(2 / 6) * 100, (4 / 6) * 100]);
    for (const r of [...ladder.asks, ...ladder.bids]) {
      expect(r.pct).toBeGreaterThanOrEqual(0);
      expect(r.pct).toBeLessThanOrEqual(100);
    }
  });

  it("slices each side to the requested rows before cumulating", () => {
    const ladder = buildLadder(depth, 2);
    expect(ladder.bids).toHaveLength(2);
    expect(ladder.bids.map((r) => r.total)).toEqual([1, 3]);
    // maxTotal is now 4 (ask side): best bid bar = 1/4.
    expect(ladder.bids[0]?.pct).toBe(25);
  });

  it("formats spread as bestAsk − bestBid plus a 3-decimal percentage", () => {
    const ladder = buildLadder(depth, 20);
    expect(ladder.spread).toBe("1.00"); // fmtPrice style
    expect(ladder.spreadPct).toBe(`${((1 / 101) * 100).toFixed(3)}%`); // "0.990%"
  });

  it("returns null spread when either side is empty", () => {
    const ladder = buildLadder({ bids: depth.bids, asks: [] }, 20);
    expect(ladder.spread).toBeNull();
    expect(ladder.spreadPct).toBeNull();
    // The populated side still cumulates and scales against itself.
    expect(ladder.bids.map((r) => r.pct)).toEqual([(1 / 6) * 100, 50, 100]);
  });
});

describe("fmtQty", () => {
  it("keeps 3 decimals at >= 1 (with grouping) and 5 below 1", () => {
    expect(fmtQty("12.3456")).toBe("12.346");
    expect(fmtQty("1234.5")).toBe("1,234.5");
    expect(fmtQty("0.1234567")).toBe("0.12346");
    expect(fmtQty("0")).toBe("0");
    expect(fmtQty(2)).toBe("2");
  });

  it("renders non-finite input as a dash", () => {
    expect(fmtQty("abc")).toBe("–");
    expect(fmtQty(Number.NaN)).toBe("–");
  });
});

describe("fmtTradeTime", () => {
  it("pads to HH:MM:SS in 24h local time", () => {
    // Local-time constructor, so the expectation holds in any TZ.
    expect(fmtTradeTime(new Date(2026, 6, 6, 9, 5, 7).getTime())).toBe("09:05:07");
    expect(fmtTradeTime(new Date(2026, 6, 6, 23, 59, 59).getTime())).toBe("23:59:59");
  });
});

// ---- Fetchers -----------------------------------------------------------------

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

function stubFetch(...responses: Response[]) {
  const mock = vi.fn<typeof fetch>();
  for (const res of responses) mock.mockResolvedValueOnce(res);
  vi.stubGlobal("fetch", mock);
  return mock;
}

describe("fetchDepth", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("maps spot tuples to {price, qty} preserving best-first API order", async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        bids: [
          ["100", "1"],
          ["99", "2"],
        ],
        asks: [
          ["101", "2"],
          ["102", "2"],
        ],
      }),
    );

    const snap = await fetchDepth("spot", "BTCUSDT");

    expect(mock.mock.calls[0]?.[0]).toBe(
      "https://data-api.binance.vision/api/v3/depth?symbol=BTCUSDT&limit=20",
    );
    expect(mock.mock.calls[0]?.[1]).toEqual({ cache: "no-store" });
    expect(snap.bids).toEqual([
      { price: "100", qty: "1" },
      { price: "99", qty: "2" },
    ]);
    expect(snap.asks).toEqual([
      { price: "101", qty: "2" },
      { price: "102", qty: "2" },
    ]);
  });

  it("throws the market/futures error text on non-ok responses", async () => {
    stubFetch(jsonResponse(500, {}), jsonResponse(451, {}));
    await expect(fetchDepth("spot", "BTCUSDT")).rejects.toThrow("market data 500");
    await expect(fetchDepth("futures", "BTCUSDT", 20)).rejects.toThrow("futures data 451");
  });
});

describe("fetchRecentTrades", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("returns newest first and drops extra raw fields", async () => {
    const mock = stubFetch(
      jsonResponse(200, [
        // API order: oldest → newest, with extra fields to ignore.
        { id: 1, price: "100", qty: "1", quoteQty: "100", time: 1000, isBuyerMaker: false, isBestMatch: true },
        { id: 2, price: "101", qty: "2", quoteQty: "202", time: 2000, isBuyerMaker: true, isBestMatch: true },
      ]),
    );

    const trades = await fetchRecentTrades("spot", "BTCUSDT", 30);

    expect(mock.mock.calls[0]?.[0]).toBe(
      "https://data-api.binance.vision/api/v3/trades?symbol=BTCUSDT&limit=30",
    );
    expect(trades).toEqual([
      { id: 2, price: "101", qty: "2", time: 2000, isBuyerMaker: true },
      { id: 1, price: "100", qty: "1", time: 1000, isBuyerMaker: false },
    ]);
  });

  it("throws the futures error text on non-ok responses", async () => {
    stubFetch(jsonResponse(451, {}));
    await expect(fetchRecentTrades("futures", "BTCUSDT")).rejects.toThrow("futures data 451");
  });
});
