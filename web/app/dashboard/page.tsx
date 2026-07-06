"use client";

// Live control-plane dashboard: strategy fleet overview polled through the
// /api/cp session proxy, plus the non-negotiable safety invariants and the
// autonomy ladder reference. (Moved here from / — the root is now the
// public landing.)

import Link from "next/link";
import { useCallback, useState } from "react";

import { fetchStrategies } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner, Pager, StateBadge } from "../strategies/ui";
import { MarketDesk, TickerTape } from "./market";

const INVARIANT_KEYS = ["inv.1", "inv.2", "inv.3", "inv.4", "inv.5", "inv.6", "inv.7"] as const;

const LADDER = [
  { code: "L0", key: "l0", tone: "badge-neutral" },
  { code: "L1", key: "l1", tone: "badge-accent" },
  { code: "L2", key: "l2", tone: "badge-yellow" },
  { code: "L3", key: "l3", tone: "badge-green" },
] as const;

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

export default function DashboardPage() {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const load = useCallback(() => fetchStrategies(page), [page]);
  const { data, error } = usePoll(load);

  const items = data?.items ?? [];
  const partial = data !== null && data.total > items.length;
  const live = items.filter((s) => s.lifecycle_state.startsWith("live_")).length;
  const paper = items.filter((s) => s.lifecycle_state === "paper").length;
  const attention = items.filter(
    (s) => s.lifecycle_state === "killed" || s.lifecycle_state === "paused",
  ).length;

  return (
    <>
      <TickerTape />
      <div className="page-head">
        <h1 className="page-title">{t("dash.title")}</h1>
        <p className="page-sub">{t("dash.sub")}</p>
      </div>

      <section className="section section-first">
        <h2 className="section-title">{t("market.title")}</h2>
        <MarketDesk />
      </section>

      <div className="grid grid-4" style={{ marginTop: 16 }}>
        <div className="stat">
          <div className="stat-label">{t("dash.stat.total")}</div>
          <div className="stat-value">{data ? data.total : "\u2014"}</div>
          <div className="stat-meta">{t("dash.stat.total.meta")}</div>
        </div>
        <div className="stat">
          <div className="stat-label">{t("dash.stat.live")}</div>
          <div className="stat-value">{data ? live : "\u2014"}</div>
          <div className="stat-meta">
            {t("dash.stat.live.meta")}
            {partial ? t("dash.ofpage") : ""}.
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">{t("dash.stat.paper")}</div>
          <div className="stat-value">{data ? paper : "\u2014"}</div>
          <div className="stat-meta">
            {t("dash.stat.paper.meta")}
            {partial ? t("dash.ofpage") : ""}.
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">{t("dash.stat.attention")}</div>
          <div className="stat-value">{data ? attention : "\u2014"}</div>
          <div className="stat-meta">
            {t("dash.stat.attention.meta")}
            {partial ? t("dash.ofpage") : ""}.
          </div>
        </div>
      </div>

      <section className="section">
        <h2 className="section-title">
          {t("dash.strategies")}
          {data && <span className="count">{data.total}</span>}
        </h2>
        {error && <ErrorBanner message={error} />}
        {!data && !error && (
          <div className="grid">
            <div className="skeleton" style={{ height: 36 }} />
            <div className="skeleton" style={{ height: 36 }} />
            <div className="skeleton" style={{ height: 36 }} />
          </div>
        )}
        {data && (
          <>
            <div className="table-wrap">
              {items.length === 0 ? (
                <div className="empty">
                  {t("dash.empty")}
                  <div className="empty-hint">{t("dash.empty.hint")}</div>
                </div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>{t("tbl.name")}</th>
                      <th>{t("tbl.state")}</th>
                      <th>{t("tbl.tenant")}</th>
                      <th>{t("tbl.id")}</th>
                      <th>{t("tbl.created")}</th>
                      <th>{t("tbl.updated")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {items.map((s) => (
                      <tr key={s.strategy_id}>
                        <td>
                          <Link href={`/strategies/${s.strategy_id}`}>{s.name}</Link>
                        </td>
                        <td>
                          <StateBadge state={s.lifecycle_state} />
                        </td>
                        <td className="muted">{s.tenant_id}</td>
                        <td className="mono-cell">
                          <span
                            className="truncate"
                            style={{ maxWidth: 140, display: "inline-block" }}
                          >
                            {s.strategy_id}
                          </span>
                        </td>
                        <td className="mono-cell">{fmtTime(s.created_at)}</td>
                        <td className="mono-cell">{fmtTime(s.updated_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
            <Pager page={data.page} total={data.total} limit={data.limit} onPage={setPage} />
          </>
        )}
      </section>

      <section className="section">
        <h2 className="section-title">
          {t("dash.invariants")}
          <span className="count">{INVARIANT_KEYS.length}</span>
        </h2>
        <div className="grid grid-2">
          {INVARIANT_KEYS.map((key, i) => (
            <div className="card" key={key}>
              <h3 className="card-title">{String(i + 1).padStart(2, "0")}</h3>
              <p className="page-sub">{t(key)}</p>
            </div>
          ))}
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">
          {t("dash.ladder")}
          <span className="count">{LADDER.length}</span>
        </h2>
        <div className="grid grid-4">
          {LADDER.map(({ code, key, tone }) => (
            <div className="card" key={key}>
              <h3 className="card-title row">
                <span className={`badge ${tone}`}>
                  <span className="dot" />
                  {code}
                </span>
                {t(`ladder.${key}.name`)}
              </h3>
              <p className="small muted">{t(`ladder.${key}.detail`)}</p>
            </div>
          ))}
        </div>
      </section>
    </>
  );
}
