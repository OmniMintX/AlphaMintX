// Read-only public market data for the dashboard, fetched from the
// browser against Binance's data-only mirror (no keys, no trading
// surface — ARCHITECTURE.md plane rules allow read-only market feeds).
// Prices stay strings end-to-end (ADR-0003); Number() is used only for
// display formatting and sparkline geometry, never for accounting.

const DATA_BASE = "https://data-api.binance.vision/api/v3";

export const DESK_SYMBOLS = ["BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "XRPUSDT"] as const;

export interface Ticker24h {
  symbol: string;
  lastPrice: string;
  priceChangePercent: string;
  highPrice: string;
  lowPrice: string;
  quoteVolume: string;
}

export interface MarketSnapshot {
  tickers: Ticker24h[];
  // symbol -> hourly closes (oldest first) for the sparkline.
  closes: Record<string, number[]>;
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${DATA_BASE}${path}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`market data ${res.status}`);
  return (await res.json()) as T;
}

export async function fetchTickers(symbols: readonly string[]): Promise<Ticker24h[]> {
  const list = encodeURIComponent(JSON.stringify(symbols));
  const raw = await getJSON<Ticker24h[]>(`/ticker/24hr?symbols=${list}`);
  const bySymbol = new Map(raw.map((t) => [t.symbol, t]));
  return symbols.flatMap((s) => {
    const t = bySymbol.get(s);
    return t ? [t] : [];
  });
}

// Hourly closes for the last `limit` hours (kline close = index 4).
export async function fetchCloses(symbol: string, limit = 48): Promise<number[]> {
  const raw = await getJSON<(string | number)[][]>(
    `/klines?symbol=${symbol}&interval=1h&limit=${limit}`,
  );
  return raw.map((k) => Number(k[4]));
}

export async function fetchMarketSnapshot(
  symbols: readonly string[] = DESK_SYMBOLS,
): Promise<MarketSnapshot> {
  const [tickers, closesList] = await Promise.all([
    fetchTickers(symbols),
    Promise.all(symbols.map((s) => fetchCloses(s))),
  ]);
  const closes: Record<string, number[]> = {};
  symbols.forEach((s, i) => {
    closes[s] = closesList[i] ?? [];
  });
  return { tickers, closes };
}

// "BTCUSDT" -> "BTC/USDT" for display.
export function displayPair(symbol: string): string {
  return symbol.endsWith("USDT") ? `${symbol.slice(0, -4)}/USDT` : symbol;
}

// Format an exchange price string for display: group thousands, keep
// sensible precision per magnitude (108342.10 -> "108,342.10").
export function fmtPrice(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw;
  const digits = n >= 1000 ? 2 : n >= 1 ? 2 : 4;
  return n.toLocaleString("en-US", {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

// "+2.41%" (always signed) from Binance's "2.410".
export function fmtPct(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw;
  return `${n >= 0 ? "+" : ""}${n.toFixed(2)}%`;
}

// Compact quote volume: 1.9B / 421.3M / 87.2K.
export function fmtVolume(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return raw;
  if (n >= 1e9) return `${(n / 1e9).toFixed(1)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return n.toFixed(0);
}

// USD-M futures public REST. Unlike spot there is no data-only mirror, so
// browsers in some regions get HTTP 451 (geo-block) — callers must treat the
// futures feed as independently fallible (the spot desk keeps working).
const FAPI_BASE = "https://fapi.binance.com/fapi/v1";

export interface FuturesTicker24h {
  symbol: string;
  lastPrice: string;
  priceChangePercent: string;
  quoteVolume: string;
}

export interface PremiumIndex {
  symbol: string;
  markPrice: string;
  indexPrice: string;
  lastFundingRate: string; // decimal string, e.g. "0.00010000"
  nextFundingTime: number; // ms epoch
}

export interface OpenInterestInfo {
  symbol: string;
  openInterest: string; // base-asset amount
}

// One desk row per symbol; numeric values stay strings (ADR-0003).
export interface FuturesRow {
  symbol: string;
  markPrice: string;
  indexPrice: string;
  priceChangePercent: string;
  quoteVolume: string;
  lastFundingRate: string;
  nextFundingTime: number;
  openInterest: string;
}

async function getFapiJSON<T>(path: string): Promise<T> {
  const res = await fetch(`${FAPI_BASE}${path}`, { cache: "no-store" });
  if (!res.ok) throw new Error(`futures data ${res.status}`);
  return (await res.json()) as T;
}

// Per-symbol parallel fetches (the unfiltered /ticker/24hr returns every
// listed contract — far too big for five desk symbols).
export async function fetchFuturesSnapshot(
  symbols: readonly string[] = DESK_SYMBOLS,
): Promise<FuturesRow[]> {
  return Promise.all(
    symbols.map(async (symbol) => {
      const [ticker, premium, oi] = await Promise.all([
        getFapiJSON<FuturesTicker24h>(`/ticker/24hr?symbol=${symbol}`),
        getFapiJSON<PremiumIndex>(`/premiumIndex?symbol=${symbol}`),
        getFapiJSON<OpenInterestInfo>(`/openInterest?symbol=${symbol}`),
      ]);
      return {
        symbol,
        markPrice: premium.markPrice,
        indexPrice: premium.indexPrice,
        priceChangePercent: ticker.priceChangePercent,
        quoteVolume: ticker.quoteVolume,
        lastFundingRate: premium.lastFundingRate,
        nextFundingTime: premium.nextFundingTime,
        openInterest: oi.openInterest,
      };
    }),
  );
}

// "+0.0100%" (always signed, 4 decimals) from Binance's "0.00010000".
export function fmtFundingRate(raw: string): string {
  const n = Number(raw) * 100;
  if (!Number.isFinite(n)) return raw;
  return `${n >= 0 ? "+" : ""}${n.toFixed(4)}%`;
}

// "hh:mm" countdown to the next funding timestamp, clamped at 00:00.
export function fmtFundingCountdown(nextFundingTime: number, now = Date.now()): string {
  const left = Math.max(0, nextFundingTime - now);
  const h = Math.floor(left / 3_600_000);
  const m = Math.floor((left % 3_600_000) / 60_000);
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}`;
}

// One candle for the desk chart; time is openTime in seconds (the unit
// lightweight-charts expects). Number() here is chart geometry (ADR-0003),
// never accounting.
export interface Candle {
  time: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
}

// Klines for either market: a kline row is [openTime(ms), open, high, low,
// close, volume, ...] with the numeric fields as strings.
export async function fetchCandles(
  market: "spot" | "futures",
  symbol: string,
  interval: string,
  limit = 300,
): Promise<Candle[]> {
  const path = `/klines?symbol=${symbol}&interval=${interval}&limit=${limit}`;
  const raw =
    market === "spot"
      ? await getJSON<(string | number)[][]>(path)
      : await getFapiJSON<(string | number)[][]>(path);
  return raw.map((k) => ({
    time: Number(k[0]) / 1000,
    open: Number(k[1]),
    high: Number(k[2]),
    low: Number(k[3]),
    close: Number(k[4]),
    volume: Number(k[5]),
  }));
}

// One order book level; price/qty stay strings (ADR-0003).
export interface DepthLevel {
  price: string;
  qty: string;
}

// Both sides best-first: bids descending by price, asks ascending —
// exactly the order the API returns them in.
export interface DepthSnapshot {
  bids: DepthLevel[];
  asks: DepthLevel[];
}

// Order book snapshot for either market. Raw shape on both endpoints is
// { bids: [price, qty][], asks: [price, qty][] }. Note: futures accepts
// only limits 5/10/20/50/100/500/1000, and 20 costs request weight 2.
export async function fetchDepth(
  market: "spot" | "futures",
  symbol: string,
  limit = 20,
): Promise<DepthSnapshot> {
  const path = `/depth?symbol=${symbol}&limit=${limit}`;
  const raw =
    market === "spot"
      ? await getJSON<{ bids: [string, string][]; asks: [string, string][] }>(path)
      : await getFapiJSON<{ bids: [string, string][]; asks: [string, string][] }>(path);
  const toLevel = ([price, qty]: [string, string]): DepthLevel => ({ price, qty });
  return { bids: raw.bids.map(toLevel), asks: raw.asks.map(toLevel) };
}

// One recent public trade; isBuyerMaker true means the taker sold.
export interface RecentTrade {
  id: number;
  price: string;
  qty: string;
  time: number; // ms epoch
  isBuyerMaker: boolean;
}

// Recent trades for either market, NEWEST FIRST (the API returns
// oldest→newest, so we reverse a mapped copy). Extra raw fields
// (quoteQty, isBestMatch, ...) are dropped.
export async function fetchRecentTrades(
  market: "spot" | "futures",
  symbol: string,
  limit = 30,
): Promise<RecentTrade[]> {
  const path = `/trades?symbol=${symbol}&limit=${limit}`;
  const raw =
    market === "spot"
      ? await getJSON<RecentTrade[]>(path)
      : await getFapiJSON<RecentTrade[]>(path);
  return raw
    .map(({ id, price, qty, time, isBuyerMaker }) => ({ id, price, qty, time, isBuyerMaker }))
    .reverse();
}
