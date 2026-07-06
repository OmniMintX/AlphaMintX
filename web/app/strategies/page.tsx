"use client";

// Strategy list: GET /api/v1/strategies, paginated {items,total,page,limit},
// revalidated on a simple polling interval (SSE/websocket is deferred).
// Owners/admins (and platform_admin) also get an inline create disclosure.

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";

import { createStrategy, fetchMe, fetchStrategies } from "../../src/lib/api/client";
import type { SessionUser } from "../../src/lib/api/schema";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner, Pager, StateBadge } from "./ui";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// Session identity for the create-strategy gate, mirroring app/nav.tsx: a
// failed fetch leaves it null (identity is cosmetic — data fetches own the
// 401 redirect).
function useSessionUser(): SessionUser | null {
  const [user, setUser] = useState<SessionUser | null>(null);
  useEffect(() => {
    let cancelled = false;
    fetchMe()
      .then((u) => {
        if (!cancelled) setUser(u);
      })
      .catch(() => {
        // identity is cosmetic here; data fetches own the 401 redirect
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return user;
}

// Roles that may create strategies (strategy-provisioning.md SP-2): tenant
// owner/admin in their own tenant, platform_admin in any tenant. This gates
// the disclosure only — the API still enforces the role server-side.
const CREATE_ROLES = new Set(["platform_admin", "owner", "admin"]);

export default function StrategiesPage() {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const load = useCallback(() => fetchStrategies(page), [page]);
  const { data, error, refresh } = usePoll(load);
  const user = useSessionUser();

  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("strat.title")}</h1>
        <p className="page-sub">{t("strat.sub")}</p>
      </header>
      {user !== null && CREATE_ROLES.has(user.role) && (
        <CreateStrategyPanel user={user} onCreated={refresh} />
      )}
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

// Create-strategy disclosure (collapsed by default): platform_admin sessions
// have tenant_id null and must name the target tenant; tenant owners/admins
// always create in their own tenant. A 409 STRATEGY_NAME_TAKEN surfaces
// verbatim in the error banner.
function CreateStrategyPanel({ user, onCreated }: { user: SessionUser; onCreated: () => void }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [lifecycle, setLifecycle] = useState<"draft" | "paper">("draft");
  const [tenantId, setTenantId] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [createdId, setCreatedId] = useState<string | null>(null);

  const isPlatformAdmin = user.role === "platform_admin";
  const targetTenant = isPlatformAdmin ? tenantId.trim() : (user.tenant_id ?? "");
  const valid = name.trim() !== "" && targetTenant !== "";

  // Opening (or reopening) the form discards any previously shown result.
  const toggle = () => {
    setOpen((o) => !o);
    setError(null);
    setCreatedId(null);
  };

  const submit = async () => {
    setPending(true);
    setError(null);
    try {
      const res = await createStrategy({
        tenant_id: targetTenant,
        name: name.trim(),
        lifecycle_state: lifecycle,
      });
      setCreatedId(res.strategy_id);
      setName("");
      setTenantId("");
      setLifecycle("draft");
      setOpen(false);
      onCreated();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div style={{ marginBottom: 16 }}>
      {error && <ErrorBanner message={error} />}
      <div className="row">
        <button type="button" className="btn" aria-expanded={open} onClick={toggle}>
          {open ? t("strat.create.cancel") : t("strat.create.btn")}
        </button>
      </div>
      {open && (
        <div className="row" style={{ marginTop: 10 }}>
          <label className="field" htmlFor="cs-name">
            <span className="field-label">{t("tbl.name")}</span>
            <input
              id="cs-name"
              className="input"
              style={{ minWidth: "16rem" }}
              placeholder={t("strat.create.name.ph")}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="field" htmlFor="cs-state">
            <span className="field-label">{t("strat.create.state")}</span>
            <select
              id="cs-state"
              className="select"
              value={lifecycle}
              onChange={(e) => setLifecycle(e.target.value as "draft" | "paper")}
            >
              <option value="draft">{t("state.draft")}</option>
              <option value="paper">{t("state.paper")}</option>
            </select>
          </label>
          {isPlatformAdmin && (
            <label className="field" htmlFor="cs-tenant">
              <span className="field-label">{t("tbl.tenant")}</span>
              <input
                id="cs-tenant"
                className="input mono"
                style={{ minWidth: "12rem" }}
                placeholder={t("strat.create.tenant.ph")}
                autoComplete="off"
                spellCheck={false}
                value={tenantId}
                onChange={(e) => setTenantId(e.target.value)}
              />
            </label>
          )}
          <button
            type="button"
            className="btn btn-primary"
            disabled={pending || !valid}
            onClick={() => void submit()}
          >
            {t("strat.create.submit")}
          </button>
        </div>
      )}
      {createdId && (
        <div className="result-line" role="status">
          <span>{t("strat.create.done")}</span>
          <span className="mono" title={createdId}>
            {createdId}
          </span>
          <button
            type="button"
            className="btn btn-ghost dismiss"
            aria-label={t("strat.create.dismiss")}
            onClick={() => setCreatedId(null)}
          >
            &times;
          </button>
        </div>
      )}
    </div>
  );
}
