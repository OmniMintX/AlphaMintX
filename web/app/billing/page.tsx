"use client";

// Billing surface: monthly invoices and LLM-cost reconciliation runs, each a
// paginated {items,total,page,limit} envelope polled on the shared interval
// with independent page state. The endpoints are tenant admin/owner (own
// tenant) + platform_admin — a viewer/trader session 403s on the data fetch
// and the denied message renders in place of the section content (same
// treatment as the alerts feed). Every *_usd field is an ADR-0003 decimal
// string rendered verbatim — never parsed to float. Platform admins also get
// the billing-ops card (close period / run reconcile) above the sections.

import Link from "next/link";
import { Fragment, useCallback, useEffect, useState } from "react";

import {
  closeBillingPeriod,
  fetchInvoiceDetail,
  fetchInvoices,
  fetchMe,
  fetchReconciliationDetail,
  fetchReconciliations,
  fetchTenants,
  runBillingReconcile,
} from "../../src/lib/api/client";
import type {
  Discrepancy,
  Invoice,
  InvoiceLine,
  ReconciliationRun,
  SessionUser,
} from "../../src/lib/api/schema";
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

// status is an OPEN set — tone by substring heuristic, never an exhaustive
// switch (the alerts kindTone precedent).
function statusTone(status: string): string {
  const s = status.toLowerCase();
  if (s.includes("discrepan") || s.includes("mismatch") || s.includes("error")) return "badge-red";
  if (s.includes("clean") || s.includes("ok")) return "badge-green";
  return "badge-neutral";
}

// Discrepancy class is likewise an open set.
function classTone(cls: string): string {
  const c = cls.toLowerCase();
  if (c.includes("mismatch") || c.includes("error")) return "badge-red";
  if (c.includes("orphan") || c.includes("unattributed")) return "badge-yellow";
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

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function Dash() {
  return <span className="faint small">&mdash;</span>;
}

function LoadingSkeleton() {
  return (
    <div className="grid" role="status" aria-busy="true">
      <div className="skeleton" style={{ height: 36 }} />
      <div className="skeleton" style={{ height: 36 }} />
      <div className="skeleton" style={{ height: 36 }} />
    </div>
  );
}

// One-shot detail fetch on expand (not polled): tracks loading/error locally
// and refetches only when the id changes.
function useDetail<T>(load: () => Promise<T>): { detail: T | null; error: string | null } {
  const [detail, setDetail] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(null);
    load()
      .then((d) => {
        if (!cancelled) setDetail(d);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errText(err));
      });
    return () => {
      cancelled = true;
    };
  }, [load]);
  return { detail, error };
}

// Session identity for the ops-card gate; a failed fetch leaves it null
// (identity is cosmetic — data fetches own the 401 redirect). Mirrors the
// nav.tsx hook.
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

export default function BillingPage() {
  const { t } = useI18n();
  const user = useSessionUser();
  // Bumped after a successful close/reconcile: each section's load callback
  // depends on its key, so a bump gives usePoll a new identity and refetches.
  const [invoicesKey, setInvoicesKey] = useState(0);
  const [reconsKey, setReconsKey] = useState(0);
  return (
    <>
      <header className="page-head">
        <h1 className="page-title">{t("billing.title")}</h1>
        <p className="page-sub">{t("billing.sub")}</p>
      </header>
      {user?.role === "platform_admin" && (
        <BillingOpsCard
          onClosed={() => setInvoicesKey((k) => k + 1)}
          onReconciled={() => setReconsKey((k) => k + 1)}
        />
      )}
      <InvoicesSection refreshKey={invoicesKey} />
      <ReconsSection refreshKey={reconsKey} />
    </>
  );
}

// ---- billing ops (platform_admin only) ---------------------------------------

// Normative YYYY-MM UTC month shape (mirrors billingPeriod in schema.ts).
const PERIOD_PATTERN = /^[0-9]{4}-(0[1-9]|1[0-2])$/;

// Close-period / run-reconcile controls over a shared (tenant, period) pair.
// Backend errors (400 INVALID_PERIOD — including the running month, 404
// UNKNOWN_TENANT, 409 PERIOD_CLOSED) surface verbatim in the card banner.
function BillingOpsCard({
  onClosed,
  onReconciled,
}: {
  onClosed: () => void;
  onReconciled: () => void;
}) {
  const { t } = useI18n();
  const tenants = usePoll(fetchTenants);
  const [tenantId, setTenantId] = useState("");
  const [period, setPeriod] = useState("");
  const [closePending, setClosePending] = useState(false);
  const [reconPending, setReconPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [closeResult, setCloseResult] = useState<Awaited<
    ReturnType<typeof closeBillingPeriod>
  > | null>(null);
  const [reconResult, setReconResult] = useState<Awaited<
    ReturnType<typeof runBillingReconcile>
  > | null>(null);

  const pending = closePending || reconPending;
  const ready = tenantId !== "" && PERIOD_PATTERN.test(period);

  const close = async () => {
    setClosePending(true);
    setError(null);
    try {
      const res = await closeBillingPeriod(tenantId, period);
      setCloseResult(res);
      onClosed();
    } catch (err) {
      setError(errText(err));
    } finally {
      setClosePending(false);
    }
  };

  const reconcile = async () => {
    setReconPending(true);
    setError(null);
    try {
      const res = await runBillingReconcile(tenantId, period);
      setReconResult(res);
      onReconciled();
    } catch (err) {
      setError(errText(err));
    } finally {
      setReconPending(false);
    }
  };

  // Defense in depth: the card is already gated on the session role, but a
  // 403 on the tenant list (tenant principal) hides it silently too.
  if (tenants.errorStatus === 403) return null;

  return (
    <section className="section">
      <h2 className="section-title">{t("billing.ops.title")}</h2>
      <div className="card">
        <div className="grid">
          {tenants.error && <ErrorBanner message={tenants.error} />}
          {error && <ErrorBanner message={error} />}
          <div className="row">
            <label className="field" htmlFor="ops-tenant">
              <span className="field-label">{t("billing.tbl.tenant")}</span>
              <select
                id="ops-tenant"
                className="select mono"
                value={tenantId}
                onChange={(e) => setTenantId(e.target.value)}
              >
                <option value="">{t("billing.ops.tenant.ph")}</option>
                {(tenants.data?.items ?? []).map((tn) => (
                  <option key={tn.tenant_id} value={tn.tenant_id}>
                    {tn.tenant_id}
                  </option>
                ))}
              </select>
            </label>
            <label className="field" htmlFor="ops-period">
              <span className="field-label">{t("billing.tbl.period")}</span>
              <input
                id="ops-period"
                className="input mono"
                type="month"
                value={period}
                onChange={(e) => setPeriod(e.target.value)}
              />
            </label>
          </div>
          <span className="muted small">{t("billing.ops.hint")}</span>
          <div className="row">
            <button
              type="button"
              className="btn btn-primary"
              disabled={pending || !ready}
              onClick={() => void close()}
            >
              {closePending ? t("billing.ops.close.pending") : t("billing.ops.close")}
            </button>
            <button
              type="button"
              className="btn"
              disabled={pending || !ready}
              onClick={() => void reconcile()}
            >
              {reconPending ? t("billing.ops.recon.pending") : t("billing.ops.recon")}
            </button>
          </div>
          {closeResult && (
            <div className="result-line" role="status">
              <span>{t("billing.ops.close.done")}</span>
              <span className="mono">{closeResult.invoice.period}</span>
              <span className="muted small">
                {t("billing.tbl.total")}: <span className="mono">{closeResult.invoice.total_usd}</span>
              </span>
              <span className="muted small">
                {t("billing.tbl.lines")}: <span className="mono">{closeResult.invoice.line_count}</span>
              </span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("billing.ops.dismiss")}
                onClick={() => setCloseResult(null)}
              >
                &times;
              </button>
            </div>
          )}
          {reconResult && (
            <div className="result-line" role="status">
              <span>{t("billing.ops.recon.done")}</span>
              <span className={`badge ${statusTone(reconResult.run.status)}`}>
                {reconResult.run.status}
              </span>
              <span className="muted small">
                {t("billing.tbl.counts")}:{" "}
                <span className="mono">
                  {reconResult.run.matched_count} / {reconResult.run.discrepancy_count}
                </span>
              </span>
              <button
                type="button"
                className="btn btn-ghost dismiss"
                aria-label={t("billing.ops.dismiss")}
                onClick={() => setReconResult(null)}
              >
                &times;
              </button>
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

// ---- invoices ---------------------------------------------------------------

function InvoicesSection({ refreshKey }: { refreshKey: number }) {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());

  // refreshKey is an extra dep on purpose: bumping it refetches after a close.
  const load = useCallback(() => fetchInvoices(page, 20), [page, refreshKey]);
  const { data, error, errorStatus } = usePoll(load);
  const denied = errorStatus === 403;

  const toggle = (invoiceId: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(invoiceId)) next.delete(invoiceId);
      else next.add(invoiceId);
      return next;
    });

  return (
    <section className="section">
      <h2 className="section-title">{t("billing.invoices")}</h2>
      {error && <ErrorBanner message={denied ? t("billing.denied") : error} />}
      {!data && !error && <LoadingSkeleton />}
      {data && !denied && (
        <>
          <div className="table-wrap">
            {data.items.length === 0 ? (
              <div className="empty" role="status">
                {t("billing.empty.invoices")}
                <div className="empty-hint">{t("billing.empty.invoices.hint")}</div>
              </div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr>
                    <th scope="col">{t("billing.tbl.period")}</th>
                    <th scope="col">{t("billing.tbl.tenant")}</th>
                    <th scope="col">{t("billing.tbl.total")}</th>
                    <th scope="col">{t("billing.tbl.lines")}</th>
                    <th scope="col">{t("billing.tbl.generated")}</th>
                    <th scope="col">{t("billing.tbl.details")}</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((inv: Invoice) => {
                    const expanded = open.has(inv.invoice_id);
                    return (
                      <Fragment key={inv.invoice_id}>
                        <tr>
                          <td className="mono-cell">{inv.period}</td>
                          <td className="mono-cell" title={inv.tenant_id} aria-label={inv.tenant_id}>
                            {shortId(inv.tenant_id)}
                          </td>
                          <td className="num">{inv.total_usd}</td>
                          <td className="num">{inv.line_count}</td>
                          <td className="mono-cell">{fmtTime(inv.generated_at)}</td>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost small"
                              aria-expanded={expanded}
                              aria-controls={`invoice-details-${inv.invoice_id}`}
                              onClick={() => toggle(inv.invoice_id)}
                            >
                              {t("billing.tbl.details")}
                            </button>
                          </td>
                        </tr>
                        {expanded && (
                          <tr className="details-row" id={`invoice-details-${inv.invoice_id}`}>
                            <td colSpan={6}>
                              <InvoiceDetail invoiceId={inv.invoice_id} />
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
    </section>
  );
}

function InvoiceDetail({ invoiceId }: { invoiceId: string }) {
  const { t } = useI18n();
  const load = useCallback(() => fetchInvoiceDetail(invoiceId), [invoiceId]);
  const { detail, error } = useDetail(load);

  if (error) return <ErrorBanner message={error} />;
  if (!detail) {
    return <div className="skeleton" style={{ height: 36 }} role="status" aria-busy="true" />;
  }
  if (detail.lines.length === 0) {
    return (
      <div className="empty" role="status">
        {t("billing.lines.empty")}
      </div>
    );
  }
  return (
    <table className="tbl">
      <thead>
        <tr>
          <th scope="col">{t("billing.tbl.strategy")}</th>
          <th scope="col">{t("billing.tbl.model")}</th>
          <th scope="col">{t("billing.tbl.entry")}</th>
          <th scope="col">{t("billing.tbl.origperiod")}</th>
          <th scope="col">{t("billing.tbl.intok")}</th>
          <th scope="col">{t("billing.tbl.outtok")}</th>
          <th scope="col">{t("billing.tbl.amount")}</th>
        </tr>
      </thead>
      <tbody>
        {detail.lines.map((line: InvoiceLine) => (
          <tr key={line.line_id}>
            <td className="mono-cell" title={line.strategy_id} aria-label={line.strategy_id}>
              <Link href={`/strategies/${line.strategy_id}`}>{shortId(line.strategy_id)}</Link>
            </td>
            <td className="mono-cell">{line.model}</td>
            <td>
              <span className="badge badge-neutral">{line.entry_type}</span>
            </td>
            <td className="mono-cell">{line.original_period ?? <Dash />}</td>
            <td className="num">{line.input_tokens}</td>
            <td className="num">{line.output_tokens}</td>
            <td className="num">{line.amount_usd}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

// ---- reconciliations --------------------------------------------------------

function ReconsSection({ refreshKey }: { refreshKey: number }) {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());

  // refreshKey is an extra dep on purpose: bumping it refetches after a run.
  const load = useCallback(() => fetchReconciliations(page, 20), [page, refreshKey]);
  const { data, error, errorStatus } = usePoll(load);
  const denied = errorStatus === 403;

  const toggle = (reconId: string) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (next.has(reconId)) next.delete(reconId);
      else next.add(reconId);
      return next;
    });

  return (
    <section className="section">
      <h2 className="section-title">{t("billing.recons")}</h2>
      {error && <ErrorBanner message={denied ? t("billing.denied") : error} />}
      {!data && !error && <LoadingSkeleton />}
      {data && !denied && (
        <>
          <div className="table-wrap">
            {data.items.length === 0 ? (
              <div className="empty" role="status">
                {t("billing.empty.recons")}
                <div className="empty-hint">{t("billing.empty.recons.hint")}</div>
              </div>
            ) : (
              <table className="tbl">
                <thead>
                  <tr>
                    <th scope="col">{t("billing.tbl.period")}</th>
                    <th scope="col">{t("billing.tbl.status")}</th>
                    <th scope="col">{t("billing.tbl.counts")}</th>
                    <th scope="col">{t("billing.tbl.totals")}</th>
                    <th scope="col">{t("billing.tbl.runat")}</th>
                    <th scope="col">{t("billing.tbl.details")}</th>
                  </tr>
                </thead>
                <tbody>
                  {data.items.map((run: ReconciliationRun) => {
                    const expanded = open.has(run.recon_id);
                    return (
                      <Fragment key={run.recon_id}>
                        <tr>
                          <td className="mono-cell">{run.period}</td>
                          <td>
                            <span className={`badge ${statusTone(run.status)}`}>{run.status}</span>
                          </td>
                          <td className="num">
                            {run.matched_count} / {run.discrepancy_count}
                          </td>
                          <td className="num">
                            {run.invoice_total_usd} / {run.matched_client_cost_usd}
                          </td>
                          <td className="mono-cell">{fmtTime(run.run_at)}</td>
                          <td>
                            <button
                              type="button"
                              className="btn btn-ghost small"
                              aria-expanded={expanded}
                              aria-controls={`recon-details-${run.recon_id}`}
                              onClick={() => toggle(run.recon_id)}
                            >
                              {t("billing.tbl.details")}
                            </button>
                          </td>
                        </tr>
                        {expanded && (
                          <tr className="details-row" id={`recon-details-${run.recon_id}`}>
                            <td colSpan={6}>
                              <ReconDetail reconId={run.recon_id} />
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
    </section>
  );
}

function ReconDetail({ reconId }: { reconId: string }) {
  const { t } = useI18n();
  const load = useCallback(() => fetchReconciliationDetail(reconId), [reconId]);
  const { detail, error } = useDetail(load);

  if (error) return <ErrorBanner message={error} />;
  if (!detail) {
    return <div className="skeleton" style={{ height: 36 }} role="status" aria-busy="true" />;
  }
  const buckets = [
    ["billing.bucket.matched", detail.run.matched_client_cost_usd],
    ["billing.bucket.orphan", detail.run.orphan_client_cost_usd],
    ["billing.bucket.estimated", detail.run.estimated_client_cost_usd],
    ["billing.bucket.unattributed", detail.run.unattributed_client_cost_usd],
  ] as const;
  return (
    <div className="grid">
      <div className="row">
        {buckets.map(([key, value]) => (
          <span key={key} className="muted small">
            {t(key)}: <span className="mono">{value}</span>
          </span>
        ))}
      </div>
      {detail.discrepancies.length === 0 ? (
        <div className="empty" role="status">
          {t("billing.disc.empty")}
        </div>
      ) : (
        <table className="tbl">
          <thead>
            <tr>
              <th scope="col">{t("billing.tbl.class")}</th>
              <th scope="col">{t("billing.tbl.request")}</th>
              <th scope="col">{t("billing.tbl.strategy")}</th>
              <th scope="col">{t("billing.tbl.details")}</th>
            </tr>
          </thead>
          <tbody>
            {detail.discrepancies.map((d: Discrepancy) => (
              <tr key={d.discrepancy_id}>
                <td>
                  <span className={`badge ${classTone(d.class)}`}>{d.class}</span>
                </td>
                <td className="mono-cell" title={d.request_id ?? undefined} aria-label={d.request_id ?? undefined}>
                  {d.request_id ?? <Dash />}
                </td>
                <td className="mono-cell" title={d.strategy_id ?? undefined} aria-label={d.strategy_id ?? undefined}>
                  {d.strategy_id ? (
                    <Link href={`/strategies/${d.strategy_id}`}>{shortId(d.strategy_id)}</Link>
                  ) : (
                    <Dash />
                  )}
                </td>
                <td>
                  <pre className="codeblock">{prettyDetails(d.details_json)}</pre>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
