"use client";

// Reasoning-viewer sections: analyst summaries (with "unavailable:" degradation
// markers), debate transcript, trader decision (forced-hold markers), proposal
// JSON, verdict + limits_snapshot, model costs, orders/fills, approvals timeline.

import { Fragment, type ReactNode } from "react";

import type { TradeProposal, RiskVerdict } from "../../../../../src/lib/contract/schema";
import type { AgentTrace, ApprovalDecision, Fill, Order } from "../../../../../src/lib/api/schema";
import { useI18n, type MessageKey } from "../../../../../src/lib/i18n";
import {
  forcedHoldKind,
  isDegradedDebate,
  isDegradedSummary,
  modelCostTotals,
} from "../../../../../src/lib/view/run";

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

function approvalTone(approval: ApprovalDecision): { li: string; badge: string } {
  if (approval.outcome === "approved") {
    return approval.submitted === false
      ? { li: "tl-red", badge: "badge-red" }
      : { li: "tl-green", badge: "badge-green" };
  }
  if (approval.outcome === "rejected") return { li: "tl-red", badge: "badge-red" };
  return { li: "tl-yellow", badge: "badge-yellow" }; // approved_but_blocked, timeout
}

// i18n keys mirroring approvalDecisionLabel / forcedHoldLabel (src/lib/view/run.ts).
function approvalLabelKey(approval: ApprovalDecision): MessageKey {
  if (approval.outcome === "approved" && approval.submitted === false) {
    return "run.approval.submitfailed";
  }
  switch (approval.outcome) {
    case "approved":
      return "run.approval.approved";
    case "approved_but_blocked":
      return "run.approval.blocked";
    case "rejected":
      return "run.approval.rejected";
    case "timeout":
      return "run.approval.timeout";
  }
}

const HOLD_LABEL_KEYS = {
  budget_exhausted: "run.hold.budget",
  rate_limited: "run.hold.ratelimited",
  llm_failure: "run.hold.llm",
} as const;

function Table({
  head,
  cols,
  rows,
}: {
  head: string[];
  cols?: (string | undefined)[];
  rows: ReactNode[][];
}) {
  return (
    <div className="table-wrap">
      <table className="tbl">
        <thead>
          <tr>
            {head.map((h, i) => (
              <th key={h} className={cols?.[i] === "num" ? "num" : undefined}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((cells, i) => (
            <tr key={i}>
              {cells.map((cell, j) => (
                <td key={j} className={cols?.[j]}>
                  {cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function AnalystSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const { t } = useI18n();
  const summaries = trace?.analyst_summaries ?? proposal?.analyst_summaries;
  if (!summaries) return null;
  return (
    <section className="section">
      <h2 className="section-title">{t("run.analysts")}</h2>
      <div className="grid grid-3">
        {(["market", "news", "fundamental"] as const).map((role) => {
          const s = summaries[role];
          const degraded = isDegradedSummary(s);
          return (
            <div key={role} className="card">
              <h3 className="card-title">{role}</h3>
              <div className="row">
                <span className={`badge ${SIGNAL_TONES[s.signal] ?? "badge-neutral"}`}>
                  {s.signal}
                </span>
                <span className="faint mono small">{s.confidence}</span>
              </div>
              {degraded && <div className="banner banner-warn">{t("run.degraded")}</div>}
              <p className="small">{s.summary}</p>
            </div>
          );
        })}
      </div>
    </section>
  );
}

export function DebateSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const { t } = useI18n();
  const debateSummary = trace?.debate_summary ?? proposal?.debate_summary;
  if (!trace && debateSummary === undefined) return null;
  return (
    <section className="section">
      <h2 className="section-title">
        {t("run.debate")}
        {trace && <span className="count">{trace.debate_rounds.length}</span>}
      </h2>
      <div className="card">
        {trace?.debate_rounds.map((round) => (
          <div key={round.round_index}>
            <p>
              <strong>{t("run.round.bull", { n: round.round_index + 1 })}</strong>{" "}
              <span className="faint mono small">{t("run.score", { score: round.bull_score })}</span>:{" "}
              {round.bull_argument}
            </p>
            <p>
              <strong>{t("run.round.bear", { n: round.round_index + 1 })}</strong>{" "}
              <span className="faint mono small">{t("run.score", { score: round.bear_score })}</span>:{" "}
              {round.bear_argument}
            </p>
          </div>
        ))}
        {trace && trace.debate_rounds.length === 0 && (
          <p className="muted">{t("run.debate.empty")}</p>
        )}
        {debateSummary !== undefined && (
          <>
            {isDegradedDebate(debateSummary) && (
              <div className="banner banner-warn">{t("run.degraded")}</div>
            )}
            <p className="muted">
              <strong>{t("run.judge")}</strong> {debateSummary}
            </p>
          </>
        )}
      </div>
    </section>
  );
}

export function ProposalSection({ proposal }: { proposal: TradeProposal }) {
  const { t } = useI18n();
  const hold = forcedHoldKind(proposal);
  return (
    <section className="section">
      <h2 className="section-title">{t("run.trader")}</h2>
      <div className="card">
        <div className="row">
          <strong>{proposal.action}</strong>
          <span className="mono">{proposal.symbol}</span>
          {hold && <span className="badge badge-yellow">{t(HOLD_LABEL_KEYS[hold])}</span>}
        </div>
        <hr className="divider" />
        <dl className="kv">
          <dt>{t("run.k.confidence")}</dt>
          <dd className="mono">{proposal.confidence}</dd>
          <dt>{t("run.k.size")}</dt>
          <dd className="mono">{proposal.size_quote}</dd>
          <dt>{t("run.k.entry")}</dt>
          <dd>
            {proposal.entry.type}
            {proposal.entry.limit_price ? ` @ ${proposal.entry.limit_price}` : ""}
          </dd>
          <dt>{t("run.k.stoploss")}</dt>
          <dd className="mono">{proposal.stop_loss ?? "n/a"}</dd>
          <dt>{t("run.k.takeprofit")}</dt>
          <dd className="mono">{proposal.take_profit ?? "n/a"}</dd>
        </dl>
        <p>{proposal.reasoning}</p>
        <details>
          <summary className="muted small">{t("run.rawjson")}</summary>
          <pre className="codeblock">{JSON.stringify(proposal, null, 2)}</pre>
        </details>
      </div>
    </section>
  );
}

export function VerdictSection({ verdict }: { verdict: RiskVerdict }) {
  const { t } = useI18n();
  return (
    <section className="section">
      <h2 className="section-title">{t("run.verdict")}</h2>
      <div className="card">
        <div className="row">
          <span className={`badge ${DECISION_TONES[verdict.decision] ?? "badge-neutral"}`}>
            {verdict.decision}
          </span>
          {verdict.clipped_size_quote && (
            <span className="mono muted small">
              {t("run.clipped", { size: verdict.clipped_size_quote })}
            </span>
          )}
        </div>
        {verdict.reasons.length > 0 && (
          <ul>
            {verdict.reasons.map((reason) => (
              <li key={reason.code}>
                <code>{reason.code}</code>: {reason.message}
              </li>
            ))}
          </ul>
        )}
        <details>
          <summary className="muted small">{t("run.limitssnapshot")}</summary>
          <dl className="kv">
            {Object.entries(verdict.limits_snapshot).map(([field, value]) => (
              <Fragment key={field}>
                <dt className="mono">{field}</dt>
                <dd className="mono">{JSON.stringify(value)}</dd>
              </Fragment>
            ))}
          </dl>
        </details>
        <p className="faint small">
          {t("run.evaluatedat")} <span className="mono">{verdict.evaluated_at}</span>
        </p>
      </div>
    </section>
  );
}

export function CostsSection({ trace, proposal }: { trace: AgentTrace | null; proposal: TradeProposal | null }) {
  const { t } = useI18n();
  const costs = trace?.model_costs ?? proposal?.model_costs;
  if (!costs) return null;
  const estimatedNodes = new Set(trace?.estimated_cost_nodes ?? []);
  const totals = modelCostTotals(costs);
  return (
    <section className="section">
      <h2 className="section-title">
        {t("run.costs")} <span className="count">{costs.length}</span>
      </h2>
      <Table
        head={[
          t("run.costs.node"),
          t("run.costs.model"),
          t("run.costs.in"),
          t("run.costs.out"),
          t("run.costs.usd"),
        ]}
        cols={["mono-cell", "mono-cell", "num", "num", "num"]}
        rows={[
          ...costs.map((cost, i): ReactNode[] => [
            <span key={`n${i}`}>
              {cost.node}{" "}
              {estimatedNodes.has(cost.node) && (
                <span className="badge badge-yellow">{t("run.estimated")}</span>
              )}
            </span>,
            cost.model,
            String(cost.input_tokens),
            String(cost.output_tokens),
            cost.cost_usd,
          ]),
          [
            <strong key="t">{t("run.total")}</strong>,
            "",
            <strong key="ti">{String(totals.input_tokens)}</strong>,
            <strong key="to">{String(totals.output_tokens)}</strong>,
            <strong key="tc">{totals.cost_usd}</strong>,
          ],
        ]}
      />
      {estimatedNodes.size > 0 && (
        <p className="muted small">
          <span className="badge badge-yellow">{t("run.estimated")}</span>{" "}
          {t("run.estimated.note")}
        </p>
      )}
    </section>
  );
}

export function OrdersSection({ orders, fills }: { orders: Order[]; fills: Fill[] }) {
  const { t } = useI18n();
  return (
    <section className="section">
      <h2 className="section-title">
        {t("run.orders")} <span className="count">{orders.length}</span>
      </h2>
      {orders.length === 0 && (
        <div className="table-wrap">
          <div className="empty">{t("run.orders.empty")}</div>
        </div>
      )}
      {orders.length > 0 && (
        <Table
          head={[
            t("run.o.order"),
            t("run.o.origin"),
            t("run.o.class"),
            t("run.o.side"),
            t("run.o.type"),
            t("run.o.qty"),
            t("run.o.status"),
          ]}
          cols={["mono-cell", undefined, undefined, undefined, undefined, "num", undefined]}
          rows={orders.map((order) => [
            order.order_id,
            order.origin,
            order.class,
            order.side,
            order.type,
            order.qty_base,
            <span key={`${order.order_id}-s`} className="badge badge-neutral">
              {order.status}
            </span>,
          ])}
        />
      )}
      {fills.length > 0 && (
        <div className="section">
          <h3 className="section-title">
            {t("run.fills")} <span className="count">{fills.length}</span>
          </h3>
          <Table
            head={[
              t("run.f.fill"),
              t("run.o.order"),
              t("run.o.qty"),
              t("run.f.price"),
              t("run.f.fee"),
              t("run.f.at"),
            ]}
            cols={["mono-cell", "mono-cell", "num", "num", "num", "mono-cell"]}
            rows={fills.map((fill) => [
              fill.fill_id,
              fill.order_id,
              fill.qty_base,
              fill.fill_price,
              fill.fee_quote,
              fill.fill_ts,
            ])}
          />
        </div>
      )}
    </section>
  );
}

export function ApprovalsSection({ approvals }: { approvals: ApprovalDecision[] }) {
  const { t } = useI18n();
  if (approvals.length === 0) return null;
  return (
    <section className="section">
      <h2 className="section-title">
        {t("run.approvals")} <span className="count">{approvals.length}</span>
      </h2>
      <div className="card">
        <ul className="timeline">
          {approvals.map((approval) => {
            const tone = approvalTone(approval);
            return (
              <li key={approval.approval_id} className={tone.li}>
                <div className="row">
                  <span className={`badge ${tone.badge}`}>{approval.outcome}</span>
                  <span>{t(approvalLabelKey(approval))}</span>
                </div>
                <div className="small muted">
                  {t("run.by")} <span className="mono">{approval.decided_by}</span>{" "}
                  {t("run.at")} <span className="tl-time">{approval.decided_at}</span>
                </div>
                {approval.outcome === "approved_but_blocked" && approval.preflight_reasons && (
                  <ul className="small muted">
                    {approval.preflight_reasons.map((reason) => (
                      <li key={reason}>{reason}</li>
                    ))}
                  </ul>
                )}
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}
