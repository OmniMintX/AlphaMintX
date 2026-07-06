"use client";

// Admin console (platform_admin only): tenant directory with inline create,
// API token management (mint/revoke), the read-only user directory, plus
// platform ops — tenant/platform kill switches, backup & restore, and OMS
// reconciliation. A non-admin session 403s on the data fetches — the
// ApiError message renders in the error banner instead of the tables.

import { Fragment, useCallback, useState } from "react";

import {
  ackRestore,
  clearPlatformKill,
  clearTenantKill,
  createTenant,
  fetchBackups,
  fetchOmsReconStatus,
  fetchRestoreStatus,
  fetchTenants,
  fetchTokens,
  fetchUsers,
  killPlatform,
  killTenant,
  mintToken,
  revokeToken,
  runBackup,
  runOmsRecon,
} from "../../src/lib/api/client";
import {
  TENANT_ID_PATTERN,
  type MintTokenRequest,
  type MintedToken,
  type Tenant,
  type TenantsResponse,
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

// Human-readable artifact size for the backups table.
function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

// Observed epoch inputs must be a plain non-negative integer.
function epochOk(s: string): boolean {
  return /^[0-9]+$/.test(s.trim());
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
        <SafetyCard />
        <OpsCard />
        <OmsReconCard />
      </div>
    </>
  );
}

function TenantsCard({ tenants }: { tenants: PollState<TenantsResponse> }) {
  const { t } = useI18n();
  const [tenantId, setTenantId] = useState("");
  const [name, setName] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Owner token plaintext from the create response — shown exactly once
  // (same one-shot rule as minted tokens), dismissed from state only.
  const [ownerToken, setOwnerToken] = useState<{ tenantId: string; token: string } | null>(null);
  const [copied, setCopied] = useState(false);
  // Per-row kill (arm/confirm + flatten) and clear (reason + observed epoch).
  const [killArmed, setKillArmed] = useState<string | null>(null);
  const [killFlatten, setKillFlatten] = useState(false);
  const [clearOpen, setClearOpen] = useState<string | null>(null);
  const [clearReason, setClearReason] = useState("");
  const [clearEpoch, setClearEpoch] = useState("");
  const [acting, setActing] = useState(false);
  const [killDone, setKillDone] = useState<{ id: string; epoch: number } | null>(null);
  const [clearDone, setClearDone] = useState<{ id: string; epoch: number } | null>(null);

  // The regex itself matches "default" — the server reserves it separately,
  // so it is rejected explicitly alongside the pattern.
  const idTrim = tenantId.trim();
  const idValid = TENANT_ID_PATTERN.test(idTrim) && idTrim !== "default";

  const create = async () => {
    setPending(true);
    setError(null);
    try {
      const res = await createTenant(idTrim, name.trim());
      setTenantId("");
      setName("");
      setOwnerToken({ tenantId: res.tenant_id, token: res.owner_token.token });
      setCopied(false);
      tenants.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  const copyToken = async () => {
    if (!ownerToken) return;
    try {
      await navigator.clipboard.writeText(ownerToken.token);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  const armKill = (id: string) => {
    setClearOpen(null);
    setKillArmed((cur) => (cur === id ? null : id));
    setKillFlatten(false);
  };

  const openClear = (id: string) => {
    setKillArmed(null);
    setClearOpen((cur) => (cur === id ? null : id));
    setClearReason("");
    setClearEpoch("");
  };

  const kill = async (id: string) => {
    setActing(true);
    setError(null);
    try {
      const res = await killTenant(id, killFlatten);
      setKillDone({ id, epoch: res.kill_epoch });
      setKillArmed(null);
    } catch (err) {
      setError(errText(err));
    } finally {
      setActing(false);
    }
  };

  const clear = async (id: string) => {
    setActing(true);
    setError(null);
    try {
      const res = await clearTenantKill(id, clearReason.trim(), Number(clearEpoch.trim()));
      setClearDone({ id, epoch: res.cleared_epoch });
      setClearOpen(null);
    } catch (err) {
      setError(errText(err));
    } finally {
      setActing(false);
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
          {killDone && (
            <div className="result-line" role="status">
              <span>
                {t("admin.tenants.kill.done", { id: shortId(killDone.id), epoch: killDone.epoch })}
              </span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("admin.kill.dismiss")}
                onClick={() => setKillDone(null)}
              >
                &times;
              </button>
            </div>
          )}
          {clearDone && (
            <div className="result-line" role="status">
              <span>
                {t("admin.tenants.clear.done", {
                  id: shortId(clearDone.id),
                  epoch: clearDone.epoch,
                })}
              </span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("admin.kill.dismiss")}
                onClick={() => setClearDone(null)}
              >
                &times;
              </button>
            </div>
          )}
          <div className="row">
            <input
              className="input mono"
              style={{ minWidth: "12rem" }}
              placeholder={t("admin.tenantid.placeholder")}
              aria-label={t("admin.tenantid.placeholder")}
              autoComplete="off"
              spellCheck={false}
              value={tenantId}
              onChange={(e) => setTenantId(e.target.value)}
            />
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
              disabled={pending || !idValid || name.trim() === ""}
              onClick={() => void create()}
            >
              {t("admin.create")}
            </button>
          </div>
          <p className="faint small" style={{ margin: "6px 0 0" }}>
            {t("admin.tenantid.hint")}
          </p>
          {ownerToken && (
            <div style={{ marginTop: 10 }}>
              <div className="banner banner-warn" role="alert">
                <span aria-hidden>&#9888;</span>
                <span>{t("admin.tenants.ownertoken.once", { id: ownerToken.tenantId })}</span>
              </div>
              <pre className="codeblock" style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
                {ownerToken.token}
              </pre>
              <div className="row">
                <button type="button" className="btn" onClick={() => void copyToken()}>
                  {t("admin.tokens.copy")}
                </button>
                <span role="status" className="faint small">
                  {copied ? t("admin.tokens.copied") : ""}
                </span>
                <button
                  type="button"
                  className="btn btn-ghost dismiss"
                  aria-label={t("admin.kill.dismiss")}
                  onClick={() => {
                    setOwnerToken(null);
                    setCopied(false);
                  }}
                >
                  &times;
                </button>
              </div>
            </div>
          )}
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
                  <th scope="col" aria-label={t("admin.tenants.actions")} />
                </tr>
              </thead>
              <tbody>
                {tenants.data.items.map((tn) => (
                  <Fragment key={tn.tenant_id}>
                    <tr>
                      <td className="mono-cell" title={tn.tenant_id} aria-label={tn.tenant_id}>
                        {shortId(tn.tenant_id)}
                      </td>
                      <td>{tn.name}</td>
                      <td className="mono-cell">{fmtTime(tn.created_at)}</td>
                      <td>
                        <div className="row" style={{ gap: 6 }}>
                          <button
                            type="button"
                            className="btn btn-danger"
                            disabled={acting}
                            aria-expanded={killArmed === tn.tenant_id}
                            onClick={() => armKill(tn.tenant_id)}
                          >
                            {t("admin.tenants.kill.btn")}
                          </button>
                          <button
                            type="button"
                            className="btn"
                            disabled={acting}
                            aria-expanded={clearOpen === tn.tenant_id}
                            onClick={() => openClear(tn.tenant_id)}
                          >
                            {t("admin.tenants.clear.btn")}
                          </button>
                        </div>
                      </td>
                    </tr>
                    {killArmed === tn.tenant_id && (
                      <tr className="details-row">
                        <td colSpan={4}>
                          <div className="row">
                            <label className="checkbox-row">
                              <input
                                type="checkbox"
                                checked={killFlatten}
                                onChange={(e) => setKillFlatten(e.target.checked)}
                              />
                              {t("admin.kill.flatten")}
                            </label>
                            <button
                              type="button"
                              className="btn btn-danger"
                              disabled={acting}
                              onClick={() => void kill(tn.tenant_id)}
                            >
                              {t("admin.tenants.kill.confirm")}
                            </button>
                            <button
                              type="button"
                              className="btn"
                              disabled={acting}
                              onClick={() => setKillArmed(null)}
                            >
                              {t("admin.kill.cancel")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    )}
                    {clearOpen === tn.tenant_id && (
                      <tr className="details-row">
                        <td colSpan={4}>
                          <div className="row">
                            <label className="field" htmlFor={`clr-reason-${tn.tenant_id}`}>
                              <span className="field-label">{t("admin.kill.reason.label")}</span>
                              <input
                                id={`clr-reason-${tn.tenant_id}`}
                                className="input"
                                style={{ minWidth: "16rem" }}
                                placeholder={t("admin.kill.reason.ph")}
                                value={clearReason}
                                onChange={(e) => setClearReason(e.target.value)}
                              />
                            </label>
                            <label className="field" htmlFor={`clr-epoch-${tn.tenant_id}`}>
                              <span className="field-label">{t("admin.kill.epoch.label")}</span>
                              <input
                                id={`clr-epoch-${tn.tenant_id}`}
                                type="number"
                                className="input mono"
                                value={clearEpoch}
                                onChange={(e) => setClearEpoch(e.target.value)}
                              />
                              <span className="faint small">{t("admin.kill.epoch.hint")}</span>
                            </label>
                            <button
                              type="button"
                              className="btn"
                              disabled={acting || clearReason.trim() === "" || !epochOk(clearEpoch)}
                              onClick={() => void clear(tn.tenant_id)}
                            >
                              {t("admin.tenants.clear.submit")}
                            </button>
                            <button
                              type="button"
                              className="btn btn-ghost"
                              disabled={acting}
                              onClick={() => setClearOpen(null)}
                            >
                              {t("admin.kill.cancel")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
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

// Platform-wide kill switch: collapsed by default behind a disclosure. The
// confirm buttons stay disabled until the operator has typed the exact
// literal; the typed value is passed through to the API unchanged — the
// typing itself is the safeguard, the literal is never hardcoded in the call.
function SafetyCard() {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [ack, setAck] = useState("");
  const [flatten, setFlatten] = useState(false);
  const [clearAck, setClearAck] = useState("");
  const [clearReason, setClearReason] = useState("");
  const [clearEpoch, setClearEpoch] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [killedEpoch, setKilledEpoch] = useState<number | null>(null);
  const [clearedEpoch, setClearedEpoch] = useState<number | null>(null);

  const kill = async () => {
    setPending(true);
    setError(null);
    try {
      const res = await killPlatform(ack, flatten);
      setKilledEpoch(res.kill_epoch);
      setAck("");
      setFlatten(false);
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  const clear = async () => {
    setPending(true);
    setError(null);
    try {
      const res = await clearPlatformKill(clearAck, clearReason.trim(), Number(clearEpoch.trim()));
      setClearedEpoch(res.cleared_epoch);
      setClearAck("");
      setClearReason("");
      setClearEpoch("");
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="card danger-card">
      <h3 className="card-title">{t("admin.safety.title")}</h3>
      <div className="row">
        <button
          type="button"
          className="btn btn-danger"
          aria-expanded={open}
          onClick={() => setOpen((o) => !o)}
        >
          {open ? t("admin.safety.close") : t("admin.safety.open")}
        </button>
      </div>
      {open && (
        <>
          <div className="banner banner-error" role="alert">
            <span aria-hidden>&#9888;</span>
            <span>{t("admin.safety.warn")}</span>
          </div>
          {error && <ErrorBanner message={error} />}
          <div className="row">
            <label className="field" htmlFor="pk-ack">
              <span className="field-label">{t("admin.safety.ack.label")}</span>
              <input
                id="pk-ack"
                className="input ack-input"
                style={{ minWidth: "16rem" }}
                placeholder="KILL-PLATFORM"
                autoComplete="off"
                spellCheck={false}
                value={ack}
                onChange={(e) => setAck(e.target.value)}
              />
            </label>
          </div>
          <label className="checkbox-row" style={{ marginTop: 8 }}>
            <input
              type="checkbox"
              checked={flatten}
              onChange={(e) => setFlatten(e.target.checked)}
            />
            {t("admin.kill.flatten")}
          </label>
          <div className="row" style={{ marginTop: 8 }}>
            <button
              type="button"
              className="btn btn-danger"
              disabled={pending || ack !== "KILL-PLATFORM"}
              onClick={() => void kill()}
            >
              {t("admin.safety.kill.btn")}
            </button>
          </div>
          {killedEpoch !== null && (
            <div className="result-line" role="status">
              <span>{t("admin.safety.killed")}</span>
              <span className="kill-epoch">{killedEpoch}</span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("admin.kill.dismiss")}
                onClick={() => setKilledEpoch(null)}
              >
                &times;
              </button>
            </div>
          )}
          <hr className="divider" />
          <p className="field-label" style={{ margin: "0 0 6px" }}>
            {t("admin.safety.clear.title")}
          </p>
          <div className="row">
            <label className="field" htmlFor="pk-clear-ack">
              <span className="field-label">{t("admin.safety.clear.ack.label")}</span>
              <input
                id="pk-clear-ack"
                className="input ack-input"
                style={{ minWidth: "16rem" }}
                placeholder="CLEAR-PLATFORM"
                autoComplete="off"
                spellCheck={false}
                value={clearAck}
                onChange={(e) => setClearAck(e.target.value)}
              />
            </label>
            <label className="field" htmlFor="pk-clear-reason">
              <span className="field-label">{t("admin.kill.reason.label")}</span>
              <input
                id="pk-clear-reason"
                className="input"
                style={{ minWidth: "16rem" }}
                placeholder={t("admin.kill.reason.ph")}
                value={clearReason}
                onChange={(e) => setClearReason(e.target.value)}
              />
            </label>
            <label className="field" htmlFor="pk-clear-epoch">
              <span className="field-label">{t("admin.kill.epoch.label")}</span>
              <input
                id="pk-clear-epoch"
                type="number"
                className="input mono"
                value={clearEpoch}
                onChange={(e) => setClearEpoch(e.target.value)}
              />
              <span className="faint small">{t("admin.kill.epoch.hint")}</span>
            </label>
          </div>
          <div className="row" style={{ marginTop: 8 }}>
            <button
              type="button"
              className="btn"
              disabled={
                pending ||
                clearAck !== "CLEAR-PLATFORM" ||
                clearReason.trim() === "" ||
                !epochOk(clearEpoch)
              }
              onClick={() => void clear()}
            >
              {t("admin.safety.clear.btn")}
            </button>
          </div>
          {clearedEpoch !== null && (
            <div className="result-line" role="status">
              <span>{t("admin.safety.cleared")}</span>
              <span className="kill-epoch">{clearedEpoch}</span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("admin.kill.dismiss")}
                onClick={() => setClearedEpoch(null)}
              >
                &times;
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// Backup & restore: the restore gate poll drives a red banner + arm/confirm
// acknowledge; the backups list 404s when CONTROLPLANE_BACKUP_DIR is unset on
// this deployment — that renders as a neutral hint, not an error.
function OpsCard() {
  const { t } = useI18n();
  const restore = usePoll(fetchRestoreStatus);
  const backups = usePoll(fetchBackups);
  const [ackArmed, setAckArmed] = useState(false);
  const [ackPending, setAckPending] = useState(false);
  const [runPending, setRunPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<Awaited<ReturnType<typeof runBackup>> | null>(null);

  const acknowledge = async () => {
    setAckPending(true);
    setError(null);
    try {
      await ackRestore();
      restore.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setAckPending(false);
      setAckArmed(false);
    }
  };

  const run = async () => {
    setRunPending(true);
    setError(null);
    try {
      const res = await runBackup();
      setResult(res);
      backups.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setRunPending(false);
    }
  };

  const backupsUnconfigured = backups.errorStatus === 404;

  return (
    <div className="card">
      <h3 className="card-title">{t("admin.ops.title")}</h3>
      {error && <ErrorBanner message={error} />}
      {restore.error && <ErrorBanner message={restore.error} />}
      {!restore.data && !restore.error && (
        <div className="skeleton" style={{ height: 24 }} role="status" aria-busy="true" />
      )}
      {restore.data &&
        (restore.data.engaged ? (
          <>
            <div className="banner banner-error" role="alert">
              <span aria-hidden>&#9888;</span>
              <span>{t("admin.ops.restore.engaged")}</span>
            </div>
            <div className="row">
              <button
                type="button"
                className="btn btn-danger"
                disabled={ackPending}
                onClick={() => (ackArmed ? void acknowledge() : setAckArmed(true))}
              >
                {ackArmed ? t("admin.ops.restore.ack.confirm") : t("admin.ops.restore.ack")}
              </button>
              {ackArmed && (
                <button
                  type="button"
                  className="btn btn-ghost"
                  disabled={ackPending}
                  onClick={() => setAckArmed(false)}
                >
                  {t("admin.kill.cancel")}
                </button>
              )}
            </div>
          </>
        ) : (
          <p className="faint small">{t("admin.ops.restore.ok")}</p>
        ))}
      <hr className="divider" />
      {backupsUnconfigured ? (
        <p className="faint small">{t("admin.ops.backups.notconfigured")}</p>
      ) : (
        <>
          {backups.error && <ErrorBanner message={backups.error} />}
          {!backups.data && !backups.error && (
            <div className="skeleton" style={{ height: 80 }} role="status" aria-busy="true" />
          )}
          {backups.data && (
            <>
              <div className="row">
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={runPending}
                  onClick={() => void run()}
                >
                  {t("admin.ops.backups.run")}
                </button>
              </div>
              {result && (
                <div className="result-line" role="status">
                  <span className="mono">{result.artifact}</span>
                  <span className="mono" title={result.sha256}>
                    {result.sha256.slice(0, 12)}…
                  </span>
                  <span className={`badge ${result.verified ? "badge-green" : "badge-yellow"}`}>
                    {result.verified ? t("admin.ops.verified") : t("admin.ops.unverified")}
                  </span>
                  <button
                    type="button"
                    className="btn btn-ghost dismiss"
                    aria-label={t("admin.kill.dismiss")}
                    onClick={() => setResult(null)}
                  >
                    &times;
                  </button>
                </div>
              )}
              {backups.data.items.length === 0 ? (
                <div className="empty" role="status">
                  {t("admin.ops.backups.none")}
                </div>
              ) : (
                <table className="tbl" style={{ marginTop: 10 }}>
                  <thead>
                    <tr>
                      <th scope="col">{t("admin.ops.tbl.artifact")}</th>
                      <th scope="col">{t("admin.ops.tbl.size")}</th>
                      <th scope="col">{t("admin.ops.tbl.modified")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {backups.data.items.map((b) => (
                      <tr key={b.artifact}>
                        <td className="mono-cell">{b.artifact}</td>
                        <td className="mono-cell">{fmtBytes(b.bytes)}</td>
                        <td className="mono-cell">{fmtTime(b.modified_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}

// Badge color for the last reconcile run: completed=green, failed=red,
// running/incomplete=yellow.
function reconRunBadge(status: string): string {
  if (status === "completed") return "badge-green";
  if (status === "failed") return "badge-red";
  return "badge-yellow";
}

// OMS reconciliation (live-OMS deployments only): status poll plus a manual
// R1-R7 run behind an arm/confirm. In paper mode the route is unregistered —
// the GET 404s and renders as a neutral hint, not an error.
function OmsReconCard() {
  const { t } = useI18n();
  const status = usePoll(fetchOmsReconStatus);
  const [armed, setArmed] = useState(false);
  const [acceptReset, setAcceptReset] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showCounters, setShowCounters] = useState(false);

  const run = async () => {
    setPending(true);
    setError(null);
    try {
      await runOmsRecon(acceptReset);
      status.refresh();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
      setArmed(false);
      setAcceptReset(false);
    }
  };

  const notWired = status.errorStatus === 404;

  return (
    <div className="card">
      <h3 className="card-title">{t("admin.recon.title")}</h3>
      {notWired ? (
        <p className="faint small">{t("admin.recon.notwired")}</p>
      ) : (
        <>
          {status.error && <ErrorBanner message={status.error} />}
          {error && <ErrorBanner message={error} />}
          {!status.data && !status.error && (
            <div className="skeleton" style={{ height: 80 }} role="status" aria-busy="true" />
          )}
          {status.data && (
            <>
              <div className="row">
                <span className="mono">{status.data.mode}</span>
                <span className="mono">{status.data.venue_env}</span>
                {status.data.reconciled ? (
                  <span className="badge badge-green">{t("admin.recon.reconciled")}</span>
                ) : (
                  <span className="badge badge-yellow">{t("admin.recon.notreconciled")}</span>
                )}
              </div>
              <p className="small" style={{ margin: "8px 0 0" }}>
                {t("admin.recon.pending", { count: status.data.pending_intents })}
              </p>
              {status.data.venue_epoch !== undefined && (
                <p className="small" style={{ margin: "4px 0 0" }}>
                  {t("admin.recon.venueepoch", { epoch: status.data.venue_epoch })}
                </p>
              )}
              <p className="field-label" style={{ margin: "10px 0 6px" }}>
                {t("admin.recon.lastrun")}
              </p>
              {status.data.last_run ? (
                <>
                  <div className="row">
                    <span className={`badge ${reconRunBadge(status.data.last_run.status)}`}>
                      {status.data.last_run.status}
                    </span>
                    {status.data.last_run.completed_at && (
                      <span className="mono small">
                        {fmtTime(status.data.last_run.completed_at)}
                      </span>
                    )}
                  </div>
                  {status.data.last_run.counters && (
                    <>
                      <div className="row" style={{ marginTop: 8 }}>
                        <button
                          type="button"
                          className="btn btn-ghost"
                          aria-expanded={showCounters}
                          onClick={() => setShowCounters((s) => !s)}
                        >
                          {t("admin.recon.counters")}
                        </button>
                      </div>
                      {showCounters && (
                        <pre className="codeblock">
                          {JSON.stringify(status.data.last_run.counters, null, 2)}
                        </pre>
                      )}
                    </>
                  )}
                </>
              ) : (
                <p className="faint small">{t("admin.recon.norun")}</p>
              )}
              {status.data.watermarks && status.data.watermarks.length > 0 && (
                <table className="tbl" style={{ marginTop: 10 }}>
                  <thead>
                    <tr>
                      <th scope="col">{t("admin.recon.tbl.symbol")}</th>
                      <th scope="col">{t("admin.recon.tbl.venueepoch")}</th>
                      <th scope="col">{t("admin.recon.tbl.tradeid")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {status.data.watermarks.map((w) => (
                      <tr key={w.symbol}>
                        <td className="mono-cell">{w.symbol}</td>
                        <td className="mono-cell">{w.venue_epoch}</td>
                        <td className="mono-cell">{w.exchange_trade_id}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              <div className="row" style={{ marginTop: 10 }}>
                <button
                  type="button"
                  className={`btn${armed ? " btn-danger" : ""}`}
                  disabled={pending}
                  aria-expanded={armed}
                  onClick={() => (armed ? void run() : setArmed(true))}
                >
                  {armed ? t("admin.recon.run.confirm") : t("admin.recon.run")}
                </button>
                {armed && (
                  <button
                    type="button"
                    className="btn btn-ghost"
                    disabled={pending}
                    onClick={() => {
                      setArmed(false);
                      setAcceptReset(false);
                    }}
                  >
                    {t("admin.kill.cancel")}
                  </button>
                )}
              </div>
              {armed && (
                <label className="checkbox-row" style={{ marginTop: 8 }}>
                  <input
                    type="checkbox"
                    checked={acceptReset}
                    onChange={(e) => setAcceptReset(e.target.checked)}
                  />
                  {t("admin.recon.acceptreset")}
                </label>
              )}
            </>
          )}
        </>
      )}
    </div>
  );
}
