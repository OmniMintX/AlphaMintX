"use client";

// Candlestick + volume chart on lightweight-charts v5 (createChart +
// chart.addSeries(CandlestickSeries, …)) with optional indicator overlays
// (MA/EMA/BOLL lines on the price pane) and sub-panes (RSI in pane 1, MACD
// below it — addSeries takes a paneIndex and creates panes on demand).
// Axis/grid colors come from the design tokens on <html> and are re-applied
// when the theme toggle flips data-theme; 15s refreshes update the series in
// place so the user's zoom/scroll survives — callers remount (key) on
// pair/interval change, which is the only time the chart refits.

import { useEffect, useRef } from "react";

import {
  CandlestickSeries,
  ColorType,
  HistogramSeries,
  LineSeries,
  LineStyle,
  createChart,
  type IChartApi,
  type ISeriesApi,
  type UTCTimestamp,
} from "lightweight-charts";

import type { Candle } from "../../src/lib/market/binance";
import { bollinger, ema, macd, rsi, sma } from "../../src/lib/market/indicators";

// Which indicator groups are drawn; owned by the chart panel's chip row.
export interface IndicatorFlags {
  ma: boolean;
  ema: boolean;
  boll: boolean;
  rsi: boolean;
  macd: boolean;
}

// Binance's canonical candle palette (readable on both themes). The faded
// pair is volume-only, one notch lighter than the solid MACD histogram.
const UP = "#0ecb81";
const DOWN = "#f6465d";
const UP_FADED = "rgba(14, 203, 129, 0.35)";
const DOWN_FADED = "rgba(246, 70, 93, 0.35)";

// Indicator palettes — hues that read on both dark and light surfaces.
const MA_PERIODS = [7, 25, 99] as const;
const MA_COLORS = ["#f0b90b", "#e056fd", "#7f8fa6"];
const EMA_PERIODS = [12, 26] as const;
const EMA_COLORS = ["#3fb9c7", "#b07ff0"];
const BOLL_COLORS = ["#c47fd6", "#8a94a6", "#5fa8d3"]; // upper / middle / lower
const RSI_COLOR = "#c9a227";
const MACD_LINE_COLOR = "#4cb3d4";
const MACD_SIGNAL_COLOR = "#f0b90b";

// Thin overlay line without the per-series price tag noise.
function lineOpts(color: string) {
  return {
    color,
    lineWidth: 1 as const,
    priceLineVisible: false,
    lastValueVisible: false,
    crosshairMarkerVisible: false,
  };
}

function tokenColors() {
  const style = getComputedStyle(document.documentElement);
  return {
    text: style.getPropertyValue("--text-3").trim() || "#646b76",
    grid: style.getPropertyValue("--border").trim() || "#21252c",
    gridStrong: style.getPropertyValue("--border-strong").trim() || "#2d323b",
  };
}

export function CandleChart({
  candles,
  indicators,
}: {
  candles: Candle[];
  indicators: IndicatorFlags;
}) {
  const boxRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const candleRef = useRef<ISeriesApi<"Candlestick"> | null>(null);
  const volumeRef = useRef<ISeriesApi<"Histogram"> | null>(null);
  const maRef = useRef<ISeriesApi<"Line">[]>([]);
  const emaRef = useRef<ISeriesApi<"Line">[]>([]);
  const bollRef = useRef<ISeriesApi<"Line">[]>([]);
  const rsiRef = useRef<ISeriesApi<"Line"> | null>(null);
  const macdLineRef = useRef<ISeriesApi<"Line"> | null>(null);
  const macdSignalRef = useRef<ISeriesApi<"Line"> | null>(null);
  const macdHistRef = useRef<ISeriesApi<"Histogram"> | null>(null);
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
    const candleSeries = chart.addSeries(CandlestickSeries, {
      upColor: UP,
      downColor: DOWN,
      borderVisible: false,
      wickUpColor: UP,
      wickDownColor: DOWN,
      priceLineVisible: true,
      lastValueVisible: true,
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
      candleRef.current = null;
      volumeRef.current = null;
      maRef.current = [];
      emaRef.current = [];
      bollRef.current = [];
      rsiRef.current = null;
      macdLineRef.current = null;
      macdSignalRef.current = null;
      macdHistRef.current = null;
      fittedRef.current = false;
      chart.remove();
    };
  }, []);

  // (Re)build indicator series when the chip toggles change. Tear down and
  // recreate the whole set on every change: pane indices shift when RSI/MACD
  // flip, and a toggle click is rare enough that a rebuild is cheap. The data
  // effect below runs right after and fills the fresh series.
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    for (const s of [...maRef.current, ...emaRef.current, ...bollRef.current]) {
      chart.removeSeries(s);
    }
    maRef.current = [];
    emaRef.current = [];
    bollRef.current = [];
    for (const ref of [rsiRef, macdLineRef, macdSignalRef, macdHistRef] as const) {
      if (ref.current) {
        chart.removeSeries(ref.current);
        ref.current = null;
      }
    }
    if (indicators.ma) {
      maRef.current = MA_COLORS.map((c) => chart.addSeries(LineSeries, lineOpts(c)));
    }
    if (indicators.ema) {
      emaRef.current = EMA_COLORS.map((c) => chart.addSeries(LineSeries, lineOpts(c)));
    }
    if (indicators.boll) {
      bollRef.current = BOLL_COLORS.map((c) => chart.addSeries(LineSeries, lineOpts(c)));
    }
    if (indicators.rsi) {
      const series = chart.addSeries(LineSeries, { ...lineOpts(RSI_COLOR), lineWidth: 2 }, 1);
      const guide = tokenColors().gridStrong;
      for (const price of [30, 70]) {
        series.createPriceLine({
          price,
          color: guide,
          lineWidth: 1,
          lineStyle: LineStyle.Dashed,
          axisLabelVisible: false,
          title: "",
        });
      }
      rsiRef.current = series;
    }
    if (indicators.macd) {
      const pane = indicators.rsi ? 2 : 1;
      macdHistRef.current = chart.addSeries(
        HistogramSeries,
        { priceLineVisible: false, lastValueVisible: false },
        pane,
      );
      macdLineRef.current = chart.addSeries(
        LineSeries,
        { ...lineOpts(MACD_LINE_COLOR), lineWidth: 2 },
        pane,
      );
      macdSignalRef.current = chart.addSeries(
        LineSeries,
        { ...lineOpts(MACD_SIGNAL_COLOR), lineWidth: 2 },
        pane,
      );
    }
  }, [indicators]);

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
    // Indicator series: drop the NaN warmup slots the pure math leaves in.
    const closes = candles.map((c) => c.close);
    const lineData = (values: number[]) =>
      candles.flatMap((c, i) => {
        const v = values[i];
        return v !== undefined && Number.isFinite(v)
          ? [{ time: c.time as UTCTimestamp, value: v }]
          : [];
      });
    MA_PERIODS.forEach((n, i) => maRef.current[i]?.setData(lineData(sma(closes, n))));
    EMA_PERIODS.forEach((n, i) => emaRef.current[i]?.setData(lineData(ema(closes, n))));
    if (bollRef.current.length === 3) {
      const bands = bollinger(closes, 20, 2);
      bollRef.current[0]?.setData(lineData(bands.upper));
      bollRef.current[1]?.setData(lineData(bands.middle));
      bollRef.current[2]?.setData(lineData(bands.lower));
    }
    if (rsiRef.current) rsiRef.current.setData(lineData(rsi(closes, 14)));
    if (macdLineRef.current && macdSignalRef.current && macdHistRef.current) {
      const m = macd(closes, 12, 26, 9);
      macdLineRef.current.setData(lineData(m.macd));
      macdSignalRef.current.setData(lineData(m.signal));
      macdHistRef.current.setData(
        candles.flatMap((c, i) => {
          const v = m.hist[i];
          return v !== undefined && Number.isFinite(v)
            ? [
                {
                  time: c.time as UTCTimestamp,
                  value: v,
                  color: v >= 0 ? UP : DOWN,
                },
              ]
            : [];
        }),
      );
    }
    if (!fittedRef.current && candles.length > 0) {
      chart.timeScale().fitContent();
      fittedRef.current = true;
    }
  }, [candles, indicators]);

  return (
    <div
      ref={boxRef}
      className={`chart-body${indicators.rsi || indicators.macd ? " chart-body-tall" : ""}`}
    />
  );
}
