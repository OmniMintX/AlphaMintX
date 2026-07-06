// Pure view-model for the orderbook ladder + recent trades panel — no
// React, fully unit-testable. Prices/qtys stay strings (ADR-0003);
// Number() below is display/geometry only (cumulative depth bars, spread
// readout), never accounting.

import { fmtPrice, type DepthLevel, type DepthSnapshot } from "./binance";

export interface LadderRow {
  price: string;
  qty: string;
  // Cumulative base-asset qty from the best price down to this row
  // (Number, display-only — drives the depth bar).
  total: number;
  // total / maxTotal * 100 where maxTotal is the max cumulative across
  // BOTH sides, so bid and ask bars share a comparable scale. 0..100.
  pct: number;
}

export interface Ladder {
  asks: LadderRow[];
  bids: LadderRow[];
  spread: string | null;
  spreadPct: string | null;
}

// Cumulate one side (already best-first); pct is filled in afterwards
// once the cross-side max is known.
function cumulate(levels: DepthLevel[]): LadderRow[] {
  let total = 0;
  return levels.map(({ price, qty }) => {
    total += Number(qty);
    return { price, qty, total, pct: 0 };
  });
}

export function buildLadder(depth: DepthSnapshot, rows: number): Ladder {
  const asks = cumulate(depth.asks.slice(0, rows));
  const bids = cumulate(depth.bids.slice(0, rows));

  const maxTotal = Math.max(
    asks[asks.length - 1]?.total ?? 0,
    bids[bids.length - 1]?.total ?? 0,
  );
  for (const row of [...asks, ...bids]) {
    row.pct = maxTotal > 0 ? Math.min(100, Math.max(0, (row.total / maxTotal) * 100)) : 0;
  }

  let spread: string | null = null;
  let spreadPct: string | null = null;
  const bestAsk = asks[0];
  const bestBid = bids[0];
  if (bestAsk && bestBid) {
    const diff = Number(bestAsk.price) - Number(bestBid.price);
    spread = fmtPrice(String(diff));
    spreadPct = `${((diff / Number(bestAsk.price)) * 100).toFixed(3)}%`;
  }

  return { asks, bids, spread, spreadPct };
}

// Display-only qty formatting: finer precision below 1 (0.00123 BTC),
// coarser with thousands grouping above (1,234.567).
export function fmtQty(qty: string | number): string {
  const n = Number(qty);
  if (!Number.isFinite(n)) return "–";
  return n.toLocaleString("en-US", { maximumFractionDigits: n >= 1 ? 3 : 5 });
}

// HH:MM:SS in local time, 24h — manual pad keeps it deterministic across
// locales (Binance shows trade times in local time too).
export function fmtTradeTime(ms: number): string {
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
