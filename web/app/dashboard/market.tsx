"use client";

// Market desk widgets: the ticker tape and the Binance-style tabbed desk
// (Spot / Futures card grids with a per-pair candlestick panel). Public
// read-only data polled from the browser (src/lib/market/binance.ts); a
// fetch failure renders a quiet placeholder — the desk never blocks the
// control-plane view.

import { useCallback, useState } from "react";

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
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { CandleChart } from "./candle-chart";

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

// Candle poll body, keyed by interval (and remounted with the panel on pair
// change) so stale candles never flash and the chart refits exactly once per
// pair/interval. A chart fetch failure only mutes the panel, never the tabs.
function ChartBody({
  market,
  symbol,
  interval,
}: {
  market: DeskTab;
  symbol: string;
  interval: string;
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
  return <CandleChart candles={data} />;
}

// Chart panel above the grid (Binance layout: chart first, list under it):
// pair header with the same stats as the cards, a 15m/1h/4h/1d switcher,
// and the candlestick chart. Remounted (key) by the desk on pair change.
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
      <ChartBody key={interval} market={market} symbol={symbol} interval={interval} />
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
  const [selected, setSelected] = useState<{ market: DeskTab; symbol: string } | null>(null);

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
          key={`${selected.market}:${selected.symbol}`}
          market={selected.market}
          symbol={selected.symbol}
          spot={spot.data}
          futures={futures.data}
          onClose={() => setSelected(null)}
        />
      )}
      {tab === "spot" ? (
        <MarketGrid
          data={spot.data}
          error={spot.error}
          selected={selected?.market === "spot" ? selected.symbol : null}
          onSelect={(symbol) => setSelected({ market: "spot", symbol })}
        />
      ) : (
        <FuturesGrid
          data={futures.data}
          error={futures.error}
          selected={selected?.market === "futures" ? selected.symbol : null}
          onSelect={(symbol) => setSelected({ market: "futures", symbol })}
        />
      )}
    </>
  );
}
