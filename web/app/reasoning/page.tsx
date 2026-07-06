// Phase-0 reasoning viewer placeholder: renders the golden fixtures through the
// zod contract schemas. Phase 1 replaces the fixtures with persisted traces
// fetched from the control-plane API. Server component; no client JS.

import proposalFixture from "../../../contracts/fixtures/proposal_open_long.json";
import verdictFixture from "../../../contracts/fixtures/verdict_reject_daily_loss.json";
import { riskVerdictSchema, tradeProposalSchema } from "../../src/lib/contract/schema";

const proposal = tradeProposalSchema.parse(proposalFixture);
const verdict = riskVerdictSchema.parse(verdictFixture);

const SIGNAL_TONES: Record<string, string> = {
  bullish: "badge-green",
  bearish: "badge-red",
  neutral: "badge-neutral",
};

const DECISION_TONES: Record<string, string> = {
  approve: "badge-green",
  clip: "badge-green",
  reject: "badge-red",
  escalate: "badge-yellow",
};

export default function ReasoningPage() {
  return (
    <>
      <header className="page-head">
        <h1 className="page-title">Reasoning viewer</h1>
        <p className="page-sub">
          Golden-fixture trace (Phase 0). Proposal{" "}
          <span className="mono">{proposal.proposal_id}</span> &rarr; verdict{" "}
          <span className="mono">{verdict.verdict_id}</span>.
        </p>
      </header>

      <section className="section">
        <h2 className="section-title">Proposal</h2>
        <div className="card">
          <div className="row">
            <strong>{proposal.action}</strong>
            <span className="mono">{proposal.symbol}</span>
          </div>
          <hr className="divider" />
          <dl className="kv">
            <dt>confidence</dt>
            <dd className="mono">{proposal.confidence}</dd>
            <dt>size (quote)</dt>
            <dd className="mono">{proposal.size_quote}</dd>
            <dt>entry</dt>
            <dd>
              {proposal.entry.type}
              {proposal.entry.limit_price ? ` @ ${proposal.entry.limit_price}` : ""}
            </dd>
            <dt>stop loss</dt>
            <dd className="mono">{proposal.stop_loss ?? "n/a"}</dd>
            <dt>take profit</dt>
            <dd className="mono">{proposal.take_profit ?? "n/a"}</dd>
          </dl>
          <p>{proposal.reasoning}</p>
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">Analyst summaries</h2>
        <div className="grid grid-3">
          {(["market", "news", "fundamental"] as const).map((role) => {
            const s = proposal.analyst_summaries[role];
            return (
              <div key={role} className="card">
                <h3 className="card-title">{role}</h3>
                <div className="row">
                  <span className={`badge ${SIGNAL_TONES[s.signal] ?? "badge-neutral"}`}>
                    {s.signal}
                  </span>
                  <span className="faint mono small">{s.confidence}</span>
                </div>
                <p className="small">{s.summary}</p>
              </div>
            );
          })}
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">Debate</h2>
        <div className="card">
          <p className="muted">{proposal.debate_summary}</p>
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">Risk Gate verdict</h2>
        <div className="card">
          <div className="row">
            <span className={`badge ${DECISION_TONES[verdict.decision] ?? "badge-neutral"}`}>
              {verdict.decision}
            </span>
          </div>
          <ul>
            {verdict.reasons.map((reason) => (
              <li key={reason.code}>
                <code>{reason.code}</code>: {reason.message}
              </li>
            ))}
          </ul>
          <dl className="kv">
            <dt>evaluated at</dt>
            <dd className="mono">{verdict.evaluated_at}</dd>
            <dt>daily realized PnL</dt>
            <dd className="mono">{verdict.limits_snapshot.daily_realized_pnl_quote}</dd>
            <dt>daily loss limit</dt>
            <dd className="mono">{verdict.limits_snapshot.daily_loss_limit_quote}</dd>
          </dl>
        </div>
      </section>
    </>
  );
}
