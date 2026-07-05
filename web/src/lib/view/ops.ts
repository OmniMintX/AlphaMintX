// Pure view-model helpers for the ops panel (operator-surface.md OS-23..OS-29):
// the PINNED lifecycle display table (OS-26, a deliberate v1 subset — the
// server remains the sole transition authority), the killed-resume exception,
// kill-banner selection, watchdog liveness labels (WD-15 threshold,
// display-only), and defensive details_json formatting.

import type { BoundKill, LifecycleState, PaperGateReport, SafetyStatus } from "../api/schema";

export function isLiveState(state: LifecycleState): boolean {
  return state.startsWith("live_");
}

// One rendered lifecycle button. `to: null` means rendered-but-disabled
// (resume with unknown provenance, OS-26). `confirm` marks transitions INTO a
// live_* state, which require an explicit confirm step before the POST.
export interface OpsAction {
  verb: "activate" | "pause" | "resume" | "promote" | "demote" | "unlock";
  to: LifecycleState | null;
  label: string;
  confirm: boolean;
}

function action(verb: OpsAction["verb"], to: LifecycleState | null): OpsAction {
  return {
    verb,
    to,
    label: to ? `${verb} to ${to}` : verb,
    confirm: to !== null && isLiveState(to),
  };
}

// Resume rule (OS-26): `to` = the server-reported paused_from — never
// re-derived — EXCEPT paused_from "killed", whose sole paused-exit is paper.
// `to: "killed"` is never sent. null provenance ⇒ null (button disabled).
export function resumeTarget(pausedFrom: LifecycleState | null): LifecycleState | null {
  if (pausedFrom === null) return null;
  return pausedFrom === "killed" ? "paper" : pausedFrom;
}

// The OS-26 display table verbatim: single-step promotion, single-step
// demotion plus demote-to-paper, flat unlock only. Multi-step promotes,
// skip-demote, and killed → paused are deliberately not rendered.
export function legalActions(
  state: LifecycleState,
  pausedFrom: LifecycleState | null,
): OpsAction[] {
  switch (state) {
    case "draft":
      return [action("activate", "paper")];
    case "paper":
      return [action("pause", "paused"), action("promote", "live_l1")];
    case "live_l1":
      return [action("pause", "paused"), action("promote", "live_l2"), action("demote", "paper")];
    case "live_l2":
      return [
        action("pause", "paused"),
        action("promote", "live_l3"),
        action("demote", "live_l1"),
        action("demote", "paper"),
      ];
    case "live_l3":
      return [action("pause", "paused"), action("demote", "live_l2"), action("demote", "paper")];
    case "paused":
      return [action("resume", resumeTarget(pausedFrom))];
    case "killed":
      return [action("unlock", "paper")];
  }
}

// Kill-flatten default (OS-28, safety-wiring.md §Flatten choice): checked for
// a displayed live_* state, unchecked otherwise; the wire default stays false.
export function defaultFlatten(state: LifecycleState): boolean {
  return isLiveState(state);
}

export function unclearedKills(kills: readonly BoundKill[]): BoundKill[] {
  return kills.filter((k) => k.cleared === null);
}

// The clear control's CAS source (OS-29): the NEWEST uncleared strategy-scope
// kill as displayed — its kill_epoch is the observed_epoch, never a guess.
// Tenant/platform kills carry no clear control here (other tiers).
export function newestUnclearedStrategyKill(kills: readonly BoundKill[]): BoundKill | null {
  const candidates = unclearedKills(kills).filter((k) => k.scope === "strategy");
  if (candidates.length === 0) return null;
  return candidates.reduce((newest, k) => (k.kill_epoch > newest.kill_epoch ? k : newest));
}

// WD-15 staleness threshold, display-only (OS-23).
export const WATCHDOG_STALE_SECONDS = 90;

export interface WatchdogView {
  tone: "off" | "none" | "ok" | "stale";
  label: string;
}

// Never fabricate liveness (invariant 7): a missing beat renders as missing,
// never as a baseline-derived timestamp.
export function watchdogView(watchdog: SafetyStatus["watchdog"]): WatchdogView {
  if (!watchdog.enabled) return { tone: "off", label: "watchdog off" };
  if (watchdog.last_heartbeat_at === null || watchdog.seconds_since === null) {
    return { tone: "none", label: "no heartbeat observed" };
  }
  const stale = watchdog.seconds_since > WATCHDOG_STALE_SECONDS;
  return {
    tone: stale ? "stale" : "ok",
    label: `last heartbeat ${watchdog.seconds_since}s ago${stale ? " (stale)" : ""}`,
  };
}

// details_json is stored TEXT rendered verbatim (OS-24): pretty-print when it
// parses as JSON, raw on parse failure — never dropped.
export function formatDetailsJson(details: string): string {
  try {
    return JSON.stringify(JSON.parse(details), null, 2);
  } catch {
    return details;
  }
}

export interface PaperGateView {
  report: PaperGateReport | null;
  rateLimited: boolean;
}

// Paper-gate degradation is PINNED (OS-25): on a 429 keep the last-rendered
// report and show a rate-limited note; the poll interval is never tightened.
// Detection is the structured HTTP status (usePoll's errorStatus), never a
// substring match over the error message.
export function paperGateView(
  last: PaperGateReport | null,
  errorStatus: number | null,
): PaperGateView {
  return {
    report: last,
    rateLimited: errorStatus === 429,
  };
}
