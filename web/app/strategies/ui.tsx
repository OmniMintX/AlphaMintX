"use client";

// Shared presentational pieces for the strategies / reasoning-viewer pages.
// Styling matches the existing inline-style conventions (app/page.tsx).

import { hasNextPage, hasPrevPage, totalPages } from "../../src/lib/api/pagination";
import type { LifecycleState } from "../../src/lib/api/schema";

export const section = { marginTop: "1.5rem" } as const;
export const card = {
  background: "#fff",
  border: "1px solid #e0e0e0",
  borderRadius: "6px",
  padding: "1rem 1.25rem",
  marginTop: "0.75rem",
} as const;
export const mono = {
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
} as const;

const STATE_COLORS: Record<LifecycleState, { bg: string; fg: string }> = {
  draft: { bg: "#f0f0f0", fg: "#555" },
  paper: { bg: "#e7f0fd", fg: "#0a5bd3" },
  live_l1: { bg: "#e6f4ea", fg: "#1a7f37" },
  live_l2: { bg: "#e6f4ea", fg: "#1a7f37" },
  live_l3: { bg: "#e6f4ea", fg: "#1a7f37" },
  paused: { bg: "#fdf3e0", fg: "#9a6700" },
  killed: { bg: "#fbe9e7", fg: "#b3261e" },
};

export function StateBadge({ state }: { state: LifecycleState }) {
  const colors = STATE_COLORS[state];
  return (
    <span
      style={{
        ...mono,
        background: colors.bg,
        color: colors.fg,
        borderRadius: "4px",
        padding: "0.1rem 0.45rem",
        fontSize: "0.85rem",
      }}
    >
      {state}
    </span>
  );
}

export function ErrorBanner({ message }: { message: string }) {
  return (
    <p
      style={{
        background: "#fbe9e7",
        border: "1px solid #b3261e",
        borderRadius: "6px",
        color: "#b3261e",
        padding: "0.6rem 1rem",
      }}
    >
      {message}
    </p>
  );
}

// Draft / advisory: proposals and verdicts are persisted and shown;
// nothing is ever submitted to any OMS.
export function AdvisoryBanner() {
  return (
    <p
      style={{
        background: "#e7f0fd",
        border: "1px solid #0a5bd3",
        borderRadius: "6px",
        color: "#0a5bd3",
        padding: "0.6rem 1rem",
      }}
    >
      Advisory only: proposals and verdicts are shown here but never
      submitted to the OMS.
    </p>
  );
}

// Paper simulation: approve/clip verdicts auto-execute on the paper OMS
// (persistence-and-api.md §L0 / L1 execution semantics); no exchange orders.
export function PaperBanner() {
  return (
    <p
      style={{
        background: "#e7f0fd",
        border: "1px solid #0a5bd3",
        borderRadius: "6px",
        color: "#0a5bd3",
        padding: "0.6rem 1rem",
      }}
    >
      Paper trading: approved verdicts execute against the paper OMS
      (simulated fills); nothing reaches a live exchange.
    </p>
  );
}

export function Pager({
  page,
  total,
  limit,
  onPage,
}: {
  page: number;
  total: number;
  limit: number;
  onPage: (page: number) => void;
}) {
  const button = {
    border: "1px solid #e0e0e0",
    borderRadius: "4px",
    background: "#fff",
    padding: "0.2rem 0.6rem",
    cursor: "pointer",
  } as const;
  return (
    <div
      style={{
        display: "flex",
        alignItems: "center",
        gap: "0.75rem",
        marginTop: "0.75rem",
        fontSize: "0.9rem",
      }}
    >
      <button
        type="button"
        style={button}
        disabled={!hasPrevPage(page)}
        onClick={() => onPage(page - 1)}
      >
        Prev
      </button>
      <span style={{ color: "#555" }}>
        Page {page} of {totalPages(total, limit)} ({total} total)
      </span>
      <button
        type="button"
        style={button}
        disabled={!hasNextPage(page, total, limit)}
        onClick={() => onPage(page + 1)}
      >
        Next
      </button>
    </div>
  );
}
