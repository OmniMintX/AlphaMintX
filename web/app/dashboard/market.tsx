"use client";

// Market desk widgets: the ticker tape and the market-overview cards.
// Public read-only data polled from the browser (src/lib/market/binance.ts);
// a fetch failure renders a quiet placeholder — the desk never blocks the
// control-plane view.

import { useCallback } from "react";

import {
  DESK_SYMBOLS,
  displayPair,
  fetchFuturesSnapshot,
  fetchMarketSnapshot,
  fmtFundingCountdown,
  fmtFundingRate,
  fmtPct,
  fmtPrice,
  fmtVolume,
  type FuturesRow,
  type MarketSnapshot,
} from "../../src/lib/market/binance";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";

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

export function MarketGrid() {
  const { t } = useI18n();
  const load = useCallback(() => fetchMarketSnapshot(DESK_SYMBOLS), []);
  const { data, error } = usePoll<MarketSnapshot>(load, MARKET_POLL_MS);

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
          <div className="market-card" key={tk.symbol}>
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
// geo-blocked (HTTP 451) while spot keeps working, so this polls and fails
// independently: on error it renders a one-line notice, never breaking the
// spot section; if the feed recovers on a later poll the cards reappear.
export function FuturesGrid() {
  const { t } = useI18n();
  const load = useCallback(() => fetchFuturesSnapshot(DESK_SYMBOLS), []);
  const { data, error } = usePoll<FuturesRow[]>(load, MARKET_POLL_MS);

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
          <div className="market-card" key={row.symbol}>
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
