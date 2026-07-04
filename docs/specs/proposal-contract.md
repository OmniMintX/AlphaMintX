# Spec: TradeProposal / RiskVerdict contract (v1)

Normative companion to `contracts/proposal.schema.json` and
`contracts/riskverdict.schema.json`. Schemas define shape; this spec defines
semantics and rules beyond JSON Schema. Both planes MUST enforce both.

## TradeProposal — field semantics

| Field | Semantics |
|---|---|
| `schema_version` | Contract version as `"MAJOR.MINOR"` (`"1.x"` in v1; currently `"1.0"`). Both planes MUST reject any version they do not support (see Versioning rules). |
| `proposal_id` | UUID, unique per proposal. Idempotency key for submission: control-plane enforces uniqueness with an atomic constraint (see rule 6). Resubmitting the same `proposal_id` MUST NOT create a second proposal/verdict/order. |
| `strategy_id` | UUID of the strategy instance the proposal belongs to. Limits and autonomy level are resolved from it. |
| `agent_trace_id` | UUID of the pipeline run (== LangGraph run/checkpoint id). Links proposal to the persisted agent trace for the reasoning viewer and audit (invariant 7). |
| `created_at` | RFC 3339 UTC timestamp of proposal emission, `Z` suffix mandatory (schema-enforced by pattern). Risk Gate MUST reject stale proposals; staleness is measured against the control-plane clock (see rule 5). |
| `symbol` | Trading pair in canonical `BASE/QUOTE` uppercase form (e.g. `BTC/USDT`), pattern `^[A-Z0-9]{2,15}/[A-Z0-9]{2,10}$`. Control-plane MUST reject any non-canonical form (lowercase, `BTC-USDT`, missing `/`) as `SCHEMA_INVALID`; whitelist matching is exact string equality on the canonical form. |
| `action` | `open_long` \| `open_short` \| `close` \| `hold`. `close` closes the existing position on `symbol`; `hold` is a no-op record (still persisted). |
| `size_quote` | Requested notional in quote currency, decimal string. For `close`: `"0"` = full close; a value > 0 = partial close of at most that quote notional (values ≥ current position notional close fully). For `hold`: `"0"`. |
| `entry` | `{type: market\|limit, limit_price}`. `limit_price` REQUIRED iff `type=limit`, FORBIDDEN for `market` (schema-enforced). |
| `stop_loss` | Stop price, decimal string. REQUIRED iff `action` ∈ {`open_long`, `open_short`} (invariant 2: OMS places it exchange-resident); FORBIDDEN for `close`/`hold` (schema-enforced). |
| `take_profit` | Optional target price, decimal string, permitted only for `open_long`/`open_short`; FORBIDDEN for `close`/`hold` (schema-enforced). Exchange-resident when supported. |
| `time_in_force` | `gtc` or `ioc`. |
| `confidence` | Trader agent's conviction, number in [0,1]. Advisory only — never overrides limits. |
| `reasoning` | Trader agent's natural-language rationale, ≤8000 chars. Shown in reasoning viewer; part of the immutable record. |
| `analyst_summaries` | Exactly `market`, `news`, `fundamental`, each `{signal: bullish\|bearish\|neutral, confidence: [0,1], summary}`. Each `summary` ≤2000 chars. |
| `debate_summary` | Judge's summary of the bull/bear debate, ≤4000 chars. |
| `model_costs` | Per-node LLM cost: `{node, model, input_tokens, output_tokens, cost_usd}` (`node`/`model` ≤64 chars, ≤32 items). Source for billing/metering. MAY be empty only in stub/test mode. |

## RiskVerdict — field semantics

| Field | Semantics |
|---|---|
| `schema_version` | `"MAJOR.MINOR"` (`"1.x"`; currently `"1.0"`); unknown versions MUST be rejected. |
| `verdict_id` | UUID, unique per verdict. Exactly one verdict per proposal; verdicts are immutable (see Escalation below for how post-verdict outcomes are recorded). |
| `proposal_id` | The evaluated proposal's `proposal_id`. |
| `decision` | `approve` (pass to OMS as-is), `reject` (no order), `clip` (pass with reduced size), `escalate` (above the strategy's autonomy envelope; queued for human L1/L2 approval — no order until an ApprovalDecision approves it). |
| `clipped_size_quote` | Decimal string. REQUIRED iff `decision=clip`, FORBIDDEN otherwise (schema-enforced). MUST be > 0 and < proposal `size_quote`. |
| `reasons` | Machine codes + human messages. `code` is SCREAMING_SNAKE, ≤64 chars (e.g. `DAILY_LOSS_LIMIT_BREACHED`, `SYMBOL_NOT_WHITELISTED`, `MISSING_STOP_LOSS`, `KILL_SWITCH_ACTIVE`, `NOTIONAL_CAP_CLIPPED`, `ESCALATED_ABOVE_ENVELOPE`); `message` ≤500 chars. `reject`/`clip` MUST have ≥1 reason (schema-enforced); `escalate` SHOULD carry `ESCALATED_ABOVE_ENVELOPE`; `approve` SHOULD have `[]`. |
| `limits_snapshot` | The limit values in force at evaluation time PLUS the runtime inputs the gate actually evaluated, all REQUIRED (schema-enforced) so verdicts are reproducible: `equity_quote`, `peak_equity_quote`, `daily_realized_pnl_quote` (signed decimal string), `open_positions_count`, `pending_entry_orders_count`, `mark_price`, alongside the configured limits. See `docs/specs/risk-limits.md` for limit fields. |
| `evaluated_at` | RFC 3339 UTC timestamp of gate evaluation, `Z` suffix mandatory (schema-enforced by pattern). |

## Escalation and the L1 approval flow

The gate verdict is immutable and there is exactly one verdict per proposal.
Human approval outcomes are therefore NEVER recorded by mutating a verdict or
emitting a second one. When `decision=escalate` (or an L1 strategy requires
human sign-off), the outcome is recorded as a separate append-only follow-up
record referencing the verdict:

- **ApprovalDecision** (shape, briefly): `{approval_id (uuid), verdict_id (uuid),
  proposal_id (uuid), outcome: approved | rejected | timeout, decided_by
  (user id, or "timeout"), decided_at (RFC 3339 UTC)}`.
- No decision within `l1_approval_timeout` ⇒ an ApprovalDecision with
  `outcome=timeout` is persisted; the proposal is not executed. `APPROVAL_TIMEOUT`
  is an ApprovalDecision outcome, never a verdict reason.
- The OMS submits an order only for `approve`/`clip` verdicts, or for `escalate`
  verdicts that have an ApprovalDecision with `outcome=approved`.

## Decimal-as-string rationale (ADR-0003)

All money/price/size fields (`size_quote`, `limit_price`, `stop_loss`,
`take_profit`, `cost_usd`, `clipped_size_quote`, quote-denominated limits) are
**decimal strings**, never JSON numbers. JSON numbers are IEEE-754 doubles in
most parsers; binary floats cannot represent decimal quantities exactly and
round-trip differently across Go/Python/JS. Producers MUST serialize from a
decimal type (Python `Decimal`, Go `shopspring/decimal`); consumers MUST parse
into a decimal type and MUST NOT pass money through float64. Format:
`^(0|[1-9][0-9]*)(\.[0-9]+)?$` — non-negative, no exponent, no leading `+`/`-`,
no leading zeros, no bare `.5`, empty string invalid; ≤34 chars. Fields that may
be negative (only `daily_realized_pnl_quote` in v1) use the signed variant
`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`.

## Timestamps and UUIDs (assertion, not annotation)

In JSON Schema draft 2020-12, `format` is annotation-only by default; it does
not validate. Both schemas therefore assert shape with explicit `pattern`s:

- Timestamps (`created_at`, `evaluated_at`): RFC 3339 UTC with mandatory `Z`
  suffix — `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$`.
  Offsets (`+05:00`) and empty strings are schema-invalid. An unparseable
  `created_at` ⇒ reject `SCHEMA_INVALID`.
- UUIDs (`proposal_id`, `strategy_id`, `agent_trace_id`, `verdict_id`):
  lowercase-hex pattern `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`.

Contract-test harnesses MUST NOT rely on `format` assertion being enabled; the
patterns are the contract.

## Versioning rules

- `schema_version` is `"MAJOR.MINOR"`. v1 documents carry `"1.x"`; the current
  version is `"1.0"`.
- `additionalProperties: false` is kept everywhere: unknown fields are a
  validation error, not ignorable extensions.
- Within a major version, changes MUST be additive only (new OPTIONAL fields)
  and MUST bump the minor version (`"1.0"` → `"1.1"`) with an updated schema
  file. New enum values only where the spec marks the enum extensible (none in
  v1).
- Consumers MUST reject documents whose `schema_version` they do not support
  (`UNSUPPORTED_SCHEMA_VERSION`). Never "best-effort parse" an unknown version.
- Rollout order for any contract change: consumers upgrade first (control-plane
  accepts both the old and new minor version), then producers (agent-plane)
  start emitting the new version. A producer MUST NOT emit a version before
  every consumer supports it.
- Any breaking change (rename, type change, new required field, semantics
  change) ⇒ bump the major version, new schema file, migration note.

## Validation rules beyond JSON Schema (normative)

Both planes MUST enforce; the Risk Gate is the last line of defense (invariant 1):

1. **Stop placement.** For `open_long`: `stop_loss` < entry price (limit_price,
   or current mark for market entries). For `open_short`: `stop_loss` > entry
   price. Violation ⇒ reject `INVALID_STOP_PLACEMENT`.
2. **Take-profit placement.** If present: `take_profit` > entry for `open_long`,
   < entry for `open_short`. Violation ⇒ reject `INVALID_TAKE_PROFIT_PLACEMENT`.
3. **Size positivity.** `size_quote` MUST be > 0 unless `action` ∈
   {`close`, `hold`}. Violation ⇒ reject `INVALID_SIZE`.
4. **Low confidence.** `confidence` < 0.3 MUST map to `action=hold` in the
   agent-plane Trader node. The Risk Gate MAY additionally reject low-confidence
   opens per strategy config (`LOW_CONFIDENCE`).
5. **Staleness.** Staleness is evaluated against the control-plane clock, which
   is authoritative: the gate MUST reject a proposal whose `created_at` is older
   than `staleness_threshold_seconds` (default 60) at control-plane receipt time
   ⇒ reject `PROPOSAL_STALE`. A `created_at` in the future by more than the skew
   tolerance (5 s) ⇒ reject `PROPOSAL_STALE` (clock-skew guard). The producer's
   clock is never trusted on its own.
6. **Idempotency.** `proposal_id` uniqueness is an atomic constraint at
   ingestion: control-plane MUST enforce it with a unique index / atomic
   insert-or-fetch, so concurrent duplicate deliveries cannot both evaluate.
   A duplicate submission returns the original persisted verdict verbatim — an
   idempotent response, NOT a new verdict — with no re-evaluation and no second
   order. If a duplicate `proposal_id` arrives with a different payload
   (canonical hash mismatch) ⇒ reject `IDEMPOTENCY_CONFLICT`, no re-evaluation.
7. **Per-trade risk bound.** The worst-case loss of an open at its stop is
   defined as: `max_loss_at_stop = |entry − stop_loss| / entry × size_quote`
   for `open_long`; `max_loss_at_stop = (stop_loss − entry) / entry ×
   size_quote` for `open_short` (entry = `limit_price`, or current mark for
   market entries). This value is a gate input: it feeds the daily-loss
   headroom check and any per-trade risk limit (see
   `docs/specs/risk-limits.md`).
8. **Symbol normalization.** The canonical symbol form is uppercase
   `BASE/QUOTE`. Producers MUST emit it; the control plane MUST reject
   non-canonical forms rather than normalizing them.

## Golden fixtures (`contracts/fixtures/`)

Contract tests in Go and Python MUST both consume these files:

- `proposal_open_long.json` — valid, all fields populated. MUST validate.
- `proposal_hold.json` — valid, minimal (no `stop_loss`/`take_profit`,
  `size_quote: "0"`, empty `model_costs`). MUST validate.
- `proposal_invalid_no_sl.json` — `open_long` without `stop_loss`; otherwise
  fully valid, so it violates exactly one rule. MUST FAIL validation for that
  single reason (tests assert failure; the missing `stop_loss` conditional is
  the rule under test).
- `verdict_reject_daily_loss.json` — valid `reject` verdict with reason
  `DAILY_LOSS_LIMIT_BREACHED` and a full `limits_snapshot` including the
  runtime evaluation inputs. MUST validate.

## v1 limitations / deferred

The following review findings concern this contract and are deliberately
deferred (recorded here so the gap is chosen, not accidental):

- **MS-17 / SS-15** — re-running the gate at L1/L2 approval time (staleness and
  limits re-validation before OMS submission). Design noted above
  (ApprovalDecision); enforcement lands with the Phase-1 L1 UI.
- **MS-18** — `max_order_age` for resting GTC entry orders (RiskLimits field,
  Phase 1).
- **MS-32** — take-profit emulation where the venue lacks native TP/OCO order
  types (`TP_NOT_SUPPORTED_DROPPED`), Phase 1.
- **SS-13 (API part)** — per-strategy proposal-rate limit and max request body
  size at the API layer, Phase 1. The schema-side bounds (`maxLength`,
  `maxItems`, bounded decimal patterns) are already in v1.0.
- **SS-24** — `confidence` remains a JSON number in v1; bit-determinism is
  defined after canonical JSON normalization. May become a decimal string in
  v2.
- **SS-25** — a canonical reason-code registry. In v1 reason codes are an open
  set constrained to SCREAMING_SNAKE; consumers MUST treat unknown codes as
  opaque and fall back to `message`.
- **SS-26** — expanded negative-fixture coverage (clip verdicts, limit_price
  conditionals, decimal edge cases). v1.0 ships the four golden fixtures above;
  additional fixtures land with the Phase-0 test harnesses.
