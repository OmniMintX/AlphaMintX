# Watchdog: Heartbeat Receiver and Escalation Ladder (SW-1)

Normative. Implements safety-wiring.md §Deferred item SW-1: the
control-plane heartbeat receiver and the two-rung watchdog escalation
ladder of `docs/specs/risk-limits.md` §Watchdog and heartbeats (the
normative parent — this spec never contradicts it, it wires it). The
kill this ladder escalates INTO is the EXISTING fully-specified
strategy-tier kill of `docs/specs/safety-wiring.md` (§Kill endpoints,
§Safety-effects driver): this spec REUSES `store.AppendStrategyKill`
plus the async `DriveSafetyEffects` re-driver and respecifies neither.
Companion to `docs/ARCHITECTURE.md` §Plane authentication, delivery,
and heartbeats (the 30 s sender cadence), `docs/specs/
persistence-and-api.md` (endpoint conventions), and
`docs/specs/live-oms-and-reconciler.md` (order model, cancel sweep,
reconcile gate).

## Goals and non-goals

- Goal: detect agent-plane silence per strategy and react on the
  parent's ladder — silence > 90 s ⇒ cancel the strategy's ENTRY
  orders only and raise a strategy-tier alert (no flatten, no kill on
  first expiry; PROTECTIVE reduce-only stops stay on the exchange);
  silence > 10 min, OR silence > 90 s with open positions carrying
  unprotected exposure ⇒ escalate to a strategy-tier kill
  (flatten=false; stops remain per safety-wiring.md kill semantics).
- Goal: land the heartbeat receiver endpoint ARCHITECTURE.md deferred
  ("endpoint deferred to the watchdog slice") and correct the
  agent-plane client stub's non-conforming path.
- Non-goals (v1): no web UI surface for watchdog state (alerts land in
  `safety_alerts`; a viewer surface is a later slice); no per-heartbeat
  persistence (§Heartbeat state); no tenant- or platform-tier watchdog
  escalation (the ladder is strategy-tier only, matching the parent);
  no agent-plane self-restart or supervision logic (the watchdog reacts
  on the control plane; reviving the agent is an operator act); no
  kill UNLOCK machinery (still SW-2).

## Heartbeat endpoint

- **WD-1.** The control plane MUST serve
  `POST /api/v1/strategies/{id}/heartbeat`. The repo-wide path
  convention is `/api/v1/...` (persistence-and-api.md §HTTP API); the
  existing agent-plane stub `heartbeat_path` in
  `agent-plane/src/alphamintx_agent_plane/client/controlplane.py`
  returns `/v1/strategies/{id}/heartbeat` — a BUG in the stub, and it
  MUST be corrected to `/api/v1/strategies/{id}/heartbeat` in the same
  change (§Agent-plane sender). The spec path is authoritative.
- **WD-2.** Auth is exactly the proposals/traces pattern: the route
  joins `api.Permissions()` with `Classes: [agent]` — env
  `AgentTokens` AND DB agent tokens both resolve to `classAgent`
  (`resolvePrincipal`, `internal/api/auth.go`) — and the existing
  `guard` middleware enforces the strategy scope: a `classAgent`
  principal whose `strategyID` differs from the path `{id}` is 403
  `STRATEGY_SCOPE_MISMATCH` before the handler runs. No user role, no
  read/operator/env-admin class may POST heartbeats.
- **WD-3.** The route is ALWAYS registered (`Requires: ""`), in paper
  AND live modes: heartbeat RECEIPT is mode-independent (a timestamp
  update), and an unregistered route would make every paper-mode agent
  log WARNING per 30 s for a 404. In paper mode (no monitor wired) the
  handler accepts and discards (§Wiring seams). The row joins the
  exported permission matrix so `TestRBACMatrix` covers it
  automatically (multi-tenant-rbac.md §Test requirements).
- **WD-4.** Body: `{}` — no fields in v1. Decode via
  `decodeStrictOptional` (`internal/api/recon.go`): an EMPTY body is
  accepted as `{}`; any present body MUST decode strictly
  (`DisallowUnknownFields`); unknown fields or trailing data are 400
  `SCHEMA_INVALID`; an oversized body is 413 `BODY_TOO_LARGE`.
- **WD-5.** Response: 200 `{"received_at": "<RFC 3339 UTC Z>"}` — the
  server clock instant recorded as the beat. The response acknowledges
  RECEIPT only; it never reflects watchdog evaluation state.
- **WD-6.** Rate limiting: the standard per-token POST limit (60
  req/min, `rateLimitBurst` in `internal/api/auth.go`) applies via the
  guard and comfortably admits the 30 s cadence (2/min). Heartbeats
  MUST NOT charge the per-strategy 30/min proposal limiter (`s.prl`) —
  that limiter is charged only inside `handlePostProposal` and the
  heartbeat handler MUST NOT touch it.
- **WD-7.** The handler performs NO lifecycle-state predicate and NO
  store write: a beat for a paused, killed, or paper strategy is
  accepted 200 identically (receipt is just a timestamp; the WATCH SET
  decides what silence means, §Watch set). A `classAgent` token whose
  scope matches the path is sufficient — the token's existence already
  binds it to a real strategy at mint time (`handleMintToken`
  resolves the strategy in-tenant; env tokens are deploy-time wiring).

## Heartbeat state (in-memory, normative)

- **WD-8.** Heartbeats are NEVER persisted per-POST. 30 s × N
  strategies forever is pure write amplification on an append-only
  SQLite store with no VACUUM allowed (persistence-and-api.md); a
  heartbeat is evidence of liveness NOW, worthless as history beyond
  the alerts the watchdog derives from it. Last-seen state lives in
  memory: a mutex-guarded map `strategy_id -> lastSeen time.Time` on
  the `safety.Monitor` (§Placement), updated by a NEW
  `Monitor.Beat(strategyID string, at time.Time)` method.
- **WD-9. Restart baseline (accepted, documented liveness gap).** A
  control-plane restart loses `lastSeen`. The baseline for a strategy
  with no recorded beat is `max(process start, the moment the strategy
  first entered the watch set)` — the Monitor records both in memory
  (monitor start time; a `firstWatched` map stamped when a strategy is
  first observed in the watch set). Consequences, stated honestly: a
  restart grants every watched strategy a fresh 90 s / 10 min window
  even if its agent died long before; a strategy whose agent NEVER
  sends a first heartbeat is still caught — 90 s after it enters the
  watch set, then killed at 10 min. This gap is accepted for v1: the
  alternative (persisting beats or last-seen) violates WD-8, and the
  window is bounded by the same thresholds that bound normal detection.

## Watch set (normative)

- **WD-10.** The watchdog evaluates strategies in `live_*` lifecycle
  states ONLY (`strategy-lifecycle.md`). Justification per excluded
  state: `draft`/`paper` strategies have no venue orders — rung 1 has
  nothing to cancel and a paper agent's silence is not a live-money
  event; `killed` strategies are already locked and swept by the
  standing kill's own effects — the lifecycle lock is driver step 3b
  and the no-auto-restart rule is invariant 9 of safety-wiring.md
  (invariant 5 there is the ENTRY-only destruction rule, a different
  property); `paused` strategies were suspended by a human who
  deliberately stopped the agent's runs — heartbeat silence is the
  EXPECTED state, and a paused
  book's open positions remain covered exactly as a killed book's are:
  protective stops rest on the exchange (invariant 2 of
  ARCHITECTURE.md), the protective drive keeps driving them, and the
  breaker monitor keeps the position in its monitored set regardless
  of lifecycle state (`monitored()` in `internal/safety/tick.go`).
  Watchdog silence adds no protection beyond what pause already
  implies; killing a paused book for expected silence would be a false
  positive by construction. The watch set is therefore STRICTLY the
  `live_*` subset of the Monitor's existing candidate scan.
- **WD-11 (AMENDED by lifecycle-api.md LC-34b).** Entering the watch
  set stamps `firstWatched` (WD-9); leaving it (kill lock, pause, any
  non-live transition) removes the strategy from evaluation on the
  next tick AND deletes BOTH its in-memory map entries (`firstWatched`
  and `lastSeen`). Re-entry after an absence re-stamps
  `firstWatched = now` and deletes any stale `lastSeen`: with kill
  clears making re-promotion reachable, stale entries stopped being
  harmless — after clear + unlock + re-promotion the first pass never
  escalates on pre-kill staleness.

## Placement and cadence (normative)

- **WD-12.** The watchdog runs INSIDE the existing `safety.Monitor`
  tick (`internal/safety/monitor.go`, `tick.go`) — NOT a new
  goroutine. The Monitor already owns everything the watchdog needs:
  an injected clock (`Config.Now`), panic isolation (`safeTick` →
  `monitor_tick_panic`, invariant 14 of safety-wiring.md), poke
  coalescing, the ACTIVE/IDLE cadence, the store seam, the
  `HasSafetyAlertToday` dedupe helpers, and live-mode-only lifecycle
  (started by `cmd/controlplane` iff `CONTROLPLANE_OMS_MODE=live`).
  The watchdog pass runs once per tick — after the breaker
  evaluation on reconciled ticks; on every tick otherwise
  (pre-reconcile ticks run the watchdog pass alone) — over the
  WD-10 watch set. Tick-structure requirement: today's
  `tick()` returns EARLY while `!recon.Reconciled()` (the step-1
  reconcile gate in `tick.go`), BEFORE any per-strategy evaluation —
  a watchdog pass placed only "after the breaker evaluation" would
  therefore be silently recon-gated in full, deferring `firstWatched`
  stamping (WD-9/WD-11) and the rung-2 kill append (WD-14). `tick()`
  is restructured so the watchdog pass runs on EVERY tick,
  pre-reconcile ticks included (like the deferred stall scan); only
  the WD-14-recon-gated actions — the rung-1 ENTRY sweep and the
  unprotected-exposure fast path — are skipped while `Reconciled()`
  is false.
- **WD-13. Detection-latency bound (cadence decision).** The Monitor
  ticks at `CONTROLPLANE_BREAKER_INTERVAL_ACTIVE` (default 5 s) while
  ANY monitored strategy has a nonzero position or non-terminal live
  order, and `CONTROLPLANE_BREAKER_INTERVAL_IDLE` (default 60 s)
  otherwise (`interval()` in `tick.go`). Decision: ACCEPT the existing
  cadence and bound the latency — option (a) — with NO forced-ACTIVE
  rule and NO idle cap. Normative bound: a threshold crossing is
  detected within `threshold + current tick interval`; thresholds are
  lower bounds and the watchdog NEVER fires early. Quantitative
  justification: the IDLE interval is selected ONLY when every
  monitored strategy is flat with zero non-terminal orders — in
  exactly that state rung 1 is a no-op (there is no ENTRY order to
  cancel) and rung 2 protects nothing time-critical (no orders, no
  positions), so the worst case that MATTERS is the ACTIVE one.
  Only the generic `threshold + current tick interval` bound is
  normative; the concrete figures are DEFAULT-config illustrations:
  at the defaults, rung 1 within 90+5 = 95 s and rung 2 within
  600+5 = 605 s. `parseBreakerIntervals`
  (`cmd/controlplane/config.go`) admits ACTIVE ∈ [1, 10] s and
  IDLE ∈ [ACTIVE, 600] s, so legal configuration stretches the
  ACTIVE cases to 100 s / 610 s and the idle-state cases to
  690 s / 1200 s — all still within the normative bound. The
  idle-state worst cases (150 s / 660 s at the defaults) delay only
  an alert and a lifecycle lock over a provably empty book. Options
  (b) force-ACTIVE on heartbeat age and (c) a 30 s idle cap were
  rejected: both burn wake-ups forever to improve a bound that only
  applies when nothing is at stake.
- **WD-14. Reconcile gating.** Heartbeat RECEIPT is never gated —
  `Beat` is a timestamp update, valid at any time including before the
  startup reconcile. Watchdog EVALUATION splits per rung:
  - Rung 1 (ENTRY cancel) and the unprotected-exposure fast path
    REQUIRE `Reconciled()` == true (the Monitor's existing `ReconGate`
    seam): the cancel sweep and the exposure predicate read local
    order/position state that is unverified before the first completed
    startup reconcile, and live-oms-and-reconciler.md invariant 2
    blocks all sends pre-reconcile anyway. While `Reconciled()` is
    false these actions are SKIPPED for the tick; silence is durably
    re-observable, so nothing is lost — the next post-reconcile tick
    re-detects and acts. No watchdog-specific alert is appended for
    the deferral: the Monitor's existing `breaker_mark_stale` /
    `not_reconciled` daily alert and the live OMS's
    `recon_blocked_safety` path already cover a stuck reconcile.
  - Rung 2 on the PURE 10-minute condition is NOT recon-gated: it is
    computable from the clock alone, the kill append is
    persist-then-execute (safety-wiring.md invariant 1), and its
    effects are independently recon-gated inside `DriveSafetyEffects`
    (invariant 16 there). A control plane that restarts against a dead
    venue AND a dead agent therefore still locks the strategy and
    gate-blocks new flow 10 minutes after startup.

## Evaluation ladder (normative)

Per watched strategy per tick, with `age = now − max(lastSeen,
baseline)` (WD-8/WD-9):

- **WD-15. Quiet.** `age ≤ 90 s` ⇒ no action. There is no
  "recovered" bookkeeping: rung 1 is stateless (WD-17), and a resumed
  heartbeat simply stops the ladder from re-triggering.
- **WD-16. Standing-kill skip (idempotence — AMENDED by
  lifecycle-api.md LC-34).** If `ActiveKill(strategyID)` is true (the
  LC-28 active-kill predicate — any UNCLEARED strategy/tenant/platform
  kill binding the strategy; the `safety.Store` interface swapped
  `GlobalMaxKillEpoch` for `ActiveKill`),
  the watchdog SKIPS the strategy entirely: the kill's own effect
  machinery owns the sweep and the lock, and a second kill row MUST
  NOT be appended. A CLEARED kill no longer skips: after a clear the
  watchdog is RE-ARMED and may kill again on fresh silence. This check runs BEFORE any rung on every tick, so
  escalation is exactly-once-ish: the window between two ticks cannot
  double-fire (one goroutine), and the post-escalation ticks skip
  until the lifecycle lock removes the strategy from the watch set.
  Escalation-alert back-fill (crash repair): the back-fill needs
  the standing kill row's `event_id` and `actor_id`, which NO
  existing read exposes — `GlobalMaxKillEpoch` returns the epoch
  alone (`internal/store/runtime.go`) and
  `ListUnservedSafetyEvents` excludes served rows
  (`internal/store/safety.go`) — so it reads them via
  the NEW `LatestStrategyKillEvent` accessor (§Wiring seams); the
  back-fill runs BEFORE and REGARDLESS of the skip (LC-34), so a
  cleared kill still gets its crash-lost alert repaired. When
  that newest strategy-scope kill row has `actor_id = 'watchdog'` and
  `HasSafetyAlert("watchdog_kill_escalation", strategyID, event_id)`
  finds no alert for that `event_id`, the skip path MUST append the
  missing `watchdog_kill_escalation` alert (idempotent late append)
  — a crash between the WD-19 step-2 kill append and the step-3
  alert append would otherwise lose the alert FOREVER, since every
  subsequent tick lands in this skip. The back-filled alert's
  `details_json` is `{"cause":"backfill"}` exactly: the original
  cause and the `last_seen`/`silence_seconds` figures did not
  survive the crash and MUST NOT be fabricated (WD-21). Lift
  semantics (REVISITED, as this paragraph demanded —
  lifecycle-api.md LC-34): with the skip keyed on the clear-aware
  `ActiveKill` predicate, a kill-clear re-arms the watchdog and a
  re-promoted strategy is watched again; the OMS entry path's
  standing-kill gate (`internal/oms/live/submit.go`) moved to
  `ActiveKill` in the SAME change, so both gates stayed consistent.
- **WD-17. Rung 1 — silence > 90 s (derive-from-state, NOT
  persisted).** `kill_breaker_events.kind` carries a baked-in CHECK
  `(kind IN ('kill','breaker'))` and the migration discipline is
  additive-only (safety-wiring.md §Tables — SQLite cannot ALTER a
  CHECK), so the 90 s reaction CANNOT be a new persisted event kind
  and MUST NOT be one. Instead it derives from state: silence is
  durably re-observable, so on EVERY tick while `age > 90 s` holds
  (and rung 2 has not fired) the watchdog:
  1. Appends the `watchdog_silence` alert FIRST (WD-21) — the alert
     must not be lost if the cancel errors — deduped once per strategy
     per UTC day via `HasSafetyAlertToday`;
  2. Invokes the ENTRY-cancel sweep via the Entries seam (WD-18),
     subject to the WD-14 recon gate.
  Alert-append failure path: if the `watchdog_silence` append errors
  (`AppendSafetyAlert` failure), the sweep does NOT run this tick —
  fail closed for the tick, the breaker precedent (`fire` returns
  without driving when its row append fails,
  `internal/safety/alert.go`); silence is durably re-observable, so
  the next tick retries alert-then-sweep. A PERSISTENTLY failing
  store (e.g., disk full) therefore suppresses BOTH rungs — rung
  2's `AppendStrategyKill` writes to the same store — a systemic
  failure surfaced by the Monitor's own error logging, out of
  watchdog scope.
  The CANCEL action repeats every tick while silence persists —
  idempotent by the sweep's own semantics (already-canceled orders are
  NotFound = success) — so a crash mid-cancel self-heals on the next
  tick with no persisted checkpoint, and a fresh ENTRY that somehow
  appears during silence is swept too. Only the alert is deduped;
  the action never is.
- **WD-18. The cancel sweep is the kill driver's sweep, reused.** The
  reaction calls the EXISTING `CancelOpenEntries(ctx, strategyID)`
  (`internal/oms/live/safety.go`) — the same function
  `DriveSafetyEffects` step 3a uses — with its exact semantics
  (safety-wiring.md §Safety-effects driver step 3a, not duplicated
  here): ENTRY class only; NotFound is success; a claimed-but-unsent
  `pending_new` intent has its claim REVOKED first
  (`RecordIntentClaimRevoked`) so the send cannot follow the cancel;
  an Ambiguous outcome is left for the next tick (here: the next
  watchdog re-cancel, since no served-marker machinery applies —
  silence itself is the standing re-drive condition); an
  unconfigured-symbol order is skipped. Because the watchdog is in
  package `safety`, which never imports `oms/live`, this is a NEW
  consumer-side seam: `safety.Config` gains an `Entries` field of NEW
  interface type `EntryCanceller { CancelOpenEntries(ctx
  context.Context, strategyID string) error }`, satisfied by
  `*live.OMS`'s existing method and wired in `cmd/controlplane`
  alongside `Driver`/`Recon`. Cancel errors are logged and the tick
  continues with the next strategy (the safety-wiring invariant-17
  error-isolation pattern).
- **WD-19. Rung 2 — escalate to the strategy-tier kill.** Condition:
  `age > 600 s`, OR (`age > 90 s` AND the strategy has unprotected
  exposure per WD-20 — fast path, recon-gated per WD-14). Reaction,
  in order:
  1. Re-check WD-16 (already evaluated this tick; restated: no append
     if any standing kill binds the strategy).
  2. `AppendStrategyKill(eventID, strategyID, actorID="watchdog",
     recordedAt, flatten=false)` (`internal/store/safety.go`) — the
     existing epoch-in-transaction strategy-kill append, VERBATIM
     semantics from safety-wiring.md §Kill endpoints: `kind='kill'`,
     `scope='strategy'`, epoch MAX+1 in tx, tenant resolved in tx for
     audit. `flatten=false` per the parent ("flatten off by default;
     stops remain"): the watchdog reacts to ABSENCE of information —
     destroying positions is the operator's escalation, not the
     machine's. `trigger_ref` is NULL for `AppendStrategyKill` today;
     the silence evidence goes in the alert's `details_json` (WD-21).
     Actor registry note: safety-wiring.md uses `'safety-engine'` (the
     lifecycle-lock actor, unchanged — the driver still records
     `AppendKillLifecycleLock(..., "safety-engine", ...)`) and
     `'breaker-monitor'`; `actor_id` is an open TEXT column and
     `'watchdog'` extends that set explicitly here.
  3. Append the `watchdog_kill_escalation` alert (WD-21), ref_id = the
     kill `event_id` — after the row exists (the ref needs the id),
     before the effect attempt. A crash between step 2 and this step
     is repaired by the WD-16 back-fill on a later tick's skip path.
  4. Invoke `DriveSafetyEffects` asynchronously in a
     panic-recovered goroutine — exactly the Monitor's existing
     breaker `fire` pattern (`internal/safety/alert.go`). Everything
     downstream (ENTRY sweep, lifecycle lock to `killed`, served
     marker, crash-resume via the unserved-event re-drive, stall
     alerting) is safety-wiring.md machinery, reused not respecified.
     Crash between step 2 and step 4 is safe: the kill row is an
     unserved event and the next reconcile-hooked drive pass serves it.
- **WD-20. Unprotected exposure (computable predicate).** Strategy S
  has unprotected exposure iff there EXISTS a position row
  (`ListPositions(S)`, `internal/store`) with `qty_base ≠ 0` in symbol
  Y such that NO row of `ListNonTerminalLiveOrders()`
  (`internal/store/liveoms.go` — statuses `pending_new`/`open`/
  `partially_filled`, FSM ranks 0–2) has `StrategyID == S AND Symbol
  == Y AND Class == "PROTECTIVE"`. Grounding in the real columns:
  `store.LiveOrder` embeds `store.Order`, whose relevant fields are
  `Class` (`orders.class`, `'ENTRY'`/`'PROTECTIVE'`), `ReduceOnly`
  (`orders.reduce_only`), `Status`, `Symbol`, `StrategyID`, `Type` —
  there is no `purpose`/`kind` order column in this codebase. The
  predicate keys on `Class == "PROTECTIVE"` alone, type- and
  origin-agnostic — matching `findLiveProtective`'s
  `Class == "PROTECTIVE"` scan (`internal/oms/live/protective.go`)
  WITHOUT its type filter (that helper additionally filters on
  `Type`; the predicate here deliberately does not): any
  non-terminal PROTECTIVE-class order — resting stop (`stop_limit`)
  or in-flight reduce-only market close — counts as protection
  (PROTECTIVE-class orders are reduce-only by construction,
  risk-limits.md §Order classes and reduce-only; `reduce_only` is
  recorded but not re-tested here). A position whose
  protective was filled/canceled and not yet re-driven counts as
  UNPROTECTED — conservative in the correct direction: an unmanaged
  agent with naked exposure is precisely the fast-path case.
  Dust carve-out (fail toward PROTECTED). A residue the OMS itself
  cannot flatten or protect MUST NOT arm the fast path: the flatten
  path abandons quantities below `minQty`, or below `minNotional` at
  a fresh mark, with a `flatten_dust` event and NO order
  (`internal/oms/live/flatten.go`), and the protective drive rejects
  placements below the venue minimum (`ErrBelowMinNotional`,
  `internal/oms/live/protective.go`) — such a position can NEVER
  acquire a PROTECTIVE-class order, so counting it as unprotected
  would hold the fast path armed forever and turn any > 90 s blip
  into a kill of a healthy strategy. The predicate therefore
  EXCLUDES a position whose |`qty_base`| is below the symbol's
  `minQty`, and a position whose notional at a FRESH mark is below
  `minNotional`; when no fresh mark is available to price the
  notional test, the position is EXCLUDED too — fail toward
  PROTECTED. This is safe because the fast path is only an
  ACCELERATOR: the unconditional 10-minute rung remains the
  backstop for any exposure the carve-out misjudges.
  `minQty`/`minNotional` come from the NEW Filters seam (§Wiring
  seams): the venue minimums live only inside the live OMS
  (`symbolFiltersFor`, `internal/oms/live/filters.go`) and package
  `safety` never imports `oms/live`; a Filters miss (filters
  unloaded or expired — the `ErrFilterUnavailable` condition)
  likewise EXCLUDES the position. "Fresh mark" is DEFINED as the
  Monitor's existing `MarkSource.Mark(symbol, now)` returning ok
  (`internal/safety/monitor.go`) — the SAME freshness the breaker
  applies in `staleSymbol` (`internal/safety/tick.go`); the
  watchdog introduces no second freshness notion. Consequence,
  stated plainly: during a mark outage the no-fresh-mark exclusion
  disarms the fast path VENUE-WIDE (every notional test fails
  toward PROTECTED) and only the 10-minute rung remains — accepted
  because the breaker is equally mark-blind in exactly that state
  (`staleSymbol` alerts and returns instead of firing). This
  DELIBERATELY diverges from the flatten path's no-mark behavior —
  "send anyway and let the venue arbitrate"
  (`internal/oms/live/flatten.go`) — because the stakes differ: a
  markless flatten risks only a venue rejection, while a markless
  fast-path kill would destroy a healthy strategy. Implementers
  MUST NOT "correct" either behavior to match the other.

## Wiring seams (normative; NEW markers)

- NEW `Monitor.Beat(strategyID string, at time.Time)` — mutex-guarded
  map write (WD-8); safe from any goroutine; never blocks a tick.
- NEW `safety.Config.Entries EntryCanceller` (WD-18) — required in
  live wiring; `New` rejects a nil `Entries` ONLY when the watchdog
  is enabled AND the deployment is live — since the Monitor is
  constructed only in live mode, the check inside `New` reduces to
  `!WatchdogDisabled` (a disabled watchdog must not demand a seam it
  never calls).
- NEW `safety.Config.Filters FiltersProvider` (WD-20) — NEW
  interface type `FiltersProvider { MinFilters(symbol string)
  (minQty, minNotional decimal.Decimal, ok bool) }`, ONE method
  returning both venue minimums; ok=false when filters are
  unloaded or expired (the `ErrFilterUnavailable` condition),
  which WD-20 treats as EXCLUDE — fail toward PROTECTED. Backed by
  the live OMS's filter state (`symbolFiltersFor`,
  `internal/oms/live/filters.go`) via a small exported adapter and
  wired in `cmd/controlplane` from the oms/live side exactly like
  `Entries`; the same `!WatchdogDisabled` nil-check rule applies.
- NEW `safety.Config.WatchdogDisabled bool` (§Config) — parsed in
  `cmd/controlplane` from `CONTROLPLANE_WATCHDOG_DISABLED` (`1`/
  `true` disables; anything else, including unset, enables), read
  only in live mode exactly like the breaker-interval knobs
  (`parseBreakerIntervals`, `cmd/controlplane/config.go`); paper
  deployments never read it.
- EXTENDED `safety.Store` interface:
  `GlobalMaxKillEpoch(strategyID string) (int64, error)`,
  `AppendStrategyKill(eventID, strategyID, actorID, recordedAt string,
  flatten bool) (int64, error)`, and `HasSafetyAlert(kind, strategyID,
  refID string) (bool, error)` ALREADY EXIST on `*store.Store` (pure
  consumer-side widening), PLUS one genuinely NEW read-only store
  accessor required by the WD-16 back-fill:
  `LatestStrategyKillEvent(strategyID string) (eventID, actorID
  string, ok bool, err error)` — the newest `kind='kill'`,
  `scope='strategy'` row for the strategy, served or not (ok=false
  when none) — because `GlobalMaxKillEpoch` returns the epoch alone
  (`internal/store/runtime.go`) and `ListUnservedSafetyEvents`
  excludes served rows (`internal/store/safety.go`). RETRACTED: an
  earlier draft claimed "zero new store methods" and no
  `TestStoreSurfaceIsAppendOnly` change; a read-only accessor does
  not violate the append-only surface, and that test's allowlist
  gains exactly `LatestStrategyKillEvent`.
- NEW `api.Config.Heartbeats` field of NEW interface type
  `HeartbeatSink { Beat(strategyID string, at time.Time) }`, wired to
  the Monitor in `cmd/controlplane` (live mode); nil in paper mode —
  the handler answers 200 either way (WD-3). No `api` → `safety`
  import inversion concern: `api` declares the interface, `main.go`
  wires the implementation (the `SafetyDriver` precedent).
- NEW handler `handleHeartbeat` in `internal/api` plus the handlers
  map entry and permission row
  `{Method: "POST", Path: "/api/v1/strategies/{id}/heartbeat",
  Classes: [agent]}` in `permissions.go`.

## Agent-plane sender (normative)

- **WD-22.** The sender is a SEPARATE asyncio background task per
  strategy inside `Scheduler.run` (`agent-plane/src/
  alphamintx_agent_plane/scheduler/loop.py`), named
  `heartbeat:{strategy_id}` beside the existing
  `scheduler:{strategy_id}` tasks — NOT a per-tick piggyback: the tick
  interval (default 60 s) exceeds the 30 s cadence, and a slow tick
  (LLM latency, debate rounds) would false-trigger the watchdog.
  Task loop: `POST heartbeat; sleep to the next interval boundary`.
  Bounded-run termination: `Scheduler.run(max_ticks=…)` gathers all
  tasks and cancels only in its `finally` (`loop.py`), so a
  never-ending heartbeat task would hang a bounded run at the
  primary gather. Heartbeat tasks MUST be cancelled once every
  strategy loop has finished — equivalently, excluded from the
  primary gather and cancelled in the shutdown path — so
  `run(max_ticks=N)` terminates.
- **WD-23.** Failure isolation: any exception from the POST
  (`ControlPlaneUnavailableError`, auth errors, transport errors) is
  logged at WARNING and the loop CONTINUES on cadence; the heartbeat
  task never crashes the scheduler, never blocks or delays ticks, and
  never touches the checkpoint DB (`SqliteSaver`) or tick state.
  SIGTERM/SIGINT cancel it with the other tasks (the existing
  `run_task.cancel` → `asyncio.gather(..., return_exceptions=True)`
  shutdown path in `__main__.py`/`Scheduler.run`).
  Executor isolation: `asyncio.wait_for` around an executor future
  cancels the AWAIT, not the worker thread — an abandoned transport
  chain runs to completion, worst case ≈ 90 s (3 × 10 s timeouts
  plus two backoffs each clamped to 30 s via `Retry-After`;
  `client/http.py`), occupying its thread throughout.
  `Scheduler.run_tick` runs its pipeline and POSTs via
  `asyncio.to_thread` on the loop's DEFAULT executor (`loop.py`),
  so heartbeat blocking calls MUST NOT touch that executor. A
  single DEDICATED size-1 executor per strategy is ALSO wrong: one
  hung chain occupies its only thread for the full ≈ 90 s, every
  subsequent send queues behind it and is `wait_for`-cancelled
  WITHOUT ever executing, and the earliest next receipt lands at
  ≈ t = 120 — rung 1 would fire from ONE hung send. The sender
  therefore runs EACH attempt on its OWN short-lived single-use
  executor: spawn a fresh one-thread executor per attempt, submit
  the blocking `client.heartbeat()` call to it
  (`loop.run_in_executor(...)`, never `asyncio.to_thread`), and
  shut it down WITHOUT waiting once the attempt resolves or is
  abandoned. An abandoned hung chain then blocks NOTHING — not the
  next attempt, never a tick, never another strategy's sender —
  and zombie concurrency is explicitly bounded: at most
  ceil(90 s max chain / 30 s cadence) = 3 concurrent abandoned
  threads per strategy at the default, each self-terminating when
  its transport chain finishes. Queue depth is moot under
  per-attempt threads (no attempt ever queues behind another), and
  a future cancelled before its thread starts never executes.
- **WD-24.** Retries are the transport's EXISTING policy
  (`client/http.py`: `MAX_ATTEMPTS = 3` — 1 initial + ≤ 2 retries,
  only on 429/5xx/timeout/transport errors, backoff base 1 s,
  `Retry-After` clamped to 30 s, default timeout 10 s) — the sender
  adds NO retry layer. Two rules bound the cadence:
  1. Per-attempt wait cap: the blocking `client.heartbeat()` call
     runs on its WD-23 per-attempt executor thread under
     `asyncio.wait_for` with a cap of `min(interval, 15 s)`; on
     timeout the beat is ABANDONED from the loop's perspective
     (logged WARNING). The cap bounds the WAIT, not the thread: the
     transport chain keeps running to completion on its abandoned
     thread, blocking nothing (WD-23).
  2. Start-anchored cadence: sends run on a FIXED schedule anchored
     to the attempt START — the next attempt starts at
     `start + interval`, never `end + interval`; an abandoned
     attempt consumes its own slot, not the next one (an attempt
     overrunning its slot makes the next send fire immediately).
  Worst-case arithmetic at the defaults (interval 30 s, cap 15 s),
  receipt at t = 0: the next send starts at t = 30 and, if hung, is
  abandoned at t = 45 (its zombie thread lives on, WD-23); the
  following send starts at t = 60 on a FRESH per-attempt thread —
  the hung chain cannot block it — and resolves by t = 75 even at
  the full cap — TWO fresh receipt opportunities (sends starting at
  t = 30 and t = 60) provably land inside the 90 s window, so ONE
  hung send can never trip the watchdog. (Both alternatives fail
  exactly here. A shared size-1 executor: the hung chain occupies
  the sole thread ≈ 90 s, the t = 60 send queues and is cancelled
  unrun, earliest next receipt ≈ t = 120. An end-anchored schedule
  with a full-interval cap: success t = 0 → sleep to t = 30 → send
  hangs, abandoned t = 60 → sleep to t = 90 → first possible
  receipt only after the threshold.)
- **WD-25.** Cadence env: `ALPHAMINTX_HEARTBEAT_INTERVAL_SECONDS`,
  OPTIONAL, default 30 (the existing
  `HEARTBEAT_INTERVAL_SECONDS = 30` constant in `controlplane.py`
  stays the default's source of truth). Bounds: `0 <
  interval <= 45`, fail-fast at startup on violation (the
  `__main__.py` `_tick_interval` parse pattern). 45 is half the 90 s
  threshold. Two properties, stated separately: (a) with the WD-24
  start-anchored cadence, ANY conforming interval schedules ≥ 2
  attempt STARTS inside every 90 s silence window; (b) the stronger
  guarantee that both attempts also RESOLVE inside the window —
  under the `min(interval, 15 s)` wait cap on WD-23 per-attempt
  threads (the WD-24 timeline) — holds at the DEFAULT 30 s interval
  ONLY, where ONE lost/timed-out POST can never trip the watchdog
  by itself. At interval = 45 the second attempt starts at t = 90
  and its receipt can land after the threshold; accepted, because
  45 is the operator-chosen headroom bound, not the default.
- **WD-26.** `heartbeat_path` is corrected to
  `/api/v1/strategies/{strategy_id}/heartbeat` (WD-1);
  `ControlPlaneClient.heartbeat()` keeps posting `{}` with the bearer
  header, treats exactly 200 as success, and MAY ignore the
  `received_at` field (it MUST NOT require more than a JSON object —
  the cross-plane envelope test pins the shape, §Test obligations).
  The `DryRunTransport` heartbeat stub answer is updated to the WD-5
  envelope.

## Config (normative)

| Env var | Meaning |
|---|---|
| `CONTROLPLANE_WATCHDOG_DISABLED` | Escape hatch: `1`/`true` disables watchdog EVALUATION (the heartbeat endpoint still accepts beats; the `safety.Config.WatchdogDisabled` seam, §Wiring seams). Default: watchdog ENABLED whenever `CONTROLPLANE_OMS_MODE=live`. Justified for testnet drills and manual live-OMS operation where no agent-plane runs — without it every drill strategy would be killed 10 minutes after promotion. Startup MUST log LOUDLY when set; setting it in paper mode is a no-op (no monitor runs). |
| `ALPHAMINTX_HEARTBEAT_INTERVAL_SECONDS` | Agent-plane sender cadence (WD-25). Default 30; bounds (0, 45]; fail-fast. |

- **WD-27.** The thresholds are CONSTANTS, not env-tunable:
  `WatchdogSilenceThreshold = 90 * time.Second` and
  `WatchdogEscalationThreshold = 600 * time.Second` (NEW constants in
  `internal/safety`, beside `DefaultActiveInterval`). risk-limits.md
  §Watchdog pins 90 s / 10 min normatively; tunability would let one
  misconfigured env var silently defeat the ladder (a 24 h "90 s"
  threshold is indistinguishable from no watchdog), and no v1
  deployment scenario needs other values. Loosening requires a spec
  change, deliberately.
- Paper deployments read neither variable's control-plane half; no new
  secrets; redaction/no-read-back invariants of multi-tenant-rbac.md
  bind unchanged.

## API surface

| Method + path | Roles | Classes | Requires | Returns / body |
|---|---|---|---|---|
| `POST /api/v1/strategies/{id}/heartbeat` | — | agent (own strategy, guard-enforced) | — (both modes) | Body `{}` (strict, optional-empty); 200 `{"received_at"}`; 400 `SCHEMA_INVALID`; 401 `UNAUTHORIZED`; 403 `FORBIDDEN` / `STRATEGY_SCOPE_MISMATCH`; 413 `BODY_TOO_LARGE`; 429 `RATE_LIMITED` (60/min/token; the 30/min proposal limiter is never charged). |

- Status semantics per multi-tenant-rbac.md: auth → class → agent
  scope, all before the handler. No new error codes: every code above
  already exists in `internal/api/respond.go`.

## Tables and migration (normative)

- **ZERO new DDL.** No new tables, no new columns, no new event kinds:
  the 90 s reaction is derive-from-state (WD-17 — the
  `kill_breaker_events.kind` CHECK cannot admit a new kind
  additively), the escalation persists as a NORMAL kill row through
  `AppendStrategyKill`, and the alerts use the existing open-kind-set
  `safety_alerts` table (its `kind` deliberately carries no CHECK,
  safety-wiring.md §Safety-effects). An existing soak `control.db`
  opens and serves unchanged.
- Row rules unchanged: RFC 3339 UTC `Z` timestamps; decimals as
  strings; `safety_alerts` append-only.

## Alerts (registry additions — safety-wiring.md §Alerts table)

**WD-21.** The two watchdog alert kinds, both appended BEFORE their
effect attempt (invariant 7):

| Table | kind | Appended by | Meaning |
|---|---|---|---|
| `safety_alerts` | `watchdog_silence` | Watchdog (rung 1) | Silence > 90 s: ENTRY sweep engaged. `ref_id` = cause (`'silence'` — the cause-keyed pattern of `breaker_mark_stale`, open for future causes); `strategy_id` set; `details_json` `{cause, last_seen, silence_seconds}` (`last_seen` empty when the baseline applies). ≤ 1 per (strategy, cause) per UTC day via `HasSafetyAlertToday`; a log line EVERY tick the sweep runs. Appended BEFORE the cancel attempt (WD-17). |
| `safety_alerts` | `watchdog_kill_escalation` | Watchdog (rung 2) | Escalated into a strategy-tier kill. `ref_id` = the kill `event_id`; ONE-TIME per kill event via `HasSafetyAlert` (any-age, the `safety_residue_abandoned` pattern); `details_json` `{cause: 'silence_10m' \| 'unprotected_exposure', last_seen, silence_seconds}` — the WD-16 back-fill variant carries `{cause: 'backfill'}` ONLY (`last_seen`/`silence_seconds` are unrecoverable across the crash). Appended after the kill row, before the drive (WD-19); the back-fill is the late-append exception (WD-16). |

## Invariants

1. **No per-beat persistence.** A heartbeat POST writes no store row,
   no checkpoint, no tick state — receipt is an in-memory timestamp
   update on the Monitor, on both planes' hot paths.
2. **First expiry destroys ENTRY orders only.** Rung 1 never flattens,
   never kills, never touches lifecycle state, and never cancels a
   PROTECTIVE-class order; protective reduce-only stops stay on the
   exchange throughout (parent invariant, restated).
3. **The escalation kill IS the strategy-tier kill.** Rung 2 appends
   via `AppendStrategyKill` (actor `'watchdog'`, `flatten=false`) and
   drives via `DriveSafetyEffects`; every downstream semantic — epoch
   bump, gate-block, ENTRY sweep, lifecycle lock, served marker,
   crash-resume, stall alert, no auto-restart — is safety-wiring.md's,
   unmodified.
4. **Idempotent escalation.** No kill row is appended while an
   UNCLEARED kill binds the strategy (`ActiveKill` — WD-16 as amended
   by lifecycle-api.md LC-34); post-escalation ticks skip
   the strategy until the lifecycle lock removes it from the watch set.
5. **Rung 1 is stateless and self-healing.** No persisted watchdog
   state exists; silence is re-detected and the sweep re-run every
   tick while it persists; a crash mid-cancel converges on the next
   tick; already-canceled orders are no-ops.
6. **Never early, boundedly late.** A rung fires only after its
   threshold is truly exceeded; detection lags by at most the current
   tick interval (95 s / 605 s at the default intervals whenever any
   order or position exists; up to 100 s / 610 s at the legal ACTIVE
   bound — WD-13).
7. **Alert before effect; dedupe alerts, never actions.**
   `watchdog_silence` precedes the cancel attempt and dedupes daily;
   `watchdog_kill_escalation` precedes the drive and dedupes per kill
   event; the cancel action itself repeats undeduped.
8. **Recon gates venue-affecting evaluation.** No rung-1 sweep and no
   unprotected-exposure fast path before `Reconciled()`; the pure
   10-minute escalation may append its kill row pre-reconcile because
   its effects are independently recon-gated in the driver.
9. **Paper invariance.** The heartbeat endpoint exists in both modes;
   watchdog evaluation exists only where the Monitor runs (live mode);
   zero DDL; paper behavior is otherwise unchanged.
10. **Restart baseline.** After a control-plane restart (or watch-set
    entry) the silence clock starts at the WD-9 baseline — a fresh,
    bounded window; this liveness gap is accepted and documented, not
    silent.
11. **Sender isolation.** The agent-plane heartbeat task never blocks
    or delays ticks (each blocking call runs on its own short-lived
    per-attempt executor thread, never the default executor ticks
    use — WD-23), never crashes the scheduler, never adds retries
    beyond the transport's ≤ 2, caps each beat's WAIT at
    `min(interval, 15 s)` (the thread itself is bounded only by the
    transport chain; abandoned threads number ≤ 3 per strategy at
    the default and block nothing — WD-23/WD-24), and sends on a
    start-anchored cadence so ≥ 2 attempts START inside every 90 s
    window at any conforming interval and both RESOLVE inside it at
    the default interval (WD-25).

## Test obligations

Fake-venue watchdog drills — deterministic, injected clock
(`safety.Config.Now`), scripted fake `Exchange`, fake heartbeat feed
via `Monitor.Beat`:

| # | Scenario | Test |
|---|---|---|
| WD1 | Beats every 30 s ⇒ no alert, no cancel, no kill across many ticks | `TestWatchdogDrill_HeartbeatKeepsQuiet` |
| WD2 | 90 s silence ⇒ resting ENTRY canceled AND claimed-unsent intent claim-revoked; PROTECTIVE order untouched at the venue; ONE `watchdog_silence` alert; NO kill row; lifecycle unchanged | `TestWatchdogDrill_SilenceCancelsEntriesOnly` |
| WD3 | Silence persists across ticks ⇒ sweep re-runs each tick (a fresh ENTRY appearing mid-silence is swept), zero duplicate `watchdog_silence` rows same UTC day; next UTC day alerts once more | `TestWatchdogDrill_RepeatCancelNoAlertSpam` |
| WD4 | Silence > 10 min ⇒ ONE kill row (`actor_id='watchdog'`, `flatten=0`, epoch bumped), `watchdog_kill_escalation` alert (ref_id = event_id), effects driven: entries swept, strategy `killed`, protectives remain, row eventually served | `TestWatchdogDrill_TenMinuteKillEscalation` |
| WD5 | Open position with NO non-terminal PROTECTIVE order + silence 91 s ⇒ immediate rung-2 kill (fast path), not just rung 1 | `TestWatchdogDrill_UnprotectedExposureFastPath` |
| WD6 | Open position WITH a resting non-terminal PROTECTIVE ⇒ 91 s silence stays rung 1; escalation only at > 10 min | `TestWatchdogDrill_ProtectedExposureNoEscalation` |
| WD7 | Standing kill (any tier) already binds the strategy ⇒ watchdog skips entirely: no second kill row, no watchdog alerts | `TestWatchdogDrill_AlreadyKilledSkip` |
| WD8 | Monitor restart mid-silence ⇒ baseline = process start: no fire at +89 s, rung 1 at > 90 s, rung 2 at > 10 min from the baseline; a never-heartbeating live strategy is caught from watch-set entry | `TestWatchdogDrill_RestartBaselineGrace` |
| WD9 | `Reconciled()` false ⇒ no sweep, no fast path; the pure 10-min rung still appends the kill row WHILE `Reconciled()` is still false (WD-12 tick restructure) and the effects run only after reconcile completes | `TestWatchdogDrill_ReconGateDeferral` |
| WD10 | Watch-set scope: paper/paused/killed strategies with silence are never evaluated; a strategy promoted to `live_*` mid-run gets its `firstWatched` baseline | `TestWatchdogDrill_WatchSetScope` |
| WD11 | Venue cancel error on the first sweep tick ⇒ logged, tick continues; next tick's sweep succeeds (self-heal, invariant 5) | `TestWatchdogDrill_CancelRetryAfterError` |
| WD12 | Breaker/kill flatten leaves dust residue (`flatten_dust`: below minQty, or below minNotional at a fresh mark) with no protective possible + 91 s silence ⇒ NO fast-path kill (WD-20 carve-out; no fresh mark likewise fails toward PROTECTED); the 10-min rung still escalates | `TestWatchdogDrill_DustResidueNotUnprotected` |

Endpoint tests (`internal/api`):

- The new route joins `TestRBACMatrix` automatically via the
  permissions table (agent class only; 401/403 matrix).
- `TestHeartbeatEndpoint`: 200 `{received_at}` (RFC 3339 Z) for an
  in-scope agent token (env AND DB token variants); empty body and
  `{}` both accepted; unknown field ⇒ 400 `SCHEMA_INVALID`; foreign
  strategy ⇒ 403 `STRATEGY_SCOPE_MISMATCH`; nil `Heartbeats` sink
  (paper mode) still 200; a burst of heartbeats never charges the
  proposal limiter (a subsequent proposal POST is not 429).

Agent-plane tests (pytest):

- Sender cadence: with a fake clock/sleep, beats POST every interval;
  a slow tick does not delay beats (separate task).
- Failure isolation: transport raising
  `ControlPlaneUnavailableError` ⇒ WARNING logged, loop continues,
  scheduler ticks unaffected; a beat exceeding the `wait_for` cap is
  abandoned on cadence, and a hung attempt never delays the NEXT
  attempt (per-attempt executor, WD-23).
- Shutdown: task cancellation via the SIGTERM path leaves no pending
  task; a bounded `run(max_ticks=…)` terminates (heartbeat tasks are
  cancelled when the strategy loops finish, WD-22); no checkpoint-DB
  access occurs from the heartbeat task.
- Path: `heartbeat_path` returns
  `/api/v1/strategies/{id}/heartbeat` (pins the stub fix).
- Cross-plane contract test: the client accepts the WD-5 response
  envelope `{"received_at": ...}` (and the `DryRunTransport` stub
  matches it), the persistence-and-api.md envelope-test pattern.

Testnet drill (non-vacuous evidence, the safety-wiring.md pattern):

- `TestTestnetDrill_Watchdog`: SKIPPED unless
  `CONTROLPLANE_BINANCE_API_KEY`+`_SECRET` are set with env=testnet
  (lands skipped without operator keys, consistent with
  `TestTestnetDrill_KillSwitch`/`_Breaker`). Places a REAL resting
  marketable-limit ENTRY plus a protective; sends NO heartbeats;
  advances real time past 90 s; asserts via venue `OpenOrders` that
  the ENTRY is gone while the protective rests; continues past 10 min
  and asserts the kill row, the `killed` lifecycle state, and the
  served marker. Non-vacuity: ≥ 1 REAL venue ENTRY cancel observed —
  zero fails.

## Companion edits (listed here; applied in the implementation change,
not by this spec)

- `docs/specs/persistence-and-api.md` — HTTP API table: add the row
  `POST /api/v1/strategies/{id}/heartbeat` (agent token only; body
  `{}`; 200 `{received_at}`; error codes per §API surface here;
  normative in `docs/specs/watchdog.md`).
- `docs/ARCHITECTURE.md` — §Plane authentication heartbeat paragraph:
  drop "(endpoint deferred to the watchdog slice ...)"; point to
  `docs/specs/watchdog.md` for the receiver, ladder, and endpoint.
  ALSO the safety bullet of the what-exists list: drop its trailing
  "Watchdog deferred." sentence (same change, second site).
- `docs/specs/safety-wiring.md` — §Deferred SW-1: mark landed,
  pointing here ("SW-1 — landed: `docs/specs/watchdog.md`").
- `docs/PLAN.md` — Phase 3 progress line for the watchdog slice
  (heartbeat receiver + escalation ladder + WD drills).

## Deferred (recorded, not silent)

- WD-D1 — persisted last-seen / heartbeat history (closing the WD-9
  restart gap): rejected for v1 per WD-8; revisit only with evidence
  the bounded restart window is operationally insufficient.
- WD-D2 — tenant/platform-tier watchdog escalation and cross-strategy
  agent-host liveness: v1 is per-strategy only, like the parent.
- WD-D3 — web viewer surface for watchdog/heartbeat status (SW-5
  neighborhood): LANDED — `docs/specs/operator-surface.md` (the OS-12
  watchdog liveness object on `GET .../safety` and the ops panel).
- WD-D4 — agent-plane self-restart / supervisor integration: reviving
  a silent agent is an operator runbook act in v1.
