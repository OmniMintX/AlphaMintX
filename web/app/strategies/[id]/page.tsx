"use client";

// Strategy detail: lifecycle state badge + paginated runs list
// (GET /api/v1/strategies/{id} and .../runs?page&limit, tick_number DESC).

import Link from "next/link";
import { useParams } from "next/navigation";
import { useCallback, useState } from "react";

import { fetchRuns, fetchStrategy } from "../../../src/lib/api/client";
import { usePoll } from "../../../src/lib/api/usePoll";
import { isAdvisoryOnly, isPaperSimulated } from "../../../src/lib/view/run";
import { AdvisoryBanner, ErrorBanner, Pager, PaperBanner, StateBadge, card, mono, section } from "../ui";

export default function StrategyDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [page, setPage] = useState(1);

  const loadStrategy = useCallback(() => fetchStrategy(id), [id]);
  const loadRuns = useCallback(() => fetchRuns(id, page), [id, page]);
  const strategy = usePoll(loadStrategy);
  const runs = usePoll(loadRuns);

  return (
    <>
      <p style={{ fontSize: "0.9rem" }}>
        <Link href="/strategies" style={{ color: "#0a5bd3", textDecoration: "none" }}>
          &larr; Strategies
        </Link>
      </p>
      {strategy.error && <ErrorBanner message={strategy.error} />}
      {!strategy.data && !strategy.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
      {strategy.data && (
        <>
          <h1 style={{ fontSize: "1.4rem", display: "flex", alignItems: "center", gap: "0.75rem" }}>
            {strategy.data.name} <StateBadge state={strategy.data.lifecycle_state} />
          </h1>
          <p style={{ ...mono, color: "#555", fontSize: "0.85rem" }}>
            {strategy.data.strategy_id}
          </p>
          {isAdvisoryOnly(strategy.data.lifecycle_state) && <AdvisoryBanner />}
          {isPaperSimulated(strategy.data.lifecycle_state) && <PaperBanner />}
        </>
      )}

      <section style={section}>
        <h2 style={{ fontSize: "1.1rem" }}>Runs</h2>
        {runs.error && <ErrorBanner message={runs.error} />}
        {!runs.data && !runs.error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
        {runs.data && (
          <>
            <div style={card}>
              {runs.data.items.length === 0 && <p style={{ color: "#555" }}>No runs yet.</p>}
              {runs.data.items.map((run) => (
                <div
                  key={run.run_id}
                  style={{
                    display: "flex",
                    alignItems: "baseline",
                    gap: "0.75rem",
                    padding: "0.4rem 0",
                  }}
                >
                  <Link
                    href={`/strategies/${id}/runs/${run.run_id}`}
                    style={{ color: "#0a5bd3", textDecoration: "none", fontWeight: 600 }}
                  >
                    Tick {run.tick_number}
                  </Link>
                  <span style={{ ...mono, color: "#555", fontSize: "0.85rem" }}>{run.run_id}</span>
                  <span style={{ color: "#555", fontSize: "0.85rem" }}>
                    {run.completed_at ? `completed ${run.completed_at}` : "in progress"}
                  </span>
                </div>
              ))}
            </div>
            <Pager page={runs.data.page} total={runs.data.total} limit={runs.data.limit} onPage={setPage} />
          </>
        )}
      </section>
    </>
  );
}
