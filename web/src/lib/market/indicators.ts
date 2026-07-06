// Pure technical-indicator math for the desk chart (no dependencies).
// Every function returns arrays aligned to the input length with NaN in the
// warmup slots — chart/readout code filters those out with Number.isFinite.
// All of this is chart geometry / display analytics (ADR-0003), never
// accounting.

// In-bounds reads still type as `number | undefined` under
// noUncheckedIndexedAccess; the ?? NaN fallbacks are dead code that keeps
// the math honest (a wrong index would surface as a NaN slot).

// Simple moving average over a sliding window of n values.
export function sma(values: number[], n: number): number[] {
  const out = new Array<number>(values.length).fill(NaN);
  if (n <= 0) return out;
  let sum = 0;
  for (let i = 0; i < values.length; i++) {
    sum += values[i] ?? NaN;
    if (i >= n) sum -= values[i - n] ?? NaN;
    if (i >= n - 1) out[i] = sum / n;
  }
  return out;
}

// Exponential moving average seeded with the SMA of the first n values.
export function ema(values: number[], n: number): number[] {
  const out = new Array<number>(values.length).fill(NaN);
  if (n <= 0 || values.length < n) return out;
  let seed = 0;
  for (let i = 0; i < n; i++) seed += values[i] ?? NaN;
  let prev = seed / n;
  out[n - 1] = prev;
  const k = 2 / (n + 1);
  for (let i = n; i < values.length; i++) {
    prev = (values[i] ?? NaN) * k + prev * (1 - k);
    out[i] = prev;
  }
  return out;
}

// Relative Strength Index with Wilder smoothing (the classic RSI(14)).
export function rsi(closes: number[], period = 14): number[] {
  const out = new Array<number>(closes.length).fill(NaN);
  if (period <= 0 || closes.length <= period) return out;
  const toRsi = (gain: number, loss: number) =>
    loss === 0 ? 100 : 100 - 100 / (1 + gain / loss);
  let gain = 0;
  let loss = 0;
  for (let i = 1; i <= period; i++) {
    const d = (closes[i] ?? NaN) - (closes[i - 1] ?? NaN);
    if (d >= 0) gain += d;
    else loss -= d;
  }
  let avgGain = gain / period;
  let avgLoss = loss / period;
  out[period] = toRsi(avgGain, avgLoss);
  for (let i = period + 1; i < closes.length; i++) {
    const d = (closes[i] ?? NaN) - (closes[i - 1] ?? NaN);
    avgGain = (avgGain * (period - 1) + Math.max(d, 0)) / period;
    avgLoss = (avgLoss * (period - 1) + Math.max(-d, 0)) / period;
    out[i] = toRsi(avgGain, avgLoss);
  }
  return out;
}

export interface MacdResult {
  macd: number[];
  signal: number[];
  hist: number[];
}

// MACD line (fast EMA − slow EMA), signal EMA over the line, histogram.
// NaN warmup propagates through subtraction, so all three stay aligned.
export function macd(closes: number[], fast = 12, slow = 26, signalPeriod = 9): MacdResult {
  const fastEma = ema(closes, fast);
  const slowEma = ema(closes, slow);
  const macdLine = closes.map((_, i) => (fastEma[i] ?? NaN) - (slowEma[i] ?? NaN));
  const signal = new Array<number>(closes.length).fill(NaN);
  const start = macdLine.findIndex((v) => Number.isFinite(v));
  if (start >= 0) {
    const tail = ema(macdLine.slice(start), signalPeriod);
    tail.forEach((v, i) => {
      signal[start + i] = v;
    });
  }
  const hist = macdLine.map((m, i) => m - (signal[i] ?? NaN));
  return { macd: macdLine, signal, hist };
}

export interface BollingerResult {
  upper: number[];
  middle: number[];
  lower: number[];
}

// Bollinger Bands: SMA(n) middle band ± k population standard deviations.
export function bollinger(closes: number[], n = 20, k = 2): BollingerResult {
  const middle = sma(closes, n);
  const upper = new Array<number>(closes.length).fill(NaN);
  const lower = new Array<number>(closes.length).fill(NaN);
  for (let i = n - 1; i < closes.length; i++) {
    const mean = middle[i] ?? NaN;
    let variance = 0;
    for (let j = i - n + 1; j <= i; j++) {
      const d = (closes[j] ?? NaN) - mean;
      variance += d * d;
    }
    const sd = Math.sqrt(variance / n);
    upper[i] = mean + k * sd;
    lower[i] = mean - k * sd;
  }
  return { upper, middle, lower };
}
