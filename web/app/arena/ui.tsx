"use client";

// Presentational pieces for the arena page: the model-battle leaderboard
// table and its decimal-string display helpers. Decimals render verbatim
// (ADR-0003) — sign checks are string-shaped, never float conversions.

import Link from "next/link";

import type { LeaderboardItem } from "../../src/lib/api/schema";
import { useI18n } from "../../src/lib/i18n";

// Distinct per-series line colors for the equity-curve overlay (hues that
// read on both dark and light surfaces, like the candle-chart palettes).
export const SERIES_PALETTE = [
  "#f0b90b",
  "#3fb9c7",
  "#e056fd",
  "#0ecb81",
  "#f6465d",
  "#5fa8d3",
  "#c9a227",
  "#b07ff0",
] as const;

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
export function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

// Sign of a signedDecimal-shaped string without float conversion.
export function decimalSign(value: string): -1 | 0 | 1 {
  const digits = value.startsWith("-") ? value.slice(1) : value;
  if (/^0+(\.0+)?$/.test(digits)) return 0;
  return value.startsWith("-") ? -1 : 1;
}

// Profit factor display: null with positive realized PnL means wins with no
// losing trades to divide by (infinite); null otherwise is simply undefined.
export function profitFactorLabel(profitFactor: string | null, realizedPnl: string): string {
  if (profitFactor !== null) return profitFactor;
  return decimalSign(realizedPnl) > 0 ? "\u221E" : "\u2014";
}

function signClass(value: string): string {
  const sign = decimalSign(value);
  return sign > 0 ? " up" : sign < 0 ? " down" : "";
}

export function LeaderboardTable({
  items,
  selected,
  onToggle,
}: {
  items: LeaderboardItem[];
  selected: ReadonlySet<string>;
  onToggle: (strategyId: string) => void;
}) {
  const { t } = useI18n();
  return (
    <div className="table-wrap">
      <table className="tbl">
        <thead>
          <tr>
            <th>{t("arena.tbl.chart")}</th>
            <th>{t("arena.tbl.rank")}</th>
            <th>{t("arena.tbl.strategy")}</th>
            <th>{t("arena.tbl.model")}</th>
            <th>{t("arena.tbl.return")}</th>
            <th>{t("arena.tbl.pnl")}</th>
            <th>{t("arena.tbl.maxdd")}</th>
            <th>{t("arena.tbl.trades")}</th>
            <th>{t("arena.tbl.winrate")}</th>
            <th>{t("arena.tbl.pf")}</th>
            <th>{t("arena.tbl.lastfill")}</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.strategy_id}>
              <td>
                <input
                  type="checkbox"
                  checked={selected.has(item.strategy_id)}
                  aria-label={t("arena.select.label", { name: item.name })}
                  onChange={() => onToggle(item.strategy_id)}
                />
              </td>
              <td className="num">{item.rank}</td>
              <td>
                <Link href={`/strategies/${item.strategy_id}`}>{item.name}</Link>
              </td>
              <td>
                {item.model === null ? (
                  <span className="muted">{"\u2014"}</span>
                ) : (
                  <span className="badge badge-accent">{item.model}</span>
                )}
              </td>
              <td className={`num${signClass(item.return_pct)}`}>{item.return_pct}%</td>
              <td className={`num${signClass(item.realized_pnl)}`}>{item.realized_pnl}</td>
              <td className="num">{item.max_drawdown_pct}%</td>
              <td className="num">{item.closed_trades}</td>
              <td className="num">{item.win_rate_pct}%</td>
              <td className="num">{profitFactorLabel(item.profit_factor, item.realized_pnl)}</td>
              <td className="mono-cell">
                {item.last_fill_at === null ? "\u2014" : fmtTime(item.last_fill_at)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
