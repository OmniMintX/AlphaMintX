"use client";

// Ops panel (operator-surface.md OS-22..OS-30): safety status card with kill
// banner / breaker badge / watchdog liveness, lifecycle controls per the
// PINNED OS-26 display table, strategy-tier kill and clear controls, alerts
// feed, the paper-gate report, and the DB-backed risk-limits settings card.
// Mutations go through the same-origin
// /api/cp session proxy (no credential ever reaches this bundle); every
// upstream error surfaces verbatim via ApiError (OS-30) — never remapped or
// retried.

import { useCallback, useEffect, useState } from "react";

import {
  ApiError,
  PAPER_GATE_POLL_INTERVAL_MS,
  buildKillPayload,
  buildLifecyclePayload,
  clearStrategyKill,
  fetchAlerts,
  fetchLimits,
  fetchPaperGate,
  fetchSafety,
  postKill,
  postLifecycle,
  postLimits,
} from "../../../src/lib/api/client";
import {
  buildLimitChanges,
  type LimitChangeRow,
  type RiskLimits,
  type SafetyStatus,
} from "../../../src/lib/api/schema";
import { usePoll } from "../../../src/lib/api/usePoll";
import { decimalRegex } from "../../../src/lib/contract/schema";
import { useI18n } from "../../../src/lib/i18n";
import {
  defaultFlatten,
  formatDetailsJson,
  legalActions,
  newestUnclearedStrategyKill,
  paperGateView,
  unclearedKills,
  watchdogView,
  type OpsAction,
} from "../../../src/lib/view/ops";
import { ErrorBanner, Pager } from "../ui";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function OpsPanel({
  strategyId,
  onLifecycleChange,
}: {
  strategyId: string;
  onLifecycleChange: () => void;
}) {
  const { t } = useI18n();
  const loadSafety = useCallback(() => fetchSafety(strategyId), [strategyId]);
  const safety = usePoll(loadSafety);

  return (
    <section className="section">
      <h2 className="section-title">{t("strat.ops.title")}</h2>
      {safety.error && <ErrorBanner message={safety.error} />}
      {!safety.data && !safety.error && (
        <div className="grid grid-2" style={{ marginBottom: 12 }}>
          <div className="skeleton" style={{ height: 120 }} />
          <div className="skeleton" style={{ height: 120 }} />
        </div>
      )}
      <div className="grid grid-2">
        {safety.data && (
          <>
            <SafetyCard safety={safety.data} />
            <LifecycleControls
              strategyId={strategyId}
              safety={safety.data}
              onDone={() => {
                safety.refresh();
                onLifecycleChange();
              }}
            />
            <KillControls strategyId={strategyId} safety={safety.data} onDone={safety.refresh} />
          </>
        )}
        <PaperGateSection strategyId={strategyId} />
        <div style={{ gridColumn: "1 / -1" }}>
          <LimitsSection strategyId={strategyId} />
        </div>
        <div style={{ gridColumn: "1 / -1" }}>
          <AlertsSection strategyId={strategyId} />
        </div>
      </div>
    </section>
  );
}

// OS-23: banner severity comes from the server's active_kill predicate,
// never a client-side re-derivation.
const WD_LABEL_KEYS = {
  off: "strat.ops.wd.off",
  none: "strat.ops.wd.none",
  ok: "strat.ops.wd.ok",
  stale: "strat.ops.wd.stale",
} as const;

function SafetyCard({ safety }: { safety: SafetyStatus }) {
  const { t } = useI18n();
  const standing = unclearedKills(safety.kills);
  const wd = watchdogView(safety.watchdog);
  const wdTone = { off: "badge-neutral", none: "badge-neutral", ok: "badge-green", stale: "badge-yellow" }[
    wd.tone
  ];
  return (
    <div className="card">
      <h3 className="card-title">{t("strat.ops.safety")}</h3>
      {standing.length > 0 && (
        <div className={`banner ${safety.active_kill ? "banner-error" : "banner-warn"}`}>
          <span aria-hidden>&#9888;</span>
          <div>
            <strong>
              {safety.active_kill ? t("strat.ops.killactive") : t("strat.ops.standingkill")}
            </strong>
            {standing.map((k) => (
              <p key={k.event_id} className="mono small" style={{ margin: "4px 0 0" }}>
                {t("strat.ops.killmeta", {
                  scope: k.scope,
                  actor: k.actor_id,
                  at: k.recorded_at,
                  flatten: String(k.flatten),
                  epoch: k.kill_epoch,
                })}
              </p>
            ))}
          </div>
        </div>
      )}
      <dl className="kv">
        <dt>{t("strat.ops.breaker")}</dt>
        <dd>
          {safety.breaker.active_today ? (
            <span className="badge badge-red">
              <span className="dot" />
              {t("strat.ops.breaker.today")}
              {safety.breaker.event ? ` (${safety.breaker.event.recorded_at})` : ""}
            </span>
          ) : (
            <span className="badge badge-green">{t("strat.ops.breaker.none")}</span>
          )}
        </dd>
        <dt>{t("strat.ops.watchdog")}</dt>
        <dd>
          <span className={`badge ${wdTone}`}>
            {t(WD_LABEL_KEYS[wd.tone], { s: safety.watchdog.seconds_since ?? 0 })}
          </span>
        </dd>
      </dl>
      {safety.kills.length > 0 && (
        <>
          <hr className="divider" />
          <ul className="timeline">
            {safety.kills.map((k) => (
              <li key={k.event_id} className={k.cleared === null ? "tl-red" : undefined}>
                <div className="row">
                  <span className="mono">{t("strat.ops.kill.item", { scope: k.scope })}</span>
                  <span className="faint small">
                    {t("strat.ops.kill.meta", {
                      epoch: k.kill_epoch,
                      actor: k.actor_id,
                      flatten: String(k.flatten),
                    })}
                  </span>
                </div>
                <span className="tl-time">{k.recorded_at}</span>
                {k.cleared && (
                  <p className="faint small" style={{ margin: "2px 0 0" }}>
                    {t("strat.ops.kill.cleared", {
                      actor: k.cleared.actor_id,
                      at: k.cleared.recorded_at,
                      reason: k.cleared.reason,
                    })}
                  </p>
                )}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

// Button tone per verb (presentation only; the action set stays legalActions').
const LIFECYCLE_BTN_CLASS: Record<OpsAction["verb"], string> = {
  activate: "btn btn-primary",
  promote: "btn btn-primary",
  demote: "btn",
  pause: "btn",
  resume: "btn",
  unlock: "btn btn-danger",
};

// OS-26: buttons per the pinned display table; the server remains the sole
// transition authority — any 422 surfaces verbatim, never pre-suppressed.
// Transitions INTO live_* arm a second confirm click before the POST.
function LifecycleControls({
  strategyId,
  safety,
  onDone,
}: {
  strategyId: string;
  safety: SafetyStatus;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const [reason, setReason] = useState("");
  const [armed, setArmed] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const actions = legalActions(safety.lifecycle_state, safety.paused_from);

  const submit = async (a: OpsAction) => {
    if (a.to === null) return;
    setPending(true);
    setError(null);
    try {
      await postLifecycle(strategyId, buildLifecyclePayload(a.to, reason));
      setReason("");
      onDone();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
      setArmed(null);
    }
  };

  const onClick = (a: OpsAction) => {
    if (a.confirm && armed !== a.label) {
      setArmed(a.label);
      return;
    }
    void submit(a);
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("strat.ops.lifecycle")}</h3>
      {error && <ErrorBanner message={error} />}
      <div className="field">
        <span className="field-label">{t("strat.ops.reason")}</span>
        <input
          className="input"
          style={{ minWidth: "16rem" }}
          placeholder={t("strat.ops.reason.ph")}
          value={reason}
          onChange={(e) => {
            setReason(e.target.value);
            setArmed(null);
          }}
        />
      </div>
      <div className="row" style={{ marginTop: 10 }}>
        {actions.map((a) => (
          <button
            key={a.label}
            type="button"
            className={armed === a.label ? "btn btn-danger" : LIFECYCLE_BTN_CLASS[a.verb]}
            disabled={a.to === null || reason.trim() === "" || pending}
            onClick={() => onClick(a)}
          >
            {armed === a.label ? t("strat.ops.confirm", { label: a.label }) : a.label}
          </button>
        ))}
      </div>
      {actions.some((a) => a.to === null) && (
        <p className="muted small">{t("strat.ops.resume.disabled")}</p>
      )}
    </div>
  );
}

// OS-28 kill (typed KILL confirm; flatten defaults checked for live_*) and
// OS-29 clear (newest uncleared strategy kill's epoch as the CAS token; a
// 409 CLEAR_CONFLICT re-fetches the card and surfaces — never auto-retries).
function KillControls({
  strategyId,
  safety,
  onDone,
}: {
  strategyId: string;
  safety: SafetyStatus;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const [flatten, setFlatten] = useState(() => defaultFlatten(safety.lifecycle_state));
  const [ack, setAck] = useState("");
  const [clearReason, setClearReason] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setFlatten(defaultFlatten(safety.lifecycle_state));
  }, [safety.lifecycle_state]);

  const clearable = newestUnclearedStrategyKill(safety.kills);

  const kill = async () => {
    setPending(true);
    setError(null);
    try {
      await postKill(strategyId, buildKillPayload(flatten));
      setAck("");
      onDone();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  const clear = async () => {
    if (!clearable) return;
    setPending(true);
    setError(null);
    try {
      await clearStrategyKill(strategyId, clearReason, clearable.kill_epoch, onDone);
      setClearReason("");
      onDone();
    } catch (err) {
      setError(errText(err));
    } finally {
      setPending(false);
    }
  };

  return (
    <div className="card">
      <h3 className="card-title">{t("strat.ops.kill")}</h3>
      {error && <ErrorBanner message={error} />}
      <label className="checkbox-row">
        <input
          type="checkbox"
          checked={flatten}
          onChange={(e) => setFlatten(e.target.checked)}
        />
        {t("strat.ops.flatten")}
      </label>
      <div className="row" style={{ marginTop: 8 }}>
        <input
          className="input"
          placeholder={t("strat.ops.kill.ack.ph")}
          value={ack}
          onChange={(e) => setAck(e.target.value)}
        />
        <button
          type="button"
          className="btn btn-danger"
          disabled={ack !== "KILL" || pending}
          onClick={() => void kill()}
        >
          {t("strat.ops.kill.btn")}
        </button>
      </div>
      {clearable && (
        <>
          <hr className="divider" />
          <p className="faint small mono" style={{ margin: "0 0 6px" }}>
            {t("strat.ops.clear.at", { epoch: clearable.kill_epoch })}
          </p>
          <div className="row">
            <input
              className="input"
              style={{ minWidth: "16rem" }}
              placeholder={t("strat.ops.reason.ph")}
              value={clearReason}
              onChange={(e) => setClearReason(e.target.value)}
            />
            <button
              type="button"
              className="btn"
              disabled={clearReason.trim() === "" || pending}
              onClick={() => void clear()}
            >
              {t("strat.ops.clear.btn")}
            </button>
          </div>
        </>
      )}
    </div>
  );
}

// Presentation-only tone for the alerts timeline; kind stays the open set.
function alertTone(kind: string): string | undefined {
  const k = kind.toLowerCase();
  if (k.includes("critical") || k.includes("error")) return "tl-red";
  if (k.includes("warn")) return "tl-yellow";
  return undefined;
}

// OS-24: newest-first alerts feed with the shared Pager; kind verbatim (open
// set); details_json parsed defensively, shown raw on parse failure.
function AlertsSection({ strategyId }: { strategyId: string }) {
  const { t } = useI18n();
  const [page, setPage] = useState(1);
  const loadAlerts = useCallback(() => fetchAlerts(strategyId, page), [strategyId, page]);
  const alerts = usePoll(loadAlerts);

  return (
    <>
      <div className="card">
        <h3 className="card-title">{t("strat.ops.alerts")}</h3>
        {alerts.error && <ErrorBanner message={alerts.error} />}
        {!alerts.data && !alerts.error && <div className="skeleton" style={{ height: 60 }} />}
        {alerts.data &&
          (alerts.data.items.length === 0 ? (
            <div className="empty">{t("strat.ops.alerts.empty")}</div>
          ) : (
            <ul className="timeline">
              {alerts.data.items.map((alert) => (
                <li key={alert.alert_id} className={alertTone(alert.kind)}>
                  <div className="row">
                    <span className="mono">{alert.kind}</span>
                    <span className="tl-time">{alert.recorded_at}</span>
                  </div>
                  {alert.details_json && (
                    <details>
                      <summary className="faint small" style={{ cursor: "pointer" }}>
                        {t("strat.ops.details")}
                      </summary>
                      <pre className="codeblock" style={{ whiteSpace: "pre-wrap" }}>
                        {formatDetailsJson(alert.details_json)}
                      </pre>
                    </details>
                  )}
                </li>
              ))}
            </ul>
          ))}
      </div>
      {alerts.data && (
        <Pager
          page={alerts.data.page}
          total={alerts.data.total}
          limit={alerts.data.limit}
          onPage={setPage}
        />
      )}
    </>
  );
}

// OS-25: polls at 6 × POLL_INTERVAL_MS only (the GET self-charges the READ
// token's 60/min bucket). On a 429 the last-rendered report stays up with a
// rate-limited note; the interval is never tightened.
function PaperGateSection({ strategyId }: { strategyId: string }) {
  const { t } = useI18n();
  const loadGate = useCallback(() => fetchPaperGate(strategyId), [strategyId]);
  const gate = usePoll(loadGate, PAPER_GATE_POLL_INTERVAL_MS);
  const view = paperGateView(gate.data, gate.errorStatus);

  return (
    <div className="card">
      <h3 className="card-title">{t("strat.ops.gate")}</h3>
      {view.rateLimited && (
        <div className="banner banner-warn">
          <span aria-hidden>&#9888;</span>
          <span>{t("strat.ops.gate.ratelimited")}</span>
        </div>
      )}
      {gate.error && !view.rateLimited && <ErrorBanner message={gate.error} />}
      {!view.report && !gate.error && <div className="skeleton" style={{ height: 60 }} />}
      {view.report && (
        <>
          <div className="row">
            {view.report.passed ? (
              <span className="badge badge-green">PASS</span>
            ) : (
              <span className="badge badge-red">FAIL</span>
            )}
            <span className="faint small mono">
              {t("strat.ops.gate.window", {
                start: view.report.window_started_at ?? "—",
                end: view.report.evaluated_at,
              })}
            </span>
          </div>
          <table className="tbl" style={{ marginTop: 10 }}>
            <thead>
              <tr>
                <th>{t("strat.ops.gate.condition")}</th>
                <th>{t("strat.ops.gate.passed")}</th>
                <th>{t("strat.ops.gate.measured")}</th>
                <th>{t("strat.ops.gate.required")}</th>
              </tr>
            </thead>
            <tbody>
              {view.report.conditions.map((c) => (
                <tr key={c.name}>
                  <td>{c.name}</td>
                  <td>
                    {c.passed ? (
                      <span className="badge badge-green">{t("strat.ops.yes")}</span>
                    ) : (
                      <span className="badge badge-red">{t("strat.ops.no")}</span>
                    )}
                  </td>
                  <td className="mono-cell">{c.measured}</td>
                  <td className="mono-cell">{c.required}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}


// Settings — DB-backed risk limits: effective values, the runtime edit form
// (the five changeable fields; blank = unchanged), and the change audit
// trail. Decimals stay strings end-to-end — never parsed to floats.
function LimitsSection({ strategyId }: { strategyId: string }) {
  const { t } = useI18n();
  const loadLimits = useCallback(() => fetchLimits(strategyId), [strategyId]);
  const limits = usePoll(loadLimits);

  return (
    <div className="card">
      <h3 className="card-title">{t("strat.ops.limits")}</h3>
      {limits.error && <ErrorBanner message={limits.error} />}
      {!limits.data && !limits.error && <div className="skeleton" style={{ height: 120 }} />}
      {limits.data && (
        <>
          <EffectiveLimits effective={limits.data.effective} />
          <hr className="divider" />
          <LimitsEditForm
            strategyId={strategyId}
            effective={limits.data.effective}
            onDone={limits.refresh}
          />
          <hr className="divider" />
          <LimitChangeHistory changes={limits.data.changes} />
        </>
      )}
    </div>
  );
}

function EffectiveLimits({ effective }: { effective: RiskLimits }) {
  const { t } = useI18n();
  const q = effective.accounting_quote;
  return (
    <dl className="kv">
      <dt>{t("strat.ops.limits.whitelist")}</dt>
      <dd>
        {effective.symbol_whitelist.length === 0 ? (
          <span className="faint small">—</span>
        ) : (
          effective.symbol_whitelist.map((s) => (
            <span key={s} className="badge badge-neutral" style={{ marginRight: 4 }}>
              {s}
            </span>
          ))
        )}
      </dd>
      <dt>{t("strat.ops.limits.maxopen")}</dt>
      <dd className="mono">{effective.max_open_positions}</dd>
      <dt>{t("strat.ops.limits.notionalcap")}</dt>
      <dd className="mono">
        {effective.per_position_notional_cap_quote} {q}
      </dd>
      <dt>{t("strat.ops.limits.dailyloss")}</dt>
      <dd className="mono">
        {effective.daily_loss_limit_quote} {q}
      </dd>
      <dt>{t("strat.ops.limits.maxdd")}</dt>
      <dd className="mono">{effective.max_drawdown_pct}</dd>
      <dt>{t("strat.ops.limits.lossatstop")}</dt>
      <dd className="mono">
        {effective.max_loss_at_stop_quote} {q}
      </dd>
      <dt>{t("strat.ops.limits.stopdist")}</dt>
      <dd className="mono">
        {effective.min_stop_distance_pct} – {effective.max_stop_distance_pct}
      </dd>
      <dt>{t("strat.ops.limits.maxorders")}</dt>
      <dd className="mono">{effective.max_orders_per_minute}</dd>
      <dt>{t("strat.ops.limits.requiresl")}</dt>
      <dd>
        {effective.require_stop_loss ? (
          <span className="badge badge-green">ON</span>
        ) : (
          <span className="badge badge-neutral">off</span>
        )}
      </dd>
      <dt>{t("strat.ops.limits.capital")}</dt>
      <dd className="mono">
        {effective.allocated_capital_quote} {q}
      </dd>
      <dt>{t("strat.ops.limits.quote")}</dt>
      <dd className="mono">{q}</dd>
      <dt>{t("strat.ops.limits.staleness")}</dt>
      <dd className="mono">{effective.staleness_threshold_seconds}s</dd>
      <dt>{t("strat.ops.limits.l1timeout")}</dt>
      <dd className="mono">{effective.l1_approval_timeout_seconds}s</dd>
      <dt>{t("strat.ops.limits.l2env")}</dt>
      <dd>
        {effective.l2_envelope === null ? (
          <span className="faint small">{t("strat.ops.limits.none")}</span>
        ) : (
          <>
            <span className="mono">
              {t("strat.ops.limits.maxsize")} {effective.l2_envelope.max_size_quote} {q}
            </span>{" "}
            {effective.l2_envelope.allowed_symbols.map((s) => (
              <span key={s} className="badge badge-neutral" style={{ marginRight: 4 }}>
                {s}
              </span>
            ))}
          </>
        )}
      </dd>
    </dl>
  );
}

// The five runtime-changeable fields. Blank = unchanged; the payload carries
// only entered fields (buildLimitChanges drops undefined keys). A 403 means
// the proxy's credential lacks the required role — rendered visibly, never
// hidden.
function LimitsEditForm({
  strategyId,
  effective,
  onDone,
}: {
  strategyId: string;
  effective: RiskLimits;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const [maxOpen, setMaxOpen] = useState("");
  const [maxOrders, setMaxOrders] = useState("");
  const [notionalCap, setNotionalCap] = useState("");
  const [dailyLoss, setDailyLoss] = useState("");
  const [lossAtStop, setLossAtStop] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<{ status: number | null; message: string } | null>(null);
  const [appliedCount, setAppliedCount] = useState<number | null>(null);

  const intOr = (v: string) => (v.trim() === "" ? undefined : Number(v.trim()));
  const strOr = (v: string) => (v.trim() === "" ? undefined : v.trim());
  // Client-side mirror of the server's ADR-0003 decimal shape: reject before
  // the POST so a typo ("1e5", "-3", "1,5") gets an inline hint, not a 400.
  const decOk = (v: string) => v.trim() === "" || decimalRegex.test(v.trim());

  const input = {
    max_open_positions: intOr(maxOpen),
    max_orders_per_minute: intOr(maxOrders),
    per_position_notional_cap_quote: strOr(notionalCap),
    daily_loss_limit_quote: strOr(dailyLoss),
    max_loss_at_stop_quote: strOr(lossAtStop),
  };
  const nothingEntered = Object.values(input).every((v) => v === undefined);
  const decimalErrors = {
    notionalCap: !decOk(notionalCap),
    dailyLoss: !decOk(dailyLoss),
    lossAtStop: !decOk(lossAtStop),
  };
  const anyDecimalError = Object.values(decimalErrors).some(Boolean);

  const submit = async () => {
    setPending(true);
    setError(null);
    setAppliedCount(null);
    try {
      const res = await postLimits(strategyId, buildLimitChanges(input));
      setMaxOpen("");
      setMaxOrders("");
      setNotionalCap("");
      setDailyLoss("");
      setLossAtStop("");
      setAppliedCount(res.changes.length);
      onDone();
    } catch (err) {
      setError({
        status: err instanceof ApiError ? err.status : null,
        message: errText(err),
      });
    } finally {
      setPending(false);
    }
  };

  return (
    <>
      {error &&
        (error.status === 403 ? (
          <div className="banner banner-error" role="alert">
            <span aria-hidden>&#9888;</span>
            <div>
              <strong>{t("strat.ops.limits.forbidden")}</strong>
              <p className="small" style={{ margin: "4px 0 0" }}>
                {t("strat.ops.limits.forbidden.hint", { message: error.message })}
              </p>
            </div>
          </div>
        ) : (
          <ErrorBanner message={error.message} />
        ))}
      {appliedCount !== null && (
        <p className="faint small">
          {t(
            appliedCount === 1 ? "strat.ops.limits.applied.one" : "strat.ops.limits.applied.many",
            { count: appliedCount },
          )}
        </p>
      )}
      <div className="row">
        <label className="field">
          <span className="field-label">{t("strat.ops.limits.maxopen")}</span>
          <input
            className="input"
            type="number"
            step={1}
            min={0}
            value={maxOpen}
            onChange={(e) => setMaxOpen(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">{t("strat.ops.limits.maxorders")}</span>
          <input
            className="input"
            type="number"
            step={1}
            min={0}
            value={maxOrders}
            onChange={(e) => setMaxOrders(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field-label">
            {t("strat.ops.limits.notionalcap")} ({effective.accounting_quote})
          </span>
          <input
            className={`input${decimalErrors.notionalCap ? " error" : ""}`}
            placeholder={effective.per_position_notional_cap_quote}
            value={notionalCap}
            onChange={(e) => setNotionalCap(e.target.value)}
            aria-invalid={decimalErrors.notionalCap || undefined}
          />
          {decimalErrors.notionalCap && (
            <span className="field-error">{t("strat.ops.limits.decimal.invalid")}</span>
          )}
        </label>
        <label className="field">
          <span className="field-label">
            {t("strat.ops.limits.dailyloss")} ({effective.accounting_quote})
          </span>
          <input
            className={`input${decimalErrors.dailyLoss ? " error" : ""}`}
            placeholder={effective.daily_loss_limit_quote}
            value={dailyLoss}
            onChange={(e) => setDailyLoss(e.target.value)}
            aria-invalid={decimalErrors.dailyLoss || undefined}
          />
          {decimalErrors.dailyLoss && (
            <span className="field-error">{t("strat.ops.limits.decimal.invalid")}</span>
          )}
        </label>
        <label className="field">
          <span className="field-label">
            {t("strat.ops.limits.lossatstop")} ({effective.accounting_quote})
          </span>
          <input
            className={`input${decimalErrors.lossAtStop ? " error" : ""}`}
            placeholder={effective.max_loss_at_stop_quote}
            value={lossAtStop}
            onChange={(e) => setLossAtStop(e.target.value)}
            aria-invalid={decimalErrors.lossAtStop || undefined}
          />
          {decimalErrors.lossAtStop && (
            <span className="field-error">{t("strat.ops.limits.decimal.invalid")}</span>
          )}
        </label>
      </div>
      <div className="row" style={{ marginTop: 10 }}>
        <button
          type="button"
          className="btn btn-primary"
          disabled={pending || nothingEntered || anyDecimalError}
          onClick={() => void submit()}
        >
          {t("strat.ops.limits.apply")}
        </button>
        <span className="faint small">{t("strat.ops.limits.blankhint")}</span>
      </div>
    </>
  );
}

function LimitChangeHistory({ changes }: { changes: LimitChangeRow[] }) {
  const { t } = useI18n();
  const sorted = [...changes].sort((a, b) => b.changed_at.localeCompare(a.changed_at));
  return (
    <details>
      <summary className="faint small" style={{ cursor: "pointer" }}>
        {t("strat.ops.history", { count: changes.length })}
      </summary>
      {sorted.length === 0 ? (
        <div className="empty">{t("strat.ops.history.empty")}</div>
      ) : (
        <table className="tbl" style={{ marginTop: 8 }}>
          <thead>
            <tr>
              <th>{t("strat.ops.history.at")}</th>
              <th>{t("strat.ops.history.field")}</th>
              <th>{t("strat.ops.history.change")}</th>
              <th>{t("strat.ops.history.actor")}</th>
            </tr>
          </thead>
          <tbody>
            {sorted.map((c) => (
              <tr key={c.change_id}>
                <td className="mono-cell">{c.changed_at}</td>
                <td className="mono-cell">{c.field}</td>
                <td className="mono-cell">
                  {c.old_value ?? "—"} &rarr; {c.new_value}
                </td>
                <td
                  className="mono-cell"
                  style={{
                    maxWidth: "10rem",
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                    whiteSpace: "nowrap",
                  }}
                >
                  {c.actor_id}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </details>
  );
}
