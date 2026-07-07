// @vitest-environment jsdom

// Arena leaderboard table: decimal-string sign coloring, the null model /
// profit-factor display rules (∞ only when realized PnL is positive), the
// strategy detail + reasoning links, checkbox selection wiring, and the
// equity-delta trade-marker derivation. Rendered bare — useI18n's default
// context is the "en" catalog, no provider needed.

import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { LeaderboardItem } from "../../src/lib/api/schema";
import { LeaderboardTable, decimalSign, deriveEquityMarkers, profitFactorLabel } from "./ui";

afterEach(cleanup);

const winner: LeaderboardItem = {
  rank: 1,
  strategy_id: "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e",
  name: "BTC momentum",
  tenant_id: "tenant-1",
  lifecycle_state: "paper",
  model: "gpt-4o",
  seed: "10000",
  equity: "10012.3",
  realized_pnl: "12.3",
  return_pct: "0.123",
  max_drawdown_pct: "1.2",
  closed_trades: 3,
  win_rate_pct: "66.67",
  profit_factor: null,
  last_fill_at: "2026-07-04T12:00:00Z",
};

const loser: LeaderboardItem = {
  ...winner,
  rank: 2,
  strategy_id: "c3d4e5f6-a7b8-4c9d-8e0f-2a3b4c5d6e7f",
  name: "ETH revert",
  model: null,
  equity: "9996.5",
  realized_pnl: "-3.5",
  return_pct: "-0.035",
  profit_factor: null,
  last_fill_at: null,
};

describe("decimalSign / profitFactorLabel", () => {
  it("signs decimal strings without float conversion", () => {
    expect(decimalSign("12.3")).toBe(1);
    expect(decimalSign("-3.5")).toBe(-1);
    expect(decimalSign("0")).toBe(0);
    expect(decimalSign("0.00")).toBe(0);
  });

  it("renders ∞ only for a null factor with positive realized PnL", () => {
    expect(profitFactorLabel(null, "12.3")).toBe("\u221E");
    expect(profitFactorLabel(null, "-3.5")).toBe("\u2014");
    expect(profitFactorLabel(null, "0")).toBe("\u2014");
    expect(profitFactorLabel("2.5", "12.3")).toBe("2.5");
  });
});

describe("deriveEquityMarkers", () => {
  it("marks each non-seed point by its equity delta sign", () => {
    const markers = deriveEquityMarkers([
      { time: 100, value: 10000 },
      { time: 200, value: 10010 },
      { time: 300, value: 9990 },
      { time: 400, value: 9990 },
    ]);
    expect(markers).toHaveLength(3);
    expect(markers[0]).toMatchObject({ time: 200, position: "belowBar", shape: "arrowUp" });
    expect(markers[1]).toMatchObject({ time: 300, position: "aboveBar", shape: "arrowDown" });
    expect(markers[2]).toMatchObject({ time: 400, position: "inBar", shape: "circle" });
  });

  it("returns no markers for an empty or seed-only curve", () => {
    expect(deriveEquityMarkers([])).toEqual([]);
    expect(deriveEquityMarkers([{ time: 100, value: 10000 }])).toEqual([]);
  });
});

describe("LeaderboardTable", () => {
  it("renders rows with sign-toned returns, model badge / — , and detail links", () => {
    render(<LeaderboardTable items={[winner, loser]} selected={new Set()} onToggle={() => {}} />);
    expect(screen.getByRole("link", { name: "BTC momentum" })).toHaveAttribute(
      "href",
      `/strategies/${winner.strategy_id}`,
    );
    expect(screen.getByText("0.123%")).toHaveClass("up");
    expect(screen.getByText("-0.035%")).toHaveClass("down");
    expect(screen.getByText("gpt-4o")).toHaveClass("badge");
    // Null profit factor: ∞ for the profitable row, — for the losing one.
    expect(screen.getByText("\u221E")).toBeInTheDocument();
    expect(screen.getByText("2026-07-04 12:00")).toBeInTheDocument();
  });

  it("links each row to the reasoning explorer preselecting the strategy", () => {
    render(<LeaderboardTable items={[winner, loser]} selected={new Set()} onToggle={() => {}} />);
    expect(screen.getByRole("link", { name: "Reasoning for BTC momentum" })).toHaveAttribute(
      "href",
      `/reasoning?strategy=${winner.strategy_id}`,
    );
    expect(screen.getByRole("link", { name: "Reasoning for ETH revert" })).toHaveAttribute(
      "href",
      `/reasoning?strategy=${loser.strategy_id}`,
    );
  });

  it("checks selected rows and reports toggles by strategy id", async () => {
    const user = userEvent.setup();
    const onToggle = vi.fn();
    render(
      <LeaderboardTable
        items={[winner, loser]}
        selected={new Set([winner.strategy_id])}
        onToggle={onToggle}
      />,
    );
    expect(screen.getByRole("checkbox", { name: "Chart BTC momentum" })).toBeChecked();
    const loserBox = screen.getByRole("checkbox", { name: "Chart ETH revert" });
    expect(loserBox).not.toBeChecked();
    await user.click(loserBox);
    expect(onToggle).toHaveBeenCalledWith(loser.strategy_id);
  });
});
