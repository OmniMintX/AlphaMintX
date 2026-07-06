# Safety Wiring: 3-Tier Kill-Switch, PnL Breaker Monitor, Resumable Effects (Phase 3)

Normative. Defines the production wiring of the 3-tier kill-switch into the
Live OMS, the live PnL circuit-breaker monitor, the crash-resumable safety
effects driver, and the drill evidence for PLAN.md Phase 3 exit criteria 2
and 3. This spec is the WIRING/IMPLEMENTATION contract for
`docs/specs/risk-limits.md` §Circuit breaker, §Kill-switch (the normative
parent). Two parent deviations are explicit OVERRIDES, not drift: this
spec overrides the parent's effect order (lock-lifecycle-BEFORE-flatten
instead of after; rationale: an early lock closes the approval window
while flattens are in flight) and narrows the lifecycle-lock scope to
`live_*` states (draft/paper/paused strategies keep their state; the
standing kill row already gate-blocks them). Risk-limits.md §Kill-switch
delegates the normative effect order here. Companion to
`docs/specs/live-oms-and-reconciler.md` (§Safety-engine integration, kill
epoch, claims, flatten, protective obligations, Reconciler R1–R7),
`docs/specs/multi-tenant-rbac.md` (permission matrix, tenant isolation,
§Tenant kill-switch predicate), and `docs/specs/strategy-lifecycle.md`
(`killed` state, unlock guards, no auto-restart).
Push dispatch of the safety events persisted here is normative in
`docs/specs/alert-notifier.md` (AN-*); operator procedures in
`docs/RUNBOOK.md` §9.

## Goals and non-goals

- Goal: make PLAN.md Phase 3 exit criteria 2 and 3 provable. Verbatim:
  - Criterion 2: "Kill-switch drill executed at strategy, tenant, and
    platform tier: ENTRY orders canceled, protective stops preserved
    (canceled only after flatten fills), optional reduce-only flatten, no
    auto-restart, effects resumable across a control-plane restart."
  - Criterion 3: "Circuit breaker fires from the PnL monitor in a
    live-testnet scenario: reduce-only flatten + demote to L0 until next
    UTC day."
- This spec completes the effects engine multi-tenant-rbac.md §Tenant
  kill-switch deferred to "the Phase 3 drills", and lands the platform
  tier deferred there.
- Non-goals (v1, §Deferred): the watchdog heartbeat receiver and its
  escalation to strategy-tier kill (risk-limits.md §Watchdog); kill
  UNLOCK machinery; kill-driven token revocation; manual breaker reset /
  re-trip cool-down (MS-28); per-tenant platform dashboards.

## Kill endpoints — the three tiers

Per risk-limits.md §Kill-switch, all three tiers follow the same procedure:
the activation (intent, scope, flatten choice) is persisted append-only to
`kill_breaker_events` and acknowledged BEFORE any side effect executes
(invariant 1); effects are idempotent and resumable (§Safety-effects
driver). All three endpoints are registered in BOTH paper and live modes —
the gate-block half of a kill (reject `KILL_SWITCH_ACTIVE`, block approval
preflight) is mode-independent; only the effects driver and the monitor are
live-OMS-only (invariant 10).

| Tier | Endpoint | Roles (own tenant) | Classes | Body |
|---|---|---|---|---|
| Strategy | `POST /api/v1/strategies/{id}/kill` | trader, admin, owner | env-admin (any tenant) | `{"flatten": bool}` optional |
| Tenant | `POST /api/v1/tenants/{tenant_id}/kill` (existing, EXTENDED) | admin, owner | env-admin | `{"flatten": bool}` optional |
| Platform | `POST /api/v1/platform/kill` (NEW) | — none | env-admin ONLY | `{"ack": "KILL-PLATFORM", "flatten": bool}` |

Roles follow risk-limits.md §Kill-switch exactly (Trader may trigger a
strategy kill; only Admin/Owner a tenant kill; only the platform
operator — the env-admin class — a platform kill). Normative rules:

- **Strategy tier.** The path `{id}` strategy is tenant-resolved FIRST
  (multi-tenant-rbac.md §Tenancy rules): a foreign or absent strategy is
  404 `UNKNOWN_STRATEGY`, identical to absence — no existence oracle. A new
  `store.AppendStrategyKill` follows the `AppendTenantKill`
  epoch-in-transaction pattern (`internal/store/append.go`): one
  transaction computes `kill_epoch = COALESCE(MAX(kill_epoch), 0) + 1`
  over the WHOLE table and INSERTs the row — `kind='kill'`,
  `scope='strategy'`, `strategy_id` = the strategy, `tenant_id` = the
  strategy's `strategies.tenant_id` (resolved inside the transaction;
  recorded for AUDIT — the kill predicate matches strategy rows on
  `strategy_id`, see below), `flatten` as requested, `trigger_ref` NULL,
  `actor_id` from the principal (token_id or env principal id,
  multi-tenant-rbac.md §Audit identity).
- **Tenant tier.** `handleTenantKill` is EXTENDED, backward compatibly:
  the body gains an OPTIONAL `flatten` field (strict decode; an absent
  field or the v1 empty body `{}` means `flatten=0` — existing callers are
  unaffected). `store.AppendTenantKill` gains the flatten parameter and
  persists it; row shape otherwise unchanged (`scope='tenant'`,
  `strategy_id` NULL, `tenant_id` set, epoch MAX+1 in tx). The v1 "gate
  block ONLY" restriction of multi-tenant-rbac.md §Tenant kill-switch is
  hereby LIFTED: tenant kills now drive effects (§Safety-effects driver).
- **Platform tier.** env-admin ONLY — no tenant role may kill the
  platform, matching "Platform Admin" in risk-limits.md §Kill-switch (the
  env-admin class is the platform operator). The body MUST carry the
  literal acknowledgment `"ack": "KILL-PLATFORM"`; anything else is 400
  `PLATFORM_KILL_ACK_REQUIRED` and NO row is written — the ack literal
  prevents a fat-fingered platform-wide halt (the same explicit-literal
  pattern as `CONTROLPLANE_LIVE_PROD_ACK`, live-oms-and-reconciler.md
  §Config). Row: `scope='platform'`, `strategy_id` NULL, `tenant_id` NULL
  — exactly the Phase-1 global shape, so the EXISTING 3-clause predicate
  (below) makes a platform kill bind every strategy of every tenant with
  no predicate change. Epoch MAX+1 in tx via `store.AppendPlatformKill`.
- **Flatten choice.** `flatten` is the operator's choice at trigger time
  (risk-limits.md §Kill-switch, which delegates the default split here).
  The WIRE default is `false` — an absent field never flattens; flatten
  is the destructive option and MUST be explicit. Live-deployment
  runbooks and UI affordances MUST default the checkbox/flag to
  flatten=true for live books, but the API treats absence as false: the
  tenant-tier extension stays backward compatible and no client can
  flatten by omission.
- **Response** (all three): 200 with
  `{event_id, kill_epoch, recorded_at, flatten}` (the tenant response
  keeps its existing `tenant_id` field; the strategy response adds
  `strategy_id`). The response acknowledges PERSISTENCE, never effect
  completion: effects run asynchronously (§Safety-effects driver) and the
  handler MUST NOT wait on them.
- **Predicate (unchanged).** Kill-active and epoch derivation reuse
  `GlobalMaxKillEpoch` / `MaxKillEpoch` with the normative 3-clause SQL of
  multi-tenant-rbac.md §Tenant kill-switch verbatim
  (`internal/store/runtime.go`): global/platform rows (both ids NULL)
  bind everyone; `strategy_id = :sid` binds strategy rows regardless of
  their audit `tenant_id`; tenant rows (`tenant_id = :tid AND strategy_id
  IS NULL`) bind only their tenant. Strategy-kill rows carrying a non-NULL
  `tenant_id` therefore need NO predicate change (invariant 11).
  Since SW-2 landed, the standing-condition BLOCKERS (hydrator
  `KillActive`, the entry check below, the WD-16 skip) key on the
  clear-aware `ActiveKill` predicate instead (lifecycle-api.md
  LC-28/LC-34); the RAW-epoch reads here keep backing the staleness
  ordering.
- **Epoch monotonicity.** One global counter across all three scopes,
  computed inside the insert transaction (race-free under the store's
  single-connection invariant) — the OMS kill re-check's stale-epoch
  comparison (live-oms-and-reconciler.md §Write-ahead intent journal
  step 3, `internal/oms/live/submit.go` `transmit`) stays well-defined
  whatever tier fired last (invariant 2).

### Standing-kill check in the OMS entry path (normative)

- `submitEntry` (`internal/oms/live/submit.go`) MUST reject with
  `KILL_SWITCH_ACTIVE` when `ActiveKill(strategyID)` is true — the LC-28
  active-kill predicate of `docs/specs/lifecycle-api.md` (SW-2 landed):
  an UNCLEARED kill at any tier blocks, a cleared kill no longer does —
  mirroring the existing `BreakerActiveToday` ENTRY halt. This closes
  the L2/L3 window in which a submission CREATED after the kill stamps
  the fresh post-kill epoch and therefore passes the transmit-loop
  staleness comparison (`transmit`'s `maxEpoch > intent.KillEpoch`
  re-check only catches submissions journaled BEFORE the kill)
  (invariant 15). Stamp-then-check order (LC-34a): the RAW stamp epoch
  (`GlobalMaxKillEpoch`) is read BEFORE the `ActiveKill` check and the
  stamped value is that PRE-check read — a kill committing after the
  stamp carries a higher epoch and the transmit-loop comparison catches
  it; no interleaving lets an intent transmit under a kill it never
  observed.
- Safety-origin submissions (the flatten, protective, and orphan-cancel
  paths) are EXEMPT from this standing check — they MUST run during a
  kill — and rely on the transmit-loop staleness comparison alone. They
  stamp the CURRENT `GlobalMaxKillEpoch` at journal time (post-bump), so
  a kill's own flatten is not self-deadlocked; a SECOND kill mid-flatten
  legitimately abandons the in-flight flatten as `kill_epoch_stale` and
  the re-drive converges on the new epoch.

## Crash-resumable safety effects

### Served/unserved derivation and the `safety_effects` table

Risk-limits.md requires kill/breaker effects to be "idempotent and
resumable: after a control-plane restart, incomplete effects are re-driven
from the persisted intent; completion is recorded per order/position";
live-oms-and-reconciler.md §Reconciler promises pending safety effects
"execute IMMEDIATELY once reconciliation completes". This spec defines
that mechanism: **derive-from-state re-driving keyed on the kill/breaker
row itself** — no task-queue table, no per-order checklist rows.
Per-order completion is already recorded by the order rows the effects
produce (canceled entries, journaled flatten orders, booked fills); the
only NEW persistent fact needed is "this event's effects are complete":

- A `kill_breaker_events` row is **unserved** iff no `safety_effects` row
  references it. Inserting the marker IS the completion record.
- **Mechanism decision (normative).** The marker does NOT extend
  `oms_recon_events` with a `safety_effect_done` kind: its `kind` CHECK
  allowlist is baked into the `CREATE TABLE IF NOT EXISTS` in `schemaDDL`
  (`internal/store/schema.go`) — a no-op on every EXISTING database,
  SQLite cannot `ALTER` a CHECK, and a full table rebuild violates the
  additive-only migration discipline both parent specs pin (an existing
  soak `control.db` MUST open and serve unchanged). A separate tiny
  table is additive, and its PRIMARY KEY gives served-marker idempotence
  for free:

```sql
CREATE TABLE IF NOT EXISTS safety_effects (             -- served markers: the insert IS completion
  event_id TEXT PRIMARY KEY REFERENCES kill_breaker_events,
  completed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS safety_alerts (              -- append-only monitor/driver alerts
  alert_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,                                   -- OPEN set (SS-25 pattern); registry in §Alerts
  strategy_id TEXT, ref_id TEXT,                        -- ref_id: nullable dedupe key (§Alerts)
  details_json TEXT NOT NULL, recorded_at TEXT NOT NULL);
```

- The `safety_effects.event_id` FOREIGN KEY is ENFORCED: `store.Open`'s
  DSN applies `_pragma=foreign_keys(1)` to every connection
  (`internal/store/store.go`), so a served marker can never reference a
  nonexistent event row. Stated as an assumption this spec relies on,
  not a change.

- `safety_alerts.kind` deliberately carries NO CHECK: the
  oms_recon_events lesson is that a baked-in allowlist cannot be extended
  additively; the registry lives in §Alerts and events and consumers
  treat unknown kinds as opaque (the SS-25 open-set rule,
  risk-limits.md §v1 limitations). Both tables are append-only: no
  UPDATE, no DELETE, ever (invariant 13).
- **Unserved scan scope (normative).** BOTH kinds are scanned at ANY
  age until a `safety_effects` marker exists. Kill rows are standing
  conditions; breaker rows carry effect INTENT (flatten to bound the
  loss) that survives the UTC-day boundary — ONLY the ENTRY-halt latch
  (`BreakerActiveToday`) is day-scoped. A prior-day breaker row whose
  effects never completed IS re-driven for effects even though its
  halt expired at 00:00 UTC.
- No migration backfill is needed: every pre-existing kill row is v1
  gate-block-only with `flatten=0`, so on a paper database its effect
  set is empty by construction and the first live drive marks it served
  with zero side effects. A database carrying open v1 gate-block-era
  kill rows that is PROMOTED to live mode WILL have them retroactively
  served on the first drive (entry-cancel sweep plus `live_*` lifecycle
  locks) — intended hardening: a standing kill always meant those
  effects. Runbook note (normative): operators promoting an old
  database to live mode MUST review open kill rows first.

### `DriveSafetyEffects` — the re-driver

The live OMS gains a `DriveSafetyEffects(ctx)` step. Cadence (normative):

- Immediately after EVERY completed reconcile run — startup, periodic,
  stream-reconnect, and on-demand — BEFORE the protective drive, in the
  R7 completion hook (`internal/oms/live/reconcile.go`). Startup order:
  reconcile R1–R7 → DriveSafetyEffects → protective drive — venue truth
  precedes safety sends, per live-oms-and-reconciler.md invariant 2
  (reconcile-before-trade blocks ALL sends, safety flattens included;
  its `recon_blocked_safety` alerting path is unchanged).
- On demand: the kill handlers and the breaker monitor invoke it (via the
  `SafetyDriver` seam, §API surface) asynchronously right after their row
  is acknowledged, so effects do not wait for the next periodic run. In
  paper mode the seam is nil and no drive runs.
- **Reconcile gates the drive (normative).** A served marker asserts
  venue-verified completion. Normative serve condition: a drive pass
  may EXECUTE effects whenever `Reconciled()` is true (startup
  reconcile completed AND no venue reset pending,
  `internal/oms/live/live.go`) — no passes run before that — but it
  may INSERT the served marker for an event ONLY if the last completed
  reconcile run finished STRICTLY after that event row's `recorded_at`
  (second-precision timestamps from independent clocks make an
  equal-second tie ambiguous; strictness costs at most one extra pass).
  Otherwise (e.g. an on-demand pass right after the row append, when
  only a PRE-event startup reconcile has completed) the pass runs the
  effects and DEFERS the marker to the next R7-hooked pass, which
  follows a post-event reconcile (invariant 16). The `resetPending`
  suppression window is covered LOUDLY by the time-based stall alert
  (step 5 below).
- One drive at a time: the drive acquires the SAME `driveMu` that
  serializes protective drives (`internal/oms/live/protective.go`);
  on-demand invocations coalesce on it.

Per drive pass (normative):

1. Scan unserved rows (`ListUnservedSafetyEvents`) in tier precedence
   order platform → tenant → strategy, then insertion (`rowid`) order —
   an efficiency/clarity rule (effects are idempotent, any order
   converges; the platform sweep first makes narrower rows mostly no-ops).
2. Resolve the affected strategy set: strategy scope ⇒ that strategy;
   tenant scope ⇒ every strategy with `strategies.tenant_id` = the row's
   tenant; platform scope ⇒ every strategy (the cancel sweep is global:
   `CancelOpenEntries` with empty strategyID), with flatten applying to
   every strategy holding a nonzero position or non-terminal live order.
3. Effects, in THIS spec's order (the parent override recorded in the
   header: lock BEFORE flatten):
   a. **Cancel ENTRY orders** in scope via `CancelOpenEntries`
      (`internal/oms/live/safety.go`): ENTRY class only; NotFound is
      success (already gone); a claimed-but-unsent `pending_new` intent
      has its claim REVOKED first so the send cannot follow the cancel
      (live-oms-and-reconciler.md §In-flight exclusion); an Ambiguous
      cancel outcome is residual work for the next pass. Journaling
      exception (blessed — a recorded exception to
      live-oms-and-reconciler.md invariant 16, annotated there): the
      kill/breaker row itself is the write-ahead intent for the
      entry-cancel sweep; per-order events are NOT required for
      cancels of LOCAL orders — the cancel updates the local order row
      status — and the drill evidence relies on venue `OpenOrders`
      plus local order statuses. A non-terminal
      ENTRY on a symbol NOT in the configured `Symbols` set has no
      venue mapping and cannot be canceled: it is alerted once
      (`safety_residue_abandoned`, cause `unconfigured_symbol`, step 4)
      and EXCLUDED from residual work.
   b. **Lock lifecycle** (kill rows ONLY): every affected strategy is
      locked via `AppendKillLifecycleLock` (§Store-surface amendment)
      — ONE transaction reads the strategy's CURRENT lifecycle state
      (`from_state` read INSIDE the transaction, never a stale
      pre-read); a `live_*` state appends the transition row to
      `killed` — `actor_id='safety-engine'`, `actor_role='system'`,
      reason referencing the `event_id` — and updates the strategy
      state; strategies already `killed` or non-live no-op with
      `locked=false` (idempotent). Strategies
      in `draft`/`paper`/`paused` keep their state — the standing kill
      condition already gate-blocks them (risk-limits.md gate step 1)
      and destroying non-live state is not what "lock affected
      strategies" protects. Breaker rows NEVER touch lifecycle state
      (strategy-lifecycle.md invariant 4: the breaker demotes effective
      autonomy only).
   c. **Flatten** (iff the row's `flatten` flag is set): for each affected
      strategy with a nonzero position, `FlattenAll(strategy, origin)`
      with origin `'kill'` (or `'breaker'` for breaker rows) — reduce-only
      market orders through the FULL journal path, min(local position,
      venue free balance) sizing, dust and short-balance handling per
      live-oms-and-reconciler.md §Safety-engine integration. The
      double-flatten skip is NEW logic and lives in the safety DRIVER,
      before it calls `Flatten` (no existing code implements it): per
      position, skip iff ANY non-terminal reduce-only market order
      exists for the (strategy, symbol), REGARDLESS of origin —
      matching the origin-agnostic `findLiveProtective(live, sid, sym,
      "market")` pattern in `protective.go`. Origin-independence
      matters: a gate-approved close also produces a non-terminal
      reduce-only market order (origin `'proposal'`, class PROTECTIVE)
      that the ENTRY-only sweep never cancels and that must not be
      stacked with a second flatten. The resting flatten order's
      completion is owned by the journal/Reconciler/fill machinery.
      Pinned by drill KD9.
   d. **Protective stops are NEVER canceled by the kill or breaker
      itself.** Stops-after-flatten is owned by the EXISTING protective
      drive: once flatten fills book the position flat, the drive cancels
      that position's resting protectives (live-oms-and-reconciler.md
      §Safety-engine integration and `driveProtectives`' flat-position
      branch). A kill without flatten leaves every protective resting at
      the venue, and Reconciler R3 never orphan-cancels
      protective-shaped orders (its invariant 11).
4. **Mark served** (`AppendSafetyEffectDone`, INSERT OR IGNORE semantics)
   iff the row's residual work is ZERO after the pass AND the reconcile
   gate above holds: no in-scope non-terminal ENTRY order remains —
   statuses `pending_new`/`open`/`partially_filled`, FSM ranks 0–2 of
   live-oms-and-reconciler.md §Order state machine (rank-3 statuses
   `filled`/`canceled`/`rejected`/`expired` are terminal) — every
   required lifecycle lock is recorded, AND — for flatten rows — every
   in-scope position is flat (`qty_base` zero). Any Ambiguous outcome,
   unresolved flatten, or still-open position leaves the row unserved
   for the next pass.
   **Terminal-residue carve-out (normative).** A position counts as
   ZERO residual work despite a nonzero remainder when (a) the
   remaining qty is dust — below venue minQty or minNotional, the
   `flatten_dust` journal path; or (b) a sell-side shortfall vs the
   venue free balance was alerted (`flatten_short_balance`) and the
   venue-bounded flatten completed; or (c) the symbol is not in the
   configured `Symbols` set (unconfigured-symbol residue; likewise the
   step-3a unconfigured-symbol ENTRY orders). Each carve-out appends a
   ONE-TIME `safety_alerts` row (`kind='safety_residue_abandoned'`,
   `details_json` carrying `cause`: `dust`/`short_balance`/
   `unconfigured_symbol` plus the abandoned quantity) recording the
   residue for OPERATOR action, deduped per (event, strategy, symbol)
   via `ref_id` (§Alerts).
   Serving does NOT wait for post-flatten stop cancels: that obligation
   is independently restart-safe in the protective drive (flat position +
   resting protective ⇒ cancel now, every drive).
5. **Stall alerting (TIME-based, normative).** If an unserved event row
   is older than `safety_effect_stall_seconds` (default 600 — 10
   minutes) and either NO drive pass has run since it was recorded or
   residual work remains, append a `safety_alerts` row
   `kind='safety_effect_stalled'` with `ref_id` = the event_id (at most
   one per event per UTC day; dedupe key (kind, strategy_id, ref_id,
   utcDate) with strategy_id NULL for this kind) plus
   an operator log line, and KEEP retrying. The stall scan MUST run on
   a clock independent of drive passes — normatively, on every
   `safety.Monitor` tick (including ticks skipped pre-reconcile) — so a
   drive suppressed by `resetPending` or a never-completing reconcile
   still alerts LOUDLY.

**Error isolation (normative).** An error while serving one
(event, strategy) — a failed cancel sweep, flatten error, lifecycle
append failure — is logged, counted as residual work for that event,
and the pass CONTINUES with the next strategy and the next event; a
pass never aborts early (invariant 17).

Crash-resumability follows by construction (invariant 4): every step
derives from current store+venue state; a restart runs startup reconcile
then DriveSafetyEffects, which re-computes and finishes the remaining
work; a second restart over a completed world is a pure no-op. Kill rows
with `flatten=0` are served after entry cancels and lifecycle locks alone.

### Ordering, precedence, and unlock

- Precedence is platform > tenant > strategy in the standing-condition
  sense: the gate predicate and epoch counter already encode "most
  restrictive wins, any active kill rejects" (risk-limits.md gate step 1).
- Unlock LANDED (SW-2 — `docs/specs/lifecycle-api.md`): append-only
  `kill_clear_events` rows clear the standing condition per scope —
  strategy, tenant, and platform clears, with the CAS-verified
  `cleared_epoch` and the `ActiveKill` predicate (LC-27/LC-28)
  superseding "every kill stands once fired" — and
  strategy-lifecycle.md's `killed → paper/paused` human-unlock
  transitions run through the lifecycle endpoint (LC-36: clear and
  unlock are two audited acts).
  **No auto-restart** holds by construction and by the absence of any
  code path out of `killed` without a human actor — nothing clears a
  kill as a side effect (lifecycle-api.md invariant 1; restated, not
  re-specified, invariant 9).

## Live PnL circuit-breaker monitor

Risk-limits.md §Circuit breaker requires a monitor that evaluates the
daily-loss condition "on every fill and on a timer (≤ 10 s while positions
are open)" — the breaker fires from the monitor, not only on proposal
arrival. The gate's step 4 and the live OMS's ENTRY halt
(`BreakerActiveToday` in `submitEntry`) and the approval preflight
(`DailyLossBreached` in `cmd/controlplane/main.go`) already exist; this
section wires the MONITOR.

### Placement (normative)

New package `control-plane/internal/safety` owning `safety.Monitor`.
Justification: the monitor needs `runstate.Hydrator.DailyPnL` (risk math),
the per-strategy `daily_loss_limit_quote` from the effective-limits
provider (multi-tenant-rbac.md §Runtime limit changes — NEVER a startup
capture), the store (predicates, event append), and the live OMS's drive
seam. `oms/live` deliberately does not import `runstate` (venue mechanics
vs risk math) and must not start; a monitor inside `oms/live` would invert
that layering. The `safety` package declares narrow consumer-side
interfaces (`DailyPnL(strategyID, now)`, `Limits(strategyID)`,
`DriveSafetyEffects(ctx)`, `Reconciled()` — consulted by
evaluation-loop step 1 — plus the store reads it needs) so fakes drive
the drills; `cmd/controlplane` wires the real implementations.

### Evaluation loop (normative)

- **Cadence.** A timer whose interval is chosen per tick: the ACTIVE
  interval (`CONTROLPLANE_BREAKER_INTERVAL_ACTIVE`, default 5 s, hard
  bound ≤ 10 s per risk-limits.md) while ANY monitored strategy has a
  nonzero position or a non-terminal live order; the IDLE interval
  (`CONTROLPLANE_BREAKER_INTERVAL_IDLE`, default 60 s) when all are flat
  and quiet. The live OMS also invokes `Monitor.Poke(strategyID)` after
  every booked fill (stream and backfill alike) — the "on every fill"
  half of the rule. Poke seam (normative): `live.Config` gains an
  OPTIONAL `OnFill func(strategyID string)` hook, called after booking
  ANY fill; `cmd/controlplane` wires it to `Monitor.Poke` — no `safety`
  import in `oms/live`, no import cycle.
- **Monitored set.** Every strategy in a `live_*` lifecycle state, plus
  any strategy holding a nonzero live position regardless of state
  (`paused`/`killed` books with residual exposure stay monitored, at
  the ACTIVE cadence — the interval rule above counts their positions;
  strategy-lifecycle.md: positions remain managed in every state).
- **Per strategy per tick:**
  1. **Reconcile gate.** The monitor MUST NOT fire before the live
     OMS's first completed startup reconcile: local positions and PnL
     are unverified. While `Reconciled()` is false, skip the tick and
     append `breaker_mark_stale` with `cause='not_reconciled'` (same
     daily dedupe as step 4). Crash-resume of already-persisted breaker
     rows is unaffected — their effects re-drive through the shared
     served/unserved mechanism once reconcile completes.
  2. Skip if `BreakerActiveToday(strategyID, utcDate(now))` — the dedupe
     AND the latch (below).
  3. **Limit guard.** `daily_loss_limit_quote` unset, zero, or negative
     for the strategy (the effective-limits provider's CURRENT value) ⇒
     the monitor NEVER fires for it: skip and append a config alert
     `kind='breaker_limit_unset'` once per strategy per UTC day. A zero
     limit must not instantly kill a misconfigured book; fail loud
     instead.
  4. **Mark staleness (fail-open for firing, loud).** If any open
     position of the strategy lacks a FRESH mark (checked against the
     mark source directly — `Hydrator.DailyPnL` silently contributes
     ZERO unrealized for stale marks, so the monitor MUST perform its own
     freshness check), or if `DailyPnL` errors: the monitor MUST NOT fire
     on this tick, MUST NOT treat the tick as an all-clear, and MUST
     alert — append `safety_alerts` `kind='breaker_mark_stale'` with
     `details_json.cause` `stale_mark` or `pnl_error`. Dedupe is per
     (kind, cause, strategy, UTC day): encode the cause as `ref_id` =
     cause, so the existing (kind, strategy_id, ref_id, utcDate) read
     implements it and a `not_reconciled` alert never suppresses a
     later same-day `stale_mark`/`pnl_error` alert (a log line every
     occurrence). TOCTOU rule
     (normative): the tick's freshness check and the PnL fold MUST use
     the SAME mark snapshot — pass the checked marks into the fold, or
     re-verify freshness after it — so a mark cannot expire between
     check and fold and silently contribute zero. The proposal gate
     remains the fail-closed backstop: stale marks already reject opens
     with `MARK_PRICE_UNAVAILABLE` (market-data.md), so fail-open here
     cannot leak new exposure.
  5. **Trigger.** `DailyPnL(strategyID, now) <=
     -daily_loss_limit_quote` (the provider's CURRENT per-strategy limit;
     decimal comparison, shopspring/decimal — the identical predicate the
     approval preflight uses in `main.go`).
  6. **Fire (persisted-then-executed).** Append the breaker row FIRST:
     `kind='breaker'`, `scope='strategy'`, `strategy_id` set, `tenant_id`
     = the strategy's tenant (audit), `kill_epoch` NULL (a breaker is not
     a kill and never bumps the epoch), `flatten=1` (the breaker ALWAYS
     flattens, risk-limits.md §Circuit breaker — including strategies
     previously killed WITHOUT flatten: the loss bound wins over manual
     book management), `trigger_ref` = the monitor sample as JSON
     `{daily_pnl, limit, evaluated_at}`, `actor_id='breaker-monitor'`.
     THEN invoke `DriveSafetyEffects` — the breaker's effects (cancel
     all ENTRY orders, reduce-only flatten, stops-after-flatten,
     verify-flat) run through the SAME driver and served/unserved
     mechanism as kills, in this spec's effect order.
     Breaker/flatten submissions are exempt from `max_orders_per_minute`
     (risk-limits.md, unchanged).
- **Dedupe.** At most ONE breaker row per strategy per UTC day: the
  `BreakerActiveToday` predicate check precedes the append. Check and
  append are not one transaction — the monitor is a single goroutine, so
  the race is theoretical and a duplicate row would be benign (COUNT > 0
  predicate, idempotent effects) — an accepted benign race. Should
  duplicates ever exist, EACH row needs its own served marker (the serve
  unit is the event row): the first row's pass performs the effects; the
  second's no-op to zero residual work and it is served immediately.
- **Latch.** The halt IS the derived predicate `BreakerActiveToday`
  (`internal/store/liveoms.go`): it survives restart, cannot flap, and
  auto-re-arms at the next 00:00 UTC boundary because the derivation is
  by `recorded_at` UTC date — no reset job, no mutable state
  (live-oms-and-reconciler.md §Safety-engine integration). ENTRY
  submissions halt (`ErrBreakerActive` in `submitEntry`; gate step 4
  rejects opens via the hydrated `BreakerActive`); `close` proposals stay
  exempt (gate step 3) and protectives/reduce-only continue. Demote to L0
  is this same halt: proposals persist, no orders result. Operational
  note (normative): the host clock MUST be NTP-synced (the UTC-day
  latch and re-arm depend on it).
- **Reconcile before fire.** The monitor never fires before the first
  completed startup reconcile (per-tick step 1); already-persisted
  breaker rows (crash-resume) wait for venue truth and re-drive
  immediately after R7 (§Safety-effects driver); the bounded
  `recon_blocked_safety` alerting of live-oms-and-reconciler.md covers
  a reconcile that cannot complete while effects are pending.
- **Lifecycle.** Started by `cmd/controlplane` iff `CONTROLPLANE_OMS_MODE
  =live` (paper deployments run NO monitor — the paper gate's step 4 is
  their breaker, unchanged); stops with server shutdown (context). A
  panic in a monitor tick MUST be recovered, logged, and recorded as
  `safety_alerts` `kind='monitor_tick_panic'`; the monitor continues on
  the next tick; the process NEVER crashes from it (invariant 14).

## No auto-restart (restated)

- Breaker: the demotion ends at the next 00:00 UTC boundary
  (risk-limits.md §Circuit breaker reset; the derived latch above);
  lifecycle state unchanged throughout.
- Kill: recovery ONLY via explicit human unlock per strategy-lifecycle.md
  (`killed → paper/paused`, Admin/Owner, recorded reason, positions-flat
  guard, standing condition cleared) — the clear + unlock machinery is
  `docs/specs/lifecycle-api.md` (SW-2). Nothing in this spec
  re-specifies those transitions.

## Paper-mode invariance (normative)

- The three kill endpoints exist and function in BOTH modes: they are
  store-level appends plus the mode-independent gate-block. A paper
  deployment's kill drills prove gate rejection and approval-preflight
  blocking; only the effects half needs the live OMS.
- `DriveSafetyEffects` and `safety.Monitor` are live-OMS-only wiring: in
  paper mode the `SafetyDriver` seam is nil, no monitor starts, and no
  new behavior reaches the paper OMS. All DDL is additive
  (`CREATE TABLE IF NOT EXISTS` appended to `schemaDDL`, zero ALTERs);
  an existing soak `control.db` opens and serves unchanged.

## Config (normative)

| Env var | Meaning |
|---|---|
| `CONTROLPLANE_BREAKER_INTERVAL_ACTIVE` | Monitor tick seconds while any monitored strategy has a nonzero position or non-terminal order. Integer seconds; DEFAULT 5; bounds [1, 10] (risk-limits.md: ≤ 10 s while positions are open). Out of bounds or non-integer: refuse to start. |
| `CONTROLPLANE_BREAKER_INTERVAL_IDLE` | Monitor tick seconds when all monitored strategies are flat and quiet. DEFAULT 60; bounds [ACTIVE, 600]. Same fail-closed parse rule. |

- Both are read only in live mode; paper deployments ignore them. No new
  secrets or venue credentials are introduced; the no-read-back and
  redaction invariants of multi-tenant-rbac.md and
  live-oms-and-reconciler.md bind unchanged. `safety_effect_stall_seconds`
  (default 600 — the normative stall threshold, §Safety-effects driver
  step 5) joins `CONTROLPLANE_LIVE_OMS_TUNING` as an optional JSON field
  (DisallowUnknownFields pattern).

## API surface

| Method + path | Roles | Classes | Requires |
|---|---|---|---|
| `POST /api/v1/strategies/{id}/kill` | trader/admin/owner (own tenant) | env-admin | — (both modes) |
| `POST /api/v1/platform/kill` | — | env-admin ONLY | — (both modes) |

- Both rows join the exported `api.Permissions()` table with
  `Requires: ""` (always registered — the kill gate-block is
  mode-independent), so `TestRBACMatrix` covers them automatically and
  the registered-route enumeration equality holds (multi-tenant-rbac.md).
- Status semantics per multi-tenant-rbac.md: auth → role/class → object
  resolution; insufficient role is 403 without revealing existence;
  foreign/absent strategy is 404 `UNKNOWN_STRATEGY`.
- **Wiring seam** (normative): `api.Config` gains an OPTIONAL
  `SafetyDriver` interface — `DriveSafetyEffects(ctx) error` — wired to
  the live OMS in `main.go`; nil in paper mode. Kill handlers invoke it
  in a detached goroutine AFTER the 200 response; driver errors are
  logged, never surfaced (the persisted row guarantees eventual effects
  via the reconcile cadence).
- Error codes (registry additions): `PLATFORM_KILL_ACK_REQUIRED` (400 —
  missing/wrong ack literal). `UNKNOWN_STRATEGY`, `UNKNOWN_TENANT`
  reused with their existing meanings.

## Tables and migration (normative)

- New DDL: the two `CREATE TABLE IF NOT EXISTS` statements of
  §Safety-effects (appended to `schemaDDL`; purely additive, no ALTERs,
  no backfill — §Paper-mode invariance). `kill_breaker_events` itself is
  UNCHANGED: `kind` CHECK ('kill','breaker') already admits both row
  kinds; `scope` carries no CHECK, so `'strategy'`/`'platform'` values
  need no DDL; `flatten` and `tenant_id` columns already exist.
- Row rules: timestamps RFC 3339 UTC `Z`; `trigger_ref` and
  `details_json` decimals are strings (ADR-0003); `safety_effects` and
  `safety_alerts` are append-only — no UPDATE, no DELETE.

## Store-surface amendment (normative)

Complete list of new/changed store methods (the
`TestStoreSurfaceIsAppendOnly` allowlist MUST be extended in the SAME
commit; `Append*`/`Insert*`/reads only — no new mutators):

- `AppendStrategyKill(eventID, strategyID, actorID, recordedAt string,
  flatten bool) (int64, error)` — epoch MAX+1 and the tenant-id
  resolution from `strategies` inside ONE transaction; `ErrNotFound` for
  an unknown strategy (defense in depth behind the handler's 404).
- `AppendPlatformKill(eventID, actorID, recordedAt string, flatten bool)
  (int64, error)` — epoch-in-tx, both scope ids NULL.
- `AppendTenantKill(...)` — EXTENDED with the `flatten bool` parameter
  (callers: `handleTenantKill`; existing tests updated in the same
  change).
- `AppendSafetyEffectDone(eventID, completedAt string) error` — INSERT
  OR IGNORE semantics: a duplicate marker is a silent no-op (PK
  idempotence).
- `AppendSafetyAlert(a SafetyAlert) error`.
- `AppendKillLifecycleLock(strategyID, eventID, actorID, recordedAt
  string) (locked bool, err error)` — driver step 3b's mutator: ONE
  transaction reads the strategy's current lifecycle state; a `live_*`
  state appends the lifecycle transition row to `killed` (actor
  'safety-engine', role 'system', reason referencing the kill
  `event_id`) and updates the strategy state; already-`killed` or
  non-live states no-op and return locked=false.
- Reads: `ListUnservedSafetyEvents()` (kill AND breaker rows at ANY age,
  LEFT JOIN `safety_effects`, tier-precedence then rowid order),
  `ListSafetyAlerts(filter)`, `HasSafetyAlertToday(kind, strategyID,
  refID, utcDate string)` (daily dedupe keyed (kind, strategy_id,
  ref_id, utcDate); the empty-matches-NULL rule applies to strategyID
  as well as refID), `HasSafetyAlert(kind, strategyID, refID string)`
  (any-age dedupe for the one-time `safety_residue_abandoned` rows;
  same empty-matches-NULL rule).

## Alerts and events

Read surface: `docs/specs/operator-surface.md` (per-strategy and global
alert feeds, safety-status composite).

| Table | kind / marker | Appended by | Meaning |
|---|---|---|---|
| `safety_effects` | (row presence) | DriveSafetyEffects | The referenced kill/breaker row's effects completed (zero residual work after carve-outs). |
| `safety_alerts` | `breaker_mark_stale` | Monitor | A tick was skipped without firing; `details_json.cause` ∈ {`stale_mark`, `pnl_error`, `not_reconciled`}. Gate remains the fail-closed backstop. `ref_id` = cause; ≤ 1 per (cause, strategy)/UTC day — a `not_reconciled` alert never suppresses a later same-day `stale_mark`/`pnl_error` alert. |
| `safety_alerts` | `breaker_limit_unset` | Monitor | `daily_loss_limit_quote` unset/zero/negative: the monitor never fires for the strategy. ≤ 1/strategy/UTC day. |
| `safety_alerts` | `monitor_tick_panic` | Monitor | A recovered tick panic; the monitor continued. |
| `safety_alerts` | `safety_effect_stalled` | Stall scan (monitor tick) | An event row unserved past `safety_effect_stall_seconds`; retries continue. `ref_id` = event_id; ≤ 1/event/UTC day. |
| `safety_alerts` | `safety_residue_abandoned` | DriveSafetyEffects | ONE-TIME terminal-residue carve-out record for operator action; `details_json.cause` ∈ {`dust`, `short_balance`, `unconfigured_symbol`}. `ref_id` = `event_id/symbol`, strategy in its own column: deduped per (kind, strategy_id, ref_id) — i.e. per (event, strategy, symbol) — any age. |

No new `oms_recon_events` kinds are introduced (invariant 12); the driver
and monitor REUSE existing kinds where they apply (`recon_blocked_safety`,
`flatten_dust`, `flatten_short_balance`, `intent_resolved_absent` with
reason `kill_epoch_stale`).

## Invariants

1. **Persist-then-execute.** Every kill/breaker activation is appended to
   `kill_breaker_events` and acknowledged BEFORE any side effect; API
   responses acknowledge persistence only and never wait on effects.
2. **One epoch counter.** `kill_epoch = MAX(kill_epoch)+1` over the whole
   table, computed inside the insert transaction, across all three
   scopes; breaker rows never carry an epoch.
3. **Served means done.** A `safety_effects` marker is inserted ONLY when
   the event's residual work is zero (no in-scope non-terminal ENTRY,
   lifecycle locks recorded, flatten rows fully flat — where dust,
   alerted short-balance, and unconfigured-symbol residue count as zero
   AFTER their one-time `safety_residue_abandoned` alert); Ambiguous
   outcomes leave the row unserved. Pinned carve-out (lifecycle-api.md
   LC-38): a marker written by a kill-CLEAR records that the clear is
   the audited resolution — one `kill_effects_superseded` alert per
   superseded event — not that effects executed; the driver re-checks
   marker absence immediately before executing an event's effects.
4. **Derive-from-state resumability.** Effects re-derive from store+venue
   state on every drive; restart ⇒ reconcile ⇒ DriveSafetyEffects
   converges; a second restart over a completed world is a no-op.
5. **ENTRY-only destruction.** Kill/breaker sweeps cancel ENTRY orders
   only; protective stops are never canceled by the kill/breaker itself
   and only stops-after-flatten (via the protective drive, after flatten
   fills book flat) removes them.
6. Flatten orders are reduce-only local-intent markers sized
   min(local position, venue free balance) through the full journal path;
   an in-scope position covered by a resting reduce-only market flatten
   (any origin) is never double-flattened.
7. **Breaker once per day.** At most one breaker row per strategy per UTC
   day; the halt is the derived `BreakerActiveToday` predicate — latched
   across restart, auto-re-armed at 00:00 UTC, no mutable latch state.
8. **Loud fail-open monitor.** A stale-mark or errored tick never fires
   and never silently passes: it alerts. The proposal gate's
   `MARK_PRICE_UNAVAILABLE` fail-closed rule is the exposure backstop.
9. **No auto-restart.** Kill recovery requires explicit human unlock
   (strategy-lifecycle.md; machinery landed — `docs/specs/lifecycle-api.md`:
   clear the standing condition, then the lifecycle endpoint's
   `killed → paper/paused` unlock); the breaker changes effective
   autonomy only, never lifecycle state.
10. Kill endpoints exist and gate-block in BOTH modes;
    `DriveSafetyEffects` and the monitor exist ONLY when the live OMS is
    wired; paper behavior is unchanged and all DDL is additive.
11. **Tenant isolation preserved.** Foreign-strategy kill is 404
    identical to absence; the 3-clause kill predicate is unchanged — a
    strategy kill binds its strategy, a tenant kill only its tenant, a
    platform kill (both ids NULL) everyone.
12. No new `oms_recon_events` kinds: its CHECK allowlist is not
    additively extensible on existing databases; new persisted safety
    records live in the new tables.
13. Decimal values are strings end to end; timestamps RFC 3339 UTC `Z`;
    `safety_effects`/`safety_alerts` are append-only; store changes go
    through the enumerated methods only.
14. A monitor tick panic is recovered, logged, and alerted; the monitor
    and the process keep running.
15. **Standing-kill entry check.** `submitEntry` rejects with
    `KILL_SWITCH_ACTIVE` whenever `ActiveKill(strategyID)` is true
    (lifecycle-api.md LC-28 — a cleared kill no longer blocks), with
    the RAW stamp epoch read BEFORE the check (LC-34a); safety-origin
    submissions are exempt and stamp the CURRENT
    (post-bump) epoch at journal time, so a kill's own flatten never
    self-deadlocks and a second kill abandons it as `kill_epoch_stale`.
16. **Reconcile gates serving.** A drive pass may execute effects
    whenever `Reconciled()` is true (no passes run before that), but
    inserts an event's served marker ONLY if the last completed
    reconcile run finished strictly after the event's `recorded_at` —
    otherwise the marker is deferred to the next R7-hooked pass; the
    stall alert is TIME-based and fires even when no pass can run.
    Clear-written markers are exempt (invariant 3's LC-38 carve-out):
    they record supersession, not venue-verified completion, and need
    no post-event reconcile.
17. **Error isolation.** An error serving one (event, strategy) is
    logged and counted as residual work; the pass continues with the
    next strategy and the next event, never aborting early.

## Test obligations

Fake-venue kill drills — deterministic, injected clock, scripted fake
`Exchange`, run as SUBTESTS per tier (strategy, tenant, platform) where the
scenario is tier-independent:

| # | Scenario | Test |
|---|---|---|
| KD1 | Resting ENTRY canceled at venue AND a claimed-unsent intent claim-revoked (no attempt-1 send ever) | `TestKillDrill_EntryCancelAndClaimRevoke` |
| KD2 | Throttled resend mid-kill: kill fires between journal and resend ⇒ abandon `kill_epoch_stale`, id retired | `TestKillDrill_ThrottledResendKillStale` |
| KD3 | Kill WITHOUT flatten: protectives remain at the venue; Reconciler R3 does not orphan-cancel them | `TestKillDrill_ProtectivesPreservedNoFlatten` |
| KD4 | Kill WITH flatten: flatten fills book, stops canceled ONLY AFTER the covering fill (event order pinned), final state flat | `TestKillDrill_FlattenStopsAfterFill` |
| KD5 | Crash between kill append and effects (row appended, effects never run, OMS restarted over the SAME store) ⇒ startup reconcile + drive completes cancels/flatten and marks served; a SECOND restart is a no-op (no duplicate cancels/flattens/markers) | `TestKillDrill_CrashResume` |
| KD6 | Tenant kill does not bleed: tenant B entries still submittable, tenant B approvals pass preflight | `TestKillDrill_TenantScopeNoBleed` |
| KD7 | Platform kill covers ALL tenants: every tenant's entries canceled and gate-blocked | `TestKillDrill_PlatformCoversAllTenants` |
| KD8 | After kill: `SubmitApproved` of an ENTRY returns `KILL_SWITCH_ACTIVE` via the standing-kill check (the fresh submission stamps the post-kill epoch, so the transmit staleness re-check alone would pass it); affected live strategies read `killed`; no transition rows OUT of `killed` ever appended; no code path restarts them | `TestKillDrill_SubmitRejectedNoAutoRestart` |
| KD9 | Double-flatten skip: a resting unfilled non-terminal reduce-only market flatten exists (any origin, including a gate-approved close with origin `proposal`); a re-drive pass produces ZERO new order submissions for that (strategy, symbol) | `TestKillDrill_NoDoubleFlatten` |
| KD10 | Dust carve-out: kill with flatten over a below-minQty/minNotional remainder ⇒ `flatten_dust` journaled, ONE `safety_residue_abandoned` (cause `dust`), row SERVED | `TestKillDrill_DustResidueServed` |

Breaker drills — fake venue, injected clock AND injected marks:

| # | Scenario | Test |
|---|---|---|
| BD1 | Tick with loss ≥ limit fires EXACTLY once (second tick dedupes); effects run in order (entry cancels, reduce-only flatten, stops-after-flatten); same-day ENTRY halt; `close` still allowed | `TestBreakerDrill_FiresOnceWithEffects` |
| BD2 | Latch across restart same UTC day: `BreakerActiveToday` true after restart, entries still halted | `TestBreakerDrill_LatchAcrossRestart` |
| BD3 | Auto re-arm: advance the injected clock past 00:00 UTC ⇒ entries permitted again, no reset job ran | `TestBreakerDrill_RearmNextUTCDay` |
| BD4 | Stale-mark tick: no fire, `breaker_mark_stale` alert appended (once per day), gate still rejects opens | `TestBreakerDrill_StaleMarkNoFire` |
| BD5 | Breaker row appended but effects crashed ⇒ restart re-drives via the shared served/unserved mechanism; an unserved PRIOR-DAY breaker row IS re-driven for effects (the flatten intent survives the day boundary) while its ENTRY halt has expired — entries permitted, position still flattened | `TestBreakerDrill_CrashResumeEffects` |

**Non-vacuous testnet evidence (normative — the drill of criteria 2/3).**
Both tests are skipped unless `CONTROLPLANE_BINANCE_API_KEY`+`_SECRET` are
set with env=testnet, and FAIL on vacuous evidence by construction (the
live-oms-and-reconciler.md §Test obligations pattern):

- `TestTestnetDrill_KillSwitch`: places REAL resting marketable-limit
  entries plus a protective on the testnet; fires a kill with
  flatten=true; asserts via venue `OpenOrders` that zero ENTRY orders
  remain while the protective was preserved until the flatten fill and
  canceled only after it; kills the process mid-effects and asserts the
  restart resumes and serves the row. Non-vacuity: ≥ 1 REAL venue cancel
  and ≥ 1 REAL flatten fill matched by venue trade id — zero fails.
- `TestTestnetDrill_Breaker`: forces a breach with an injected tiny
  `daily_loss_limit_quote` against a real position; asserts the monitor
  fires (breaker row with monitor-sample `trigger_ref`), the venue shows
  the reduce-only flatten fill (by trade id), and same-day entries are
  halted. Zero real flatten fills fails.
- Testnet operational tolerances per live-oms-and-reconciler.md §Config
  (periodic resets, sparse fills ⇒ marketable limits on liquid symbols).
- The "kills the process" step MAY use the in-proc restart equivalent —
  close the OMS and reopen over the SAME store: the resumability under
  test derives from the store, not from process identity.

Additional obligations:

- RBAC: the two new routes join `TestRBACMatrix` automatically via the
  permissions table; `TestPlatformKillRequiresAck` pins the 400 on a
  missing/wrong ack literal (no row written).
- Paper-mode invariance: kill endpoints work in a paper deployment
  (gate-block proven), no monitor goroutine exists, and the RBAC matrix
  is unchanged apart from the two new always-registered rows.
- Store/unit: epoch strictly increases across interleaved
  strategy/tenant/platform kills; `ListUnservedSafetyEvents` excludes
  marked rows ONLY (kill and breaker rows both return at any age until
  marked); `AppendSafetyEffectDone` is idempotent; the alert-dedupe
  reads key on (kind, strategy_id, ref_id, utcDate) and (kind,
  strategy_id, ref_id); pre-existing v1 kill rows on a paper store are
  served with zero side effects on first drive.

## Deferred (recorded, not silent)

- SW-1 — landed: `docs/specs/watchdog.md` (watchdog heartbeat receiver
  and its escalation ladder — silence > 90 s ⇒ ENTRY cancel + alert;
  > 10 min ⇒ the strategy-tier kill fully specified here).
- SW-2 — landed: `docs/specs/lifecycle-api.md` (append-only
  `kill_clear_events` + the `ActiveKill` predicate + the three clear
  endpoints, and the lifecycle endpoint's `killed → paper/paused`
  unlock wiring).
- SW-3 — kill-driven token revocation (ARCHITECTURE.md "tokens are
  revoked on kill-switch"), deferred from multi-tenant-rbac.md and
  consciously deferred again: gate + OMS re-check already deny all
  order flow in scope.
- SW-4 — manual breaker reset and re-trip cool-down policy (MS-28).
- SW-5 — per-tenant platform dashboards for kill/breaker status; v1
  surfaces are the audit tables and recon status.
- SW-6 — tenant/platform-scope BREAKER rows: v1 breaker fires per
  strategy only (limits are per-strategy, MS-16).
- SW-7 — alert DELIVERY (pager/email): alerts are persisted rows plus
  log lines in v1.
