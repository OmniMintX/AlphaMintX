"use client";

// Admin console (platform_admin only): tenant directory with inline create,
// plus the read-only user directory. A non-admin session 403s on the data
// fetches — the ApiError message renders in the error banner instead of the
// tables.

import { useState } from "react";

import { createTenant, fetchTenants, fetchUsers } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner } from "../strategies/ui";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// "2026-07-04T12:00:00Z" -> "2026-07-04 12:00" (UTC, deterministic).
function fmtTime(iso: string): string {
  return iso.slice(0, 16).replace("T", " ");
}

// Short-form id for dense tables; the full id stays in the title tooltip.
function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

export default function AdminPage() {
  const { t } = useI18n();
  return (
    <>
      <div className="page-head">
        <h1 className="page-title">{t("nav.admin")}</h1>
        <p className="page-sub">{t("admin.sub")}</p>
      </div>
      <div className="grid">
        <TenantsCard />
        <UsersCard />
      </div>
    </>
  );
}

function TenantsCard() {
  const { t } = useI18n();
  const tenants = usePoll(fetchTenants);
  const [name, setName] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const create = async () => {
    setPending(true);
    setError(null);
    try {
      await createTenant(name.trim());
      setName("");
      tenants.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("admin.tenants")}</h3>
      {tenants.error && <ErrorBanner message={tenants.error} />}
      {!tenants.data && !tenants.error && (
        <div className="skeleton" style={{ height: 80 }} role="status" aria-busy="true" />
      )}
      {tenants.data && (
        <>
          {error && <ErrorBanner message={error} />}
          <div className="row">
            <input
              className="input"
              style={{ minWidth: "16rem" }}
              placeholder={t("admin.tenantname.placeholder")}
              aria-label={t("admin.tenantname.placeholder")}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <button
              type="button"
              className="btn btn-primary"
              disabled={pending || name.trim() === ""}
              onClick={() => void create()}
            >
              {t("admin.create")}
            </button>
          </div>
          {tenants.data.items.length === 0 ? (
            <div className="empty" role="status">{t("admin.notenants")}</div>
          ) : (
            <table className="tbl" style={{ marginTop: 10 }}>
              <thead>
                <tr>
                  <th scope="col">{t("tbl.id")}</th>
                  <th scope="col">{t("tbl.name")}</th>
                  <th scope="col">{t("tbl.created")}</th>
                </tr>
              </thead>
              <tbody>
                {tenants.data.items.map((tn) => (
                  <tr key={tn.tenant_id}>
                    <td className="mono-cell" title={tn.tenant_id} aria-label={tn.tenant_id}>
                      {shortId(tn.tenant_id)}
                    </td>
                    <td>{tn.name}</td>
                    <td className="mono-cell">{fmtTime(tn.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </div>
  );
}

function UsersCard() {
  const { t } = useI18n();
  const users = usePoll(fetchUsers);

  return (
    <div className="card">
      <h3 className="card-title">{t("admin.users")}</h3>
      {users.error && <ErrorBanner message={users.error} />}
      {!users.data && !users.error && (
        <div className="skeleton" style={{ height: 80 }} role="status" aria-busy="true" />
      )}
      {users.data &&
        (users.data.items.length === 0 ? (
          <div className="empty" role="status">{t("admin.nousers")}</div>
        ) : (
          <table className="tbl">
            <thead>
              <tr>
                <th scope="col">{t("auth.email")}</th>
                <th scope="col">{t("admin.tbl.role")}</th>
                <th scope="col">{t("tbl.tenant")}</th>
                <th scope="col">{t("tbl.created")}</th>
                <th scope="col">{t("admin.tbl.status")}</th>
              </tr>
            </thead>
            <tbody>
              {users.data.items.map((u) => (
                <tr key={u.user_id}>
                  <td>{u.email}</td>
                  <td>
                    <span
                      className={`badge ${u.role === "platform_admin" ? "badge-accent" : "badge-neutral"}`}
                    >
                      {u.role}
                    </span>
                  </td>
                  <td
                    className="mono-cell"
                    title={u.tenant_id ?? undefined}
                    aria-label={u.tenant_id ?? undefined}
                  >
                    {u.tenant_id ? (
                      shortId(u.tenant_id)
                    ) : (
                      <span className="faint">{t("admin.platform")}</span>
                    )}
                  </td>
                  <td className="mono-cell">{fmtTime(u.created_at)}</td>
                  <td>
                    {u.disabled ? (
                      <span className="badge badge-red">{t("admin.disabled")}</span>
                    ) : (
                      <span className="faint small">&mdash;</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ))}
    </div>
  );
}
