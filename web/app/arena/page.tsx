"use client";

// Arena — model battle (Phase 28): GET /api/v1/arena/leaderboard ranked by
// return_pct desc, with per-strategy equity curves overlaid on one chart.
// Both GETs share the paper-gate 60/min rate bucket (LC-24), so the page
// polls at PAPER_GATE_POLL_INTERVAL_MS and curves refetch sequentially on
// the leaderboard tick only — never on their own interval.

import { useCallback, useEffect, useRef, useState } from "react";

import {
  PAPER_GATE_POLL_INTERVAL_MS,
  fetchLeaderboard,
  fetchPerformance,
} from "../../src/lib/api/client";
import type { StrategyPerformance } from "../../src/lib/api/schema";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner } from "../strategies/ui";
import { EquityChart, type EquitySeries } from "./equity-chart";
import { LeaderboardTable, SERIES_PALETTE, fmtTime } from "./ui";

const MAX_POINTS = 500;
const DEFAULT_SELECTED = 5;

export default function ArenaPage() {
  const { t } = useI18n();
  const { data, error } = usePoll(fetchLeaderboard, PAPER_GATE_POLL_INTERVAL_MS);

  // Which strategies' curves are charted; null until the first leaderboard
  // arrives, then defaults to the top DEFAULT_SELECTED rows.
  const [selected, setSelected] = useState<ReadonlySet<string> | null>(null);
  const [curves, setCurves] = useState<Record<string, StrategyPerformance>>({});
  // Ids already fetched for the current leaderboard tick: a selection toggle
  // fetches only NEW ids; a fresh tick clears the set so all selected curves
  // refresh together.
  const fetchedRef = useRef(new Set<string>());
  const tickRef = useRef<string | null>(null);

  useEffect(() => {
    if (selected === null && data !== null) {
      setSelected(new Set(data.items.slice(0, DEFAULT_SELECTED).map((i) => i.strategy_id)));
    }
  }, [selected, data]);

  const evaluatedAt = data?.evaluated_at ?? null;
  useEffect(() => {
    if (selected === null || evaluatedAt === null) return;
    if (tickRef.current !== evaluatedAt) {
      tickRef.current = evaluatedAt;
      fetchedRef.current = new Set();
    }
    const ids = [...selected].filter((id) => !fetchedRef.current.has(id));
    if (ids.length === 0) return;
    let cancelled = false;
    void (async () => {
      // Sequential on purpose: these GETs share the paper-gate rate bucket.
      for (const id of ids) {
        try {
          const perf = await fetchPerformance(id, MAX_POINTS);
          if (cancelled) return;
          fetchedRef.current.add(id);
          setCurves((prev) => ({ ...prev, [id]: perf }));
        } catch {
          // keep the last known curve; the next tick retries
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [selected, evaluatedAt]);

  const toggle = useCallback((strategyId: string) => {
    setSelected((prev) => {
      const next = new Set(prev ?? []);
      if (next.has(strategyId)) next.delete(strategyId);
      else next.add(strategyId);
      return next;
    });
  }, []);

  // Chart series in leaderboard order for the selected rows that have a
  // fetched curve; palette colors assigned by that stable order.
  const series: EquitySeries[] = [];
  if (data !== null && selected !== null) {
    for (const item of data.items) {
      if (!selected.has(item.strategy_id)) continue;
      const perf = curves[item.strategy_id];
      if (!perf) continue;
      series.push({
        id: item.strategy_id,
        label: item.model === null ? item.name : `${item.name} (${item.model})`,
        color: SERIES_PALETTE[series.length % SERIES_PALETTE.length] ?? "#f0b90b",
        points: perf.equity_curve.map((p) => ({
          time: Math.floor(Date.parse(p.ts) / 1000),
          value: Number(p.equity),
        })),
      });
    }
  }
  const hasPoints = series.some((s) => s.points.length > 0);

  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("arena.title")}</h1>
        <p className="page-sub">{t("arena.sub")}</p>
        {data !== null && (
          <p className="page-sub mono-cell">
            {t("arena.evaluated", { at: fmtTime(data.evaluated_at) })}
          </p>
        )}
      </header>
      {error && <ErrorBanner message={error} />}
      {!data && !error && (
        <div className="grid" role="status" aria-busy="true">
          <div className="skeleton" style={{ height: 36 }} />
          <div className="skeleton" style={{ height: 36 }} />
          <div className="skeleton" style={{ height: 36 }} />
        </div>
      )}
      {data && (
        <>
          {data.items.length === 0 ? (
            <div className="empty" role="status">
              {t("arena.empty")}
            </div>
          ) : (
            <>
              <LeaderboardTable
                items={data.items}
                selected={selected ?? new Set<string>()}
                onToggle={toggle}
              />
              <section className="section">
                <h2 className="section-title">{t("arena.chart.title")}</h2>
                {hasPoints ? (
                  <EquityChart series={series} />
                ) : (
                  <div className="empty" role="status">
                    {t("arena.chart.empty")}
                  </div>
                )}
              </section>
            </>
          )}
        </>
      )}
    </>
  );
}
