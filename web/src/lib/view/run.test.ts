// View-model helpers: degradation markers (llm-routing §5), forced-hold
// classification (§4-5), exact decimal cost totals, approval outcome labels,
// and the L0 advisory rule.

import { describe, expect, it } from "vitest";

import proposalOpenLong from "../../../../contracts/fixtures/proposal_open_long.json";
import proposalHold from "../../../../contracts/fixtures/proposal_hold.json";
import { tradeProposalSchema } from "../contract/schema";
import {
  addDecimals,
  approvalDecisionLabel,
  approvalOutcomeLabel,
  forcedHoldKind,
  isAdvisoryOnly,
  isDegradedDebate,
  isDegradedSummary,
  isPaperSimulated,
  modelCostTotals,
} from "./run";

const openLong = tradeProposalSchema.parse(proposalOpenLong);
const hold = tradeProposalSchema.parse(proposalHold);

describe("degradation markers", () => {
  it("flags the exact §5 unavailable analyst marker", () => {
    expect(
      isDegradedSummary({
        signal: "neutral",
        confidence: 0,
        summary: "unavailable: news_analyst LLM call failed",
      }),
    ).toBe(true);
    expect(isDegradedSummary(openLong.analyst_summaries.market)).toBe(false);
  });

  it("flags a cut-short debate summary", () => {
    expect(isDegradedDebate("Debate cut short: unavailable: debate_judge LLM call failed.")).toBe(true);
    expect(isDegradedDebate(openLong.debate_summary)).toBe(false);
  });
});

describe("forcedHoldKind", () => {
  const forced = (reasoning: string) => ({
    ...hold,
    confidence: 0,
    reasoning,
  });

  it("classifies BUDGET_EXHAUSTED and RATE_LIMITED holds distinctly", () => {
    expect(
      forcedHoldKind(forced("BUDGET_EXHAUSTED: strategy b2c3... exhausted for 2026-07-04")),
    ).toBe("budget_exhausted");
    expect(forcedHoldKind(forced("RATE_LIMITED: 429 persisted after retries"))).toBe("rate_limited");
  });

  it("classifies other zero-confidence holds as llm_failure", () => {
    expect(forcedHoldKind(forced("unavailable: trader LLM call failed"))).toBe("llm_failure");
  });

  it("never marks non-hold proposals or confident holds", () => {
    expect(forcedHoldKind(openLong)).toBeNull();
    expect(forcedHoldKind({ ...hold, confidence: 0.4 })).toBeNull();
  });
});

describe("model cost totals (decimal strings, never float)", () => {
  it("adds decimal strings exactly", () => {
    expect(addDecimals("0.1", "0.2")).toBe("0.3");
    expect(addDecimals("0.000593", "0.028980")).toBe("0.029573");
    expect(addDecimals("0", "12.5")).toBe("12.5");
    expect(addDecimals("99.99", "0.01")).toBe("100.00");
  });

  it("totals the golden fixture's model_costs", () => {
    const totals = modelCostTotals(openLong.model_costs);
    expect(totals.input_tokens).toBe(12727);
    expect(totals.output_tokens).toBe(2027);
    expect(totals.cost_usd).toBe("0.030790");
  });

  it("returns zero totals for an empty list (pre-call forced hold)", () => {
    expect(modelCostTotals([])).toEqual({ input_tokens: 0, output_tokens: 0, cost_usd: "0" });
  });
});

describe("approvalOutcomeLabel", () => {
  it("distinguishes approved-and-executed from approved_but_blocked", () => {
    expect(approvalOutcomeLabel("approved")).toContain("submitted to OMS");
    expect(approvalOutcomeLabel("approved_but_blocked")).toContain("blocked by preflight");
    expect(approvalOutcomeLabel("approved_but_blocked")).toContain("no order");
    expect(approvalOutcomeLabel("rejected")).toContain("no order");
    expect(approvalOutcomeLabel("timeout")).toContain("auto-rejected");
  });
});

describe("approvalDecisionLabel", () => {
  it("never claims a submission that failed (submitted=false)", () => {
    expect(approvalDecisionLabel({ outcome: "approved", submitted: false })).toContain(
      "submission FAILED",
    );
    expect(approvalDecisionLabel({ outcome: "approved", submitted: false })).not.toContain(
      "submitted to OMS",
    );
  });

  it("falls back to the outcome label otherwise", () => {
    expect(approvalDecisionLabel({ outcome: "approved", submitted: true })).toContain(
      "submitted to OMS",
    );
    expect(approvalDecisionLabel({ outcome: "approved" })).toContain("submitted to OMS");
    expect(approvalDecisionLabel({ outcome: "rejected" })).toContain("no order");
  });
});

describe("isAdvisoryOnly", () => {
  it("treats only draft as advisory: paper auto-executes on the paper OMS", () => {
    expect(isAdvisoryOnly("draft")).toBe(true);
    expect(isAdvisoryOnly("paper")).toBe(false);
    expect(isAdvisoryOnly("live_l1")).toBe(false);
    expect(isAdvisoryOnly("live_l3")).toBe(false);
    expect(isAdvisoryOnly("paused")).toBe(false);
    expect(isAdvisoryOnly("killed")).toBe(false);
  });
});

describe("isPaperSimulated", () => {
  it("is true only for paper", () => {
    expect(isPaperSimulated("paper")).toBe(true);
    expect(isPaperSimulated("draft")).toBe(false);
    expect(isPaperSimulated("live_l2")).toBe(false);
  });
});
