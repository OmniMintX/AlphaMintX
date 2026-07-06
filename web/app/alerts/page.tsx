"use client";

// Platform safety-alerts feed: GET /api/v1/alerts, paginated
// {items,total,page,limit}, newest first (server order), polled on the
// shared interval. The endpoint is env-class only (platform_admin) — a
// tenant-role session 403s on the data fetch, and the denied message
// renders in place of the table (same treatment as the admin console).

import Link from "next/link";
import { Fragment, useCallback, useEffect, useState } from "react";

import { fetchGlobalAlerts } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner, Pager } from "../strategies/ui";

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

// Short-form id for dense tables; the full id stays in the title tooltip.
function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

// kind is an OPEN set (SS-25) — tone by substring heuristic, never an
// exhaustive switch.
function kindTone(kind: string): string {
  const k = kind.toUpperCase();
  if (k.includes("KILL") || k.includes("BREAKER")) return "badge-red";
  if (k.includes("WATCHDOG") || k.includes("TIMEOUT")) return "badge-yellow";
  return "badge-neutral";
}

// details_json is stored TEXT verbatim: pretty-print when it parses as
// JSON, render verbatim otherwise.
function prettyDetails(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

export default function AlertsPage() {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const [kindInput, setKindInput] = useState("");
  const [kind, setKind] = useState("");
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());

  // Debounced kind filter (~400 ms); a filter change resets to page 1.
  useEffect(() => {
    const id = setTimeout(() => {
      setKind(kindInput.trim());
      setPage(1);
    }, 400);
    return () => clearTimeout(id);
  }, [kindInput]);

  const load = useCallback(() => fetchGlobalAlerts(page, 20, kind), [page, kind]);
  const { data, error, errorStatus } = usePoll(load);
  const denied = errorStatus === 403;

  const toggle = (alertId: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(alertId)) next.delete(alertId);
      else next.add(alertId);
      return next;
    });

  const clearFilter = () => {
    setKindInput("");
    setKind("");
    setPage(1);
  };

  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("alerts.title")}</h1>
        <p className="page-sub">{t("alerts.sub")}</p>
      </header>
      {error && <ErrorBanner message={denied ? t("alerts.denied") : error} />}
      {!data && !error && (
        <div className="grid" role="status" aria-busy="true">
          <div className="skeleton" style={{ height: 36 }} />
          <div className="skeleton" style={{ height: 36 }} />
          <div className="skeleton" style={{ height: 36 }} />
        </div>
      )}
      {data && !denied && (
        <>
          <div className="row alerts-filter">
            <label className="field-label" htmlFor="alerts-kind-filter">
              {t("alerts.filter.label")}
            </label>
            <input
              id="alerts-kind-filter"
              className="input"
              value={kindInput}
              onChange={(e) => setKindInput(e.target.value)}
            />
            {kindInput !== "" && (
              <button type="button" className="btn btn-ghost" onClick={clearFilter}>
                {t("alerts.filter.clear")}
              </button>
            )}
          </div>
          <div className="table-wrap">
            {data.items.length === 0 ? (
              <div className="empty" role="status">
                {t("alerts.empty")}
                <div className="empty-hint">
                  {kind !== "" ? t("alerts.empty.filtered") : t("alerts.empty.hint")}
                </div>
              </div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr>
                    <th scope="col">{t("alerts.tbl.kind")}</th>
                    <th scope="col">{t("alerts.tbl.strategy")}</th>
                    <th scope="col">{t("alerts.tbl.ref")}</th>
                    <th scope="col">{t("alerts.tbl.recorded")}</th>
                    <th scope="col">{t("alerts.tbl.details")}</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((alert) => {
                    const expanded = open.has(alert.alert_id);
                    return (
                      <Fragment key={alert.alert_id}>
                        <tr>
                          <td>
                            <span className={`badge ${kindTone(alert.kind)}`}>{alert.kind}</span>
                          </td>
                          <td
                            className="mono-cell"
                            title={alert.strategy_id ?? undefined}
                            aria-label={alert.strategy_id ?? undefined}
                          >
                            {alert.strategy_id ? (
                              <Link href={`/strategies/${alert.strategy_id}`}>
                                {shortId(alert.strategy_id)}
                              </Link>
                            ) : (
                              <span className="faint small">&mdash;</span>
                            )}
                          </td>
                          <td
                            className="mono-cell alert-ref"
                            title={alert.ref_id ?? undefined}
                            aria-label={alert.ref_id ?? undefined}
                          >
                            {alert.ref_id ?? <span className="faint small">&mdash;</span>}
                          </td>
                          <td className="mono-cell">{fmtTime(alert.recorded_at)}</td>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost small"
                              aria-expanded={expanded}
                              aria-controls={`alert-details-${alert.alert_id}`}
                              onClick={() => toggle(alert.alert_id)}
                            >
                              {t("alerts.tbl.details")}
                            </button>
                          </td>
                        </tr>
                        {expanded && (
                          <tr className="details-row" id={`alert-details-${alert.alert_id}`}>
                            <td colSpan={5}>
                              <pre className="codeblock">{prettyDetails(alert.details_json)}</pre>
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    );
                  })}
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
