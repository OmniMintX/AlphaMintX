# Spec: Strategy instance lifecycle and autonomy ladder

Every strategy instance is in exactly one lifecycle state, persisted and
append-only audited. The state machine is enforced in control-plane code;
illegal transitions are errors, not warnings.

Enforcement is normative in `docs/specs/lifecycle-api.md`: the transition
endpoint `POST /api/v1/strategies/{id}/lifecycle` (guards computed from
persisted state, never caller assertions; `→ killed` flows only through
the kill endpoints), the computed paper-gate algorithm (LC-15..LC-24,
read-only report at `GET .../paper-gate`), and the kill-clear/unlock
machinery (LC-25..LC-38). The `killed → paper/paused` unlocks below are
reachable via that endpoint once the triggering kill tier's standing
condition is cleared through the kill-clear endpoints (LC-36: clear and
unlock are two audited acts).

## States

| State | Meaning |
|---|---|
| `draft` | Being configured; no runs. |
| `paper` | Pipeline runs, paper OMS only; no exchange orders. |
| `live_l1` | Live, autonomy L1 (copilot). |
| `live_l2` | Live, autonomy L2 (semi-auto). |
| `live_l3` | Live, autonomy L3 (full-auto). |
| `paused` | Runs suspended by a human. Un-filled ENTRY orders are canceled; protective reduce-only SL/TP **remain** on the exchange; open positions remain managed by the OMS (reconciliation, SL/TP maintenance). Only new entries are blocked. |
| `killed` | Kill-switch fired: ENTRY orders canceled, protective stops kept unless flattened (`docs/specs/risk-limits.md`), locked. No auto-restart. |

There is no `live_l0` state: L0 (advisor) is the effective-autonomy floor —
`paper` strategies and circuit-breaker-demoted live strategies operate at L0.
In a live-mode deployment `paper` is part of the L0 floor for the LIVE
venue: verdicts persist and a `paper` strategy submits only to the paper
OMS bridge, never to the live Submitter (lifecycle-api.md LC-14a).

In every state with open positions — including `paused`, `killed`, and
breaker-demoted L0 — protective reduce-only stops are kept and the OMS
continues position management (reconciliation, SL/TP); only new entries are
blocked.

## Paper-gate (promotion prerequisite, enforced in code — invariant 3)

ALL conditions MUST hold, computed from the immutable track record:
- ≥ 14 calendar days in `paper` state, AND
- ≥ 30 closed paper trades, AND
- average closed-trade notional ≥ `min_avg_trade_notional_quote` (default
  25% of `per_position_notional_cap_quote`; v1 pins the default — the
  Admin-set override is deferred, lifecycle-api.md LC-D1) — thirty $1
  trades MUST NOT pass, AND
- paper max drawdown ≤ the strategy's `max_drawdown_pct` limit, AND
- paper profit_factor ≥ 1.0 (gross profit / gross loss; gross_loss = 0 with
  gross_profit > 0 passes; both 0 fails).

No role — including Admin and Platform Admin — can waive the paper-gate.

After any `killed` event or any live `max_drawdown_pct` breach, ALL paper-gate
counters reset to zero: a NEW paper period (≥ 14 days, ≥ 30 closed trades,
measured after the event) is required before any `paper → live_*` transition.
No same-day re-promotion on an old paper record.

v1 paper fills model no partial fills, queue position, or latency; the
paper-gate is a necessary-but-not-sufficient sanity floor, not validation of
live performance.

## Transition table (normative; anything not listed is illegal)

| From | To | Guard conditions |
|---|---|---|
| `draft` | `paper` | Config valid; Admin has set RiskLimits (whitelist non-empty, caps set). Actor: Trader+. |
| `paper` | `live_l1` | Paper-gate passed. Exchange keys configured (trade-only). Actor: Trader+. |
| `paper` | `live_l2` | Paper-gate passed; L2 envelope configured. Actor: Trader+. |
| `paper` | `live_l3` | Paper-gate passed; **Admin approval** recorded. Actor: Trader+ with Admin approval. |
| `live_l1` | `live_l2` | L2 envelope configured. Actor: Trader+. |
| `live_l1` | `live_l3` | **Admin approval** recorded. Actor: Trader+ with Admin approval. |
| `live_l2` | `live_l3` | **Admin approval** recorded (in addition to the already-passed paper-gate). |
| `live_l3` | `live_l2` / `live_l1` | Demotion; always allowed. Actor: Trader+. |
| `live_l2` | `live_l1` | Demotion; always allowed. Actor: Trader+. |
| `live_*` | `paper` | Positions flat (or flatten confirmed). Actor: Trader+. |
| any of `paper`, `live_*` | `paused` | Actor: Trader+. Un-filled ENTRY orders MUST be canceled; protective reduce-only SL/TP remain; positions stay managed (see States). |
| `paused` | previous state | Resume to the exact state it was paused from. Actor: Trader+. If previous state was `live_*` and limits changed while paused, gate re-validates on next proposal as usual. |
| **any** | `killed` | Kill-switch, any tier (`docs/specs/risk-limits.md`). Also watchdog escalation (prolonged heartbeat loss). Protective stops kept unless flattened. |
| `killed` | `paper` | **Human unlock only** (Admin or Owner), with recorded reason. Requires: positions flat (or flatten confirmed); the `draft → paper` guard conditions hold; the triggering kill tier's standing condition cleared (tenant kill by tenant Admin/Owner; platform kill by Platform Admin — no unlock while a tenant/platform kill is active). Paper-gate counters reset (see Paper-gate). Never automatic; never directly back to `live_*`. |
| `killed` | `paused` | **Human unlock only** (Admin or Owner) when positions are NOT flat: for manual position resolution; protective stops remain, positions stay managed. |

"Trader+" = Trader, Admin, or Owner of the tenant. Viewer can never transition.
Circuit breaker (daily loss) does NOT change lifecycle state: it demotes
effective autonomy to L0 until the next UTC day (invariant 4).

**Admin approval (v1 NARROWING, lifecycle-api.md LC-8):** the L3 approval
guard is satisfied only when the ACTING principal itself maps to
Admin/Owner — the transition's audit row (actor_id, actor_role, reason) IS
the recorded approval. A Trader cannot carry a third party's approval;
approval-by-reference is deferred (LC-D2).

## Autonomy ladder L0–L3 (invariant 3)

| Level | Who places orders | Human approval needed | Timeout / escalation |
|---|---|---|---|
| **L0 Advisor** | Nobody. Proposals persisted + shown; no OMS submission. | n/a | n/a |
| **L1 Copilot** | OMS, only after per-proposal human approval (Trader+). | Every order. | No decision within `l1_approval_timeout_seconds` (default 600 s) ⇒ **auto-reject**. The approval/timeout outcome is recorded as a separate append-only approval record referencing the verdict (`docs/specs/proposal-contract.md` covers the shape) — never as a second RiskVerdict. On human approval the gate re-evaluates the proposal against current state before OMS submission (`docs/specs/risk-limits.md`). |
| **L2 Semi-auto** | OMS automatically, within the L2 envelope. | Only above-envelope proposals. | Escalation rules: `size_quote` > `l2_max_size_quote`, symbol ∉ `l2_allowed_symbols`, or direction flip ⇒ gate decision `escalate`, routed through the L1 approve flow (same timeout → auto-reject, same approval record). |
| **L3 Full-auto** | OMS automatically for any gate-approved proposal. | None per-order (kill-switch and limits still apply). | n/a |

At every level the deterministic Risk Gate evaluates every proposal first;
autonomy only decides what happens to gate-approved proposals. Human approval
can never override a gate rejection (invariant 5).
