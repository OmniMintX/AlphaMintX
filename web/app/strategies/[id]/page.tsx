"use client";

// Strategy detail: performance panel (equity curve + stats), lifecycle state
// badge + paginated runs list (GET /api/v1/strategies/{id} and
// .../runs?page&limit, tick_number DESC), plus the ops panel
// (operator-surface.md OS-22..OS-30).

import Link from "next/link";
import { useParams } from "next/navigation";
import { useCallback, useState } from "react";

import {
  PAPER_GATE_POLL_INTERVAL_MS,
  fetchPerformance,
  fetchRuns,
  fetchStrategy,
} from "../../../src/lib/api/client";
import { PIPELINE_ROLES, type Strategy } from "../../../src/lib/api/schema";
import { usePoll } from "../../../src/lib/api/usePoll";
import { useI18n, type MessageKey } from "../../../src/lib/i18n";
import { isAdvisoryOnly, isPaperSimulated } from "../../../src/lib/view/run";
import { EquityChart, type EquitySeries } from "../../arena/equity-chart";
import {
  SERIES_PALETTE,
  deriveEquityMarkers,
  fmtTime,
  profitFactorLabel,
  signClass,
} from "../../arena/ui";
import { AdvisoryBanner, ErrorBanner, Pager, PaperBanner, StateBadge } from "../ui";
import { OpsPanel } from "./ops";

const MAX_POINTS = 500;

export default function StrategyDetailPage() {
  const { t } = useI18n();
  const { id } = useParams<{ id: string }>();
  const [page, setPage] = useState(1);

  const loadStrategy = useCallback(() => fetchStrategy(id), [id]);
  const loadRuns = useCallback(() => fetchRuns(id, page), [id, page]);
  const strategy = usePoll(loadStrategy);
  const runs = usePoll(loadRuns);

  return (
    <>
      <div className="breadcrumbs">
        <Link href="/strategies">{t("strat.title")}</Link>
        <span className="sep">/</span>
        <span className="truncate">{strategy.data?.name ?? id}</span>
      </div>
      {strategy.error && <ErrorBanner message={strategy.error} />}
      {!strategy.data && !strategy.error && (
        <div className="grid">
          <div className="skeleton" style={{ height: 28, maxWidth: 320 }} />
          <div className="skeleton" style={{ height: 16, maxWidth: 480 }} />
        </div>
      )}
      {strategy.data && (
        <>
          <header className="page-head">
            <h1 className="page-title">
              {strategy.data.name} <StateBadge state={strategy.data.lifecycle_state} />
            </h1>
            <div className="row small faint mono">
              <span>{strategy.data.strategy_id}</span>
              <span>{t("strat.meta.tenant", { id: strategy.data.tenant_id })}</span>
              <span>{t("strat.meta.created", { ts: strategy.data.created_at })}</span>
              <span>{t("strat.meta.updated", { ts: strategy.data.updated_at })}</span>
            </div>
          </header>
          {isAdvisoryOnly(strategy.data.lifecycle_state) && <AdvisoryBanner />}
          {isPaperSimulated(strategy.data.lifecycle_state) && <PaperBanner />}
        </>
      )}

      <PerformanceSection strategyId={id} strategy={strategy.data} />

      <section className="section">
        <h2 className="section-title">
          {t("strat.runs")}
          {runs.data && <span className="count">{runs.data.total}</span>}
        </h2>
        {runs.error && <ErrorBanner message={runs.error} />}
        {!runs.data && !runs.error && (
          <div className="grid">
            <div className="skeleton" style={{ height: 36 }} />
            <div className="skeleton" style={{ height: 36 }} />
            <div className="skeleton" style={{ height: 36 }} />
          </div>
        )}
        {runs.data && (
          <>
            <div className="table-wrap">
              {runs.data.items.length === 0 ? (
                <div className="empty">{t("strat.runs.empty")}</div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>{t("strat.tbl.tick")}</th>
                      <th>{t("strat.tbl.runid")}</th>
                      <th>{t("strat.tbl.status")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {runs.data.items.map((run) => (
                      <tr key={run.run_id}>
                        <td>
                          <Link href={`/strategies/${id}/runs/${run.run_id}`}>
                            {t("strat.tick", { n: run.tick_number })}
                          </Link>
                        </td>
                        <td className="mono-cell">{run.run_id}</td>
                        <td>
                          {run.completed_at ? (
                            <span className="badge badge-green">
                              {t("strat.run.completed", { ts: run.completed_at })}
                            </span>
                          ) : (
                            <span className="badge badge-yellow">
                              <span className="dot" />
                              {t("strat.run.inprogress")}
                            </span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
            <Pager page={runs.data.page} total={runs.data.total} limit={runs.data.limit} onPage={setPage} />
          </>
        )}
      </section>

      <OpsPanel strategyId={id} onLifecycleChange={strategy.refresh} />
    </>
  );
}

// Paper-window performance: stats row + single-series equity curve with
// fill markers, from GET .../performance. That GET shares the paper-gate
// 60/min rate bucket (LC-24), so it polls at PAPER_GATE_POLL_INTERVAL_MS
// only — never tighter. Also shows the strategy's role_models lineup.
function PerformanceSection({
  strategyId,
  strategy,
}: {
  strategyId: string;
  strategy: Strategy | null;
}) {
  const { t } = useI18n();
  const loadPerf = useCallback(() => fetchPerformance(strategyId, MAX_POINTS), [strategyId]);
  const perf = usePoll(loadPerf, PAPER_GATE_POLL_INTERVAL_MS);
  const data = perf.data;
  const roleModels = strategy?.role_models;

  // Single series: points are floated at render time only (ADR-0003);
  // markers derive from consecutive equity deltas (each non-seed point IS
  // a fill event — the wire carries no order side).
  const series: EquitySeries[] = [];
  if (data !== null && data.equity_curve.length > 0) {
    const points = data.equity_curve.map((p) => ({
      time: Math.floor(Date.parse(p.ts) / 1000),
      value: Number(p.equity),
    }));
    series.push({
      id: data.strategy_id,
      label: data.model === null ? (strategy?.name ?? strategyId) : `${strategy?.name ?? strategyId} (${data.model})`,
      color: SERIES_PALETTE[0],
      points,
      markers: deriveEquityMarkers(points),
    });
  }

  return (
    <section className="section">
      <h2 className="section-title">{t("strat.perf.title")}</h2>
      {perf.error && <ErrorBanner message={perf.error} />}
      {!data && !perf.error && (
        <div className="grid" role="status" aria-busy="true">
          <div className="skeleton" style={{ height: 72 }} />
          <div className="skeleton" style={{ height: 180 }} />
        </div>
      )}
      {data && (
        <>
          <div className="grid grid-4">
            <div className="stat">
              <div className="stat-label">{t("arena.tbl.return")}</div>
              <div className={`stat-value${signClass(data.stats.return_pct)}`}>
                {data.stats.return_pct}
                <span className="unit">%</span>
              </div>
            </div>
            <div className="stat">
              <div className="stat-label">{t("arena.tbl.pnl")}</div>
              <div className={`stat-value${signClass(data.stats.realized_pnl)}`}>
                {data.stats.realized_pnl}
              </div>
            </div>
            <div className="stat">
              <div className="stat-label">{t("arena.tbl.maxdd")}</div>
              <div className="stat-value">
                {data.stats.max_drawdown_pct}
                <span className="unit">%</span>
              </div>
            </div>
            <div className="stat">
              <div className="stat-label">{t("arena.tbl.trades")}</div>
              <div className="stat-value">{data.stats.closed_trades}</div>
              <div className="stat-meta">
                {t("arena.tbl.winrate")} {data.stats.win_rate_pct}% &middot;{" "}
                {t("arena.tbl.pf")}{" "}
                {profitFactorLabel(data.stats.profit_factor, data.stats.realized_pnl)}
              </div>
            </div>
          </div>
          <div style={{ marginTop: 12 }}>
            {series.length > 0 ? (
              <EquityChart series={series} />
            ) : (
              <div className="empty" role="status">
                {t("strat.perf.empty")}
              </div>
            )}
          </div>
          {data.stats.last_fill_at !== null && (
            <p className="faint small mono-cell">
              {t("arena.tbl.lastfill")}: {fmtTime(data.stats.last_fill_at)}
            </p>
          )}
        </>
      )}
      {roleModels && Object.keys(roleModels).length > 0 && (
        <>
          <h3 className="card-title" style={{ marginTop: 12 }}>
            {t("strat.perf.rolemodels")}
          </h3>
          <div className="table-wrap" style={{ marginTop: 6, maxWidth: 480 }}>
            <table className="tbl">
              <thead>
                <tr>
                  <th>{t("strat.perf.tbl.role")}</th>
                  <th>{t("arena.tbl.model")}</th>
                </tr>
              </thead>
              <tbody>
                {PIPELINE_ROLES.filter((role) => roleModels[role] !== undefined).map((role) => (
                  <tr key={role}>
                    <td>{t(`role.${role}` as MessageKey)}</td>
                    <td className="mono-cell">{roleModels[role]}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </section>
  );
}
