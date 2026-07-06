"use client";

// Shared presentational pieces for the strategies / reasoning-viewer pages.
// Styling comes from the design system in app/globals.css (dark, dense).

import { hasNextPage, hasPrevPage, totalPages } from "../../src/lib/api/pagination";
import type { LifecycleState } from "../../src/lib/api/schema";
import { useI18n } from "../../src/lib/i18n";

// Legacy inline-style tokens kept for compatibility; new code uses classes.
export const section = { marginTop: "1.5rem" } as const;
export const card = {} as const;
export const mono = {
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
} as const;

const STATE_TONES: Record<LifecycleState, string> = {
  draft: "badge-neutral",
  paper: "badge-cyan",
  live_l1: "badge-green",
  live_l2: "badge-green",
  live_l3: "badge-green",
  paused: "badge-yellow",
  killed: "badge-red",
};

const STATE_LABELS: Record<LifecycleState, string> = {
  draft: "Draft",
  paper: "Paper",
  live_l1: "Live L1",
  live_l2: "Live L2",
  live_l3: "Live L3",
  paused: "Paused",
  killed: "Killed",
};

export function StateBadge({ state }: { state: LifecycleState }) {
  const live = state.startsWith("live_");
  return (
    <span className={`badge ${STATE_TONES[state]}`}>
      <span className={`dot${live ? " dot-live" : ""}`} />
      {STATE_LABELS[state]}
    </span>
  );
}

export function ErrorBanner({ message }: { message: string }) {
  return (
    <div className="banner banner-error" role="alert">
      <span aria-hidden>&#9888;</span>
      <span>{message}</span>
    </div>
  );
}

// Draft / advisory: proposals and verdicts are persisted and shown;
// nothing is ever submitted to any OMS.
export function AdvisoryBanner() {
  const { t } = useI18n();
  return (
    <div className="banner banner-info">
      <span aria-hidden>&#9432;</span>
      <span>{t("ui.banner.advisory")}</span>
    </div>
  );
}

// Paper simulation: approve/clip verdicts auto-execute on the paper OMS
// (persistence-and-api.md §L0 / L1 execution semantics); no exchange orders.
export function PaperBanner() {
  const { t } = useI18n();
  return (
    <div className="banner banner-info">
      <span aria-hidden>&#9432;</span>
      <span>{t("ui.banner.paper")}</span>
    </div>
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
  const { t } = useI18n();
  return (
    <div className="pager">
      <button
        type="button"
        className="btn"
        disabled={!hasPrevPage(page)}
        aria-label={t("ui.pager.prev.label")}
        onClick={() => onPage(page - 1)}
      >
        {t("ui.pager.prev")}
      </button>
      <span>
        {t("ui.pager.page", { page, pages: totalPages(total, limit) })}
        <span className="faint"> &middot; {t("ui.pager.total", { total })}</span>
      </span>
      <button
        type="button"
        className="btn"
        disabled={!hasNextPage(page, total, limit)}
        aria-label={t("ui.pager.next.label")}
        onClick={() => onPage(page + 1)}
      >
        {t("ui.pager.next")}
      </button>
    </div>
  );
}
