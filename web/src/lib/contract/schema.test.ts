// Golden-fixture contract tests: same fixtures, same expectations as the Go and
// Python planes (valid fixtures parse; each proposal_invalid_* fixture fails on
// its single rule under test and nothing else).

import { describe, expect, it } from "vitest";

import proposalOpenLong from "../../../../contracts/fixtures/proposal_open_long.json";
import proposalHold from "../../../../contracts/fixtures/proposal_hold.json";
import proposalDecimalEdges from "../../../../contracts/fixtures/proposal_decimal_edges.json";
import proposalInvalidNoSl from "../../../../contracts/fixtures/proposal_invalid_no_sl.json";
import proposalInvalidNumericSize from "../../../../contracts/fixtures/proposal_invalid_numeric_size.json";
import verdictClip from "../../../../contracts/fixtures/verdict_clip.json";
import verdictRejectDailyLoss from "../../../../contracts/fixtures/verdict_reject_daily_loss.json";
import { riskVerdictSchema, tradeProposalSchema, utcTimestamp } from "./schema";

describe("TradeProposal golden fixtures", () => {
  it("parses proposal_open_long.json", () => {
    const proposal = tradeProposalSchema.parse(proposalOpenLong);
    expect(proposal.action).toBe("open_long");
    expect(proposal.entry.type).toBe("limit");
    expect(proposal.entry.limit_price).toBe("64250.50");
    expect(proposal.stop_loss).toBe("62965.49");
    expect(proposal.take_profit).toBe("66820.52");
    expect(proposal.model_costs).toHaveLength(4);
  });

  it("parses proposal_hold.json", () => {
    const proposal = tradeProposalSchema.parse(proposalHold);
    expect(proposal.action).toBe("hold");
    expect(proposal.size_quote).toBe("0");
    expect(proposal.stop_loss).toBeUndefined();
    expect(proposal.take_profit).toBeUndefined();
    expect(proposal.model_costs).toEqual([]);
  });

  it("parses proposal_decimal_edges.json preserving boundary string forms", () => {
    const proposal = tradeProposalSchema.parse(proposalDecimalEdges);
    expect(proposal.size_quote).toBe("10000.0000000000000000000000000001");
    expect(proposal.size_quote).toHaveLength(34);
    expect(proposal.entry.limit_price).toBe("64000");
    expect(proposal.stop_loss).toBe("0.00000001");
    expect(proposal.take_profit).toBe("9999999999999999999999999999999999");
    expect(proposal.model_costs[0]?.cost_usd).toBe("0");
  });

  it("rejects proposal_invalid_no_sl.json with exactly one stop_loss issue", () => {
    const result = tradeProposalSchema.safeParse(proposalInvalidNoSl);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["stop_loss"]);
    expect(result.error.issues[0]?.message).toContain("stop_loss");
  });

  it("rejects proposal_invalid_numeric_size.json with exactly one size_quote issue", () => {
    const result = tradeProposalSchema.safeParse(proposalInvalidNumericSize);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["size_quote"]);
    expect(result.error.issues[0]?.message).toContain("string");
  });

  it('rejects open_long with size_quote "0"', () => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["size_quote"] = "0";
    const result = tradeProposalSchema.safeParse(mutated);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["size_quote"]);
    expect(result.error.issues[0]?.message).toContain("size_quote must be > 0");
  });

  it("rejects open_long limit entry with stop_loss at or above limit_price", () => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["stop_loss"] = "64250.50"; // == entry.limit_price
    const result = tradeProposalSchema.safeParse(mutated);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["stop_loss"]);
    expect(result.error.issues[0]?.message).toContain("must be below entry");
  });

  it("rejects open_short with inverted stop and take-profit", () => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["action"] = "open_short";
    // Fixture stop 62965.49 sits below the 64250.50 limit entry and take_profit
    // 66820.52 above it: both directions are inverted for a short.
    const result = tradeProposalSchema.safeParse(mutated);
    expect(result.success).toBe(false);
    if (result.success) return;
    const paths = result.error.issues.map((issue) => issue.path.join("."));
    expect(paths).toContain("stop_loss");
    expect(paths).toContain("take_profit");
  });

  it("rejects hold with non-zero size_quote", () => {
    const mutated = structuredClone(proposalHold) as Record<string, unknown>;
    mutated["size_quote"] = "123.45";
    const result = tradeProposalSchema.safeParse(mutated);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["size_quote"]);
    expect(result.error.issues[0]?.message).toBe('size_quote must be "0" for hold');
  });

  it("rejects unknown schema_version", () => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["schema_version"] = "2.0";
    expect(tradeProposalSchema.safeParse(mutated).success).toBe(false);
  });

  it("rejects unknown fields", () => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["leverage"] = "10";
    expect(tradeProposalSchema.safeParse(mutated).success).toBe(false);
  });
});

describe("code-point length caps", () => {
  // JSON Schema maxLength counts Unicode code points; naive UTF-16 counting
  // double-counts astral characters ("😀" is 1 code point, 2 UTF-16 units),
  // rejecting schema-valid text the Go and Python planes accept.
  const withReasoning = (reasoning: string) => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    mutated["reasoning"] = reasoning;
    return mutated;
  };

  const withNode = (node: string) => {
    const mutated = structuredClone(proposalOpenLong) as Record<string, unknown>;
    const costs = mutated["model_costs"] as Array<Record<string, unknown>>;
    costs[0] = { ...costs[0], node };
    return mutated;
  };

  it('accepts reasoning of exactly 8000 "ệ" code points', () => {
    expect(tradeProposalSchema.safeParse(withReasoning("ệ".repeat(8000))).success).toBe(true);
  });

  it('rejects reasoning of 8001 "ệ" code points', () => {
    expect(tradeProposalSchema.safeParse(withReasoning("ệ".repeat(8001))).success).toBe(false);
  });

  it('accepts reasoning of 7000 "😀" (7000 code points, 14000 UTF-16 units)', () => {
    expect(tradeProposalSchema.safeParse(withReasoning("😀".repeat(7000))).success).toBe(true);
  });

  it('rejects reasoning of 8001 "😀" code points', () => {
    expect(tradeProposalSchema.safeParse(withReasoning("😀".repeat(8001))).success).toBe(false);
  });

  it('accepts model cost node of exactly 64 "😀" code points', () => {
    expect(tradeProposalSchema.safeParse(withNode("😀".repeat(64))).success).toBe(true);
  });

  it('rejects model cost node of 65 "😀" code points', () => {
    expect(tradeProposalSchema.safeParse(withNode("😀".repeat(65))).success).toBe(false);
  });
});

describe("utcTimestamp calendar validation", () => {
  // Regex-shaped but calendar-invalid: the Go ingestion gate (time.Parse)
  // rejects these, so the web plane must too or it accepts a timestamp the
  // control-plane loses at ingestion (cross-plane drift).
  const invalid = [
    "2026-02-30T00:00:00Z",
    "2026-13-01T00:00:00Z",
    "2026-00-01T00:00:00Z",
    "2026-01-00T00:00:00Z",
    "2026-07-32T00:00:00Z",
    "2026-11-31T00:00:00Z",
    "2100-02-29T00:00:00Z",
    "2026-07-07T24:00:00Z",
    "2026-07-07T00:60:00Z",
    "2026-07-07T00:00:61Z",
    "2026-07-07T23:59:60Z",
    "2026-07-07T99:99:99Z",
  ];
  for (const ts of invalid) {
    it(`rejects calendar-invalid ${ts}`, () => {
      expect(utcTimestamp.safeParse(ts).success).toBe(false);
    });
  }

  const valid = [
    "2024-02-29T00:00:00Z",
    "2400-02-29T00:00:00Z",
    "2026-12-31T23:59:59Z",
    "2026-07-07T00:00:00.123456789Z",
  ];
  for (const ts of valid) {
    it(`accepts calendar-valid ${ts}`, () => {
      expect(utcTimestamp.safeParse(ts).success).toBe(true);
    });
  }
});

describe("RiskVerdict golden fixtures", () => {
  it("parses verdict_reject_daily_loss.json", () => {
    const verdict = riskVerdictSchema.parse(verdictRejectDailyLoss);
    expect(verdict.decision).toBe("reject");
    expect(verdict.clipped_size_quote).toBeUndefined();
    expect(verdict.reasons[0]?.code).toBe("DAILY_LOSS_LIMIT_BREACHED");
    expect(verdict.limits_snapshot.daily_realized_pnl_quote).toBe("-512.40");
    expect(verdict.limits_snapshot.require_stop_loss).toBe(true);
  });

  it("parses verdict_clip.json", () => {
    const verdict = riskVerdictSchema.parse(verdictClip);
    expect(verdict.decision).toBe("clip");
    expect(verdict.clipped_size_quote).toBe("1200.00");
    expect(verdict.reasons[0]?.code).toBe("NOTIONAL_CAP_CLIPPED");
    expect(verdict.limits_snapshot.daily_realized_pnl_quote).toBe("84.10");
  });

  it("requires a non-empty reasons array for reject", () => {
    const mutated = structuredClone(verdictRejectDailyLoss) as Record<string, unknown>;
    mutated["reasons"] = [];
    const result = riskVerdictSchema.safeParse(mutated);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues[0]?.path).toEqual(["reasons"]);
  });
});
