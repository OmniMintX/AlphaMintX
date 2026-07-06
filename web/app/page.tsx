"use client";

// Live control-plane dashboard: strategy fleet overview polled from
// GET /api/v1/strategies, plus the non-negotiable safety invariants and the
// autonomy ladder reference.

import Link from "next/link";
import { useCallback, useState } from "react";

import { fetchStrategies } from "../src/lib/api/client";
import { usePoll } from "../src/lib/api/usePoll";
import { ErrorBanner, Pager, StateBadge } from "./strategies/ui";

const INVARIANTS = [
  "LLMs never place orders directly. Only the Go OMS talks to exchanges; every order passes the deterministic Risk Gate first.",
  "SL/TP live on the exchange, not in slow LLM loops; no open position without an exchange-resident stop-loss while require_stop_loss=true.",
  "Autonomy ladder per strategy (L0\u2013L3); promotion to real money requires a code-enforced paper-gate.",
  "Kill-switch at 3 tiers (strategy / tenant / platform): cancel ENTRY orders, preserve protective stops, no auto-restart. Circuit breaker on daily loss demotes to L0 for the UTC day.",
  "Risk limits are set by humans (Admin) \u2014 a hard ceiling neither Trader users nor AI agents can raise.",
  "Exchange API keys are write-only after save (field-level encryption); trade-only, never withdrawal-enabled.",
  "Track record is immutable/append-only; backtests free of lookahead bias; strategy code identical across backtest / paper / live.",
] as const;

const LADDER = [
  {
    level: "L0 Advisor",
    tone: "badge-neutral",
    detail: "Proposals persisted and shown only; no OMS submission.",
  },
  {
    level: "L1 Copilot",
    tone: "badge-accent",
    detail:
      "OMS submits only after per-proposal human approval; no decision within the timeout (default 600 s) \u21d2 auto-reject.",
  },
  {
    level: "L2 Semi-auto",
    tone: "badge-yellow",
    detail:
      "OMS submits automatically within the L2 envelope; above-envelope proposals escalate through the L1 approve flow.",
  },
  {
    level: "L3 Full-auto",
    tone: "badge-green",
    detail:
      "OMS submits any gate-approved proposal; kill-switch and risk limits still apply.",
  },
] as const;

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

export default function DashboardPage() {
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
      <div className="page-head">
        <h1 className="page-title">Dashboard</h1>
        <p className="page-sub">
          Live control-plane view &mdash; strategy fleet, lifecycle states, and safety
          posture, polled every 10 s.
        </p>
      </div>

      <div className="grid grid-4">
        <div className="stat">
          <div className="stat-label">Total strategies</div>
          <div className="stat-value">{data ? data.total : "\u2014"}</div>
          <div className="stat-meta">Registered across all tenants.</div>
        </div>
        <div className="stat">
          <div className="stat-label">Live</div>
          <div className="stat-value">{data ? live : "\u2014"}</div>
          <div className="stat-meta">
            Trading real money (L1&ndash;L3){partial ? ", of current page" : ""}.
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">Paper</div>
          <div className="stat-value">{data ? paper : "\u2014"}</div>
          <div className="stat-meta">
            Simulated fills, no exchange orders{partial ? ", of current page" : ""}.
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">Attention</div>
          <div className="stat-value">{data ? attention : "\u2014"}</div>
          <div className="stat-meta">
            Killed or paused &mdash; operator review{partial ? ", of current page" : ""}.
          </div>
        </div>
      </div>

      <section className="section">
        <h2 className="section-title">
          Strategies
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
                <div className="empty">No strategies yet.</div>
              ) : (
                <table className="tbl">
                  <thead>
                    <tr>
                      <th>Name</th>
                      <th>State</th>
                      <th>Tenant</th>
                      <th>ID</th>
                      <th>Created</th>
                      <th>Updated</th>
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
          Safety invariants
          <span className="count">{INVARIANTS.length}</span>
        </h2>
        <div className="grid grid-2">
          {INVARIANTS.map((text, i) => (
            <div className="card" key={i}>
              <h3 className="card-title">{String(i + 1).padStart(2, "0")}</h3>
              <p className="page-sub">{text}</p>
            </div>
          ))}
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">
          Autonomy ladder
          <span className="count">{LADDER.length}</span>
        </h2>
        <div className="grid grid-4">
          {LADDER.map(({ level, tone, detail }) => {
            const [code, ...rest] = level.split(" ");
            return (
              <div className="card" key={level}>
                <h3 className="card-title row">
                  <span className={`badge ${tone}`}>
                    <span className="dot" />
                    {code}
                  </span>
                  {rest.join(" ")}
                </h3>
                <p className="small muted">{detail}</p>
              </div>
            );
          })}
        </div>
      </section>
    </>
  );
}
