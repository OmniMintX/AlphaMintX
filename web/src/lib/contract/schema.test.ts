// Golden-fixture contract tests: same fixtures, same expectations as the Go and
// Python planes (valid fixtures parse; proposal_invalid_no_sl.json fails on the
// stop_loss conditional and nothing else).

import { describe, expect, it } from "vitest";

import proposalOpenLong from "../../../../contracts/fixtures/proposal_open_long.json";
import proposalHold from "../../../../contracts/fixtures/proposal_hold.json";
import proposalInvalidNoSl from "../../../../contracts/fixtures/proposal_invalid_no_sl.json";
import verdictRejectDailyLoss from "../../../../contracts/fixtures/verdict_reject_daily_loss.json";
import { riskVerdictSchema, tradeProposalSchema } from "./schema";

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

  it("rejects proposal_invalid_no_sl.json with exactly one stop_loss issue", () => {
    const result = tradeProposalSchema.safeParse(proposalInvalidNoSl);
    expect(result.success).toBe(false);
    if (result.success) return;
    expect(result.error.issues).toHaveLength(1);
    expect(result.error.issues[0]?.path).toEqual(["stop_loss"]);
    expect(result.error.issues[0]?.message).toContain("stop_loss");
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

describe("RiskVerdict golden fixtures", () => {
  it("parses verdict_reject_daily_loss.json", () => {
    const verdict = riskVerdictSchema.parse(verdictRejectDailyLoss);
    expect(verdict.decision).toBe("reject");
    expect(verdict.clipped_size_quote).toBeUndefined();
    expect(verdict.reasons[0]?.code).toBe("DAILY_LOSS_LIMIT_BREACHED");
    expect(verdict.limits_snapshot.daily_realized_pnl_quote).toBe("-512.40");
    expect(verdict.limits_snapshot.require_stop_loss).toBe(true);
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
