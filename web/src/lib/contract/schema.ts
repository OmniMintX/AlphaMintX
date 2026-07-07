// Zod mirrors of contracts/proposal.schema.json and contracts/riskverdict.schema.json.
// The JSON Schemas are normative; on divergence, fix this file, never the contracts.

import { z } from "zod";

export const decimalRegex = /^(0|[1-9][0-9]*)(\.[0-9]+)?$/;
export const signedDecimalRegex = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?$/;

// Compare two decimalRegex-shaped strings without float conversion. Integer
// parts carry no leading zeros, so a longer integer part is strictly larger.
function compareDecimals(a: string, b: string): number {
  const [aInt = "", aFrac = ""] = a.split(".");
  const [bInt = "", bFrac = ""] = b.split(".");
  if (aInt.length !== bInt.length) return aInt.length < bInt.length ? -1 : 1;
  if (aInt !== bInt) return aInt < bInt ? -1 : 1;
  const width = Math.max(aFrac.length, bFrac.length);
  const aPadded = aFrac.padEnd(width, "0");
  const bPadded = bFrac.padEnd(width, "0");
  if (aPadded === bPadded) return 0;
  return aPadded < bPadded ? -1 : 1;
}

function isZeroDecimal(value: string): boolean {
  return compareDecimals(value, "0") === 0;
}

export const uuid = z
  .string()
  .regex(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
export const decimal = z.string().max(34).regex(decimalRegex);
export const signedDecimal = z.string().max(35).regex(signedDecimalRegex);
export const symbol = z.string().regex(/^[A-Z0-9]{2,15}\/[A-Z0-9]{2,10}$/);

const daysInMonth = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
function isLeapYear(year: number): boolean {
  return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
}
// The regex only guarantees the YYYY-MM-DDThh:mm:ss shape; it accepts month 13,
// Feb 30, hour 24, second 60, etc. The control-plane ingestion gate (Go
// time.Parse) rejects those, so accepting them here would let the web plane
// pass a timestamp the control-plane loses at ingestion (Phase 18 drift class).
// Mirror Go exactly: months 1-12, Gregorian leap-day bound, hh 0-23, mm/ss 0-59.
function isValidUtcCalendar(value: string): boolean {
  const month = Number(value.slice(5, 7));
  const day = Number(value.slice(8, 10));
  const hour = Number(value.slice(11, 13));
  const minute = Number(value.slice(14, 16));
  const second = Number(value.slice(17, 19));
  if (month < 1 || month > 12) return false;
  let maxDay = daysInMonth[month - 1] as number;
  if (month === 2 && isLeapYear(Number(value.slice(0, 4)))) maxDay = 29;
  if (day < 1 || day > maxDay) return false;
  return hour <= 23 && minute <= 59 && second <= 59;
}
export const utcTimestamp = z
  .string()
  .max(35)
  .regex(/^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$/)
  .refine(isValidUtcCalendar, { message: "invalid calendar date/time in UTC timestamp" });

export const analystSummarySchema = z.strictObject({
  signal: z.enum(["bullish", "bearish", "neutral"]),
  confidence: z.number().min(0).max(1),
  summary: z.string().max(2000),
});

export const modelCostSchema = z.strictObject({
  node: z.string().max(64),
  model: z.string().max(64),
  input_tokens: z.number().int().min(0),
  output_tokens: z.number().int().min(0),
  cost_usd: decimal,
});

const entry = z
  .strictObject({
    type: z.enum(["market", "limit"]),
    limit_price: decimal.optional(),
  })
  .superRefine((e, ctx) => {
    if (e.type === "limit" && e.limit_price === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["limit_price"],
        message: 'limit_price is required when entry.type is "limit"',
      });
    }
    if (e.type === "market" && e.limit_price !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["limit_price"],
        message: 'limit_price is forbidden when entry.type is "market"',
      });
    }
  });

export const tradeProposalSchema = z
  .strictObject({
    schema_version: z.literal("1.0"),
    proposal_id: uuid,
    strategy_id: uuid,
    agent_trace_id: uuid,
    created_at: utcTimestamp,
    symbol,
    action: z.enum(["open_long", "open_short", "close", "hold"]),
    size_quote: decimal,
    entry,
    stop_loss: decimal.optional(),
    take_profit: decimal.optional(),
    time_in_force: z.enum(["gtc", "ioc"]),
    confidence: z.number().min(0).max(1),
    reasoning: z.string().max(8000),
    analyst_summaries: z.strictObject({
      market: analystSummarySchema,
      news: analystSummarySchema,
      fundamental: analystSummarySchema,
    }),
    debate_summary: z.string().max(4000),
    model_costs: z.array(modelCostSchema).max(32),
  })
  .superRefine((p, ctx) => {
    const opens = p.action === "open_long" || p.action === "open_short";
    if (opens && p.stop_loss === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["stop_loss"],
        message: `stop_loss is required when action is "${p.action}"`,
      });
    }
    // Rule 3: size positivity for opens; hold must carry "0".
    if (opens && isZeroDecimal(p.size_quote)) {
      ctx.addIssue({
        code: "custom",
        path: ["size_quote"],
        message: `size_quote must be > 0 for ${p.action}`,
      });
    }
    if (!opens) {
      if (p.stop_loss !== undefined) {
        ctx.addIssue({
          code: "custom",
          path: ["stop_loss"],
          message: `stop_loss is forbidden when action is "${p.action}"`,
        });
      }
      if (p.take_profit !== undefined) {
        ctx.addIssue({
          code: "custom",
          path: ["take_profit"],
          message: `take_profit is forbidden when action is "${p.action}"`,
        });
      }
      if (p.action === "hold" && !isZeroDecimal(p.size_quote)) {
        ctx.addIssue({
          code: "custom",
          path: ["size_quote"],
          message: 'size_quote must be "0" for hold',
        });
      }
    }
    // Rules 1-2 against a known entry price (limit entries only; market
    // entries are checked by the Risk Gate against the current mark).
    if (p.entry.type !== "limit" || p.entry.limit_price === undefined) return;
    if (p.stop_loss === undefined) return;
    const entryPrice = p.entry.limit_price;
    if (isZeroDecimal(entryPrice)) {
      ctx.addIssue({
        code: "custom",
        path: ["entry", "limit_price"],
        message: "entry price must be > 0",
      });
      return;
    }
    if (p.action === "open_long") {
      if (compareDecimals(p.stop_loss, entryPrice) >= 0) {
        ctx.addIssue({
          code: "custom",
          path: ["stop_loss"],
          message: `stop_loss ${p.stop_loss} must be below entry ${entryPrice} for open_long`,
        });
      }
      if (p.take_profit !== undefined && compareDecimals(p.take_profit, entryPrice) <= 0) {
        ctx.addIssue({
          code: "custom",
          path: ["take_profit"],
          message: `take_profit ${p.take_profit} must be above entry ${entryPrice} for open_long`,
        });
      }
    } else if (p.action === "open_short") {
      if (compareDecimals(p.stop_loss, entryPrice) <= 0) {
        ctx.addIssue({
          code: "custom",
          path: ["stop_loss"],
          message: `stop_loss ${p.stop_loss} must be above entry ${entryPrice} for open_short`,
        });
      }
      if (p.take_profit !== undefined && compareDecimals(p.take_profit, entryPrice) >= 0) {
        ctx.addIssue({
          code: "custom",
          path: ["take_profit"],
          message: `take_profit ${p.take_profit} must be below entry ${entryPrice} for open_short`,
        });
      }
    }
  });

const verdictReason = z.strictObject({
  code: z.string().max(64).regex(/^[A-Z][A-Z0-9_]*$/),
  message: z.string().max(500),
});

const limitsSnapshot = z.strictObject({
  symbol_whitelist: z.array(symbol),
  max_open_positions: z.number().int().min(0),
  per_position_notional_cap_quote: decimal,
  daily_loss_limit_quote: decimal,
  max_drawdown_pct: z.number().min(0),
  max_orders_per_minute: z.number().int().min(0),
  require_stop_loss: z.boolean(),
  l2_max_size_quote: decimal.optional(),
  l2_allowed_symbols: z.array(symbol).optional(),
  equity_quote: decimal,
  peak_equity_quote: decimal,
  daily_realized_pnl_quote: signedDecimal,
  open_positions_count: z.number().int().min(0),
  pending_entry_orders_count: z.number().int().min(0),
  mark_price: decimal,
});

export const riskVerdictSchema = z
  .strictObject({
    schema_version: z.literal("1.0"),
    verdict_id: uuid,
    proposal_id: uuid,
    decision: z.enum(["approve", "reject", "clip", "escalate"]),
    clipped_size_quote: decimal.optional(),
    reasons: z.array(verdictReason).max(32),
    limits_snapshot: limitsSnapshot,
    evaluated_at: utcTimestamp,
  })
  .superRefine((v, ctx) => {
    if (v.decision === "clip" && v.clipped_size_quote === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["clipped_size_quote"],
        message: 'clipped_size_quote is required when decision is "clip"',
      });
    }
    if (v.decision !== "clip" && v.clipped_size_quote !== undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["clipped_size_quote"],
        message: `clipped_size_quote is forbidden when decision is "${v.decision}"`,
      });
    }
    if ((v.decision === "reject" || v.decision === "clip") && v.reasons.length === 0) {
      ctx.addIssue({
        code: "custom",
        path: ["reasons"],
        message: `reasons must be non-empty when decision is "${v.decision}"`,
      });
    }
  });

export type TradeProposal = z.infer<typeof tradeProposalSchema>;
export type RiskVerdict = z.infer<typeof riskVerdictSchema>;
export type AnalystSummary = z.infer<typeof analystSummarySchema>;
export type ModelCost = z.infer<typeof modelCostSchema>;
