// Phase-0 dashboard placeholder. Real dashboard (positions, track record, risk
// settings, kill-switch controls) lands in Phase 1+.

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
    detail: "Proposals persisted and shown only; no OMS submission.",
  },
  {
    level: "L1 Copilot",
    detail:
      "OMS submits only after per-proposal human approval; no decision within the timeout (default 600 s) \u21d2 auto-reject.",
  },
  {
    level: "L2 Semi-auto",
    detail:
      "OMS submits automatically within the L2 envelope; above-envelope proposals escalate through the L1 approve flow.",
  },
  {
    level: "L3 Full-auto",
    detail:
      "OMS submits any gate-approved proposal; kill-switch and risk limits still apply.",
  },
] as const;

const section = { marginTop: "1.5rem" } as const;
const card = {
  background: "#fff",
  border: "1px solid #e0e0e0",
  borderRadius: "6px",
  padding: "1rem 1.25rem",
} as const;

export default function DashboardPage() {
  return (
    <>
      <h1 style={{ fontSize: "1.4rem" }}>Dashboard</h1>
      <p style={{ color: "#555" }}>
        Phase-0 skeleton. Strategy list, positions, immutable track record, L1
        approve/reject queue, and kill-switch controls arrive with Phase 1.
      </p>

      <section style={section}>
        <h2 style={{ fontSize: "1.1rem" }}>Safety invariants (non-negotiable)</h2>
        <ol style={{ ...card, margin: 0, paddingLeft: "2.5rem" }}>
          {INVARIANTS.map((text, i) => (
            <li key={i} style={{ padding: "0.3rem 0" }}>
              {text}
            </li>
          ))}
        </ol>
      </section>

      <section style={section}>
        <h2 style={{ fontSize: "1.1rem" }}>Autonomy ladder</h2>
        <dl style={{ ...card, margin: 0 }}>
          {LADDER.map(({ level, detail }) => (
            <div key={level} style={{ padding: "0.3rem 0" }}>
              <dt style={{ fontWeight: 600 }}>{level}</dt>
              <dd style={{ margin: 0, color: "#444" }}>{detail}</dd>
            </div>
          ))}
        </dl>
      </section>
    </>
  );
}
