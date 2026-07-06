"use client";

// Market desk widgets: the ticker tape and the Binance-style tabbed desk
// (Spot / Futures card grids with a per-pair candlestick panel). Public
// read-only data polled from the browser (src/lib/market/binance.ts); a
// fetch failure renders a quiet placeholder — the desk never blocks the
// control-plane view.

import { useCallback, useMemo, useState } from "react";

import {
  DESK_SYMBOLS,
  displayPair,
  fetchCandles,
  fetchFuturesSnapshot,
  fetchMarketSnapshot,
  fmtFundingCountdown,
  fmtFundingRate,
  fmtPct,
  fmtPrice,
  fmtVolume,
  type Candle,
  type FuturesRow,
  type MarketSnapshot,
} from "../../src/lib/market/binance";
import { bollinger, ema, macd, rsi, sma } from "../../src/lib/market/indicators";
import { ApiError, requestMarketAnalysis } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n, type MessageKey } from "../../src/lib/i18n";
import { CandleChart, type IndicatorFlags } from "./candle-chart";

const MARKET_POLL_MS = 15_000;

function dir(pct: string): "up" | "down" {
  return Number(pct) >= 0 ? "up" : "down";
}

// Inline SVG sparkline over hourly closes; stroke follows 24h direction.
function Sparkline({ closes, tone }: { closes: number[]; tone: "up" | "down" }) {
  const w = 120;
  const h = 36;
  if (closes.length < 2) return <svg className="spark" width={w} height={h} />;
  const min = Math.min(...closes);
  const max = Math.max(...closes);
  const span = max - min || 1;
  const pts = closes
    .map((c, i) => {
      const x = (i / (closes.length - 1)) * w;
      const y = h - 3 - ((c - min) / span) * (h - 6);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
  return (
    <svg className={`spark spark-${tone}`} width={w} height={h} viewBox={`0 0 ${w} ${h}`}>
      <polyline points={pts} fill="none" strokeWidth="1.5" strokeLinejoin="round" />
    </svg>
  );
}

export function TickerTape() {
  const load = useCallback(() => fetchMarketSnapshot(DESK_SYMBOLS), []);
  const { data } = usePoll<MarketSnapshot>(load, MARKET_POLL_MS);
  if (!data) return <div className="ticker-tape ticker-empty" aria-hidden />;
  return (
    <div className="ticker-tape">
      {data.tickers.map((tk) => {
        const tone = dir(tk.priceChangePercent);
        return (
          <span className="ticker-item" key={tk.symbol}>
            <span className="ticker-sym">{displayPair(tk.symbol)}</span>
            <span className="ticker-px">{fmtPrice(tk.lastPrice)}</span>
            <span className={`ticker-chg ${tone}`}>{fmtPct(tk.priceChangePercent)}</span>
          </span>
        );
      })}
      <span className="ticker-src">Binance · spot · 24h</span>
    </div>
  );
}

type DeskTab = "spot" | "futures";

const INTERVALS = ["15m", "1h", "4h", "1d"] as const;
type Interval = (typeof INTERVALS)[number];

function MarketGrid({
  data,
  error,
  selected,
  onSelect,
}: {
  data: MarketSnapshot | null;
  error: string | null;
  selected: string | null;
  onSelect: (symbol: string) => void;
}) {
  const { t } = useI18n();

  if (error && !data) {
    return <div className="empty">{t("market.unavailable")}</div>;
  }
  if (!data) {
    return (
      <div className="grid grid-4">
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
      </div>
    );
  }
  return (
    <div className="grid grid-4 market-grid">
      {data.tickers.map((tk) => {
        const tone = dir(tk.priceChangePercent);
        return (
          <div
            className={`market-card market-click${selected === tk.symbol ? " selected" : ""}`}
            key={tk.symbol}
            onClick={() => onSelect(tk.symbol)}
          >
            <div className="spread">
              <span className="market-sym">{displayPair(tk.symbol)}</span>
              <span className={`market-chg ${tone}`}>{fmtPct(tk.priceChangePercent)}</span>
            </div>
            <div className="spread market-mid">
              <span className={`market-px ${tone}`}>{fmtPrice(tk.lastPrice)}</span>
              <Sparkline closes={data.closes[tk.symbol] ?? []} tone={tone} />
            </div>
            <div className="market-foot">
              <span>
                {t("market.high")} <b>{fmtPrice(tk.highPrice)}</b>
              </span>
              <span>
                {t("market.low")} <b>{fmtPrice(tk.lowPrice)}</b>
              </span>
              <span>
                {t("market.vol")} <b>{fmtVolume(tk.quoteVolume)}</b>
              </span>
            </div>
          </div>
        );
      })}
    </div>
  );
}

// USD-M futures cards. The fapi endpoint has no public mirror and can be
// geo-blocked (HTTP 451) while spot keeps working, so its feed polls and
// fails independently: on error it renders a one-line notice, never breaking
// the spot tab; if the feed recovers on a later poll the cards reappear.
function FuturesGrid({
  data,
  error,
  selected,
  onSelect,
}: {
  data: FuturesRow[] | null;
  error: string | null;
  selected: string | null;
  onSelect: (symbol: string) => void;
}) {
  const { t } = useI18n();

  if (error && !data) {
    return <div className="empty">{t("market.futures.unavailable")}</div>;
  }
  if (!data) {
    return (
      <div className="grid grid-4">
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
        <div className="skeleton" style={{ height: 96 }} />
      </div>
    );
  }
  return (
    <div className="grid grid-4 market-grid">
      {data.map((row) => {
        const tone = dir(row.priceChangePercent);
        // Positive funding = longs pay shorts (red); negative = green.
        const fundingTone = Number(row.lastFundingRate) > 0 ? "down" : "up";
        return (
          <div
            className={`market-card market-click${selected === row.symbol ? " selected" : ""}`}
            key={row.symbol}
            onClick={() => onSelect(row.symbol)}
          >
            <div className="spread">
              <span className="market-sym">{displayPair(row.symbol)}</span>
              <span className={`market-chg ${tone}`}>{fmtPct(row.priceChangePercent)}</span>
            </div>
            <div className="spread market-mid">
              <span className={`market-px ${tone}`}>{fmtPrice(row.markPrice)}</span>
              <span className={`market-chg ${fundingTone}`}>
                {t("market.futures.funding")} {fmtFundingRate(row.lastFundingRate)}
              </span>
            </div>
            <div className="market-foot">
              <span>
                {t("market.futures.next")} <b>{fmtFundingCountdown(row.nextFundingTime)}</b>
              </span>
              <span>
                {t("market.futures.oi")} <b>{fmtVolume(row.openInterest)}</b>
              </span>
              <span>
                {t("market.vol")} <b>{fmtVolume(row.quoteVolume)}</b>
              </span>
            </div>
          </div>
        );
      })}
    </div>
  );
}

// ---- Deterministic technical readout (market.ta.*) -------------------------------
// Standard TA rules over the latest candle values; recomputed on every poll
// refresh. Display analytics only (ADR-0003), never accounting.

type TaTone = "up" | "down" | "";
type Verdict = "strongbuy" | "buy" | "neutral" | "sell" | "strongsell";

// English verdict labels for the plain-text agent summary (wire text stays
// locale-independent; the UI renders the i18n key instead).
const VERDICT_EN: Record<Verdict, string> = {
  strongbuy: "Strong Buy",
  buy: "Buy",
  neutral: "Neutral",
  sell: "Sell",
  strongsell: "Strong Sell",
};

interface TaSnapshot {
  close: number;
  rsi14: number;
  macdLine: number;
  macdSignal: number;
  macdHist: number;
  macdHistPrev: number;
  sma7: number;
  sma25: number;
  sma99: number;
  ema12: number;
  ema26: number;
  bollUpper: number;
  bollMiddle: number;
  bollLower: number;
  cross: "golden" | "death" | null;
}

function computeTa(candles: Candle[]): TaSnapshot | null {
  if (candles.length < 2) return null;
  const closes = candles.map((c) => c.close);
  const i = closes.length - 1;
  const r = rsi(closes, 14);
  const m = macd(closes, 12, 26, 9);
  const s7 = sma(closes, 7);
  const s25 = sma(closes, 25);
  const s99 = sma(closes, 99);
  const e12 = ema(closes, 12);
  const e26 = ema(closes, 26);
  const bands = bollinger(closes, 20, 2);
  // SMA7/SMA25 cross within the last 3 candles (golden/death). In-bounds
  // reads still type as `number | undefined` under noUncheckedIndexedAccess,
  // hence the ?? NaN fallbacks (NaN slots fail the finite checks anyway).
  let cross: "golden" | "death" | null = null;
  for (let j = i; j > Math.max(i - 3, 0); j--) {
    const prev = (s7[j - 1] ?? NaN) - (s25[j - 1] ?? NaN);
    const cur = (s7[j] ?? NaN) - (s25[j] ?? NaN);
    if (!Number.isFinite(prev) || !Number.isFinite(cur)) continue;
    if (prev <= 0 && cur > 0) {
      cross = "golden";
      break;
    }
    if (prev >= 0 && cur < 0) {
      cross = "death";
      break;
    }
  }
  return {
    close: closes[i] ?? NaN,
    rsi14: r[i] ?? NaN,
    macdLine: m.macd[i] ?? NaN,
    macdSignal: m.signal[i] ?? NaN,
    macdHist: m.hist[i] ?? NaN,
    macdHistPrev: m.hist[i - 1] ?? NaN,
    sma7: s7[i] ?? NaN,
    sma25: s25[i] ?? NaN,
    sma99: s99[i] ?? NaN,
    ema12: e12[i] ?? NaN,
    ema26: e26[i] ?? NaN,
    bollUpper: bands.upper[i] ?? NaN,
    bollMiddle: bands.middle[i] ?? NaN,
    bollLower: bands.lower[i] ?? NaN,
    cross,
  };
}

// Indicator value formatting for the readout and the agent summary.
function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return "n/a";
  return v.toFixed(Math.abs(v) >= 1 ? 2 : 4);
}

interface TaRow {
  label: string;
  value: string;
  key: MessageKey;
  extraKey?: MessageKey;
  tone: TaTone;
}

// One row per signal plus a bull-minus-bear score mapped to the verdict.
function taSignals(ta: TaSnapshot): { rows: TaRow[]; verdict: Verdict } {
  const rows: TaRow[] = [];
  let score = 0;
  if (Number.isFinite(ta.rsi14)) {
    let key: MessageKey;
    let tone: TaTone;
    if (ta.rsi14 > 70) {
      key = "market.ta.rsi.overbought";
      tone = "down";
      score -= 1;
    } else if (ta.rsi14 >= 50) {
      key = "market.ta.rsi.bullish";
      tone = "up";
      score += 1;
    } else if (ta.rsi14 >= 30) {
      key = "market.ta.rsi.bearish";
      tone = "down";
      score -= 1;
    } else {
      key = "market.ta.rsi.oversold";
      tone = "up";
      score += 1;
    }
    rows.push({ label: "RSI(14)", value: ta.rsi14.toFixed(1), key, tone });
  }
  if (Number.isFinite(ta.macdLine) && Number.isFinite(ta.macdSignal)) {
    const bull = ta.macdLine >= ta.macdSignal;
    score += bull ? 1 : -1;
    const rising = Number.isFinite(ta.macdHistPrev) && ta.macdHist >= ta.macdHistPrev;
    rows.push({
      label: "MACD(12,26,9)",
      value: fmtNum(ta.macdHist),
      key: bull ? "market.ta.macd.above" : "market.ta.macd.below",
      extraKey: rising ? "market.ta.macd.rising" : "market.ta.macd.falling",
      tone: bull ? "up" : "down",
    });
  }
  if (Number.isFinite(ta.sma25) && Number.isFinite(ta.sma99)) {
    let key: MessageKey = "market.ta.ma.side";
    let tone: TaTone = "";
    if (ta.close > ta.sma25 && ta.close > ta.sma99) {
      key = "market.ta.ma.up";
      tone = "up";
      score += 1;
    } else if (ta.close < ta.sma25 && ta.close < ta.sma99) {
      key = "market.ta.ma.down";
      tone = "down";
      score -= 1;
    }
    let extraKey: MessageKey | undefined;
    if (ta.cross === "golden") {
      extraKey = "market.ta.ma.golden";
      score += 1;
    } else if (ta.cross === "death") {
      extraKey = "market.ta.ma.death";
      score -= 1;
    }
    rows.push({ label: "MA(7/25/99)", value: fmtNum(ta.sma25), key, extraKey, tone });
  }
  if (Number.isFinite(ta.bollUpper) && Number.isFinite(ta.bollLower)) {
    const width = ta.bollUpper - ta.bollLower;
    const pb = width > 0 ? (ta.close - ta.bollLower) / width : 0.5;
    let key: MessageKey = "market.ta.boll.mid";
    let tone: TaTone = "";
    if (pb >= 0.95) {
      key = "market.ta.boll.upper";
      tone = "down";
      score -= 1;
    } else if (pb <= 0.05) {
      key = "market.ta.boll.lower";
      tone = "up";
      score += 1;
    }
    rows.push({ label: "BOLL(20,2)", value: fmtNum(ta.bollMiddle), key, tone });
  }
  const verdict: Verdict =
    score >= 3 ? "strongbuy" : score >= 1 ? "buy" : score <= -3 ? "strongsell" : score <= -1 ? "sell" : "neutral";
  return { rows, verdict };
}

// Highest high / lowest low over the last n candles — the swing anchors the
// agent may cite as support/resistance.
function swingRange(candles: Candle[], n: number): { high: number; low: number } {
  let high = NaN;
  let low = NaN;
  for (let j = Math.max(candles.length - n, 0); j < candles.length; j++) {
    const c = candles[j];
    if (!c) continue;
    if (!Number.isFinite(high) || c.high > high) high = c.high;
    if (!Number.isFinite(low) || c.low < low) low = c.low;
  }
  return { high, low };
}

// Plain-text snapshot for the agent (≤4000 chars; wire text stays English).
function buildTaSummary(
  symbol: string,
  market: DeskTab,
  interval: Interval,
  changePct: string | undefined,
  candles: Candle[],
  ta: TaSnapshot,
  verdict: Verdict,
): string {
  const histDir = !Number.isFinite(ta.macdHist) || !Number.isFinite(ta.macdHistPrev)
    ? "n/a"
    : ta.macdHist > ta.macdHistPrev
      ? "rising"
      : ta.macdHist < ta.macdHistPrev
        ? "falling"
        : "flat";
  const r20 = swingRange(candles, 20);
  const r50 = swingRange(candles, 50);
  const recent = candles
    .slice(-10)
    .map((c) => fmtNum(c.close))
    .join(" ");
  const lines = [
    `${displayPair(symbol)} ${market} ${interval}`,
    `last close: ${fmtNum(ta.close)}`,
    `24h change: ${changePct ? fmtPct(changePct) : "n/a"}`,
    `recent closes (oldest->newest, last 10 candles): ${recent}`,
    `swing high/low last 20 candles: ${fmtNum(r20.high)} / ${fmtNum(r20.low)}`,
    `swing high/low last 50 candles: ${fmtNum(r50.high)} / ${fmtNum(r50.low)}`,
    `RSI(14): ${fmtNum(ta.rsi14)}`,
    `MACD(12,26,9): macd=${fmtNum(ta.macdLine)} signal=${fmtNum(ta.macdSignal)} hist=${fmtNum(ta.macdHist)} (histogram ${histDir})`,
    `SMA: 7=${fmtNum(ta.sma7)} 25=${fmtNum(ta.sma25)} 99=${fmtNum(ta.sma99)}`,
    `SMA7/SMA25 cross in last 3 candles: ${ta.cross ?? "none"}`,
    `EMA: 12=${fmtNum(ta.ema12)} 26=${fmtNum(ta.ema26)}`,
    `BOLL(20,2): upper=${fmtNum(ta.bollUpper)} middle=${fmtNum(ta.bollMiddle)} lower=${fmtNum(ta.bollLower)}`,
    `technical verdict: ${VERDICT_EN[verdict]}`,
  ];
  return lines.join("\n").slice(0, 4000);
}

// Readout under the chart plus the on-demand agent analysis. The analysis
// call goes through the control plane (requestMarketAnalysis); a missing LLM
// provider surfaces as a NOT_CONFIGURED hint, other errors verbatim.
function TaReadout({
  candles,
  market,
  symbol,
  interval,
  changePct,
}: {
  candles: Candle[];
  market: DeskTab;
  symbol: string;
  interval: Interval;
  changePct?: string;
}) {
  const { t, locale } = useI18n();
  const ta = useMemo(() => computeTa(candles), [candles]);
  const [asking, setAsking] = useState(false);
  const [analysis, setAnalysis] = useState<{ text: string; model: string } | null>(null);
  const [askError, setAskError] = useState<string | null>(null);
  if (!ta) return null;
  const { rows, verdict } = taSignals(ta);
  const verdictTone: TaTone =
    verdict === "strongbuy" || verdict === "buy" ? "up" : verdict === "neutral" ? "" : "down";

  const ask = async () => {
    if (asking) return;
    setAsking(true);
    setAskError(null);
    try {
      const summary = buildTaSummary(symbol, market, interval, changePct, candles, ta, verdict);
      setAnalysis(await requestMarketAnalysis(symbol, market, interval, locale, summary));
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      const code = err instanceof ApiError ? (err.body?.code ?? "") : "";
      if (code.includes("NOT_CONFIGURED") || msg.includes("NOT_CONFIGURED")) {
        setAskError(t("market.chart.llm.notconfigured"));
      } else {
        setAskError(msg);
      }
    } finally {
      setAsking(false);
    }
  };

  return (
    <div className="ta-readout">
      {rows.map((row) => (
        <div className="ta-row" key={row.label}>
          <span className="ta-key">{row.label}</span>
          <span className={row.tone}>
            {row.value !== "" && `${row.value} — `}
            {t(row.key)}
            {row.extraKey && ` · ${t(row.extraKey)}`}
          </span>
        </div>
      ))}
      <div className="ta-row">
        <span className="ta-key">{t("market.ta.verdict")}</span>
        <span className={`ta-verdict ${verdictTone}`}>{t(`market.ta.verdict.${verdict}`)}</span>
      </div>
      <div className="ta-disclaimer">{t("market.ta.disclaimer")}</div>
      <div className="ta-actions">
        <button type="button" className="btn" onClick={ask} disabled={asking}>
          ◈ {asking ? t("market.chart.asking") : t("market.chart.ask")}
        </button>
        {askError && <span className="ta-error">{askError}</span>}
      </div>
      {analysis && (
        <>
          <div className="ta-analysis">{analysis.text}</div>
          <div className="ta-model">
            {t("market.chart.model")}: {analysis.model}
          </div>
        </>
      )}
    </div>
  );
}

// Candle poll body, keyed by market+interval (and remounted with the panel
// on symbol change) so stale candles never flash and the chart refits
// exactly once per market/pair/interval. A chart fetch failure only mutes
// the panel, never the tabs.
function ChartBody({
  market,
  symbol,
  interval,
  indicators,
  changePct,
}: {
  market: DeskTab;
  symbol: string;
  interval: Interval;
  indicators: IndicatorFlags;
  changePct?: string;
}) {
  const { t } = useI18n();
  const load = useCallback(() => fetchCandles(market, symbol, interval), [market, symbol, interval]);
  const { data, error } = usePoll<Candle[]>(load, MARKET_POLL_MS);

  if (error && !data) {
    return (
      <div className="empty">
        {t(market === "spot" ? "market.unavailable" : "market.futures.unavailable")}
      </div>
    );
  }
  if (!data) return <div className="skeleton" style={{ height: 380 }} />;
  return (
    <>
      <CandleChart candles={data} indicators={indicators} />
      <TaReadout
        candles={data}
        market={market}
        symbol={symbol}
        interval={interval}
        changePct={changePct}
      />
    </>
  );
}

// Indicator chip toggles: MA on by default, the rest off. The last choice is
// kept at module scope so it survives panel remounts for the session.
const INDICATOR_KEYS = ["ma", "ema", "boll", "rsi", "macd"] as const;
let sessionIndicators: IndicatorFlags = {
  ma: true,
  ema: false,
  boll: false,
  rsi: false,
  macd: false,
};

// Chart panel above the grid (Binance layout: chart first, list under it):
// pair header with the same stats as the cards, a 15m/1h/4h/1d switcher,
// the indicator chip row, and the candlestick chart. Remounted (key) by the
// desk on pair change; the market prop always follows the active tab.
function ChartPanel({
  market,
  symbol,
  spot,
  futures,
  onClose,
}: {
  market: DeskTab;
  symbol: string;
  spot: MarketSnapshot | null;
  futures: FuturesRow[] | null;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [interval, setInterval] = useState<(typeof INTERVALS)[number]>("1h");
  const [indicators, setIndicators] = useState<IndicatorFlags>(() => ({ ...sessionIndicators }));
  const toggleIndicator = (key: (typeof INDICATOR_KEYS)[number]) =>
    setIndicators((prev) => {
      const next = { ...prev, [key]: !prev[key] };
      sessionIndicators = next;
      return next;
    });
  const tk = market === "spot" ? spot?.tickers.find((x) => x.symbol === symbol) : undefined;
  const fu = market === "futures" ? futures?.find((x) => x.symbol === symbol) : undefined;
  const px = market === "spot" ? tk?.lastPrice : fu?.markPrice;
  const pct = tk?.priceChangePercent ?? fu?.priceChangePercent;
  const tone = pct ? dir(pct) : "up";
  return (
    <div className="chart-panel">
      <div className="chart-head">
        <span className="market-sym">{displayPair(symbol)}</span>
        {px && <span className={`chart-px ${tone}`}>{fmtPrice(px)}</span>}
        {pct && <span className={`market-chg ${tone}`}>{fmtPct(pct)}</span>}
        {tk && (
          <span className="chart-stats">
            <span>
              {t("market.high")} <b>{fmtPrice(tk.highPrice)}</b>
            </span>
            <span>
              {t("market.low")} <b>{fmtPrice(tk.lowPrice)}</b>
            </span>
            <span>
              {t("market.vol")} <b>{fmtVolume(tk.quoteVolume)}</b>
            </span>
          </span>
        )}
        {fu && (
          <span className="chart-stats">
            <span>
              {t("market.futures.funding")} <b>{fmtFundingRate(fu.lastFundingRate)}</b>
            </span>
            <span>
              {t("market.futures.next")} <b>{fmtFundingCountdown(fu.nextFundingTime)}</b>
            </span>
            <span>
              {t("market.futures.oi")} <b>{fmtVolume(fu.openInterest)}</b>
            </span>
          </span>
        )}
        <div className="seg chart-ivals" role="group" aria-label="Interval">
          {INTERVALS.map((iv) => (
            <button
              key={iv}
              type="button"
              className={`seg-btn${iv === interval ? " active" : ""}`}
              onClick={() => setInterval(iv)}
            >
              {iv}
            </button>
          ))}
        </div>
        <button type="button" className="chart-close" onClick={onClose} aria-label="close">
          ×
        </button>
      </div>
      <div className="chart-chips" role="group" aria-label="Indicators">
        {INDICATOR_KEYS.map((key) => (
          <button
            key={key}
            type="button"
            className={`chip-toggle${indicators[key] ? " active" : ""}`}
            aria-pressed={indicators[key]}
            onClick={() => toggleIndicator(key)}
          >
            {key.toUpperCase()}
          </button>
        ))}
      </div>
      <ChartBody
        key={`${market}:${interval}`}
        market={market}
        symbol={symbol}
        interval={interval}
        indicators={indicators}
        changePct={pct}
      />
    </div>
  );
}

// Binance-style tabbed desk: Spot / Futures tab bar, an optional per-pair
// chart panel above the grid, and the card grids below. Both feeds poll
// independently regardless of the active tab (exactly as the old stacked
// sections did), so a futures geo-block never affects spot.
export function MarketDesk() {
  const { t } = useI18n();
  const [tab, setTab] = useState<DeskTab>("spot");
  // One shared selection across tabs: the chart's market is always the
  // active tab (every DESK_SYMBOL exists on both markets), so switching
  // Spot↔Futures with a chart open refetches the same pair on the new market.
  const [selected, setSelected] = useState<string | null>(null);

  const loadSpot = useCallback(() => fetchMarketSnapshot(DESK_SYMBOLS), []);
  const spot = usePoll<MarketSnapshot>(loadSpot, MARKET_POLL_MS);
  const loadFutures = useCallback(() => fetchFuturesSnapshot(DESK_SYMBOLS), []);
  const futures = usePoll<FuturesRow[]>(loadFutures, MARKET_POLL_MS);

  return (
    <>
      <div className="market-tabs" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={tab === "spot"}
          className={`market-tab${tab === "spot" ? " active" : ""}`}
          onClick={() => setTab("spot")}
        >
          {t("market.tab.spot")}
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === "futures"}
          className={`market-tab${tab === "futures" ? " active" : ""}`}
          onClick={() => setTab("futures")}
        >
          {t("market.tab.futures")}
        </button>
      </div>
      {selected && (
        <ChartPanel
          key={selected}
          market={tab}
          symbol={selected}
          spot={spot.data}
          futures={futures.data}
          onClose={() => setSelected(null)}
        />
      )}
      {tab === "spot" ? (
        <MarketGrid
          data={spot.data}
          error={spot.error}
          selected={selected}
          onSelect={setSelected}
        />
      ) : (
        <FuturesGrid
          data={futures.data}
          error={futures.error}
          selected={selected}
          onSelect={setSelected}
        />
      )}
    </>
  );
}
