"use client";

// Ops panel (operator-surface.md OS-22..OS-30): safety status card with kill
// banner / breaker badge / watchdog liveness, lifecycle controls per the
// PINNED OS-26 display table, strategy-tier kill and clear controls, alerts
// feed, and the paper-gate report. Mutations go through the same-origin
// proxies (the OPERATOR_TOKEN never reaches this bundle); every upstream
// error surfaces verbatim via ApiError (OS-30) — never remapped or retried.

import { useCallback, useEffect, useState } from "react";

import {
  PAPER_GATE_POLL_INTERVAL_MS,
  buildKillPayload,
  buildLifecyclePayload,
  clearStrategyKill,
  fetchAlerts,
  fetchPaperGate,
  fetchSafety,
  postKill,
  postLifecycle,
} from "../../../src/lib/api/client";
import type { SafetyStatus } from "../../../src/lib/api/schema";
import { usePoll } from "../../../src/lib/api/usePoll";
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
import { ErrorBanner, Pager, card, mono, section } from "../ui";

const heading = { fontSize: "1.1rem" } as const;
const badge = {
  ...mono,
  borderRadius: "4px",
  padding: "0.1rem 0.45rem",
  fontSize: "0.8rem",
} as const;
const warnBadge = { ...badge, background: "#fdf3e0", color: "#9a6700" } as const;
const dangerBadge = { ...badge, background: "#fbe9e7", color: "#b3261e" } as const;
const okBadge = { ...badge, background: "#e6f4ea", color: "#1a7f37" } as const;
const offBadge = { ...badge, background: "#f0f0f0", color: "#555" } as const;
const button = {
  border: "1px solid #e0e0e0",
  borderRadius: "4px",
  background: "#fff",
  padding: "0.25rem 0.7rem",
  cursor: "pointer",
} as const;
const dangerButton = { ...button, border: "1px solid #b3261e", color: "#b3261e" } as const;
const input = {
  border: "1px solid #e0e0e0",
  borderRadius: "4px",
  padding: "0.25rem 0.5rem",
  fontSize: "0.9rem",
} as const;

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
  const loadSafety = useCallback(() => fetchSafety(strategyId), [strategyId]);
  const safety = usePoll(loadSafety);

  return (
    <section style={section}>
      <h2 style={heading}>Ops</h2>
      {safety.error && <ErrorBanner message={safety.error} />}
      {!safety.data && !safety.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
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
      <AlertsSection strategyId={strategyId} />
      <PaperGateSection strategyId={strategyId} />
    </section>
  );
}

// OS-23: banner severity comes from the server's active_kill predicate,
// never a client-side re-derivation.
function SafetyCard({ safety }: { safety: SafetyStatus }) {
  const standing = unclearedKills(safety.kills);
  const clearedRows = safety.kills.filter((k) => k.cleared !== null);
  const wd = watchdogView(safety.watchdog);
  const wdStyle = { off: offBadge, none: warnBadge, ok: okBadge, stale: dangerBadge }[wd.tone];
  return (
    <div style={card}>
      {standing.length > 0 && (
        <div
          style={{
            background: safety.active_kill ? "#fbe9e7" : "#fdf3e0",
            border: `1px solid ${safety.active_kill ? "#b3261e" : "#9a6700"}`,
            borderRadius: "6px",
            padding: "0.6rem 1rem",
            marginBottom: "0.75rem",
          }}
        >
          <strong style={{ color: safety.active_kill ? "#b3261e" : "#9a6700" }}>
            {safety.active_kill ? "KILL ACTIVE" : "Standing kill (not currently acting)"}
          </strong>
          {standing.map((k) => (
            <p key={k.event_id} style={{ ...mono, fontSize: "0.85rem", margin: "0.3rem 0 0" }}>
              scope {k.scope} &middot; by {k.actor_id} &middot; {k.recorded_at} &middot; flatten{" "}
              {String(k.flatten)} &middot; epoch {k.kill_epoch}
            </p>
          ))}
        </div>
      )}
      <p style={{ margin: "0.2rem 0" }}>
        {safety.breaker.active_today ? (
          <span style={warnBadge}>
            breaker today
            {safety.breaker.event ? ` (${safety.breaker.event.recorded_at})` : ""}
          </span>
        ) : (
          <span style={offBadge}>no breaker today</span>
        )}{" "}
        <span style={wdStyle}>{wd.label}</span>
      </p>
      {clearedRows.length > 0 && (
        <details style={{ marginTop: "0.5rem" }}>
          <summary style={{ cursor: "pointer", color: "#555" }}>
            Cleared kills ({clearedRows.length})
          </summary>
          {clearedRows.map((k) => (
            <p key={k.event_id} style={{ ...mono, fontSize: "0.85rem", color: "#555" }}>
              scope {k.scope} &middot; epoch {k.kill_epoch} &middot; killed {k.recorded_at} &middot;
              cleared by {k.cleared?.actor_id} at {k.cleared?.recorded_at} &mdash;{" "}
              {k.cleared?.reason}
            </p>
          ))}
        </details>
      )}
    </div>
  );
}

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
    <div style={card}>
      <h3 style={{ fontSize: "1rem", marginTop: 0 }}>Lifecycle</h3>
      {error && <ErrorBanner message={error} />}
      <div style={{ display: "flex", alignItems: "center", gap: "0.6rem", flexWrap: "wrap" }}>
        <input
          style={{ ...input, minWidth: "16rem" }}
          placeholder="reason (required)"
          value={reason}
          onChange={(e) => {
            setReason(e.target.value);
            setArmed(null);
          }}
        />
        {actions.map((a) => (
          <button
            key={a.label}
            type="button"
            style={armed === a.label ? dangerButton : button}
            disabled={a.to === null || reason.trim() === "" || pending}
            onClick={() => onClick(a)}
          >
            {armed === a.label ? `confirm ${a.label}` : a.label}
          </button>
        ))}
      </div>
      {actions.some((a) => a.to === null) && (
        <p style={{ color: "#555", fontSize: "0.85rem" }}>
          Resume is disabled: pause provenance unknown (paused_from is null).
        </p>
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
    <div style={card}>
      <h3 style={{ fontSize: "1rem", marginTop: 0 }}>Kill</h3>
      {error && <ErrorBanner message={error} />}
      <div style={{ display: "flex", alignItems: "center", gap: "0.6rem", flexWrap: "wrap" }}>
        <label style={{ fontSize: "0.9rem" }}>
          <input
            type="checkbox"
            checked={flatten}
            onChange={(e) => setFlatten(e.target.checked)}
          />{" "}
          flatten positions
        </label>
        <input
          style={input}
          placeholder='type "KILL" to confirm'
          value={ack}
          onChange={(e) => setAck(e.target.value)}
        />
        <button
          type="button"
          style={dangerButton}
          disabled={ack !== "KILL" || pending}
          onClick={() => void kill()}
        >
          kill strategy
        </button>
      </div>
      {clearable && (
        <div
          style={{
            display: "flex",
            alignItems: "center",
            gap: "0.6rem",
            flexWrap: "wrap",
            marginTop: "0.75rem",
          }}
        >
          <span style={{ ...mono, fontSize: "0.85rem", color: "#555" }}>
            clear kill at epoch {clearable.kill_epoch}:
          </span>
          <input
            style={{ ...input, minWidth: "16rem" }}
            placeholder="reason (required)"
            value={clearReason}
            onChange={(e) => setClearReason(e.target.value)}
          />
          <button
            type="button"
            style={button}
            disabled={clearReason.trim() === "" || pending}
            onClick={() => void clear()}
          >
            clear kill
          </button>
        </div>
      )}
    </div>
  );
}

// OS-24: newest-first alerts feed with the shared Pager; kind verbatim (open
// set); details_json parsed defensively, shown raw on parse failure.
function AlertsSection({ strategyId }: { strategyId: string }) {
  const [page, setPage] = useState(1);
  const loadAlerts = useCallback(() => fetchAlerts(strategyId, page), [strategyId, page]);
  const alerts = usePoll(loadAlerts);

  return (
    <>
      <h3 style={{ fontSize: "1rem", marginTop: "1rem" }}>Alerts</h3>
      {alerts.error && <ErrorBanner message={alerts.error} />}
      {!alerts.data && !alerts.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
      {alerts.data && (
        <>
          <div style={card}>
            {alerts.data.items.length === 0 && <p style={{ color: "#555" }}>No alerts.</p>}
            {alerts.data.items.map((alert) => (
              <div key={alert.alert_id} style={{ padding: "0.35rem 0" }}>
                <span style={warnBadge}>{alert.kind}</span>{" "}
                <span style={{ color: "#555", fontSize: "0.85rem" }}>{alert.recorded_at}</span>
                <pre
                  style={{
                    ...mono,
                    fontSize: "0.8rem",
                    background: "#f8f8f8",
                    padding: "0.4rem 0.6rem",
                    borderRadius: "4px",
                    margin: "0.3rem 0 0",
                    whiteSpace: "pre-wrap",
                  }}
                >
                  {formatDetailsJson(alert.details_json)}
                </pre>
              </div>
            ))}
          </div>
          <Pager
            page={alerts.data.page}
            total={alerts.data.total}
            limit={alerts.data.limit}
            onPage={setPage}
          />
        </>
      )}
    </>
  );
}

// OS-25: polls at 6 × POLL_INTERVAL_MS only (the GET self-charges the READ
// token's 60/min bucket). On a 429 the last-rendered report stays up with a
// rate-limited note; the interval is never tightened.
function PaperGateSection({ strategyId }: { strategyId: string }) {
  const loadGate = useCallback(() => fetchPaperGate(strategyId), [strategyId]);
  const gate = usePoll(loadGate, PAPER_GATE_POLL_INTERVAL_MS);
  const view = paperGateView(gate.data, gate.errorStatus);

  return (
    <>
      <h3 style={{ fontSize: "1rem", marginTop: "1rem" }}>Paper gate</h3>
      {view.rateLimited && (
        <p style={{ color: "#9a6700", fontSize: "0.85rem" }}>
          Rate limited — showing the last fetched report.
        </p>
      )}
      {gate.error && !view.rateLimited && <ErrorBanner message={gate.error} />}
      {!view.report && !gate.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
      {view.report && (
        <div style={card}>
          <p style={{ marginTop: 0 }}>
            {view.report.passed ? (
              <span style={okBadge}>passed</span>
            ) : (
              <span style={warnBadge}>not passed</span>
            )}{" "}
            <span style={{ color: "#555", fontSize: "0.85rem" }}>
              window started {view.report.window_started_at ?? "—"} &middot; evaluated{" "}
              {view.report.evaluated_at}
            </span>
          </p>
          <table style={{ borderCollapse: "collapse", width: "100%", fontSize: "0.9rem" }}>
            <thead>
              <tr>
                {["Condition", "Passed", "Measured", "Required"].map((h) => (
                  <th
                    key={h}
                    style={{
                      borderBottom: "1px solid #eee",
                      padding: "0.3rem 0.6rem 0.3rem 0",
                      textAlign: "left",
                      color: "#555",
                    }}
                  >
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {view.report.conditions.map((c) => (
                <tr key={c.name}>
                  <td style={{ borderBottom: "1px solid #eee", padding: "0.3rem 0.6rem 0.3rem 0" }}>
                    {c.name}
                  </td>
                  <td style={{ borderBottom: "1px solid #eee", padding: "0.3rem 0.6rem 0.3rem 0" }}>
                    {c.passed ? <span style={okBadge}>yes</span> : <span style={dangerBadge}>no</span>}
                  </td>
                  <td style={{ ...mono, borderBottom: "1px solid #eee", padding: "0.3rem 0.6rem 0.3rem 0" }}>
                    {c.measured}
                  </td>
                  <td style={{ ...mono, borderBottom: "1px solid #eee", padding: "0.3rem 0.6rem 0.3rem 0" }}>
                    {c.required}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
