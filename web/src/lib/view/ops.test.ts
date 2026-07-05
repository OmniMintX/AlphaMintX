// Ops-panel view logic (operator-surface.md OS-23..OS-29): the FULL pinned
// lifecycle display table incl. the killed-resume exception and the
// disabled-when-null resume, kill-banner selection, watchdog labels, and the
// pinned paper-gate 429 degradation.

import { describe, expect, it } from "vitest";

import type { BoundKill } from "../api/schema";
import {
  defaultFlatten,
  formatDetailsJson,
  legalActions,
  newestUnclearedStrategyKill,
  paperGateView,
  resumeTarget,
  unclearedKills,
  watchdogView,
  WATCHDOG_STALE_SECONDS,
} from "./ops";

function kill(overrides: Partial<BoundKill>): BoundKill {
  return {
    event_id: "e1f2a3b4-c5d6-4e7f-8a9b-0c1d2e3f4a5b",
    scope: "strategy",
    kill_epoch: 1,
    flatten: false,
    actor_id: "admin-1",
    recorded_at: "2026-07-04T12:00:00Z",
    cleared: null,
    ...overrides,
  };
}

const cleared = {
  clear_id: "a3b4c5d6-e7f8-4a9b-8c0d-2e3f4a5b6c7d",
  actor_id: "admin-1",
  reason: "resolved",
  recorded_at: "2026-07-04T13:00:00Z",
  cleared_epoch: 2,
};

describe("legalActions — the FULL OS-26 display table", () => {
  const row = (verb: string, to: string | null) => expect.objectContaining({ verb, to });

  it("renders exactly the pinned verbs per state", () => {
    expect(legalActions("draft", null)).toEqual([row("activate", "paper")]);
    expect(legalActions("paper", null)).toEqual([
      row("pause", "paused"),
      row("promote", "live_l1"),
    ]);
    expect(legalActions("live_l1", null)).toEqual([
      row("pause", "paused"),
      row("promote", "live_l2"),
      row("demote", "paper"),
    ]);
    expect(legalActions("live_l2", null)).toEqual([
      row("pause", "paused"),
      row("promote", "live_l3"),
      row("demote", "live_l1"),
      row("demote", "paper"),
    ]);
    expect(legalActions("live_l3", null)).toEqual([
      row("pause", "paused"),
      row("demote", "live_l2"),
      row("demote", "paper"),
    ]);
    expect(legalActions("killed", null)).toEqual([row("unlock", "paper")]);
  });

  it("resume targets the server-reported paused_from", () => {
    expect(legalActions("paused", "live_l2")).toEqual([row("resume", "live_l2")]);
    expect(legalActions("paused", "paper")).toEqual([row("resume", "paper")]);
  });

  it("killed-resume exception: paused_from killed resumes to paper, never killed", () => {
    expect(legalActions("paused", "killed")).toEqual([row("resume", "paper")]);
    expect(resumeTarget("killed")).toBe("paper");
  });

  it("resume is disabled (to: null) when paused_from is null", () => {
    expect(legalActions("paused", null)).toEqual([row("resume", null)]);
    expect(legalActions("paused", null)[0]?.confirm).toBe(false);
  });

  it("requires confirm exactly for transitions INTO live_* states", () => {
    const confirms = (state: Parameters<typeof legalActions>[0], from: string | null = null) =>
      legalActions(state, from as never).map((a) => [a.label, a.confirm]);
    expect(confirms("paper")).toEqual([
      ["pause to paused", false],
      ["promote to live_l1", true],
    ]);
    expect(confirms("live_l2")).toEqual([
      ["pause to paused", false],
      ["promote to live_l3", true],
      ["demote to live_l1", true],
      ["demote to paper", false],
    ]);
    expect(legalActions("paused", "live_l1")[0]?.confirm).toBe(true);
    expect(legalActions("killed", null)[0]?.confirm).toBe(false);
  });
});

describe("defaultFlatten (OS-28)", () => {
  it("defaults checked for live_* and unchecked otherwise", () => {
    expect(defaultFlatten("live_l1")).toBe(true);
    expect(defaultFlatten("live_l3")).toBe(true);
    expect(defaultFlatten("paper")).toBe(false);
    expect(defaultFlatten("killed")).toBe(false);
  });
});

describe("kill selection (OS-23/OS-29)", () => {
  it("picks the NEWEST uncleared strategy-scope kill for the clear control", () => {
    const kills = [
      kill({ kill_epoch: 2 }),
      kill({ event_id: "f2a3b4c5-d6e7-4f8a-9b0c-1d2e3f4a5b6c", kill_epoch: 5 }),
      kill({ event_id: "b4c5d6e7-f8a9-4b0c-8d1e-3f4a5b6c7d8e", scope: "tenant", kill_epoch: 9 }),
    ];
    expect(newestUnclearedStrategyKill(kills)?.kill_epoch).toBe(5);
  });

  it("offers no clear for tenant/platform or already-cleared kills", () => {
    expect(newestUnclearedStrategyKill([kill({ scope: "platform" })])).toBeNull();
    expect(newestUnclearedStrategyKill([kill({ cleared })])).toBeNull();
    expect(unclearedKills([kill({ cleared }), kill({ kill_epoch: 3 })])).toHaveLength(1);
  });
});

describe("watchdogView (OS-23, invariant 7)", () => {
  it("renders off / no-beat / ok / stale without fabricating liveness", () => {
    expect(watchdogView({ enabled: false, last_heartbeat_at: null, seconds_since: null }).tone).toBe("off");
    expect(watchdogView({ enabled: true, last_heartbeat_at: null, seconds_since: null })).toEqual({
      tone: "none",
      label: "no heartbeat observed",
    });
    const beat = { enabled: true, last_heartbeat_at: "2026-07-04T12:00:00Z" };
    expect(watchdogView({ ...beat, seconds_since: WATCHDOG_STALE_SECONDS }).tone).toBe("ok");
    expect(watchdogView({ ...beat, seconds_since: WATCHDOG_STALE_SECONDS + 1 }).tone).toBe("stale");
  });
});

describe("formatDetailsJson (OS-24)", () => {
  it("pretty-prints valid JSON and returns the raw text on parse failure", () => {
    expect(formatDetailsJson('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(formatDetailsJson("not json {")).toBe("not json {");
  });
});

describe("paperGateView (OS-25 pinned degradation)", () => {
  const report = {
    passed: true,
    window_started_at: null,
    evaluated_at: "2026-07-04T12:00:00Z",
    conditions: [],
  };

  it("keeps the last-rendered report with a rate-limited note on a 429", () => {
    expect(paperGateView(report, 429)).toEqual({ report, rateLimited: true });
  });

  it("does not flag ordinary errors as rate limiting", () => {
    expect(paperGateView(report, 500)).toEqual({ report, rateLimited: false });
    // Non-ApiError failures (network, parse) carry no status.
    expect(paperGateView(null, null)).toEqual({ report: null, rateLimited: false });
  });
});
