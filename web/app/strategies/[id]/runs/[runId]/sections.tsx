"use client";

// Reasoning-viewer sections: analyst summaries (with "unavailable:" degradation
// markers), debate transcript, trader decision (forced-hold markers), proposal
// JSON, verdict + limits_snapshot, model costs, orders/fills, approvals timeline.

import type { ReactNode } from "react";

import type { TradeProposal, RiskVerdict } from "../../../../../src/lib/contract/schema";
import type { AgentTrace, ApprovalDecision, Fill, Order } from "../../../../../src/lib/api/schema";
import {
  approvalDecisionLabel,
  forcedHoldKind,
  forcedHoldLabel,
  isDegradedDebate,
  isDegradedSummary,
  modelCostTotals,
} from "../../../../../src/lib/view/run";
import { card, mono } from "../../../ui";

const heading = { fontSize: "1.1rem", marginTop: 0 } as const;
const badge = {
  ...mono,
  borderRadius: "4px",
  padding: "0.1rem 0.45rem",
  fontSize: "0.8rem",
} as const;
const warnBadge = { ...badge, background: "#fdf3e0", color: "#9a6700" } as const;
const dangerBadge = { ...badge, background: "#fbe9e7", color: "#b3261e" } as const;
const cellStyle = {
  borderBottom: "1px solid #eee",
  padding: "0.3rem 0.6rem 0.3rem 0",
  textAlign: "left",
} as const;

function Table({ head, rows }: { head: string[]; rows: ReactNode[][] }) {
  return (
    <table style={{ borderCollapse: "collapse", width: "100%", fontSize: "0.9rem" }}>
      <thead>
        <tr>
          {head.map((h) => (
            <th key={h} style={{ ...cellStyle, color: "#555" }}>
              {h}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((cells, i) => (
          <tr key={i}>
            {cells.map((cell, j) => (
              <td key={j} style={cellStyle}>
                {cell}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function AnalystSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const summaries = trace?.analyst_summaries ?? proposal?.analyst_summaries;
  if (!summaries) return null;
  return (
    <section style={card}>
      <h2 style={heading}>Analyst summaries</h2>
      {(["market", "news", "fundamental"] as const).map((role) => {
        const s = summaries[role];
        const degraded = isDegradedSummary(s);
        return (
          <p key={role} style={{ margin: "0.4rem 0", color: degraded ? "#9a6700" : undefined }}>
            <strong style={{ textTransform: "capitalize" }}>{role}</strong>{" "}
            {degraded && <span style={warnBadge}>degraded</span>} &middot; {s.signal} ({s.confidence}
            ): {s.summary}
          </p>
        );
      })}
    </section>
  );
}

export function DebateSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const debateSummary = trace?.debate_summary ?? proposal?.debate_summary;
  if (!trace && debateSummary === undefined) return null;
  return (
    <section style={card}>
      <h2 style={heading}>Debate</h2>
      {trace?.debate_rounds.map((round) => (
        <div key={round.round_index} style={{ padding: "0.3rem 0" }}>
          <p style={{ margin: "0.2rem 0" }}>
            <strong>Round {round.round_index + 1} &middot; Bull</strong> (score {round.bull_score}
            ): {round.bull_argument}
          </p>
          <p style={{ margin: "0.2rem 0" }}>
            <strong>Round {round.round_index + 1} &middot; Bear</strong> (score {round.bear_score}
            ): {round.bear_argument}
          </p>
        </div>
      ))}
      {trace && trace.debate_rounds.length === 0 && (
        <p style={{ color: "#555" }}>No debate rounds recorded.</p>
      )}
      {debateSummary !== undefined && (
        <p style={{ color: "#444" }}>
          <strong>Judge:</strong> {isDegradedDebate(debateSummary) && <span style={warnBadge}>degraded</span>}{" "}
          {debateSummary}
        </p>
      )}
    </section>
  );
}

export function ProposalSection({ proposal }: { proposal: TradeProposal }) {
  const hold = forcedHoldKind(proposal);
  return (
    <section style={card}>
      <h2 style={heading}>
        Trader decision &mdash; {proposal.action} {proposal.symbol}{" "}
        {hold && <span style={dangerBadge}>{forcedHoldLabel(hold)}</span>}
      </h2>
      <p style={{ color: "#444" }}>
        size <span style={mono}>{proposal.size_quote}</span> quote &middot; entry {proposal.entry.type}
        {proposal.entry.limit_price ? ` @ ${proposal.entry.limit_price}` : ""} &middot; SL{" "}
        {proposal.stop_loss ?? "n/a"} &middot; TP {proposal.take_profit ?? "n/a"} &middot; confidence{" "}
        {proposal.confidence}
      </p>
      <p>{proposal.reasoning}</p>
      <details>
        <summary style={{ cursor: "pointer", color: "#555" }}>Proposal JSON</summary>
        <pre style={{ ...mono, fontSize: "0.8rem", overflowX: "auto" }}>
          {JSON.stringify(proposal, null, 2)}
        </pre>
      </details>
    </section>
  );
}

export function VerdictSection({ verdict }: { verdict: RiskVerdict }) {
  const rejected = verdict.decision === "reject";
  return (
    <section style={card}>
      <h2 style={heading}>
        Risk Gate verdict &mdash;{" "}
        <span style={{ color: rejected ? "#b3261e" : "#1a7f37" }}>{verdict.decision}</span>
        {verdict.clipped_size_quote && (
          <>
            {" "}
            <span style={mono}>clipped to {verdict.clipped_size_quote}</span>
          </>
        )}
      </h2>
      {verdict.reasons.length > 0 && (
        <ul style={{ margin: 0, paddingLeft: "1.25rem" }}>
          {verdict.reasons.map((reason) => (
            <li key={reason.code} style={{ padding: "0.2rem 0" }}>
              <span style={mono}>{reason.code}</span>: {reason.message}
            </li>
          ))}
        </ul>
      )}
      <details style={{ marginTop: "0.5rem" }}>
        <summary style={{ cursor: "pointer", color: "#555" }}>Limits snapshot</summary>
        <Table
          head={["field", "value"]}
          rows={Object.entries(verdict.limits_snapshot).map(([field, value]) => [
            <span key={field} style={mono}>
              {field}
            </span>,
            <span key={`${field}-v`} style={mono}>
              {JSON.stringify(value)}
            </span>,
          ])}
        />
      </details>
      <p style={{ color: "#555", fontSize: "0.9rem" }}>
        Evaluated at <span style={mono}>{verdict.evaluated_at}</span>
      </p>
    </section>
  );
}

export function CostsSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const costs = trace?.model_costs ?? proposal?.model_costs;
  if (!costs) return null;
  const estimatedNodes = new Set(trace?.estimated_cost_nodes ?? []);
  const totals = modelCostTotals(costs);
  return (
    <section style={card}>
      <h2 style={heading}>Model costs</h2>
      <Table
        head={["node", "model", "input tokens", "output tokens", "cost (USD)"]}
        rows={[
          ...costs.map((cost, i): ReactNode[] => [
            <span key={`n${i}`} style={mono}>
              {cost.node}{" "}
              {estimatedNodes.has(cost.node) && <span style={warnBadge}>estimated</span>}
            </span>,
            <span key={`m${i}`} style={mono}>
              {cost.model}
            </span>,
            String(cost.input_tokens),
            String(cost.output_tokens),
            <span key={`c${i}`} style={mono}>
              {cost.cost_usd}
            </span>,
          ]),
          [
            <strong key="t">Total</strong>,
            "",
            <strong key="ti">{String(totals.input_tokens)}</strong>,
            <strong key="to">{String(totals.output_tokens)}</strong>,
            <strong key="tc" style={mono}>
              {totals.cost_usd}
            </strong>,
          ],
        ]}
      />
      {estimatedNodes.size > 0 && (
        <p style={{ color: "#9a6700", fontSize: "0.85rem" }}>
          <span style={warnBadge}>estimated</span> marks nodes whose cost was estimated after a
          timeout/abort (no usage returned) — never silently uncounted.
        </p>
      )}
    </section>
  );
}

export function OrdersSection({ orders, fills }: { orders: Order[]; fills: Fill[] }) {
  return (
    <section style={card}>
      <h2 style={heading}>Orders &amp; fills</h2>
      {orders.length === 0 && <p style={{ color: "#555" }}>No orders (nothing submitted to the OMS).</p>}
      {orders.length > 0 && (
        <Table
          head={["order", "origin", "class", "side", "type", "qty (base)", "status"]}
          rows={orders.map((order) => [
            <span key={order.order_id} style={mono}>
              {order.order_id}
            </span>,
            order.origin,
            order.class,
            order.side,
            order.type,
            <span key={`${order.order_id}-q`} style={mono}>
              {order.qty_base}
            </span>,
            order.status,
          ])}
        />
      )}
      {fills.length > 0 && (
        <>
          <h3 style={{ fontSize: "0.95rem", marginBottom: "0.3rem" }}>Fills</h3>
          <Table
            head={["fill", "order", "qty (base)", "price", "fee (quote)", "at"]}
            rows={fills.map((fill) => [
              <span key={fill.fill_id} style={mono}>
                {fill.fill_id}
              </span>,
              <span key={`${fill.fill_id}-o`} style={mono}>
                {fill.order_id}
              </span>,
              <span key={`${fill.fill_id}-q`} style={mono}>
                {fill.qty_base}
              </span>,
              <span key={`${fill.fill_id}-p`} style={mono}>
                {fill.fill_price}
              </span>,
              <span key={`${fill.fill_id}-f`} style={mono}>
                {fill.fee_quote}
              </span>,
              <span key={`${fill.fill_id}-t`} style={mono}>
                {fill.fill_ts}
              </span>,
            ])}
          />
        </>
      )}
    </section>
  );
}

export function ApprovalsSection({ approvals }: { approvals: ApprovalDecision[] }) {
  if (approvals.length === 0) return null;
  return (
    <section style={card}>
      <h2 style={heading}>Approvals</h2>
      {approvals.map((approval) => (
        <div key={approval.approval_id} style={{ padding: "0.3rem 0" }}>
          <p style={{ margin: "0.2rem 0" }}>
            {approval.outcome === "approved_but_blocked" || approval.outcome === "timeout" ? (
              <span style={approval.outcome === "timeout" ? dangerBadge : warnBadge}>
                {approval.outcome}
              </span>
            ) : (
              <span
                style={{
                  ...badge,
                  background:
                    approval.outcome === "approved" && approval.submitted !== false
                      ? "#e6f4ea"
                      : "#fbe9e7",
                  color:
                    approval.outcome === "approved" && approval.submitted !== false
                      ? "#1a7f37"
                      : "#b3261e",
                }}
              >
                {approval.outcome}
              </span>
            )}{" "}
            {approvalDecisionLabel(approval)} &middot; by{" "}
            <span style={mono}>{approval.decided_by}</span> at{" "}
            <span style={mono}>{approval.decided_at}</span>
          </p>
          {approval.outcome === "approved_but_blocked" && approval.preflight_reasons && (
            <ul style={{ margin: 0, paddingLeft: "1.25rem", color: "#9a6700" }}>
              {approval.preflight_reasons.map((reason) => (
                <li key={reason}>{reason}</li>
              ))}
            </ul>
          )}
        </div>
      ))}
    </section>
  );
}
