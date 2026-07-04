"use client";

// Run detail = reasoning viewer: GET /api/v1/strategies/{id}/runs/{run_id}
// (proposal, verdict, trace, orders, fills, approvals embedded verbatim) plus
// the L1 approve/reject panel (POST to the same-origin approval proxy, which
// holds the server-only OPERATOR_TOKEN).

import Link from "next/link";
import { useParams } from "next/navigation";
import { useCallback, useState } from "react";

import {
  ApiError,
  buildApprovalPayload,
  fetchRunDetail,
  fetchStrategy,
  postApproval,
} from "../../../../../src/lib/api/client";
import type { ApprovalDecision } from "../../../../../src/lib/api/schema";
import { usePoll } from "../../../../../src/lib/api/usePoll";
import { isAdvisoryOnly, isPaperSimulated } from "../../../../../src/lib/view/run";
import { AdvisoryBanner, ErrorBanner, PaperBanner, StateBadge, card, mono } from "../../../ui";
import {
  AnalystSection,
  ApprovalsSection,
  CostsSection,
  DebateSection,
  OrdersSection,
  ProposalSection,
  VerdictSection,
} from "./sections";

export default function RunDetailPage() {
  const { id, runId } = useParams<{ id: string; runId: string }>();

  const loadStrategy = useCallback(() => fetchStrategy(id), [id]);
  const loadRun = useCallback(() => fetchRunDetail(id, runId), [id, runId]);
  const strategy = usePoll(loadStrategy);
  const run = usePoll(loadRun);

  const [busy, setBusy] = useState(false);
  const [decision, setDecision] = useState<ApprovalDecision | null>(null);
  const [decisionError, setDecisionError] = useState<string | null>(null);

  const decide = useCallback(
    async (verdictId: string, approved: boolean) => {
      setBusy(true);
      setDecisionError(null);
      try {
        setDecision(await postApproval(id, buildApprovalPayload(verdictId, approved)));
      } catch (err: unknown) {
        if (err instanceof ApiError && err.status === 409 && err.body?.recorded) {
          // Already decided (double-click or human-vs-timeout race): the 409
          // body carries the recorded outcome — show it, first decision wins.
          setDecision(err.body.recorded);
          setDecisionError(`Already decided: ${err.message}`);
        } else {
          setDecisionError(err instanceof Error ? err.message : String(err));
        }
      } finally {
        setBusy(false);
        run.refresh();
      }
    },
    [id, run],
  );

  const data = run.data;
  const recorded = decision ? [decision] : (data?.approvals ?? []);
  const pendingApproval = recorded.length === 0 ? (data?.pending_approval ?? null) : null;

  return (
    <>
      <p style={{ fontSize: "0.9rem" }}>
        <Link href={`/strategies/${id}`} style={{ color: "#0a5bd3", textDecoration: "none" }}>
          &larr; Strategy
        </Link>
      </p>
      {strategy.data && (
        <h1 style={{ fontSize: "1.4rem", display: "flex", alignItems: "center", gap: "0.75rem" }}>
          {strategy.data.name} &mdash; tick {data?.run.tick_number ?? "…"}{" "}
          <StateBadge state={strategy.data.lifecycle_state} />
        </h1>
      )}
      <p style={{ ...mono, color: "#555", fontSize: "0.85rem" }}>run {runId}</p>
      {strategy.data && isAdvisoryOnly(strategy.data.lifecycle_state) && <AdvisoryBanner />}
      {strategy.data && isPaperSimulated(strategy.data.lifecycle_state) && <PaperBanner />}
      {run.error && <ErrorBanner message={run.error} />}
      {!data && !run.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}

      {data && (
        <>
          {pendingApproval && (
            <section style={{ ...card, borderColor: "#9a6700" }}>
              <h2 style={{ fontSize: "1.1rem", marginTop: 0 }}>Pending L1 approval</h2>
              <p style={{ color: "#555" }}>
                Verdict <span style={mono}>{pendingApproval.verdict_id}</span> awaits a
                decision until <span style={mono}>{pendingApproval.deadline_at}</span> (no
                decision &rArr; auto-reject).
              </p>
              <div style={{ display: "flex", gap: "0.75rem" }}>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => decide(pendingApproval.verdict_id, true)}
                  style={{
                    background: "#1a7f37",
                    color: "#fff",
                    border: "none",
                    borderRadius: "4px",
                    padding: "0.4rem 1rem",
                    cursor: "pointer",
                  }}
                >
                  Approve
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => decide(pendingApproval.verdict_id, false)}
                  style={{
                    background: "#b3261e",
                    color: "#fff",
                    border: "none",
                    borderRadius: "4px",
                    padding: "0.4rem 1rem",
                    cursor: "pointer",
                  }}
                >
                  Reject
                </button>
              </div>
            </section>
          )}
          {decisionError && <ErrorBanner message={decisionError} />}
          <ApprovalsSection approvals={recorded} />

          <AnalystSection trace={data.trace} proposal={data.proposal} />
          <DebateSection trace={data.trace} proposal={data.proposal} />
          {data.proposal ? (
            <ProposalSection proposal={data.proposal} />
          ) : (
            <section style={card}>
              <h2 style={{ fontSize: "1.1rem", marginTop: 0 }}>Trader decision</h2>
              <p style={{ color: "#b3261e" }}>
                No proposal recorded for this run (the proposal POST failed after retries).
              </p>
            </section>
          )}
          {data.verdict && <VerdictSection verdict={data.verdict} />}
          <CostsSection trace={data.trace} proposal={data.proposal} />
          <OrdersSection orders={data.orders} fills={data.fills} />
        </>
      )}
    </>
  );
}
