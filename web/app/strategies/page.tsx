"use client";

// Strategy list: GET /api/v1/strategies, paginated {items,total,page,limit},
// revalidated on a simple polling interval (SSE/websocket is deferred).

import Link from "next/link";
import { useCallback, useState } from "react";

import { fetchStrategies } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner, Pager, StateBadge } from "./ui";

export default function StrategiesPage() {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const load = useCallback(() => fetchStrategies(page), [page]);
  const { data, error } = usePoll(load);

  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("strat.title")}</h1>
        <p className="page-sub">{t("strat.sub")}</p>
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
          <div className="table-wrap">
            {data.items.length === 0 ? (
              <div className="empty" role="status">{t("dash.empty")}</div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr>
                    <th>{t("tbl.name")}</th>
                    <th>{t("tbl.state")}</th>
                    <th>{t("tbl.tenant")}</th>
                    <th>{t("strat.tbl.strategyid")}</th>
                    <th>{t("tbl.created")}</th>
                    <th>{t("tbl.updated")}</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((strategy) => (
                    <tr key={strategy.strategy_id}>
                      <td>
                        <Link href={`/strategies/${strategy.strategy_id}`}>{strategy.name}</Link>
                      </td>
                      <td>
                        <StateBadge state={strategy.lifecycle_state} />
                      </td>
                      <td className="muted">{strategy.tenant_id}</td>
                      <td className="mono-cell">{strategy.strategy_id}</td>
                      <td className="mono-cell">{strategy.created_at}</td>
                      <td className="mono-cell">{strategy.updated_at}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
          <Pager page={data.page} total={data.total} limit={data.limit} onPage={setPage} />
        </>
      )}
    </>
  );
}
