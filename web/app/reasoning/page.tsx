"use client";

// Live reasoning explorer: pick a strategy and one of its persisted runs, then
// render the trace through the run-detail section components (read-only — the
// L1 approval panel stays on the run page). All three GETs are reader-tier.

import Link from "next/link";
import { useCallback, useState } from "react";

import { fetchRunDetail, fetchRuns, fetchStrategies } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import {
  AnalystSection,
  CostsSection,
  DebateSection,
  ProposalSection,
  VerdictSection,
} from "../strategies/[id]/runs/[runId]/sections";
import { ErrorBanner } from "../strategies/ui";

export default function ReasoningPage() {
  const { t } = useI18n();

  const loadStrategies = useCallback(() => fetchStrategies(), []);
  const strategies = usePoll(loadStrategies);
  const [strategyId, setStrategyId] = useState<string | null>(null);

  const items = strategies.data?.items ?? [];
  // Default to the first strategy of page 1 until the operator picks one; a
  // pick that drops off page 1 on a later poll falls back to the default.
  const picked = items.some((s) => s.strategy_id === strategyId) ? strategyId : null;
  const sid = picked ?? items[0]?.strategy_id ?? null;

  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("reason.title")}</h1>
        <p className="page-sub">{t("reason.live.sub")}</p>
      </header>

      {strategies.error && <ErrorBanner message={strategies.error} />}
      {!strategies.data && !strategies.error && (
        <div className="grid">
          <div className="skeleton" style={{ height: 36, maxWidth: 320 }} />
        </div>
      )}
      {strategies.data && items.length === 0 && (
        <div className="empty">
          {t("reason.empty.strategies")}
          <div className="empty-hint">{t("reason.empty.strategies.hint")}</div>
        </div>
      )}
      {strategies.data && sid && (
        <>
          <div className="row">
            <label className="field">
              <span className="field-label">{t("reason.pick.strategy")}</span>
              <select
                className="select"
                value={sid}
                onChange={(e) => setStrategyId(e.target.value)}
              >
                {items.map((s) => (
                  <option key={s.strategy_id} value={s.strategy_id}>
                    {s.name}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {/* Keyed remount resets the run selection and poll state on
              strategy change, so the areas below show fresh skeletons. */}
          <RunExplorer key={sid} strategyId={sid} />
        </>
      )}
    </>
  );
}

// Run picker (page 1, tick DESC) + the selected run's trace.
function RunExplorer({ strategyId }: { strategyId: string }) {
  const { t } = useI18n();
  const loadRuns = useCallback(() => fetchRuns(strategyId), [strategyId]);
  const runs = usePoll(loadRuns);
  const [runId, setRunId] = useState<string | null>(null);

  const items = runs.data?.items ?? [];
  // Default to the newest completed run; fall back to the newest run. A pick
  // that drops off page 1 on a later poll falls back to the default.
  const defaultRun = items.find((run) => run.completed_at) ?? items[0];
  const picked = items.some((run) => run.run_id === runId) ? runId : null;
  const rid = picked ?? defaultRun?.run_id ?? null;

  return (
    <>
      {runs.error && <ErrorBanner message={runs.error} />}
      {!runs.data && !runs.error && (
        <div className="grid" style={{ marginTop: 12 }}>
          <div className="skeleton" style={{ height: 36, maxWidth: 480 }} />
        </div>
      )}
      {runs.data && items.length === 0 && (
        <div className="empty">{t("reason.empty.runs")}</div>
      )}
      {rid && (
        <>
          <div className="row" style={{ marginTop: 12 }}>
            <label className="field">
              <span className="field-label">{t("reason.pick.run")}</span>
              <select className="select" value={rid} onChange={(e) => setRunId(e.target.value)}>
                {items.map((run) => (
                  <option key={run.run_id} value={run.run_id}>
                    {t("strat.tick", { n: run.tick_number })} &middot; {run.run_id.slice(0, 8)}{" "}
                    &middot;{" "}
                    {run.completed_at
                      ? t("strat.run.completed", { ts: run.completed_at })
                      : t("strat.run.inprogress")}
                  </option>
                ))}
              </select>
            </label>
          </div>
          <RunTrace key={rid} strategyId={strategyId} runId={rid} />
        </>
      )}
    </>
  );
}

// The selected run's persisted trace, rendered through the same section
// components as the run-detail page (absent proposal/verdict/trace degrade
// identically there and here).
function RunTrace({ strategyId, runId }: { strategyId: string; runId: string }) {
  const { t } = useI18n();
  const loadRun = useCallback(() => fetchRunDetail(strategyId, runId), [strategyId, runId]);
  const run = usePoll(loadRun);
  const data = run.data;

  return (
    <>
      {run.error && <ErrorBanner message={run.error} />}
      {!data && !run.error && (
        <div className="grid" style={{ marginTop: 12 }}>
          <div className="skeleton" style={{ height: 120 }} />
          <div className="skeleton" style={{ height: 120 }} />
          <div className="skeleton" style={{ height: 120 }} />
        </div>
      )}
      {data && (
        <>
          <p className="small" style={{ marginTop: 12 }}>
            <Link href={`/strategies/${strategyId}/runs/${runId}`}>{t("reason.open.run")}</Link>
          </p>
          <AnalystSection trace={data.trace} proposal={data.proposal} />
          <DebateSection trace={data.trace} proposal={data.proposal} />
          {data.proposal ? (
            <ProposalSection proposal={data.proposal} />
          ) : (
            <section className="section">
              <h2 className="section-title">{t("run.trader")}</h2>
              <div className="card">
                <div className="banner banner-error">{t("run.noproposal")}</div>
              </div>
            </section>
          )}
          {data.verdict && <VerdictSection verdict={data.verdict} />}
          <CostsSection trace={data.trace} proposal={data.proposal} />
        </>
      )}
    </>
  );
}
