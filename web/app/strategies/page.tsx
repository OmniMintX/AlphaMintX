"use client";

// Strategy list: GET /api/v1/strategies, paginated {items,total,page,limit},
// revalidated on a simple polling interval (SSE/websocket is deferred).

import Link from "next/link";
import { useCallback, useState } from "react";

import { fetchStrategies } from "../../src/lib/api/client";
import { usePoll } from "../../src/lib/api/usePoll";
import { ErrorBanner, Pager, StateBadge, card, mono } from "./ui";

export default function StrategiesPage() {
  const [page, setPage] = useState(1);
  const load = useCallback(() => fetchStrategies(page), [page]);
  const { data, error } = usePoll(load);

  return (
    <>
      <h1 style={{ fontSize: "1.4rem" }}>Strategies</h1>
      <p style={{ color: "#555" }}>
        Lifecycle state per strategy; open one for its runs and reasoning traces.
      </p>
      {error && <ErrorBanner message={error} />}
      {!data && !error && <p style={{ color: "#555" }}>Loading&hellip;</p>}
      {data && (
        <>
          <div style={card}>
            {data.items.length === 0 && <p style={{ color: "#555" }}>No strategies yet.</p>}
            {data.items.map((strategy) => (
              <div
                key={strategy.strategy_id}
                style={{
                  display: "flex",
                  alignItems: "baseline",
                  gap: "0.75rem",
                  padding: "0.4rem 0",
                }}
              >
                <Link
                  href={`/strategies/${strategy.strategy_id}`}
                  style={{ color: "#0a5bd3", textDecoration: "none", fontWeight: 600 }}
                >
                  {strategy.name}
                </Link>
                <StateBadge state={strategy.lifecycle_state} />
                <span style={{ ...mono, color: "#555", fontSize: "0.85rem" }}>
                  {strategy.strategy_id}
                </span>
              </div>
            ))}
          </div>
          <Pager page={data.page} total={data.total} limit={data.limit} onPage={setPage} />
        </>
      )}
    </>
  );
}
