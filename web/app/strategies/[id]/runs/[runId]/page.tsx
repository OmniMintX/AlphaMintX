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
import { AdvisoryBanner, ErrorBanner, PaperBanner, StateBadge } from "../../../ui";
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
      <nav className="breadcrumbs">
        <Link href="/strategies">Strategies</Link>
        <span className="sep">/</span>
        <Link href={`/strategies/${id}`} className="mono">
          {id.slice(0, 8)}
        </Link>
        <span className="sep">/</span>
        <span>Tick {data?.run.tick_number ?? "…"}</span>
      </nav>
      <header className="page-head">
        <h1 className="page-title">
          Tick {data?.run.tick_number ?? "…"}
          {strategy.data && <StateBadge state={strategy.data.lifecycle_state} />}
        </h1>
        <p className="page-sub mono faint">run {runId}</p>
      </header>
      {strategy.data && isAdvisoryOnly(strategy.data.lifecycle_state) && <AdvisoryBanner />}
      {strategy.data && isPaperSimulated(strategy.data.lifecycle_state) && <PaperBanner />}
      {run.error && <ErrorBanner message={run.error} />}
      {!data && !run.error && <p className="muted">Loading&hellip;</p>}

      {data && (
        <>
          {pendingApproval && (
            <section className="section">
              <h2 className="section-title">Pending L1 approval</h2>
              <div className="card">
                <p className="muted">
                  Verdict <span className="mono">{pendingApproval.verdict_id}</span> awaits a
                  decision until <span className="mono">{pendingApproval.deadline_at}</span> (no
                  decision &rArr; auto-reject).
                </p>
                <div className="row">
                  <button
                    type="button"
                    className="btn btn-primary"
                    disabled={busy}
                    onClick={() => decide(pendingApproval.verdict_id, true)}
                  >
                    Approve
                  </button>
                  <button
                    type="button"
                    className="btn btn-danger"
                    disabled={busy}
                    onClick={() => decide(pendingApproval.verdict_id, false)}
                  >
                    Reject
                  </button>
                </div>
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
            <section className="section">
              <h2 className="section-title">Trader decision</h2>
              <div className="card">
                <div className="banner banner-error">
                  No proposal recorded for this run (the proposal POST failed after retries).
                </div>
              </div>
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
