"use client";

// Admin console (platform_admin only): tenant directory with inline create,
// API token management (mint/revoke), plus the read-only user directory. A
// non-admin session 403s on the data fetches — the ApiError message renders
// in the error banner instead of the tables.

import { useCallback, useState } from "react";

import {
  createTenant,
  fetchTenants,
  fetchTokens,
  fetchUsers,
  mintToken,
  revokeToken,
} from "../../src/lib/api/client";
import type {
  MintTokenRequest,
  MintedToken,
  Tenant,
  TenantsResponse,
} from "../../src/lib/api/schema";
import { usePoll, type PollState } from "../../src/lib/api/usePoll";
import { useI18n } from "../../src/lib/i18n";
import { ErrorBanner, Pager } from "../strategies/ui";

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
  const tenants = usePoll(fetchTenants);
  return (
    <>
      <div className="page-head">
        <h1 className="page-title">{t("nav.admin")}</h1>
        <p className="page-sub">{t("admin.sub")}</p>
      </div>
      <div className="grid">
        <TenantsCard tenants={tenants} />
        <TokensCard tenants={tenants.data?.items ?? []} />
        <UsersCard />
      </div>
    </>
  );
}

function TenantsCard({ tenants }: { tenants: PollState<TenantsResponse> }) {
  const { t } = useI18n();
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
            <div className="empty" role="status">
              {t("admin.notenants")}
              <div className="empty-hint">{t("admin.notenants.hint")}</div>
            </div>
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

// User tokens carry a role (viewer|trader|admin|owner) and no strategy_id;
// agent tokens carry a strategy_id and no role. The plaintext token is
// returned exactly once by the mint call and is never retrievable again —
// it renders once below the form and is cleared on reopen/page change.
const USER_ROLES = ["viewer", "trader", "admin", "owner"] as const;

function TokensCard({ tenants }: { tenants: Tenant[] }) {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const load = useCallback(() => fetchTokens(page), [page]);
  const tokens = usePoll(load);

  const [open, setOpen] = useState(false);
  const [tenantId, setTenantId] = useState("");
  const [principal, setPrincipal] = useState<"user" | "agent">("user");
  const [role, setRole] = useState("");
  const [strategyId, setStrategyId] = useState("");
  const [label, setLabel] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [minted, setMinted] = useState<MintedToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [armed, setArmed] = useState<string | null>(null);
  const [revoking, setRevoking] = useState<string | null>(null);

  const labelError = submitted && label.trim() === "";
  const tenantError = submitted && tenantId === "";
  const roleError = submitted && principal === "user" && role === "";
  const strategyError = submitted && principal === "agent" && strategyId.trim() === "";
  const valid =
    label.trim() !== "" &&
    tenantId !== "" &&
    (principal === "user" ? role !== "" : strategyId.trim() !== "");

  // Opening (or reopening) the form discards any previously shown plaintext.
  const toggle = () => {
    setOpen((o) => !o);
    setMinted(null);
    setCopied(false);
    setError(null);
    setSubmitted(false);
  };

  const mint = async () => {
    if (!valid) {
      setSubmitted(true);
      return;
    }
    setPending(true);
    setError(null);
    try {
      const req: MintTokenRequest =
        principal === "user"
          ? { tenant_id: tenantId, principal, role, label: label.trim() }
          : { tenant_id: tenantId, principal, strategy_id: strategyId.trim(), label: label.trim() };
      const res = await mintToken(req);
      setMinted(res);
      setCopied(false);
      setOpen(false);
      setLabel("");
      setStrategyId("");
      setSubmitted(false);
      tokens.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  const revoke = async (tokenId: string) => {
    setRevoking(tokenId);
    setError(null);
    try {
      await revokeToken(tokenId);
      tokens.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setRevoking(null);
      setArmed(null);
    }
  };

  // Two-click arm/confirm guard for the destructive revoke (no window.confirm
  // precedent in this codebase — mirrors the lifecycle arm pattern in ops.tsx).
  const onRevoke = (tokenId: string) => {
    if (armed !== tokenId) {
      setArmed(tokenId);
      return;
    }
    void revoke(tokenId);
  };

  const tenantName = (id: string) => tenants.find((tn) => tn.tenant_id === id)?.name;

  const copy = async () => {
    if (!minted) return;
    try {
      await navigator.clipboard.writeText(minted.token);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("admin.tokens.title")}</h3>
      {tokens.error && <ErrorBanner message={tokens.error} />}
      {!tokens.data && !tokens.error && (
        <div className="skeleton" style={{ height: 80 }} role="status" aria-busy="true" />
      )}
      {tokens.data && (
        <>
          {error && <ErrorBanner message={error} />}
          <div className="row">
            <button type="button" className="btn" onClick={toggle} aria-expanded={open}>
              {open ? t("admin.tokens.cancel") : t("admin.tokens.mint")}
            </button>
          </div>
          {open && (
            <>
              <div className="row" style={{ marginTop: 10 }}>
                <label className="field" htmlFor="tok-tenant">
                  <span className="field-label">{t("tbl.tenant")}</span>
                  <select
                    id="tok-tenant"
                    className="select"
                    value={tenantId}
                    onChange={(e) => setTenantId(e.target.value)}
                    aria-invalid={tenantError || undefined}
                    aria-describedby={tenantError ? "tok-tenant-error" : undefined}
                  >
                    <option value="">{t("admin.tokens.tenant.ph")}</option>
                    {tenants.map((tn) => (
                      <option key={tn.tenant_id} value={tn.tenant_id}>
                        {tn.name}
                      </option>
                    ))}
                  </select>
                  {tenantError && (
                    <span className="field-error" id="tok-tenant-error">
                      {t("admin.tokens.err.tenant")}
                    </span>
                  )}
                </label>
                <label className="field" htmlFor="tok-principal">
                  <span className="field-label">{t("admin.tokens.principal")}</span>
                  <select
                    id="tok-principal"
                    className="select"
                    value={principal}
                    onChange={(e) => setPrincipal(e.target.value as "user" | "agent")}
                  >
                    <option value="user">user</option>
                    <option value="agent">agent</option>
                  </select>
                </label>
                {principal === "user" ? (
                  <label className="field" htmlFor="tok-role">
                    <span className="field-label">{t("admin.tbl.role")}</span>
                    <select
                      id="tok-role"
                      className="select"
                      value={role}
                      onChange={(e) => setRole(e.target.value)}
                      aria-invalid={roleError || undefined}
                      aria-describedby={roleError ? "tok-role-error" : undefined}
                    >
                      <option value="">{t("admin.tokens.role.ph")}</option>
                      {USER_ROLES.map((r) => (
                        <option key={r} value={r}>
                          {r}
                        </option>
                      ))}
                    </select>
                    {roleError && (
                      <span className="field-error" id="tok-role-error">
                        {t("admin.tokens.err.role")}
                      </span>
                    )}
                  </label>
                ) : (
                  <label className="field" htmlFor="tok-strategy">
                    <span className="field-label">{t("admin.tokens.strategy")}</span>
                    <input
                      id="tok-strategy"
                      className={`input mono${strategyError ? " error" : ""}`}
                      style={{ minWidth: "16rem" }}
                      value={strategyId}
                      onChange={(e) => setStrategyId(e.target.value)}
                      aria-invalid={strategyError || undefined}
                      aria-describedby={strategyError ? "tok-strategy-error" : undefined}
                    />
                    {strategyError && (
                      <span className="field-error" id="tok-strategy-error">
                        {t("admin.tokens.err.strategy")}
                      </span>
                    )}
                  </label>
                )}
                <label className="field" htmlFor="tok-label">
                  <span className="field-label">{t("admin.tokens.label")}</span>
                  <input
                    id="tok-label"
                    className={`input${labelError ? " error" : ""}`}
                    style={{ minWidth: "16rem" }}
                    placeholder={t("admin.tokens.label.ph")}
                    value={label}
                    onChange={(e) => setLabel(e.target.value)}
                    aria-invalid={labelError || undefined}
                    aria-describedby={labelError ? "tok-label-error" : undefined}
                  />
                  {labelError && (
                    <span className="field-error" id="tok-label-error">
                      {t("admin.tokens.err.label")}
                    </span>
                  )}
                </label>
              </div>
              <div className="row" style={{ marginTop: 10 }}>
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={pending}
                  onClick={() => void mint()}
                >
                  {t("admin.tokens.mint")}
                </button>
              </div>
            </>
          )}
          {minted && (
            <div style={{ marginTop: 10 }}>
              <div className="banner banner-warn" role="alert">
                <span aria-hidden>&#9888;</span>
                <span>{t("admin.tokens.warn.once")}</span>
              </div>
              <pre className="codeblock" style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                {minted.token}
              </pre>
              <div className="row">
                <button type="button" className="btn" onClick={() => void copy()}>
                  {t("admin.tokens.copy")}
                </button>
                <span role="status" className="faint small">
                  {copied ? t("admin.tokens.copied") : ""}
                </span>
              </div>
            </div>
          )}
          {tokens.data.items.length === 0 ? (
            <div className="empty" role="status">
              {t("admin.tokens.none")}
              <div className="empty-hint">{t("admin.tokens.none.hint")}</div>
            </div>
          ) : (
            <>
              <table className="tbl" style={{ marginTop: 10 }}>
                <thead>
                  <tr>
                    <th scope="col">{t("admin.tokens.label")}</th>
                    <th scope="col">{t("tbl.tenant")}</th>
                    <th scope="col">{t("admin.tokens.principal")}</th>
                    <th scope="col">{t("admin.tokens.tbl.rolestrategy")}</th>
                    <th scope="col">{t("tbl.created")}</th>
                    <th scope="col">{t("admin.tbl.status")}</th>
                    <th scope="col" aria-label={t("admin.tokens.revoke")} />
                  </tr>
                </thead>
                <tbody>
                  {tokens.data.items.map((tok) => (
                    <tr key={tok.token_id}>
                      <td>{tok.label}</td>
                      <td className="mono-cell" title={tok.tenant_id} aria-label={tok.tenant_id}>
                        {tenantName(tok.tenant_id) ?? shortId(tok.tenant_id)}
                      </td>
                      <td>
                        <span className="badge badge-neutral">{tok.principal}</span>
                      </td>
                      <td>
                        {tok.strategy_id ? (
                          <span className="mono" title={tok.strategy_id}>
                            {shortId(tok.strategy_id)}
                          </span>
                        ) : (
                          (tok.role ?? <span className="faint small">&mdash;</span>)
                        )}
                      </td>
                      <td className="mono-cell">{fmtTime(tok.created_at)}</td>
                      <td>
                        {tok.revoked_at ? (
                          <span className="badge badge-red">{t("admin.tokens.revoked")}</span>
                        ) : (
                          <span className="badge badge-green">{t("admin.tokens.active")}</span>
                        )}
                      </td>
                      <td>
                        <button
                          type="button"
                          className="btn btn-danger"
                          disabled={tok.revoked_at !== null || revoking !== null}
                          onClick={() => onRevoke(tok.token_id)}
                        >
                          {armed === tok.token_id
                            ? t("admin.tokens.revoke.confirm")
                            : t("admin.tokens.revoke")}
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <Pager
                page={tokens.data.page}
                total={tokens.data.total}
                limit={tokens.data.limit}
                onPage={(p) => {
                  setPage(p);
                  setMinted(null);
                  setCopied(false);
                  setArmed(null);
                }}
              />
            </>
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
          <div className="empty" role="status">
            {t("admin.nousers")}
            <div className="empty-hint">{t("admin.nousers.hint")}</div>
          </div>
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
