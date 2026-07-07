# Lifecycle API and kill-unlock (SW-2)

Normative. This spec defines (A) the strategy lifecycle transition endpoint
that wires the `control-plane/internal/strategy` state machine into the HTTP
API, (B) the enforced paper-gate evaluation behind `paper → live_*`
promotion, and (C) the SW-2 kill-unlock machinery deferred by
`docs/specs/safety-wiring.md` §Deferred: append-only kill-clear events, the
active-kill predicate, and the three clear endpoints. Rules are **LC-n**.

Companion specs: `docs/specs/strategy-lifecycle.md` (states, transition
table, paper-gate criteria — restated here only where enforcement needs
pinning), `docs/specs/safety-wiring.md` (kill tiers, effects engine,
invariants 4/9/15), `docs/specs/watchdog.md` (WD-16 standing-kill skip),
`docs/specs/multi-tenant-rbac.md` (permission matrix, tenant isolation,
audit identity), `docs/specs/persistence-and-api.md` (tables, error
envelope, Limits), `docs/specs/risk-limits.md` (kill-switch procedure, OMS
kill re-check).

## Goals and non-goals

- Goal: land the ONLY code path out of `killed` — human, audited,
  standing-condition-gated — completing safety-wiring.md SW-2, and make
  every lifecycle transition of strategy-lifecycle.md reachable over HTTP
  with its guards enforced from persisted state, never from caller
  assertions.
- Goal: make the paper-gate a computed fact (immutable track record in),
  not a flag anyone can set.
- Non-goals (§Deferred): the Admin-set `min_avg_trade_notional_quote`
  override field (v1 pins the default), approval-by-reference for L3
  promotion, kill-driven token revocation (SW-3), manual breaker reset
  (SW-4), web UI surface, futures/leverage, per-symbol notional floors.

## The lifecycle endpoint

**LC-1.** `POST /api/v1/strategies/{id}/lifecycle` performs one lifecycle
transition. It is ALWAYS registered, in both paper and live modes (a
deployment without a limits provider simply evaluates `ConfigValid` and the
paper-gate to false — fail-closed — while pause and resume keep working;
killed unlocks to `paper` additionally require a limits provider
(`ConfigValid`, LC-8) and fail closed — a flat killed strategy cannot
unlock in a no-limits deployment).

**LC-2.** Permission-matrix row (routes are registered FROM
`api.Permissions()`; no route exists without a matrix entry):
`{Method: "POST", Path: "/api/v1/strategies/{id}/lifecycle",
Roles: [trader, admin, owner], Classes: [env-admin]}`. The guard order is
unchanged: 401 → 403 role/class → per-token rate limit → body cap. The
read, operator, and agent classes can never transition.

**LC-3.** Tenant-scoped resolution: tenant-bound principals resolve the
strategy via `GetStrategyInTenant` (foreign-tenant strategy ⇒ 404
`UNKNOWN_STRATEGY`, indistinguishable from absence); env principals via
`GetStrategy`.

**LC-4.** Body, strictly decoded (unknown fields rejected; body REQUIRED):
`{"to": "<state>", "reason": "<non-empty>"}`. An unknown `to` is 400
`INVALID_LIFECYCLE_STATE`. An absent or empty `reason` is 400
`SCHEMA_INVALID` — the API's absent-required-field convention
(`api/tenants.go`); `lifecycle_transitions.reason` is NOT NULL and every
transition is an audit row, not only killed unlocks.

**LC-5.** `to = "killed"` is rejected 422 `USE_KILL_ENDPOINT` and writes
nothing: kills flow ONLY through the three kill endpoints of
safety-wiring.md, whose `kill_breaker_events` row is the activation record
the effects engine and the epoch counter key on. The lifecycle endpoint
must never mint a `killed` state without a kill event row. Taxonomy
order (pinned): the LC-4 schema checks answer FIRST — a body
`{"to": "killed", "reason": ""}` is 400 `SCHEMA_INVALID` (the
absent-required-field convention), never `USE_KILL_ENDPOINT`; the killed
redirect fires only on a schema-valid body.

**LC-6.** Actor role mapping to the machine's `strategy.Role`:
`trader → RoleTrader`, `admin → RoleAdmin`, `owner → RoleOwner`,
env-admin class → `RoleOwner`. `RoleSystem` is UNREACHABLE via the API —
it exists only for the safety driver's `AppendKillLifecycleLock` path and
watchdog escalation.

**LC-7.** Machine rehydration: the handler builds the machine from
persisted state — `strategy.NewAt(strategies.lifecycle_state)`. For
`paused`, provenance comes from the audit trail: the NEWEST
`lifecycle_transitions` row with `to_state = 'paused'` (newest =
`ORDER BY recorded_at DESC, rowid DESC`, §Store surface) supplies
`pausedFrom = from_state` (that row is necessarily the entry into the
current paused period, because leaving paused precedes any later entry). A
`from_state = 'killed'` row reproduces the paused-after-kill lock
(`transition.go`: resume only to `paper` under the full killed→paper
guard). `Instance.pausedFrom` is not a persisted column and `NewAt` cannot
express it: the strategy package gains an exported rehydration constructor
`NewPausedFrom(prev State) *Instance` (state `paused`, pausedFrom `prev`);
no paused-entry row found ⇒ `NewPausedFrom("")`, which the machine already
treats as unknown provenance (paper-only exit).

**LC-8.** Guard-input derivation (normative — the `strategy.Context` is
computed from persisted state and deployment wiring, NEVER from request
fields):

| Context field | Source |
|---|---|
| `Actor` | LC-6 mapping of the authenticated principal |
| `ConfigValid` | effective limits via `LimitsProvider.Limits(strategyID)` (CURRENT, never a startup capture): `SymbolWhitelist` non-empty AND `PerPositionNotionalCapQuote > 0` AND `DailyLossLimitQuote > 0` AND `MaxDrawdownPct > 0`; nil provider ⇒ false |
| `ExchangeKeysConfigured` | new `api.Config.ExchangeKeysConfigured` bool, wired at startup in live deployments with exchange credentials; unset ⇒ false |
| `PaperGatePassed` | §Paper-gate result (LC-15..LC-22), evaluated in-request |
| `L2EnvelopeConfigured` | effective limits' `L2Envelope != nil` |
| `AdminApproval` | the acting principal itself maps to `RoleAdmin`/`RoleOwner` (LC-6): the transition row (actor_id, actor_role, reason) IS the recorded approval. A trader cannot carry a third party's approval in v1 (§Deferred) |
| `PositionsFlat` | every `positions` row of the strategy has `qty_base` numerically zero; no rows ⇒ flat |
| `KillCleared` | NOT `store.ActiveKill(strategyID)` (LC-28) |
| `CountersReset` | constant true — every QUALIFYING re-entry into paper restarts the window and a binding kill closes it (LC-16), so a re-entered strategy can never inherit pre-kill counters; the flag is satisfied by construction |
| `Reason` | body `reason` |

Live-target kill guard (normative; the invariant-9 carve-out): any
transition with `to ∈ {live_l1, live_l2, live_l3}` — promotion OR
paused-resume — additionally REQUIRES no active kill binding the strategy
(`KillCleared` true). The machine consumes `KillCleared` only on
killed-exits, so the handler enforces this for live targets itself:
violation is 422 `ILLEGAL_TRANSITION`, message `kill tier active`, and
nothing is written. The handler's `ActiveKill` read is a fast-path
pre-check; the BINDING enforcement is the LC-28 predicate re-evaluated
INSIDE the LC-9 CAS transaction for live targets (`ErrKillActive`, same
422/message) — a kill landing between the pre-check's read and the
commit can never slip a live promotion through. A strategy under a
standing tenant or platform kill can be paused and resumed to paper, but
never to a live tier.

**LC-9.** Persistence is compare-and-swap: a new store mutator
`AppendLifecycleTransitionCAS(t, liveTarget)` runs in ONE transaction —
re-read `strategies.lifecycle_state`; if it no longer equals the
machine's `from_state`, write NOTHING and report conflict (handler: 409
`LIFECYCLE_CONFLICT`); with `liveTarget` (the handler passes
`to.IsLive()`) additionally re-evaluate the LC-28 active-kill EXISTS
predicate in the SAME transaction — an active kill writes NOTHING and
returns the sentinel `ErrKillActive` (handler: 422 `ILLEGAL_TRANSITION`,
message `kill tier active`, identical to the LC-8 pre-check); else
insert the `lifecycle_transitions` row and advance the snapshot, exactly
the two statements of `AppendLifecycleTransition`. The transaction
serializes against the safety driver's `AppendKillLifecycleLock`, so a
concurrent kill and a concurrent unlock cannot interleave into a lost
update.

**LC-10.** Audit identity: `actor_id = actorID(principal)` (token_id for
DB principals, `"env-admin"`, else `OperatorPrincipal` — the
multi-tenant-rbac.md rule, unchanged); `actor_role` is the principal's
role string, or `"env-admin"` for the env-admin class. `'system'` in
`actor_role` remains exclusively the driver's and the LC-16a bootstrap
writer's — no API principal can produce it.

**LC-11.** A machine rejection (`Transition` returns an error) is 422
`ILLEGAL_TRANSITION` with the machine's error text as the message,
verbatim — the machine is the single source of transition-table truth and
the API adds no second table. Two exceptions: a `paper → live_*` attempt
failing ONLY on the paper-gate answers 422 `PAPER_GATE_FAILED` with the
full condition report (LC-23), and the LC-8 live-target kill guard
answers 422 `ILLEGAL_TRANSITION` with the API-authored message
`kill tier active`.

**LC-12.** Effects are persist-then-execute: on CAS success the handler
executes returned effects AFTER the commit. `EffectCancelEntryOrders`
(returned on entry into `paused`) runs through the new optional
`api.Config.EntryCanceler` seam (§Wiring seams). A nil seam or an effect
error NEVER rolls back the transition: the handler logs, appends a
`safety_alerts` row `kind='lifecycle_entry_cancel_failed'`
(`strategy_id` set, `ref_id` = transition_id; the kind set is open per
SS-25) and still answers 200 — the persisted state already demotes
autonomy and the gate rejects new entries in `paused`.

**LC-12a.** Paper-mode entry cancel: the omsbridge grows the paper
equivalent of the live OMS's entry canceler — `CancelOpenEntries` cancels
the paper book's resting un-filled ENTRY orders only, reduce-only
protectives untouched (the same `EffectCancelEntryOrders` contract) — and
main.go wires it as `api.Config.EntryCanceler` in paper mode. A paused
paper strategy therefore stops filling its resting limit entries; the
seam is no longer live-only prose.

**LC-13.** Success response, 200:
`{"strategy_id", "from_state", "to_state", "transition_id",
"recorded_at"}`, timestamps RFC 3339 UTC `Z` (store convention).

**LC-14.** The handler runs under the per-strategy lock
(`Server.strategyLock(strategyID)`), the same lock serializing gate
evaluations: one lifecycle evaluation+CAS per strategy at a time, and
never concurrent with a proposal evaluation reading `lifecycle_state`.

**LC-14a.** Live-mode paper floor: when the wired Submitter is the live
OMS (`api.Config.PaperSubmitter` false, §Wiring seams), `routeExecution`
(`api/proposals.go`) treats the `paper` lifecycle state as part of the
L0 floor — approve/clip verdicts persist, NOTHING submits. A `paper`
strategy submits to the Submitter ONLY when the Submitter is the paper
bridge (`PaperSubmitter` true). A live-mode server with a `paper`-state
strategy never calls `Submitter.SubmitApproved` — paper track records
are built against the paper OMS, never against the live venue.

## Paper-gate (computed, unwaivable)

**LC-15.** The paper-gate is evaluated synchronously inside the transition
request (and the LC-24 read), from PERSISTED rows only: `fills` joined to
`orders`, `lifecycle_transitions`, and the current effective limits. There
is no cached pass, no stored flag, and no role — including Owner and
env-admin — that can waive it (strategy-lifecycle.md invariant 3;
`guardPaper` enforces it in the machine as defense in depth).

**LC-16.** The paper window. Base start `S` = `recorded_at` of the NEWEST
QUALIFYING `lifecycle_transitions` row with `to_state = 'paper'` for the
strategy (newest = `ORDER BY recorded_at DESC, rowid DESC`, §Store
surface). A `to_state='paper'` row QUALIFIES iff its
`from_state != 'paused'` — first promotion, live demotion, killed unlock
all qualify — OR its `from_state = 'paused'` AND the newest earlier
`to_state='paused'` row has `from_state = 'killed'` (the paused-after-kill
exit, LC-7's lock) — OR its `from_state = 'paused'` AND `K` exists (a
binding kill was recorded, below) AND no earlier qualifying
`to_state='paper'` row has `recorded_at ≥ K`: a pause→resume performed
after an in-place kill (a kill recorded while the strategy sat in `paper`
or `paused`) is the audited re-entry that restarts the window. An
ORDINARY `paused → paper` resume — no binding kill since the window
began — never qualifies; pre-pause fills still count. "Earlier" is
lexicographic: row X is earlier than row Y iff
(`X.recorded_at`, `X.rowid`) < (`Y.recorded_at`, `Y.rowid`). No
qualifying row ⇒ the gate FAILS closed (every condition unmet).

Kill reset keys off the kill EVENT, not the live-only lifecycle lock: let
`K` = `recorded_at` of the newest `kill_breaker_events` row
`kind='kill'` binding the strategy under the LC-28 3-clause match (cleared
or not). If `K` exists and no qualifying `to_state='paper'` row has
`recorded_at ≥ K`, the gate FAILS closed — a strategy killed in place
(e.g. a `paper` strategy under a tenant kill) must re-earn its track
record through a fresh qualifying entry into `paper`. Otherwise the
window is `[S, now)`: a binding kill either closes the gate or predates
the qualifying row, so `max(S, K)` collapses to `S`. The counter reset
demanded after kills and drawdown breaches holds by construction: no
pre-kill fill can ever sit inside the window.

**LC-16a.** Bootstrap transition row: `store.CreateStrategy` atomically
inserts the strategies row AND, when the initial lifecycle state is
`paper` or a `live_*` tier, ONE `lifecycle_transitions` row —
`from_state='draft'`, `to_state=<initial>`, `actor_id='bootstrap'`,
`actor_role='system'` (the LC-10 carve-out), `reason='bootstrap'`,
`recorded_at` = the strategy's `created_at` — in the SAME transaction.
Bootstrap initial states are restricted to `paper` and `live_*`: a
bootstrap row is never written for an initial `paused`/`killed` (creating
strategies at those states is out of scope) — no paused provenance
`'draft'` is ever minted. Migration on store `Open`: for every existing
`paper` OR `paused` strategy with no `to_state='paper'` row, insert one
synthetic draft→paper bootstrap row (`recorded_at` = `created_at`) —
additive, idempotent, guarded like the existing ALTER migrations (§Store
surface). For a `paused` strategy the synthetic row is a WINDOW-START
record only — it does not alter PausedProvenance, which still reads
`to_state='paused'` rows. LC-16 therefore never fails closed merely
because a strategy was born in `paper`. (Implementation ripple:
`store/safety_test.go` row-count assertions need updating.)

**LC-17.** Condition `min_days`: `now − S ≥ 14 × 24 h`, on the
second-precision RFC 3339 timestamps, ≥ errs closed.

**LC-18.** Replay definition: take the strategy's `fills` with
`fill_ts ≥ S`, joined to their `orders` row (`symbol`, `side`,
`reduce_only`), ordered by (`fill_ts`, `fills.rowid`). Reconstruct one
signed book per symbol with the SAME math as the OMS books
(`paper.applyFill` / live `fills.go`): weighted-average entry on
increases, realized delta on reductions, every fill's `fee_quote` realized
when paid. A CLOSED TRADE is a maximal span in which a symbol's book
leaves zero and returns to zero; its trade PnL is the sum of realized
deltas (net of ALL span fees, entry fees included) over the span; its
closing notional is Σ `qty_base × fill_price` over the span's reducing
fills. A sign-flipping fill splits exactly as the paper OMS splits it:
only the REDUCING portion (down to zero) counts toward the span's
closing notional and realized delta; the remainder opens the next span.
A `reduce_only` fill is CLAMPED: the flag is a sizing bound — the fill
can only close the book toward zero, so replay caps its quantity at the
open opposite-side quantity, and against a same-side or flat book it
replays only the fee debit. A reduce-only fill therefore never opens a
position and never flips one — replay can never mint a phantom span
from a defensively-journaled oversize. A span still open at evaluation
is not a closed trade.

**LC-19.** Condition `min_closed_trades`: closed trades ≥ 30.

**LC-20.** Condition `min_avg_notional`: (Σ closing notionals / closed
trades) ≥ 0.25 × `PerPositionNotionalCapQuote` from the CURRENT effective
limits — thirty $1 trades MUST NOT pass. v1 pins the
strategy-lifecycle.md default (25% of the cap); the Admin-set
`min_avg_trade_notional_quote` override is §Deferred. Zero closed trades
fails the condition (no division).

**LC-21.** Condition `max_drawdown`: fold the replay's realized deltas
AND every fee debit — each at the fill where it is paid, in replay
order — into an equity curve seeded at the deployment's
`AllocatedCapitalQuote` (the SAME seed the omsbridge/hydrator use; new
`api.Config.AllocatedCapitalQuote`); peak is monotone from the seed; max
of `(peak − equity)/peak × 100` over the curve must be ≤ `MaxDrawdownPct`
(`riskgate.RiskLimits.MaxDrawdownPct` is already `decimal.Decimal` —
float64 only in the contract snapshot mirror: compare directly against
the effective limits' decimal `MaxDrawdownPct`; no float arithmetic
anywhere). A seed ≤ 0 fails the condition (fail-closed, no division by a
zero peak).

**LC-22.** Condition `profit_factor`, over closed-trade PnLs:
`gross_profit` = Σ positive trade PnL, `gross_loss` = |Σ negative trade
PnL|. `gross_loss = 0` with `gross_profit > 0` passes; both zero FAILS;
otherwise pass iff `gross_profit / gross_loss ≥ 1.0`.

**LC-23.** The gate's output is a full report, never first-failure:
`{"passed": bool, "window_started_at": string|null, "evaluated_at":
string, "conditions": [{"name", "passed", "measured", "required"}, ×5]}`
with names exactly `min_days`, `min_closed_trades`, `min_avg_notional`,
`max_drawdown`, `profit_factor`; `measured`/`required` are decimal
strings. Edges pinned: `min_days` renders measured/required in DECIMAL
DAYS (elapsed seconds ÷ 86400 as a decimal string; required `"14"`);
limits absent or a fail-closed window ⇒ the affected condition reports
`required` `"0"` and `passed` false (never a null); zero closed trades ⇒
`measured` `"0"` and `passed` false for `min_avg_notional`,
`profit_factor`, AND `max_drawdown` alike. `PAPER_GATE_FAILED` (LC-11)
embeds this report in the error body as `"paper_gate"`.

**LC-24.** `GET /api/v1/strategies/{id}/paper-gate` returns the LC-23
report read-only (promotion visibility). Matrix row: Roles
[viewer, trader, admin, owner], Classes [read]; tenant-scoped resolution
per LC-3. It never caches and never persists. Cost pin: the evaluation
is O(window fills) per request; the accepted mitigation is the existing
per-token 60/min rate-limit bucket (no separate limiter, no cache). The
`api/auth.go` guard charges that bucket on non-GET requests only, so
THIS handler charges it itself on every GET — the same bucket, the same
429 `RATE_LIMITED`: burst-reads exhaust long before the replay hurts
SQLite.

### Arena read surface (Phase 28)

Two READ-ONLY endpoints over the paper-gate replay. LC-25+ are taken by
the kill-clear section below, so rules here are **AR-n**.

**AR-1.** `GET /api/v1/strategies/{id}/performance?max_points=N` returns
`{"strategy_id", "window_started_at": string|null, "evaluated_at",
"seed", "model": string|null, "equity_curve": [{"ts", "equity"}, …],
"stats": {"realized_pnl", "return_pct", "max_drawdown_pct",
"closed_trades", "wins", "losses", "win_rate_pct",
"profit_factor": string|null, "fees_paid",
"last_fill_at": string|null}}`. Every money/percent field is an ADR-0003
decimal string; `equity_curve` is `[]`, never null.

**AR-2.** `GET /api/v1/arena/leaderboard` returns `{"evaluated_at",
"items": [{"rank", "strategy_id", "name", "tenant_id",
"lifecycle_state", "model": string|null, "seed", "equity",
"realized_pnl", "return_pct", "max_drawdown_pct", "closed_trades",
"win_rate_pct", "profit_factor": string|null,
"last_fill_at": string|null}, …]}`, ranked `return_pct` desc, ties
`realized_pnl` desc, then `strategy_id` asc; `rank` is the 1-based
position after that sort. `items` is `[]`, never null. `equity` =
seed + `realized_pnl`.

**AR-3.** Shared walk (normative): both endpoints replay
`papergate.ReplayCurve`, which runs the IDENTICAL LC-18 book walk as
the gate's own replay — ONE walk implementation, no arena math of its
own. Arena numbers (closed trades, max drawdown, fees) are
byte-identical to the LC-23 report's by construction.

**AR-4.** Window: the LC-16 paper window verbatim — the SAME store read
as the gate (window start + the LC-18 fill join), persisted rows only.
A fail-closed window (LC-16) ⇒ `window_started_at` null, `equity_curve`
`[]`, zero stats — never an error.

**AR-5.** Curve: anchored at (`window_started_at`, seed), then one
post-fill equity sample per fill in replay order. `?max_points`
(default 500, clamped to ≥ 2) downsamples to evenly-spaced samples,
ALWAYS keeping the anchor (first) and the newest sample (last).

**AR-6.** Stats semantics: `realized_pnl` = final equity − seed (every
fee debit realized where paid, open-position fees included);
`return_pct` = `realized_pnl / seed × 100`, `"0"` when seed ≤ 0 (the
LC-21 fail-closed edge, no division); wins = closed trades with
PnL > 0, losses = PnL < 0, zero-PnL trades are neither; `win_rate_pct`
= wins / closed trades × 100, `"0"` with no closed trades;
`profit_factor` is null iff `gross_loss = 0` (unbounded or undefined —
never a division, never `"∞"`); `last_fill_at` is null with no window
fills.

**AR-7.** Model attribution: `model` is the strategy's NEWEST
`node='trader'` `model_costs` row's `model` (newest =
`ORDER BY recorded_at DESC, rowid DESC`, §Store surface); null when no
trader row exists.

**AR-8.** Matrix rows, both endpoints: Roles
[viewer, trader, admin, owner], Classes [read, env-admin]; agent tokens
never. The performance read is tenant-scoped per LC-3 (foreign ⇒ 404);
the leaderboard shows a tenant principal its OWN tenant's strategies
only (§Lists: no foreign rows), env classes the whole platform.

**AR-9.** Cost pin: both replays are O(window fills) per request, so
each handler charges the per-token 60/min bucket itself, exactly as
LC-24 — the same bucket, the same 429 `RATE_LIMITED`, no cache, no
separate limiter.

## Kill-clear events (SW-2 standing-condition clearing)

**LC-25.** New append-only table (additive DDL, normative):

```sql
CREATE TABLE IF NOT EXISTS kill_clear_events (clear_id TEXT PRIMARY KEY,  -- append-only SW-2 audit
  scope TEXT NOT NULL CHECK (scope IN ('strategy','tenant','platform')),
  strategy_id TEXT, tenant_id TEXT,
  cleared_epoch INTEGER NOT NULL, actor_id TEXT NOT NULL, reason TEXT NOT NULL,
  recorded_at TEXT NOT NULL,
  CHECK ((scope = 'strategy' AND strategy_id IS NOT NULL AND tenant_id IS NOT NULL)
      OR (scope = 'tenant' AND strategy_id IS NULL AND tenant_id IS NOT NULL)
      OR (scope = 'platform' AND strategy_id IS NULL AND tenant_id IS NULL)));
CREATE INDEX IF NOT EXISTS idx_kill_clear_scope
  ON kill_clear_events (scope, strategy_id, tenant_id, cleared_epoch);
```

The scope CHECK enforces LC-26's NULL-ness (strategy rows carry both ids
— `tenant_id` for audit); the index backs the LC-28
`MAX(cleared_epoch)` subqueries.

**LC-26.** Row shape mirrors `kill_breaker_events` NULL-ness: scope
`strategy` ⇒ `strategy_id` set and `tenant_id` recorded for audit (the
predicate matches on `strategy_id` alone); scope `tenant` ⇒ `tenant_id`
set, `strategy_id` NULL; scope `platform` ⇒ both NULL. Phase-1 global kill
rows (both ids NULL) classify as platform — only a platform clear clears
them, matching the tier the effects engine already assigns them.

**LC-27.** `cleared_epoch` is CAS-verified: the append transaction
recomputes `MAX(kill_epoch)` over `kind='kill'` rows matching the
clearing scope's own LC-28 clause and compares it to the body's
REQUIRED `observed_epoch` (LC-30); on mismatch it writes NOTHING — no
clear row, no supersede markers, no alerts — and the handler answers 409
`CLEAR_CONFLICT`: a kill landing between the operator's read and the
clear can never be swept away unseen. On match, `cleared_epoch` = the
verified observed value. A clear at scope `S` clears every `kind='kill'`
row OF ITS SCOPE with `kill_epoch ≤ cleared_epoch` — i.e. all of them at
clear time. A kill appended AFTER the clear necessarily carries a higher
epoch and stands. When both the LC-31 active check and the epoch
verification fail, the active check answers first (the LC-31 order): 422
`NO_ACTIVE_KILL`, never 409 `CLEAR_CONFLICT`.

**LC-28.** The ACTIVE-KILL predicate (normative SQL — the store read
`ActiveKill(strategyID) (bool, error)`): a strategy has an active kill iff

```sql
SELECT EXISTS (SELECT 1 FROM kill_breaker_events e
  WHERE e.kind = 'kill' AND (
    (e.strategy_id = ?1 AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'strategy' AND strategy_id = ?1))
    OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL
        AND e.tenant_id = (SELECT tenant_id FROM strategies WHERE strategy_id = ?1)
        AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'tenant' AND tenant_id = e.tenant_id))
    OR (e.strategy_id IS NULL AND e.tenant_id IS NULL AND e.kill_epoch >
       (SELECT COALESCE(MAX(cleared_epoch), 0) FROM kill_clear_events
        WHERE scope = 'platform'))))
```

The three clauses extend the multi-tenant-rbac.md 3-clause kill predicate;
tenant rows still bind only their tenant, both-NULL rows everyone.

**LC-29.** Clear endpoints, mirroring the kill endpoints' tiers and RBAC
exactly one level stricter on the strategy tier (unlock is Admin+, per
strategy-lifecycle.md, not Trader+ like kill):

| Route | Roles | Classes |
|---|---|---|
| `POST /api/v1/strategies/{id}/kill/clear` | admin, owner (own tenant) | env-admin |
| `POST /api/v1/tenants/{tenant_id}/kill/clear` | admin, owner (own tenant, same resolution rules as the tenant kill handler) | env-admin |
| `POST /api/v1/platform/kill/clear` | — | env-admin ONLY |

All three are always registered (mode-independent, like the kill
endpoints).

**LC-30.** Clear bodies are strictly decoded:
`{"reason": "<non-empty>", "observed_epoch": N}` — both REQUIRED; an
absent or empty `reason`, or an absent `observed_epoch`, is 400
`SCHEMA_INVALID` (the LC-4 convention). The platform tier additionally
REQUIRES the literal `"ack": "CLEAR-PLATFORM"` — anything else is 400
`PLATFORM_CLEAR_ACK_REQUIRED` and NO row is written (the KILL-PLATFORM
ack pattern).

**LC-31.** A clear with NO active kill at its scope is 422
`NO_ACTIVE_KILL` and writes NOTHING: the append transaction checks the
scope's own active clause (the matching branch of LC-28) before inserting.
Unknown strategy/tenant remain 404 (`UNKNOWN_STRATEGY`/`UNKNOWN_TENANT`,
resolution before body semantics).

**LC-32.** Scope isolation: a clear at one scope never clears another. A
strategy under both a strategy kill and a tenant kill needs BOTH cleared
before `ActiveKill` goes false; precedence platform > tenant > strategy is
preserved in the standing-condition sense.

**LC-33.** Clear responses, 200: `{"clear_id", "scope", "cleared_epoch",
"recorded_at", "superseded_event_ids": [..]}` (the LC-38 supersede list,
possibly empty) plus `"strategy_id"`+`"tenant_id"` (strategy tier) or
`"tenant_id"` (tenant tier).

**LC-34.** Caller matrix — who moves to `ActiveKill`, who stays on the RAW
epoch. MOVE (standing-condition blockers): the live OMS ENTRY standing-kill
check (`submit.go` `submitEntry`, currently `GlobalMaxKillEpoch > 0`); the
hydrator's `KillActive` (`runstate.go`: becomes
`ActiveKill(strategyID) || lifecycleState == "killed"`); the watchdog's
standing-kill skip (`watchdog.go` `watchOne` — after a clear the watchdog
is RE-ARMED and may kill again on fresh silence; its `Store` interface
swaps `GlobalMaxKillEpoch` for `ActiveKill`, §Store surface). The WD-16
crash-lost-alert BACK-FILL is NOT behind the skip: the
`LatestStrategyKillEvent` read (watchdog-authored newest row) runs before
and regardless of cleared-ness — a cleared kill still gets its lost alert
back-filled. STAY RAW (staleness ordering): the submission epoch
stamp (`GlobalMaxKillEpoch` at submit), the transmit-loop
`maxEpoch > intent.KillEpoch` comparison and `abandonStale`, and the
approval preflight's `MaxKillEpoch(strategyID, evaluated_at)` — a kill
recorded after the verdict blocks the approval EVEN IF since cleared (the
verdict's world changed; re-evaluate).

**LC-34a.** Stamp-then-check read order in `submitEntry` (normative):
the RAW stamp epoch (`GlobalMaxKillEpoch`) is read BEFORE the
`ActiveKill` standing check — or both in one read transaction — and the
stamped value is that PRE-check read. A kill committing before the
`ActiveKill` read rejects the submission; a kill committing after the
stamp read carries a higher epoch and is caught by the transmit-loop
`maxEpoch > intent.KillEpoch` comparison. No interleaving lets an intent
transmit under a kill it never observed.

**LC-34b.** Watch-set re-entry: the watchdog tracks watch-set membership
per pass; when a strategy LEAVES the set the tracker deletes BOTH its
`firstWatched` and `lastSeen` entries; when a strategy ENTERS the set
after an absence the tracker re-stamps `firstWatched = now` and deletes
any stale `lastSeen`. After clear + unlock + re-promotion, the first
watchdog pass never escalates on pre-kill staleness.

**LC-35.** Clearing never mutates `kill_breaker_events`; epochs stay
monotone (the MAX+1 appenders are untouched). A cleared kill's history —
event row, served-effect marker, alerts — is permanent.

**LC-36.** A clear does NOT transition lifecycle: the strategy stays
`killed` until the separate lifecycle-endpoint unlock (`killed → paper` or
`killed → paused`, Admin/Owner, recorded reason, LC-8 guards). Clear and
unlock are two audited acts; the same human may perform both.

**LC-37.** Breaker rows (`kind='breaker'`) are NOT clearable — the breaker
latch remains derived (active until next UTC day) and manual reset stays
deferred (SW-4). `BreakerActiveToday` is untouched.

**LC-38.** Clears do not invoke `driveSafety()` — there is no effects
half to serve. UNSERVED effects of a cleared kill are SUPERSEDED, never
served late: the clear transaction appends the `safety_effects`
done-marker for every kill event it covers that lacks one, plus one
`safety_alerts` row `kind='kill_effects_superseded'` (`strategy_id` from
the event when set, `ref_id` = event_id) per superseded event; the clear
response lists the superseded event ids (LC-33). This is a pinned
carve-out to safety-wiring's "served means done": the marker records
that the CLEAR is the resolution — audited by the alert — not that
effects executed. Rationale: an unserved flatten from a cleared kill
must never move a live book later, and paper-mode kills (whose effects
half never runs) would otherwise block clears forever. `driveSafetyEvent`
re-checks the event's unserved-ness (marker absence) immediately before
executing its effects, AND the driver re-checks it AGAIN per strategy
immediately before each strategy's flatten half — a clear landing
MID-PASS, between the strategies of a tenant/platform row, stops every
remaining flatten; a marker found ⇒ skip — an in-flight drive can never
flatten a book under a kill a concurrent clear has superseded.
Breaker rows are never covered.

## Invariants

1. **No auto-restart, preserved.** The ONLY path out of `killed` is the
   lifecycle endpoint with a human Admin/Owner principal; `RoleSystem`
   never exits `killed`; nothing clears a kill as a side effect.
2. **Append-only clearing.** Kill rows are never mutated or deleted; a
   clear is a new row; the full kill/clear history is reconstructible.
3. **Staleness survives clearing.** Order-staleness comparisons use the
   RAW epoch (LC-34): a clear never un-stales an intent stamped before the
   kill, and never resurrects an abandoned attempt.
4. **Paper-gate unwaivable and fail-closed.** No role, no code path skips
   it; missing window, missing limits, zero seed all fail.
5. **Scope isolation.** A clear affects exactly its scope (LC-32); tenant
   isolation of multi-tenant-rbac.md is unchanged (foreign objects 404).
6. **CAS on every API-originated lifecycle write.** Audit row and
   snapshot advance in one transaction keyed on the observed
   `from_state`; the driver's `AppendKillLifecycleLock` remains its own
   single-transaction mutator, serialized by the store's write
   transaction — concurrent kill locks and unlocks serialize, never
   lost-update.
7. **Persist-then-execute effects.** A failed or unwired entry-cancel
   never rolls back a transition; it alerts (LC-12).
8. **Mode invariance.** The lifecycle and clear endpoints exist and
   function identically in paper and live modes; only the EntryCanceler
   effect half needs an OMS.
9. **Machine is the single transition table.** The API maps persisted
   facts into `strategy.Context` and relays `Transition`'s verdict; it
   adds no second table (carve-outs: LC-5's killed redirect, LC-11's
   paper-gate error shape, and LC-8's live-target kill guard).
10. **Open code set.** All new error/alert codes are SCREAMING_SNAKE,
    ≤ 64 chars, open set (SS-25).

## Store surface

New DDL: `kill_clear_events` with its CHECK and index (LC-25), appended
to `schemaDDL` — additive, idempotent. One guarded migration on `Open`
(LC-16a): synthetic draft→paper bootstrap `lifecycle_transitions` rows
for legacy `paper` and `paused` strategies — additive, idempotent, the
`migrateTenancy`/`migrateBilling`/`migrateLiveOMS` pattern (`store.go`).

Changed mutator: `CreateStrategy` becomes ONE transaction — the
strategies row plus, when the initial state is `paper` or `live_*`, the
LC-16a bootstrap transition row.

New mutators (all ONE transaction each):
- `AppendKillClearStrategy(clearID, strategyID, actorID, reason string,
  observedEpoch int64, recordedAt string) (int64, []string, error)`
  — resolves `tenant_id` from `strategies` (audit; `ErrNotFound` unknown),
  checks the strategy-scope active clause, verifies `observedEpoch`
  against the recomputed scope max (LC-27), inserts the clear row, the
  LC-38 supersede markers, and their alerts. Returns the recorded
  `cleared_epoch` and the superseded event ids.
- `AppendKillClearTenant(clearID, tenantID, actorID, reason string,
  observedEpoch int64, recordedAt string) (int64, []string, error)` —
  tenant-scope clause, same shape.
- `AppendKillClearPlatform(clearID, actorID, reason string,
  observedEpoch int64, recordedAt string) (int64, []string, error)` —
  platform clause, same shape.
  All three return the sentinel `ErrNoActiveKill` (new) for LC-31 and
  `ErrClearConflict` (new) for LC-27's 409.
- `AppendLifecycleTransitionCAS(t LifecycleTransition, liveTarget bool)
  (bool, error)` — LC-9; `false` = state conflict, nothing written;
  `liveTarget` re-evaluates the LC-28 predicate in-transaction and an
  active kill is the sentinel `ErrKillActive` (new), nothing written.

New reads:
- `ActiveKill(strategyID) (bool, error)` — LC-28 SQL verbatim.
- `PaperWindowStart(strategyID) (string, bool, error)` — the FULL LC-16
  evaluation: returns `S`; `ok=false` when the gate must fail closed
  (no qualifying row, or none since the newest binding kill).
- `PausedProvenance(strategyID) (string, bool, error)` — newest
  `to_state='paused'` row's `from_state` (LC-7).
- `ListPaperGateFills(strategyID, sinceRFC3339) ([]PaperGateFill, error)`
  — the LC-18 join (`symbol`, `side`, `reduce_only`, `qty_base`,
  `fill_price`, `fee_quote`, `fill_ts`), ordered (`fill_ts`, `fills.rowid`).

Every "newest lifecycle_transitions row" read (LC-7, LC-16) orders
`ORDER BY recorded_at DESC, rowid DESC` — the rowid breaks
second-precision timestamp ties.

`strategy` package addition: `NewPausedFrom(prev State) *Instance` (LC-7).
`GlobalMaxKillEpoch`, `MaxKillEpoch`, and the kill/breaker appenders are
UNCHANGED on `*store.Store` (the LC-34 STAY-RAW callers keep them);
`AppendKillLifecycleLock` stays a non-CAS single-transaction mutator
(invariant 6). The `safety` package's `Store` interface GAINS
`ActiveKill` and DROPS `GlobalMaxKillEpoch` (its sole safety-package
caller was the WD-16 skip, now on `ActiveKill`); all other members
unchanged.

## Wiring seams

- `api.Config.EntryCanceler` (new, optional):
  `interface { CancelOpenEntries(ctx context.Context, strategyID string) error }`
  — cancels un-filled ENTRY orders only, protective reduce-only orders
  untouched (the machine's `EffectCancelEntryOrders` contract). Satisfied
  by the live OMS; in paper mode by the omsbridge's LC-12a canceler; nil
  ⇒ LC-12 alert path.
- `api.Config.PaperSubmitter bool` (LC-14a) — main.go sets it true
  exactly where it wires the omsbridge as the Submitter; a live-OMS
  Submitter leaves it false.
- `api.Config.ExchangeKeysConfigured bool` (LC-8).
- `api.Config.AllocatedCapitalQuote decimal.Decimal` (LC-21) — main.go
  wires the SAME value handed to `runstate.Hydrator`/omsbridge.
- LC-34 call-site changes: `oms/live/submit.go` `submitEntry` (stamp
  read BEFORE the `ActiveKill` check, LC-34a), `runstate/runstate.go`
  `State`, `safety/watchdog.go` `watchOne` and its watch-set membership
  tracker (LC-34b) (+ `safety/monitor.go` `Store` interface swaps
  `GlobalMaxKillEpoch` for `ActiveKill`, §Store surface). No other
  caller of `GlobalMaxKillEpoch`/`MaxKillEpoch` changes.

## Error codes (new; open set)

`INVALID_LIFECYCLE_STATE` 400 · `PLATFORM_CLEAR_ACK_REQUIRED` 400 ·
`USE_KILL_ENDPOINT` 422 · `ILLEGAL_TRANSITION` 422 (including the LC-8
live-target rejection, message "kill tier active") · `PAPER_GATE_FAILED`
422 · `NO_ACTIVE_KILL` 422 · `LIFECYCLE_CONFLICT` 409 · `CLEAR_CONFLICT`
409 (LC-27). There is NO `REASON_REQUIRED` code: an absent or empty
`reason` — and an absent `observed_epoch` — is 400 `SCHEMA_INVALID`, the
API's existing absent-required-field convention (`api/tenants.go`), one
code everywhere (LC-4, LC-30). Reused: `UNKNOWN_STRATEGY`,
`UNKNOWN_TENANT`, `UNAUTHORIZED`, `FORBIDDEN`, `RATE_LIMITED`,
`BODY_TOO_LARGE`, `SCHEMA_INVALID`. New alert kinds:
`lifecycle_entry_cancel_failed`, `kill_effects_superseded`.

## Test obligations

Deterministic, injected clock, store-level fixtures; the RBAC matrix test
(`TestRBACMatrix` pattern) covers the five new routes automatically once
their matrix rows exist.

1. Active-kill scope matrix: strategy/tenant/platform/Phase-1-global kills
   × strategy/tenant/platform clears — each clear flips exactly its scope;
   both-kills case needs both clears; re-kill after clear is active again
   (higher epoch).
2. `NO_ACTIVE_KILL` writes no row; platform ack enforcement writes no row;
   unknown strategy/tenant 404 before body semantics.
3. CAS: concurrent kill-lock vs unlock — exactly one wins, loser 409, one
   audit row per winner; snapshot never skips a state.
4. Paused provenance: paper→paused resumes only to paper; live_l2→paused
   resumes only to live_l2; killed→paused exits ONLY to paper under the
   full guard; provenance reconstructed correctly across restart
   (rehydration from `lifecycle_transitions`).
5. Paper-gate replay: thirty $1 trades fail `min_avg_notional`; PF edge
   cases (gl=0/gp>0 pass, both-zero fail); drawdown breach fails; window
   restart on re-entry to paper discards pre-kill trades; open span not
   counted; no-window fails closed; report shape LC-23 exact.
6. Unlock drills end-to-end: kill → clear (own scope) → unlock-to-paper
   (flat) writes both audit rows and restarts the window; unlock-to-paused
   (not flat) then flatten then paused→paper; unlock attempt with standing
   tenant kill rejected (`KillCleared` false).
7. LC-34 split: after a clear, ENTRY submission passes the standing-kill
   check but an intent stamped pre-kill still abandons stale; approval
   preflight still blocks on a cleared post-verdict kill; watchdog
   re-kills after clear on fresh silence; the WD-16 back-fill appends the
   crash-lost alert even when the watchdog's kill is already cleared
   (LC-34).
8. Effects: pause cancels un-filled ENTRY orders only (protectives
   survive); nil EntryCanceler ⇒ 200 + `lifecycle_entry_cancel_failed`
   alert row; in a paper-mode server a paused paper strategy stops
   filling its resting limit entries (LC-12a).
9. Mode invariance: all five routes respond identically in a paper-mode
   server (no ReconStatus/SafetyDriver wired).
10. Live-mode paper floor (LC-14a): a live-mode server (Submitter = live
    OMS) with a `paper`-state strategy persists approve/clip verdicts
    and NEVER calls `Submitter.SubmitApproved`; the same fixture with
    the paper bridge submits.
11. Stamp-then-check order (LC-34a): a kill committing between
    `submitEntry`'s stamp read and its `ActiveKill` read never transmits
    — rejected by the standing check or abandoned stale by the transmit
    loop, whichever side of the stamp it lands on.
12. LC-16 window: `paused → paper` resume does NOT restart the window
    (pre-pause fills still count, `S` unchanged); an in-place tenant
    kill on a `paper` strategy fails the gate closed even after the
    clear until a pause→resume re-entry restarts the window;
    `killed → paused → paper` restarts the window (the
    paused-after-kill exit qualifies).
13. Bootstrap (LC-16a): `CreateStrategy` at initial `paper` writes the
    strategy row and the bootstrap transition row atomically; reopening
    the store migrates legacy `paper` and `paused` strategies exactly
    once (synthetic row, idempotent; a migrated `paused` strategy's
    PausedProvenance is unchanged).
14. Clear CAS (LC-27): a stale `observed_epoch` is 409 `CLEAR_CONFLICT`
    writing NOTHING (no clear row, no markers, no alerts); the verified
    value clears.
15. Live-target guard (LC-8): `paper → live_*` promotion AND
    `paused → live_*` resume with any active kill binding the strategy
    ⇒ 422 `ILLEGAL_TRANSITION`, message "kill tier active".
16. Watch-set re-entry (LC-34b): after clear + unlock + re-promotion,
    the first watchdog pass does not escalate — `firstWatched`
    re-stamped, stale `lastSeen` deleted.
17. Supersede (LC-38): clearing a kill with unserved effects appends the
    done-marker and one `kill_effects_superseded` alert per event, lists
    the event ids in the response, and the driver never executes them;
    breaker rows are untouched.
18. LC-23 edges: `min_days` measured/required rendered as decimal days;
    absent limits ⇒ `required` "0" and `passed` false; zero closed
    trades ⇒ measured "0" and fail for `min_avg_notional`,
    `profit_factor`, and `max_drawdown`; a sign-flipping fill counts
    only its reducing portion (LC-18).
19. LC-24 cost pin: the paper-gate GET charges the per-token 60/min
    bucket — the burst exhausts to 429 `RATE_LIMITED`.

## Companion edits (required, small)

- `persistence-and-api.md`: add `kill_clear_events` (CHECK + index
  shape, LC-25) to §Tables; add the five routes and the new error codes
  (`CLEAR_CONFLICT` included) to §HTTP API; amend §Execution semantics'
  L0 note — a `paper` strategy auto-executes against the paper OMS ONLY
  when the paper bridge is the wired Submitter, and in a live-mode
  deployment `paper` is part of the L0 floor (LC-14a); replace the
  bootstrap/seeding note with a pointer to LC-16a (`CreateStrategy` is
  now atomic bootstrap).
- `safety-wiring.md`: mark SW-2 landed (pointer here); amend §Ordering,
  precedence, and unlock (clearing machinery now exists — LC-27/28
  supersede "every kill stands once fired"); pin the LC-38 carve-out to
  "served means done" — a `safety_effects` marker written by a clear
  records that the CLEAR is the resolution (audited by
  `kill_effects_superseded`), not that effects executed; invariant 9's
  "machinery deferred" clause now points here; hydrator comment "Phase 1
  has no unlock machinery" (runstate.go) is stale with LC-34; amend
  §Standing-kill check in the OMS entry path and invariant 15 to the
  ActiveKill predicate (with the LC-34a stamp-order pin); carve
  clear-written markers out of invariant 16 (reconcile-gates-serving)
  alongside invariant 3.
- `multi-tenant-rbac.md`: §Permission matrix gains the five rows (LC-2,
  LC-24, LC-29); §Tenant kill-switch gains the clear dual.
- `strategy-lifecycle.md`: add a pointer that enforcement, the paper-gate
  algorithm, and unlock wiring are specified here (LC-15..LC-24, LC-36);
  pin that `min_avg_trade_notional_quote` remains default-only in v1;
  pin the AdminApproval NARROWING — v1 satisfies the L3 approval guard
  only when the ACTING principal maps to Admin/Owner (LC-8),
  approval-by-reference deferred (LC-D2); note that in live mode a
  `paper` strategy never reaches the live venue (LC-14a).
- `watchdog.md`: WD-16's standing-kill skip now keys on `ActiveKill` and
  the crash-lost-alert back-fill runs BEFORE and regardless of the skip
  (LC-34); amend WD-11 — leaving the watch set deletes BOTH in-memory
  map entries, and re-entry after an absence re-stamps `firstWatched`
  and deletes `lastSeen` (LC-34b): stale entries stop being "harmless"
  once clears make re-promotion reachable.
- `risk-limits.md`: §Kill-switch's tier table ("after clearing the
  tenant kill") and its standing-condition paragraph now point to the
  clear rules (LC-28..LC-33) instead of implying deferred machinery.

## Deferred (recorded, not silent)

- LC-D1 — Admin-set `min_avg_trade_notional_quote` override (limit field +
  runtime-change whitelist entry); v1 uses the pinned 25%-of-cap default.
- LC-D2 — approval-by-reference for L3: a persisted admin-approval record
  a Trader can invoke; v1 requires the Admin/Owner to act (LC-8).
- LC-D3 — automated repair of a failed pause entry-cancel (today: alert
  row + gate autonomy block only).
- LC-D4 — SW-3 kill-driven token revocation, deferred again unchanged.
- LC-D5 — SW-4 manual breaker reset / re-trip cool-down (breakers remain
  un-clearable here, LC-37).
- LC-D6 — web dashboard surface for lifecycle/paper-gate/clear operations:
  LANDED — `docs/specs/operator-surface.md` (the lifecycle/clear/paper-gate
  ops panel and its proxies).
- LC-D7 — richer paper-gate analytics (per-symbol floors, Sharpe, fill
  realism) — the gate stays a necessary-but-not-sufficient sanity floor.
