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
