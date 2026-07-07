"use client";

// Equity-curve overlay on lightweight-charts v5 (createChart +
// chart.addSeries(LineSeries, …)): one line series per selected strategy,
// distinct palette colors, with a legend mapping color → strategy (+model).
// Axis/grid colors come from the design tokens on <html> and are re-applied
// when the theme toggle flips data-theme (same wiring as candle-chart.tsx);
// series data refreshes in place on the leaderboard poll, so the user's
// zoom/scroll survives — fitContent runs once, on the first non-empty data.

import { useEffect, useRef } from "react";

import {
  ColorType,
  LineSeries,
  createChart,
  type IChartApi,
  type ISeriesApi,
  type UTCTimestamp,
} from "lightweight-charts";

// One overlaid curve: time is Unix seconds, value is Number(equity) — the
// only place a decimal string is floated, for rendering only (ADR-0003).
export interface EquitySeries {
  id: string;
  label: string;
  color: string;
  points: { time: number; value: number }[];
}

function tokenColors() {
  const style = getComputedStyle(document.documentElement);
  return {
    text: style.getPropertyValue("--text-3").trim() || "#646b76",
    grid: style.getPropertyValue("--border").trim() || "#21252c",
    gridStrong: style.getPropertyValue("--border-strong").trim() || "#2d323b",
  };
}

export function EquityChart({ series }: { series: EquitySeries[] }) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const linesRef = useRef<Map<string, ISeriesApi<"Line">>>(new Map());
  const fittedRef = useRef(false);

  useEffect(() => {
    const box = boxRef.current;
    if (!box) return;
    const { text, grid, gridStrong } = tokenColors();
    const chart = createChart(box, {
      layout: {
        background: { type: ColorType.Solid, color: "transparent" },
        textColor: text,
        attributionLogo: false,
      },
      grid: { vertLines: { color: grid }, horzLines: { color: grid } },
      crosshair: {
        vertLine: { color: gridStrong, labelBackgroundColor: gridStrong },
        horzLine: { color: gridStrong, labelBackgroundColor: gridStrong },
      },
      rightPriceScale: { borderVisible: false },
      timeScale: { borderVisible: false, timeVisible: true },
      autoSize: true,
    });
    chartRef.current = chart;

    // Follow the sidebar theme toggle (useTheme flips data-theme on <html>).
    const observer = new MutationObserver(() => {
      const c = tokenColors();
      chart.applyOptions({
        layout: { textColor: c.text },
        grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
        crosshair: {
          vertLine: { color: c.gridStrong, labelBackgroundColor: c.gridStrong },
          horzLine: { color: c.gridStrong, labelBackgroundColor: c.gridStrong },
        },
      });
    });
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });

    return () => {
      observer.disconnect();
      chartRef.current = null;
      linesRef.current = new Map();
      fittedRef.current = false;
      chart.remove();
    };
  }, []);

  // Reconcile line series with the selection: drop deselected curves, add
  // new ones, refresh data in place for the rest (zoom/scroll survives).
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const lines = linesRef.current;
    const keep = new Set(series.map((s) => s.id));
    for (const [id, line] of lines) {
      if (!keep.has(id)) {
        chart.removeSeries(line);
        lines.delete(id);
      }
    }
    for (const s of series) {
      let line = lines.get(s.id);
      if (!line) {
        line = chart.addSeries(LineSeries, {
          color: s.color,
          lineWidth: 2,
          priceLineVisible: false,
          lastValueVisible: true,
          crosshairMarkerVisible: true,
        });
        lines.set(s.id, line);
      } else {
        line.applyOptions({ color: s.color });
      }
      line.setData(s.points.map((p) => ({ time: p.time as UTCTimestamp, value: p.value })));
    }
    if (!fittedRef.current && series.some((s) => s.points.length > 0)) {
      chart.timeScale().fitContent();
      fittedRef.current = true;
    }
  }, [series]);

  return (
    <div className="chart-wrap">
      <div ref={boxRef} className="chart-body" />
      {series.length > 0 && (
        <div className="chart-legend">
          {series.map((s) => (
            <span key={s.id}>
              <i style={{ background: s.color }} />
              {s.label}
            </span>
          ))}
        </div>
      )}
    </div>
  );
}
