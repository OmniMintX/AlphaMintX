"use client";

// Binance-style right-hand column inside the chart panel: an order book /
// recent trades tab pair polled from the browser (src/lib/market/binance.ts).
// The caller keys the component by market+symbol so a pair or market switch
// remounts it with clean state; a feed failure keeps the last good data on
// screen and only shows the quiet placeholder when nothing loaded at all.

import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";

import {
  displayPair,
  fetchDepth,
  fetchRecentTrades,
  fmtPrice,
  type DepthSnapshot,
  type RecentTrade,
} from "../../src/lib/market/binance";
import { buildLadder, fmtQty, fmtTradeTime } from "../../src/lib/market/depth";
import { useI18n } from "../../src/lib/i18n";

const BOOK_POLL_MS = 2_500;
const TRADES_POLL_MS = 5_000;

type DepthTab = "book" | "trades";

// Loading shimmer while a tab has no data yet (CSS mirrors the ta-skeleton).
function DepthSkeleton() {
  return (
    <div className="depth-skel" aria-busy="true">
      {Array.from({ length: 8 }, (_, i) => (
        <span className="depth-skel-line" key={i} />
      ))}
    </div>
  );
}

export function DepthPanel({ market, symbol }: { market: "spot" | "futures"; symbol: string }) {
  const { t } = useI18n();
  const [tab, setTab] = useState<DepthTab>("book");
  const [book, setBook] = useState<DepthSnapshot | null>(null);
  const [bookError, setBookError] = useState<string | null>(null);
  const [trades, setTrades] = useState<RecentTrade[] | null>(null);
  const [tradesError, setTradesError] = useState<string | null>(null);

  // Arrow keys move between the two tabs (WAI-ARIA tablist pattern); with
  // only two tabs, Left and Right both toggle to — and focus — the other.
  const bookTabRef = useRef<HTMLButtonElement | null>(null);
  const tradesTabRef = useRef<HTMLButtonElement | null>(null);
  const tabArrow = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
    e.preventDefault();
    const next: DepthTab = tab === "book" ? "trades" : "book";
    setTab(next);
    (next === "book" ? bookTabRef : tradesTabRef).current?.focus();
  };

  // Poll only the active tab's feed; ticks skip while the browser tab is
  // hidden. Errors are kept per tab so stale-but-good data stays rendered.
  useEffect(() => {
    let cancelled = false;
    const tick = async () => {
      if (typeof document !== "undefined" && document.hidden) return;
      try {
        if (tab === "book") {
          const snap = await fetchDepth(market, symbol, 20);
          if (cancelled) return;
          setBook(snap);
          setBookError(null);
        } else {
          const rows = await fetchRecentTrades(market, symbol, 30);
          if (cancelled) return;
          setTrades(rows);
          setTradesError(null);
        }
      } catch (err) {
        if (cancelled) return;
        const msg = err instanceof Error ? err.message : String(err);
        if (tab === "book") setBookError(msg);
        else setTradesError(msg);
      }
    };
    void tick();
    const id = window.setInterval(
      () => void tick(),
      tab === "book" ? BOOK_POLL_MS : TRADES_POLL_MS,
    );
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [market, symbol, tab]);

  const ladder = useMemo(() => (book ? buildLadder(book, 12) : null), [book]);
  const tabLabel = t(tab === "book" ? "market.depth.tab.book" : "market.depth.tab.trades");

  return (
    <section className="depth-panel" aria-label={`${tabLabel} ${displayPair(symbol)}`}>
      <div className="depth-tabs" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={tab === "book"}
          tabIndex={tab === "book" ? 0 : -1}
          className={`depth-tab${tab === "book" ? " active" : ""}`}
          ref={bookTabRef}
          onClick={() => setTab("book")}
          onKeyDown={tabArrow}
        >
          {t("market.depth.tab.book")}
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === "trades"}
          tabIndex={tab === "trades" ? 0 : -1}
          className={`depth-tab${tab === "trades" ? " active" : ""}`}
          ref={tradesTabRef}
          onClick={() => setTab("trades")}
          onKeyDown={tabArrow}
        >
          {t("market.depth.tab.trades")}
        </button>
      </div>
      {tab === "book" ? (
        <div role="tabpanel">
          {bookError && !book ? (
            <div className="depth-empty" role="status">
              {t("market.depth.unavailable")}
            </div>
          ) : !ladder ? (
            <DepthSkeleton />
          ) : (
            <>
              <div className="depth-head">
                <span>{t("market.depth.price")}</span>
                <span>{t("market.depth.amount")}</span>
                <span>{t("market.depth.total")}</span>
              </div>
              {/* Asks reversed so the best ask sits at the bottom, right
                  against the spread row (Binance column layout). */}
              {[...ladder.asks].reverse().map((row) => (
                <div className="depth-row ask" key={row.price}>
                  <span className="depth-bar" style={{ width: `${row.pct}%` }} aria-hidden="true" />
                  <span className="depth-price ask">{fmtPrice(row.price)}</span>
                  <span>{fmtQty(row.qty)}</span>
                  <span>{fmtQty(row.total)}</span>
                </div>
              ))}
              <div className="depth-mid">
                {t("market.depth.spread")}
                {ladder.spread !== null && <> {ladder.spread}</>}
                {ladder.spreadPct !== null && <> ({ladder.spreadPct})</>}
              </div>
              {ladder.bids.map((row) => (
                <div className="depth-row bid" key={row.price}>
                  <span className="depth-bar" style={{ width: `${row.pct}%` }} aria-hidden="true" />
                  <span className="depth-price bid">{fmtPrice(row.price)}</span>
                  <span>{fmtQty(row.qty)}</span>
                  <span>{fmtQty(row.total)}</span>
                </div>
              ))}
            </>
          )}
        </div>
      ) : (
        <div role="tabpanel">
          {tradesError && !trades ? (
            <div className="depth-empty" role="status">
              {t("market.trades.unavailable")}
            </div>
          ) : !trades ? (
            <DepthSkeleton />
          ) : (
            <>
              <div className="depth-head">
                <span>{t("market.depth.price")}</span>
                <span>{t("market.depth.amount")}</span>
                <span>{t("market.depth.time")}</span>
              </div>
              {trades.slice(0, 20).map((trade) => (
                <div
                  className={`trade-row${trade.isBuyerMaker ? " sell" : " buy"}`}
                  key={trade.id}
                >
                  <span className="trade-price">{fmtPrice(trade.price)}</span>
                  <span>{fmtQty(trade.qty)}</span>
                  <span>{fmtTradeTime(trade.time)}</span>
                </div>
              ))}
            </>
          )}
        </div>
      )}
    </section>
  );
}
