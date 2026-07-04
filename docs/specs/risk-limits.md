# Spec: RiskLimits v1 and the deterministic Risk Gate

The Risk Gate is deterministic Go code in control-plane. It evaluates every
schema-valid TradeProposal and emits exactly one persisted RiskVerdict per
schema-valid proposal. No LLM is involved (invariant 1). Limits assume the LLM
can be fooled (prompt injection); the gate is the enforcement boundary.

## Authority

- All limits are set by the **Admin** role only (invariant 5). Trader, Viewer,
  and every AI agent MUST NOT be able to raise or disable any limit.
- Limits are a hard ceiling for humans and AI alike: the L1 copilot approve
  button cannot push an order past the gate either.
- Limit changes are append-only audited (who, when, old → new).

## Order classes and reduce-only (normative)

- **ENTRY orders** — proposal-originated orders that open or increase exposure.
- **PROTECTIVE orders** — exchange-resident SL/TP, `close`, and flatten orders.
- All PROTECTIVE orders MUST be submitted **reduce-only** where the venue
  supports it; where unsupported, the OMS MUST verify current position size
  before submission and size the order to min(order, position). A protective
  order can never open or flip a position.
- Kill-switch, watchdog, and cancel sweeps act on ENTRY orders only.
  PROTECTIVE stops MUST NOT be canceled while a position is open, unless the
  action flattens the position — in which case SL/TP are canceled only
  **after** the flatten fill is confirmed. (The two reviews proposed different
  orderings; stops-after-flatten is the safer-for-money one and reduce-only
  guarantees a racing stop cannot open a position.)
- Invariant: **no code path may leave an open position without an
  exchange-resident stop-loss while `require_stop_loss=true`.**

## RiskLimits v1 — fields, defaults, units

| Field | Default | Unit / type | Notes |
|---|---|---|---|
| `symbol_whitelist` | `[]` (deny all) | list of `BASE/QUOTE` strings | Empty list blocks all opens; Admin MUST populate explicitly. Applies to opens only; `close` is exempt (gate step 3). All entries MUST share one quote currency (= `accounting_quote`). |
| `max_open_positions` | `3` | count | Open positions PLUS pending un-filled ENTRY orders per strategy. |
| `per_position_notional_cap_quote` | none — Admin MUST set | decimal string, quote ccy | Guidance: ≈5% of account equity. Exceeding size is **clipped**, not rejected. Cap `"0"` ⇒ reject all opens `NOTIONAL_CAP_ZERO`. |
| `daily_loss_limit_quote` | none — Admin MUST set | decimal string, quote ccy | Realized + unrealized loss per UTC day (00:00 UTC boundary), **including fees and funding** (see Definitions). Guidance: 2–5% of equity. Trips the circuit breaker. |
| `max_drawdown_pct` | `10` | percent of peak equity | Breach ⇒ reject all opens; strategy flagged for review. |
| `max_loss_at_stop_quote` | none — Admin MUST set | decimal string, quote ccy | Per-trade bound: worst-case loss at the stop (see Definitions) above this ⇒ reject `RISK_PER_TRADE_EXCEEDED`. |
| `min_stop_distance_pct` / `max_stop_distance_pct` | `0.1` / `25` | percent of entry | SL sanity: stop distance from entry outside `[min, max]` ⇒ reject `INVALID_STOP_PLACEMENT`. |
| `max_orders_per_minute` | `6` | orders/min per strategy | Counts proposal-originated ENTRY submissions incl. their cancels/replaces only; sliding 60 s window, counted in control-plane. Safety-path submissions (SL/TP placement, breaker/kill/watchdog cancels and flatten) are **EXEMPT**. |
| `require_stop_loss` | `true` | boolean | MUST NOT be configurable off for any live_* state; MAY be relaxed in backtest only. |
| `allocated_capital_quote` | none — Admin MUST set | decimal string, quote ccy | Per-strategy capital allocation; basis for equity/peak (drawdown). |
| `accounting_quote` | none — Admin MUST set | currency code | The strategy's single quote currency; all limits and PnL are denominated in it. |
| `staleness_threshold_seconds` | `60` | seconds | Proposal staleness threshold (contract rule 5); captured in the verdict snapshot. |
| `l1_approval_timeout_seconds` | `600` | seconds | L1 / escalation approval timeout (`docs/specs/strategy-lifecycle.md`). |

v1 scope restriction: **spot markets only — no leverage, no margin**.
Leverage limits, margin mode, liquidation-distance checks, and margin/balance
gate checks are deferred (see v1 limitations, MS-6).

### L2 envelope (semi-auto escalation bounds)

| Field | Meaning |
|---|---|
| `l2_max_size_quote` | Max `size_quote` auto-executed at L2 without human escalation. |
| `l2_allowed_symbols` | Subset of `symbol_whitelist` tradable at L2 without escalation. |
| direction flip | An L2 strategy MUST NOT auto-flip direction (close + open opposite side within the same UTC day on a symbol); flips always escalate to human approval. |

Above-envelope proposals are not rejected: they **escalate** to the L1 flow
(human approve/reject with timeout → auto-reject). Envelope checks evaluate
the **post-clip** effective size.

The evaluated values MUST be captured in the verdict's `limits_snapshot`.

## Gate evaluation order (normative)

Gate evaluations for a given `strategy_id` MUST be **serialized** (per-strategy
lock or single-writer queue); counter checks and the verdict insert MUST commit
in one transaction. An approved verdict atomically reserves its position slot,
rate token, and worst-case loss headroom; reservations are released on order
cancel/expiry or if no order is placed.

First failing check decides (evaluation MAY short-circuit); clipping applies
only if all reject-checks pass.

0a. **Parse + schema + version** — malformed JSON or missing/invalid
   `proposal_id`/`strategy_id` ⇒ HTTP 400, recorded in an append-only
   `rejected_submissions` log, **NO verdict** (verdicts exist only for
   schema-valid proposals). Parseable documents failing schema/version ⇒
   reject `SCHEMA_INVALID` / `UNSUPPORTED_SCHEMA_VERSION`.
0b. **Idempotency** — atomic insert on unique `proposal_id` (contract rule 6).
   If a verdict already exists, return it verbatim; no further steps run and
   no new order can result.
1. **Kill-switch** — platform, then tenant, then strategy tier; an active kill
   is a standing condition. Any active ⇒ reject `KILL_SWITCH_ACTIVE`
   (including `close`: killed positions are managed by the kill-switch
   procedure and human-initiated flatten, not by proposals). The verdict
   records the kill-epoch observed at evaluation (see OMS execution rules).
2. **Staleness** — per `docs/specs/proposal-contract.md` rule 5 ⇒ reject
   `PROPOSAL_STALE`.
3. **Exit exemption** — `close` proposals are risk-reducing: they skip steps
   4–7 and 9–11 (no daily-loss, drawdown, whitelist, position-count, or
   notional-cap check; never clipped), remain subject to step 8, and are
   submitted **reduce-only**. The gate MUST never block an exit except under
   an active kill (step 1).
4. **Circuit breaker / daily loss** — breaker active, or
   `daily_loss + worst_case(order) ≥ daily_loss_limit_quote` (see Definitions)
   ⇒ reject `DAILY_LOSS_LIMIT_BREACHED`. Accounting includes reserved
   worst-case exposure of pending un-filled ENTRY orders.
5. **Drawdown** — equity below peak by > `max_drawdown_pct` ⇒ reject
   `MAX_DRAWDOWN_BREACHED` (opens only; `close` exempt per step 3).
6. **Symbol whitelist** — opens only ⇒ reject `SYMBOL_NOT_WHITELISTED`.
7. **Stop-loss present + placement + risk bound** — `require_stop_loss` and
   placement rules ⇒ reject `MISSING_STOP_LOSS` / `INVALID_STOP_PLACEMENT`;
   stop distance from entry outside `[min_stop_distance_pct,
   max_stop_distance_pct]` ⇒ reject `INVALID_STOP_PLACEMENT`;
   `worst_case(order) > max_loss_at_stop_quote` ⇒ reject
   `RISK_PER_TRADE_EXCEEDED`.
8. **Order rate** — > `max_orders_per_minute` ⇒ reject `ORDER_RATE_EXCEEDED`.
9. **Open positions** — open positions + pending un-filled ENTRY orders ≥
   `max_open_positions` ⇒ reject `MAX_POSITIONS_REACHED`.
10. **Notional cap** — cap `"0"` ⇒ reject `NOTIONAL_CAP_ZERO`; else
    `size_quote` > `per_position_notional_cap_quote` ⇒ decision `clip`,
    `clipped_size_quote` = cap, reason `NOTIONAL_CAP_CLIPPED`.
11. **Autonomy / envelope** — L2 envelope checks on the post-clip size; above
    envelope ⇒ decision `escalate` (routed to the L1 approval flow; the human
    or timeout outcome is recorded as a separate approval record, see
    `docs/specs/proposal-contract.md` — never as a second verdict).

`hold` proposals skip 3–11: they produce an `approve` verdict and no order.

**Re-evaluation on approval:** when a human approves an L1/escalated proposal,
the gate MUST re-run steps 1–11 against current state before OMS submission;
a failure is recorded on the approval record as a reject.

## Definitions (normative)

- `worst_case(order)` = `size_quote × |entry − stop_loss| / entry` + estimated
  taker fees.
- `daily_loss` = Σ realized PnL of trades closed in the current UTC day
  + Σ per open position of (mark − max(entry, 00:00-UTC mark)) × signed qty
  + **fees + funding**, converted to `accounting_quote` at current mark.
  The day boundary is 00:00 UTC.
- `equity` = `allocated_capital_quote` + cumulative strategy PnL (realized +
  unrealized at exchange mark price); `peak` = max equity, sampled at every
  fill, verdict, and PnL-monitor tick since the strategy first entered a
  `live_*` state. (Rebase rules for allocation changes: deferred, MS-13.)

## OMS execution rules (normative)

- **Exchange normalization** — before submission the OMS MUST round price to
  tick and quantity DOWN to step; if the resulting notional < venue
  minNotional ⇒ reject `BELOW_MIN_NOTIONAL` (never submit); rounding MUST NOT
  increase notional above the cap / `clipped_size_quote`. Rounded values are
  persisted with the order.
- **SL placement contingency** — if the protective SL is not confirmed resting
  within `sl_placement_deadline` (default 30 s) after any entry fill
  (including each partial fill), the OMS MUST retry with backoff and, failing
  that, close the filled quantity with a reduce-only market order and raise a
  strategy-tier alert. SL quantity MUST track cumulative filled quantity. A
  position is never left naked.
- **Kill re-check** — the OMS MUST re-check persisted kill state immediately
  before every exchange submission (including human-approved L1 orders). Each
  gate verdict carries the kill-epoch observed at evaluation; the OMS MUST
  reject any submission whose kill-epoch is stale (`KILL_SWITCH_ACTIVE`).

## Circuit breaker (daily loss)

- Evaluation: control-plane MUST run a PnL monitor that evaluates the
  daily-loss condition on the price/PnL stream — on every fill and on a timer
  (≤ 10 s while positions are open). The breaker fires from the monitor, not
  only on proposal arrival.
- Trigger: `daily_loss` (per Definitions, incl. fees + funding) ≥
  `daily_loss_limit_quote`.
- Effects (persisted-then-executed, idempotent, in order): cancel all ENTRY
  orders for the strategy; flatten all its positions with **reduce-only
  market orders**; after each flatten fill is confirmed, cancel that
  position's SL/TP; verify flat via reconciliation; demote effective autonomy
  to **L0** (advisor-only — proposals persisted, no orders) until the next
  UTC day boundary; persist a breaker event with the triggering verdict or
  monitor sample.
- Breaker/flatten submissions are EXEMPT from `max_orders_per_minute`.
- Reset: automatic at 00:00 UTC. The strategy's lifecycle state is unchanged;
  only effective autonomy is demoted (invariant 4). (Re-trip cool-down policy:
  deferred, MS-28.)

## Kill-switch (3 tiers)

| Tier | Scope | Who may trigger | Who may unlock |
|---|---|---|---|
| Strategy | one strategy instance | Trader, Admin, Owner; watchdog escalation | Admin, Owner |
| Tenant | all strategies of a tenant | Admin, Owner | Admin, Owner (after clearing the tenant kill) |
| Platform | everything | Platform Admin | Platform Admin only; no unlock while active |

Procedure (normative):

- Activation (intent, scope, flatten choice) MUST be **persisted append-only
  and acknowledged BEFORE any side effect executes**. Effects are idempotent
  and resumable: after a control-plane restart, incomplete effects are
  re-driven from the persisted intent; completion is recorded per
  order/position.
- Effects, in order: cancel all ENTRY orders in scope; if flatten was selected
  (operator choice at trigger time, default on for live), flatten per the
  circuit-breaker procedure (reduce-only market orders, SL/TP canceled only
  after flatten fills confirm); lock affected strategies in `killed`.
  PROTECTIVE stops are NEVER canceled while a position remains open unless
  the action flattens it. **No auto-restart** — recovery only via explicit
  human unlock (`docs/specs/strategy-lifecycle.md`).
- Standing condition: while a tenant- or platform-tier kill is active, the
  gate rejects all proposals in scope and lifecycle transitions out of
  `killed` are blocked.
- Human flatten in `killed`: Trader+ MAY trigger flatten of remaining
  positions while a strategy is killed; these orders are reduce-only, bypass
  the gate, and are exempt from `max_orders_per_minute`.
- In-flight orders are covered by the OMS-side kill re-check (kill-epoch, see
  OMS execution rules).

## Watchdog and heartbeats

- Agent-plane POSTs an authenticated heartbeat per strategy every 30 s
  (`docs/ARCHITECTURE.md`).
- Silence > 90 s ⇒ the watchdog cancels the strategy's ENTRY orders only and
  raises a strategy-tier alert. PROTECTIVE stops stay on the exchange
  (invariant 2) and open positions remain managed by the OMS. The watchdog
  does NOT flatten and does not kill on first expiry.
- Silence > 10 min, or open positions with unprotected exposure ⇒ escalate to
  a strategy-tier kill (flatten off by default; stops remain per
  §Kill-switch).

## v1 limitations / deferred (recorded, not silent)

- MS-6 — margin/leverage/liquidation model: v1 is spot-only; leverage fields and liquidation-distance checks deferred.
- MS-13 — exact equity/peak rebase formulas for capital-allocation changes deferred (`allocated_capital_quote` exists now).
- MS-14 — multi-quote-currency accounting deferred; v1 enforces one quote currency per strategy (`accounting_quote`).
- MS-15 — paper-gate threshold hardening (PF with modeled fees/slippage) deferred to the Phase-1 fill model.
- MS-16 — tenant/account-level aggregate limits deferred; v1 limits are per-strategy while strategies share one tenant exchange account.
- MS-17 — approval-time re-validation is specified above; UI enforcement lands with the Phase-1 L1 UI.
- MS-18 — `max_order_age` for resting entry orders deferred.
- MS-24 — flatten dust/partial-flatten handling and slippage disclosure detail deferred.
- MS-28 — breaker re-trip / cool-down policy (N trips in M days ⇒ `paused`) deferred.
- MS-31 — paper-gate active-day counting (vs pure calendar days) deferred.
- MS-32 — TP emulation where the venue lacks native TP order types deferred.
- SS-11 — server-clock (`received_at`) staleness anchor: rule in `docs/specs/proposal-contract.md`; enforcement Phase 1.
- SS-13 — API body-size limits deferred to Phase 1 (per-strategy proposal rate limit is specified now in `docs/ARCHITECTURE.md`).
- SS-25 — canonical reason-code registry deferred; codes are an open set, consumers treat unknown codes as opaque.
- SS-27 — agent-trace persistence ownership/API deferred to Phase 1.
- SS-28 — web viewer plain-text rendering of LLM text: Phase-1 enforcement.
