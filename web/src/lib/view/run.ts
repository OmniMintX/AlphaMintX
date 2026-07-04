// Pure view-model helpers for the reasoning viewer: degradation markers
// (llm-routing-and-budget.md §5), forced-hold classification (§4-5), model-cost
// totals (decimal strings, never float — ADR-0003), and approval outcome labels
// (persistence-and-api.md §L0/L1 execution semantics).

import type { AnalystSummary, ModelCost, TradeProposal } from "../contract/schema";
import type { ApprovalOutcome, LifecycleState } from "../api/schema";

// §5: a failed analyst is set to {signal: neutral, confidence: 0.0,
// summary: "unavailable: <role> LLM call failed"}.
export const UNAVAILABLE_MARKER = "unavailable:";

export function isDegradedSummary(summary: AnalystSummary): boolean {
  return summary.summary.startsWith(UNAVAILABLE_MARKER);
}

// §5: researcher/judge failures cut the debate short and record the
// degradation in debate_summary.
export function isDegradedDebate(debateSummary: string): boolean {
  return debateSummary.includes(UNAVAILABLE_MARKER);
}

export type ForcedHoldKind = "budget_exhausted" | "rate_limited" | "llm_failure";

// §4-5: forced holds are action=hold, confidence=0.0, with reasoning stating
// BUDGET_EXHAUSTED, RATE_LIMITED, or the LLM failure. Ordinary holds carry
// trader confidence and no marker.
export function forcedHoldKind(proposal: TradeProposal): ForcedHoldKind | null {
  if (proposal.action !== "hold" || proposal.confidence !== 0) return null;
  if (proposal.reasoning.includes("BUDGET_EXHAUSTED")) return "budget_exhausted";
  if (proposal.reasoning.includes("RATE_LIMITED")) return "rate_limited";
  return "llm_failure";
}

export function forcedHoldLabel(kind: ForcedHoldKind): string {
  switch (kind) {
    case "budget_exhausted":
      return "Forced hold — daily token budget exhausted";
    case "rate_limited":
      return "Forced hold — rate limited (not a budget event)";
    case "llm_failure":
      return "Forced hold — LLM failure";
  }
}

// Exact decimal-string addition for non-negative decimalRegex-shaped values.
export function addDecimals(a: string, b: string): string {
  const [aInt = "0", aFrac = ""] = a.split(".");
  const [bInt = "0", bFrac = ""] = b.split(".");
  const scale = Math.max(aFrac.length, bFrac.length);
  const sum =
    BigInt(aInt + aFrac.padEnd(scale, "0")) + BigInt(bInt + bFrac.padEnd(scale, "0"));
  const digits = sum.toString().padStart(scale + 1, "0");
  const intPart = digits.slice(0, digits.length - scale);
  const fracPart = scale > 0 ? digits.slice(digits.length - scale) : "";
  return fracPart ? `${intPart}.${fracPart}` : intPart;
}

export interface ModelCostTotals {
  input_tokens: number;
  output_tokens: number;
  cost_usd: string;
}

export function modelCostTotals(costs: readonly ModelCost[]): ModelCostTotals {
  return costs.reduce<ModelCostTotals>(
    (acc, cost) => ({
      input_tokens: acc.input_tokens + cost.input_tokens,
      output_tokens: acc.output_tokens + cost.output_tokens,
      cost_usd: addDecimals(acc.cost_usd, cost.cost_usd),
    }),
    { input_tokens: 0, output_tokens: 0, cost_usd: "0" },
  );
}

// Audit distinguishes approved-and-executed from approved-but-blocked; only a
// plain "approved" outcome ever reaches the OMS.
export function approvalOutcomeLabel(outcome: ApprovalOutcome): string {
  switch (outcome) {
    case "approved":
      return "Approved — submitted to OMS";
    case "approved_but_blocked":
      return "Approved, but blocked by preflight — no order";
    case "rejected":
      return "Rejected — no order";
    case "timeout":
      return "Timed out — auto-rejected, no order";
  }
}

// An "approved" POST response may still carry submitted=false: the OMS
// rejected the submission after the approval row was committed (persisted as
// SUBMIT_FAILED). The label must never claim an execution that did not
// happen.
export function approvalDecisionLabel(decision: {
  outcome: ApprovalOutcome;
  submitted?: boolean;
}): string {
  if (decision.outcome === "approved" && decision.submitted === false) {
    return "Approved — OMS submission FAILED (recorded as SUBMIT_FAILED)";
  }
  return approvalOutcomeLabel(decision.outcome);
}

// L0 is the effective-autonomy floor: `paper` strategies (and `draft`, which
// has no runs yet) are advisory-only — nothing is ever submitted to the OMS.
export function isAdvisoryOnly(state: LifecycleState): boolean {
  return state === "draft" || state === "paper";
}
