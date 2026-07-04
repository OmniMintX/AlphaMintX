// Phase-0 reasoning viewer placeholder: renders the golden fixtures through the
// zod contract schemas. Phase 1 replaces the fixtures with persisted traces
// fetched from the control-plane API. Server component; no client JS.

import proposalFixture from "../../../contracts/fixtures/proposal_open_long.json";
import verdictFixture from "../../../contracts/fixtures/verdict_reject_daily_loss.json";
import { riskVerdictSchema, tradeProposalSchema } from "../../src/lib/contract/schema";

const proposal = tradeProposalSchema.parse(proposalFixture);
const verdict = riskVerdictSchema.parse(verdictFixture);

const card = {
  background: "#fff",
  border: "1px solid #e0e0e0",
  borderRadius: "6px",
  padding: "1rem 1.25rem",
  marginTop: "0.75rem",
} as const;
const mono = { fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace" } as const;

export default function ReasoningPage() {
  return (
    <>
      <h1 style={{ fontSize: "1.4rem" }}>Reasoning viewer</h1>
      <p style={{ color: "#555" }}>
        Golden-fixture trace (Phase 0). Proposal <span style={mono}>{proposal.proposal_id}</span>{" "}
        &rarr; verdict <span style={mono}>{verdict.verdict_id}</span>.
      </p>

      <section style={card}>
        <h2 style={{ fontSize: "1.1rem", marginTop: 0 }}>
          Proposal &mdash; {proposal.action} {proposal.symbol}
        </h2>
        <p style={{ color: "#444" }}>
          size <span style={mono}>{proposal.size_quote}</span> quote &middot; entry{" "}
          {proposal.entry.type}
          {proposal.entry.limit_price ? ` @ ${proposal.entry.limit_price}` : ""} &middot; SL{" "}
          {proposal.stop_loss ?? "n/a"} &middot; TP {proposal.take_profit ?? "n/a"} &middot;
          confidence {proposal.confidence}
        </p>
        <p>{proposal.reasoning}</p>
      </section>

      <section style={card}>
        <h2 style={{ fontSize: "1.1rem", marginTop: 0 }}>Analyst summaries</h2>
        {(["market", "news", "fundamental"] as const).map((role) => {
          const s = proposal.analyst_summaries[role];
          return (
            <p key={role} style={{ margin: "0.4rem 0" }}>
              <strong style={{ textTransform: "capitalize" }}>{role}</strong> &middot; {s.signal}{" "}
              ({s.confidence}): {s.summary}
            </p>
          );
        })}
        <p style={{ color: "#444" }}>
          <strong>Debate:</strong> {proposal.debate_summary}
        </p>
      </section>

      <section style={card}>
        <h2 style={{ fontSize: "1.1rem", marginTop: 0 }}>
          Risk Gate verdict &mdash;{" "}
          <span style={{ color: verdict.decision === "reject" ? "#b3261e" : "#1a7f37" }}>
            {verdict.decision}
          </span>
        </h2>
        <ul style={{ margin: 0, paddingLeft: "1.25rem" }}>
          {verdict.reasons.map((reason) => (
            <li key={reason.code} style={{ padding: "0.2rem 0" }}>
              <span style={mono}>{reason.code}</span>: {reason.message}
            </li>
          ))}
        </ul>
        <p style={{ color: "#555", fontSize: "0.9rem" }}>
          Evaluated at <span style={mono}>{verdict.evaluated_at}</span> &middot; daily realized
          PnL <span style={mono}>{verdict.limits_snapshot.daily_realized_pnl_quote}</span> vs
          limit <span style={mono}>{verdict.limits_snapshot.daily_loss_limit_quote}</span>
        </p>
      </section>
    </>
  );
}
