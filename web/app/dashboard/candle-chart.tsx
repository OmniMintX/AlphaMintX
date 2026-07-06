"use client";

// Candlestick + volume chart on lightweight-charts v5 (createChart +
// chart.addSeries(CandlestickSeries, …)). Axis/grid colors come from the
// design tokens on <html> and are re-applied when the theme toggle flips
// data-theme; 15s refreshes update the series in place so the user's
// zoom/scroll survives — callers remount (key) on pair/interval change,
// which is the only time the chart refits.

import { useEffect, useRef } from "react";

import {
  CandlestickSeries,
  ColorType,
  HistogramSeries,
  createChart,
  type IChartApi,
  type ISeriesApi,
  type UTCTimestamp,
} from "lightweight-charts";

import type { Candle } from "../../src/lib/market/binance";

// Binance's canonical candle palette (readable on both themes).
const UP = "#0ecb81";
const DOWN = "#f6465d";
const UP_FADED = "rgba(14, 203, 129, 0.5)";
const DOWN_FADED = "rgba(246, 70, 93, 0.5)";

function tokenColors() {
  const style = getComputedStyle(document.documentElement);
  return {
    text: style.getPropertyValue("--text-3").trim() || "#646b76",
    grid: style.getPropertyValue("--border").trim() || "#21252c",
  };
}

export function CandleChart({ candles }: { candles: Candle[] }) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const candleRef = useRef<ISeriesApi<"Candlestick"> | null>(null);
  const volumeRef = useRef<ISeriesApi<"Histogram"> | null>(null);
  const fittedRef = useRef(false);

  useEffect(() => {
    const box = boxRef.current;
    if (!box) return;
    const { text, grid } = tokenColors();
    const chart = createChart(box, {
      layout: {
        background: { type: ColorType.Solid, color: "transparent" },
        textColor: text,
        attributionLogo: false,
      },
      grid: { vertLines: { color: grid }, horzLines: { color: grid } },
      rightPriceScale: { borderVisible: false },
      timeScale: { borderVisible: false, timeVisible: true },
      autoSize: true,
    });
    const candleSeries = chart.addSeries(CandlestickSeries, {
      upColor: UP,
      downColor: DOWN,
      borderVisible: false,
      wickUpColor: UP,
      wickDownColor: DOWN,
    });
    const volumeSeries = chart.addSeries(HistogramSeries, {
      priceFormat: { type: "volume" },
      priceScaleId: "",
    });
    volumeSeries.priceScale().applyOptions({ scaleMargins: { top: 0.8, bottom: 0 } });
    chartRef.current = chart;
    candleRef.current = candleSeries;
    volumeRef.current = volumeSeries;

    // Follow the sidebar theme toggle (useTheme flips data-theme on <html>).
    const observer = new MutationObserver(() => {
      const c = tokenColors();
      chart.applyOptions({
        layout: { textColor: c.text },
        grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      });
    });
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ["data-theme"],
    });

    return () => {
      observer.disconnect();
      chartRef.current = null;
      candleRef.current = null;
      volumeRef.current = null;
      fittedRef.current = false;
      chart.remove();
    };
  }, []);

  // Refresh series data in place; the nulled refs guard against in-flight
  // poll results landing after unmount. fitContent runs once per mount.
  useEffect(() => {
    const chart = chartRef.current;
    const candleSeries = candleRef.current;
    const volumeSeries = volumeRef.current;
    if (!chart || !candleSeries || !volumeSeries) return;
    candleSeries.setData(
      candles.map((c) => ({
        time: c.time as UTCTimestamp,
        open: c.open,
        high: c.high,
        low: c.low,
        close: c.close,
      })),
    );
    volumeSeries.setData(
      candles.map((c) => ({
        time: c.time as UTCTimestamp,
        value: c.volume,
        color: c.close >= c.open ? UP_FADED : DOWN_FADED,
      })),
    );
    if (!fittedRef.current && candles.length > 0) {
      chart.timeScale().fitContent();
      fittedRef.current = true;
    }
  }, [candles]);

  return <div ref={boxRef} className="chart-body" />;
}
