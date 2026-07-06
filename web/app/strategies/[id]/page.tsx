"use client";

// Strategy detail: lifecycle state badge + paginated runs list
// (GET /api/v1/strategies/{id} and .../runs?page&limit, tick_number DESC),
// plus the ops panel (operator-surface.md OS-22..OS-30).

import Link from "next/link";
import { useParams } from "next/navigation";
import { useCallback, useState } from "react";

import { fetchRuns, fetchStrategy } from "../../../src/lib/api/client";
import { usePoll } from "../../../src/lib/api/usePoll";
import { isAdvisoryOnly, isPaperSimulated } from "../../../src/lib/view/run";
import { AdvisoryBanner, ErrorBanner, Pager, PaperBanner, StateBadge } from "../ui";
import { OpsPanel } from "./ops";

export default function StrategyDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [page, setPage] = useState(1);

  const loadStrategy = useCallback(() => fetchStrategy(id), [id]);
  const loadRuns = useCallback(() => fetchRuns(id, page), [id, page]);
  const strategy = usePoll(loadStrategy);
  const runs = usePoll(loadRuns);

  return (
    <>
      <div className="breadcrumbs">
        <Link href="/strategies">Strategies</Link>
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
              <span>tenant {strategy.data.tenant_id}</span>
              <span>created {strategy.data.created_at}</span>
              <span>updated {strategy.data.updated_at}</span>
            </div>
          </header>
          {isAdvisoryOnly(strategy.data.lifecycle_state) && <AdvisoryBanner />}
          {isPaperSimulated(strategy.data.lifecycle_state) && <PaperBanner />}
        </>
      )}

      <section className="section">
        <h2 className="section-title">
          Runs
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
                <div className="empty">No runs yet.</div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>Tick</th>
                      <th>Run ID</th>
                      <th>Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {runs.data.items.map((run) => (
                      <tr key={run.run_id}>
                        <td>
                          <Link href={`/strategies/${id}/runs/${run.run_id}`}>
                            Tick {run.tick_number}
                          </Link>
                        </td>
                        <td className="mono-cell">{run.run_id}</td>
                        <td>
                          {run.completed_at ? (
                            <span className="badge badge-green">completed {run.completed_at}</span>
                          ) : (
                            <span className="badge badge-yellow">
                              <span className="dot" />
                              in progress
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
