# Operator Surface v1: Read-Only Safety/Alerts API and the Web Ops Panel

Normative. Rules are **OS-n**. This spec adds the READ half of the
operator surface: three read-only control-plane endpoints over
already-persisted safety state (`kill_breaker_events`,
`kill_clear_events`, `safety_alerts`) plus the in-memory watchdog
liveness, and a web ops panel on the strategy detail page that reads
them and DRIVES the EXISTING mutation endpoints — kill and clear
(`docs/specs/safety-wiring.md` §Kill endpoints, `docs/specs/
lifecycle-api.md` LC-29), lifecycle (`lifecycle-api.md` LC-1..LC-14a).
It introduces ZERO new mutation endpoints, ZERO new DDL, and ZERO RBAC
widening. Companions: `docs/specs/safety-wiring.md` (alert-kind
registry, effects engine), `docs/specs/lifecycle-api.md` (ActiveKill,
clear CAS, paper-gate read), `docs/specs/watchdog.md` (heartbeat
liveness, WD-8/WD-9), `docs/specs/multi-tenant-rbac.md` (permission
matrix, registration rule, tenant isolation), `docs/specs/
persistence-and-api.md` (HTTP conventions, pagination envelope, web
auth split).
Push dispatch of the safety events this surface reads is normative in
`docs/specs/alert-notifier.md` (AN-*); operator procedures in
`docs/RUNBOOK.md` §9.

## Goals and non-goals

- Goal: an operator SEES, per strategy, the composite safety truth in
  one poll — lifecycle state, every kill binding the strategy with its
  clearing event, the breaker-today latch, and watchdog liveness — from
  the SAME predicates the machines act on, never a UI re-derivation.
- Goal: an operator SEES the `safety_alerts` journal (the delivery
  channel every safety spec appends to) per strategy and globally,
  paginated, without shelling into SQLite.
- Goal: an operator ACTS from the same panel — pause/resume/promote/
  unlock, strategy-tier kill, strategy-tier clear — through the
  EXISTING endpoints via same-origin server proxies, with the error
  bodies those endpoints already speak surfaced verbatim.
- Non-goals (v1, §Deferred): outbound notification (webhook/email) —
  the v1 delivery is log lines plus this API feed; tenant/platform
  kill or clear UI; tokens/billing admin UI; SSE/websocket push;
  widening the env operator class beyond `POST .../approvals`.

## Principal truth — what the web's OPERATOR_TOKEN can drive (verified)

- **OS-1.** Routes are REGISTERED from the exported permission table
  (`api.Permissions()`, `control-plane/internal/api/permissions.go`;
  multi-tenant-rbac.md §Permission matrix): each `RoutePermission` row
  carries `Roles` (DB user roles — the `readers`/`approvers`/`admins`
  slices) and `Classes` (non-user classes `classRead`/`classOperator`/
  `classAgent`/`classEnvAdmin`), and `allows` checks role membership
  for `classUser` principals, class membership otherwise. No route
  exists without a row; `TestRBACMatrix` iterates the table and
  enforces registered-route enumeration equality (§Test obligations).
- **OS-2.** The EXISTING rows the panel drives or reads — four
  mutation rows plus the paper-gate read row — quoted from
  `permissions.go` (this spec changes NONE of them):
  `POST .../kill` — Roles `approvers` (trader/admin/owner, own
  tenant), Classes `[env-admin]`;
  `POST .../kill/clear` — Roles `admins` (admin/owner), Classes
  `[env-admin]`;
  `POST .../lifecycle` — Roles `approvers`, Classes `[env-admin]`;
  `POST .../approvals` — Roles `approvers`, Classes `[operator]`;
  `GET .../paper-gate` — Roles `readers`, Classes `[read]`.
  The env operator class (`classOperator`) therefore drives EXACTLY
  ONE route: `POST .../approvals`. It can NOT drive kill, clear, or
  lifecycle — those admit tenant roles and `classEnvAdmin` only
  (lifecycle-api.md LC-2: "the read/operator/agent classes can never
  transition").
- **OS-3. Pinned 403 passthrough (not a bug).** The web's
  `OPERATOR_TOKEN` env var is a bearer VALUE; its class is resolved
  control-plane-side (`resolvePrincipal`, `internal/api/auth.go`).
  When it holds the env operator-class token (the Phase-1 wiring),
  the new lifecycle/kill/clear proxies (OS-27..OS-29) receive 403
  `FORBIDDEN` from the control plane and pass it through verbatim;
  the panel surfaces it honestly. This is the pinned v1 behavior —
  the fix is DEPLOYMENT configuration, never an RBAC widening here.
- **OS-4. Deployment note (normative).** A deployment that wants the
  panel's controls to work sets the web server's `OPERATOR_TOKEN` to
  a DB token whose role covers the act: trader+ drives lifecycle and
  strategy kill; admin+ additionally drives clear. Approvals keep
  working under any of these (approvers ∪ operator class). A
  trader-role token yields working kill/lifecycle controls and a 403
  clear control — the panel never hides a control merely because the
  proxy credential might lack the role (OS-30: honesty over guessing).

## GET /api/v1/strategies/{id}/safety — composite status

- **OS-5.** Matrix row: `{Method: "GET", Path:
  "/api/v1/strategies/{id}/safety", Roles: readers, Classes: [read],
  Requires: ""}` — always registered, both modes (every input below is
  persisted state or a nil-safe seam). Registration from the table
  puts it in `TestRBACMatrix` automatically.
- **OS-6.** Tenant-scoped resolution is the `rootStrategy` pattern of
  the other strategy routes (`internal/api/read.go`): tenant-bound
  principals resolve via `GetStrategyInTenant`, env classes via
  `GetStrategy`; a foreign or absent strategy is 404
  `UNKNOWN_STRATEGY`, indistinguishable from absence (no existence
  oracle, multi-tenant-rbac.md §Tenancy rules).
- **OS-7.** Response, 200:

```json
{"strategy_id": "…", "lifecycle_state": "…", "paused_from": null,
 "active_kill": false,
 "kills": [
   {"event_id": "…", "scope": "strategy",
    "kill_epoch": 7, "flatten": false, "actor_id": "…",
    "recorded_at": "…Z",
    "cleared": {"clear_id": "…", "actor_id": "…", "reason": "…",
                "recorded_at": "…Z", "cleared_epoch": 7}}],
 "breaker": {"active_today": false, "event": null},
 "watchdog": {"enabled": true,
              "last_heartbeat_at": "…Z", "seconds_since": 12}}
```

  `lifecycle_state` is `strategies.lifecycle_state` verbatim;
  timestamps RFC 3339 UTC `Z`; `cleared` is `null` for an uncleared
  kill; `breaker.event` is `null` when no breaker fired today;
  `watchdog.last_heartbeat_at`/`seconds_since` are `null` when no
  beat is known. `paused_from` is non-null iff `lifecycle_state` is
  `paused`: the from_state of the SAME server-side provenance QUERY
  the lifecycle handler's resume path uses (the `PausedProvenance`
  SELECT over `lifecycle_transitions`, lifecycle-api.md LC-7),
  evaluated inside the OS-10a snapshot transaction (the query is
  factored `dbtx`-style so both callers share one SQL text);
  `null` otherwise, and `null` for a paused strategy whose
  provenance is unknown (no `to_state='paused'` row).
- **OS-8. Kills array = every kill whose scope covers the strategy.**
  All `kind='kill'` rows matching the 3-clause scope match of the
  LC-28 predicate (`internal/store/killclear.go` `activeKillSQL`,
  WITHOUT the epoch-vs-clear condition): `strategy_id = {id}` rows;
  `strategy_id IS NULL AND tenant_id = <the strategy's tenant>` rows;
  both-ids-NULL rows. Ordered `kill_epoch DESC` (the global monotone
  counter — newest first, deterministic). The response `scope` is
  DERIVED from id NULL-ness (strategy/tenant/platform) — never read
  from the legacy `scope` column, so Phase-1 global rows report
  `platform`, matching LC-26 and the tier the effects engine assigns
  them. A NULL `flatten` column (pre-flatten-era rows) renders
  `false`. Uncleared history is deliberately unbounded: kill rows are
  rare, append-only, and each one is operator-relevant.
- **OS-8a. Wire DTO, never the store struct.** The store-side row
  struct (§Wiring seams `BoundKill`) is INTERNAL: the handler maps
  it to a dedicated wire DTO emitting EXACTLY the OS-7 kill keys —
  `{event_id, scope, kill_epoch, flatten, actor_id, recorded_at,
  cleared}`. `scope` is DERIVED from id NULL-ness (OS-8), never the
  legacy `scope` column; the store struct's json tags never reach
  the response — no `tenant_id`, no `trigger_ref`, no `kind`.
- **OS-8b. NULL `kill_epoch` defense.** Writer invariant: all three
  kill append paths (`AppendStrategyKill`/`AppendTenantKill`/
  `AppendPlatformKill`) assign `kill_epoch = MAX + 1` inside the
  insert transaction, so every `kind='kill'` row carries a non-NULL
  `kill_epoch`. The OS-8 join nonetheless filters `kill_epoch IS
  NOT NULL` defensively: a legacy NULL-epoch row is invisible to
  every acting predicate (the LC-28 clauses compare `kill_epoch`)
  and must never render a phantom banner.
- **OS-9. Cleared join.** Per kill row, `cleared` is the NEWEST
  `kill_clear_events` row COVERING it, where covering uses the
  VERBATIM watermark clauses of the acting predicate
  (`internal/store/killclear.go` `activeKillSQL`): a clear row
  covers a strategy kill iff `clear.scope = 'strategy' AND
  clear.strategy_id = kill.strategy_id`; a tenant kill iff
  `clear.scope = 'tenant' AND clear.tenant_id = kill.tenant_id`; a
  platform kill iff `clear.scope = 'platform'`. The clear's SCOPE
  COLUMN is part of the key — matching ids alone is WRONG: a
  strategy-scope clear (which carries a non-NULL `tenant_id` by
  DDL) NEVER covers a tenant kill. Spec note (counterexample): a
  tenant kill at epoch 3 followed by a strategy clear at epoch 5
  carrying the SAME `tenant_id` MUST NOT annotate the tenant kill
  cleared. Among covering rows with `cleared_epoch >= kill_epoch`,
  newest = highest `cleared_epoch`, `rowid DESC` tiebreak. `null`
  when no such clear exists — i.e. exactly when that row still
  stands at its own scope (LC-27: a clear covers every kill of its
  scope with `kill_epoch ≤ cleared_epoch`).
- **OS-10. Single-source active_kill (invariant 1).** `active_kill`
  is the verbatim result of the LC-28 predicate (`killclear.go`
  `activeKillSQL`) — the SAME SQL the OMS entry gate, the hydrator,
  the lifecycle CAS, and the watchdog skip consume (lifecycle-api.md
  LC-34) — evaluated INSIDE the OS-10a snapshot transaction, never
  via a separate `store.ActiveKill` call. The handler MUST NOT
  re-derive it from the OS-8/OS-9 join; the join is display detail,
  the predicate is truth, and they may never drift.
- **OS-10a. One snapshot, no torn reads.** `kills`, `active_kill`,
  `breaker` (latch AND event), and `paused_from` all derive from
  ONE store snapshot: the single read method
  `SafetyStatus(strategyID, utcDate)` (§Wiring seams) evaluates the
  `activeKillSQL` predicate, the OS-8/OS-9 kills/cleared join, the
  OS-11 breaker latch and event, and the paused-provenance read
  inside ONE read transaction, so a kill or clear landing between
  reads can never produce a response whose banner and predicate
  disagree. The `watchdog` object stays OUTSIDE the snapshot,
  deliberately: its truth source is the in-memory `lastSeen` map
  (OS-12) — a different truth source no store transaction can
  include.
- **OS-11. Breaker-today.** `breaker.active_today` is the verbatim
  `BreakerActiveToday` latch PREDICATE (the SQL in
  `internal/store/liveoms.go`, factored `dbtx`-style), evaluated in
  the OS-10a snapshot — no re-derivation. `breaker.event` is the
  newest `kind='breaker'` row with `recorded_at` on today's UTC
  date matching the SAME 3-clause scope match as
  `BreakerActiveToday`: `strategy_id = {id}` rows; `tenant_id =
  <the strategy's tenant> AND strategy_id IS NULL` rows;
  both-ids-NULL rows; newest = `recorded_at DESC, rowid DESC` (the
  store's stated tiebreak convention). Predicate and event agree by
  construction (same date derivation, same scope clauses):
  `active_today: true` ALWAYS has a matching event row, and `event`
  is `null` exactly when no breaker row matches today. Shape:
  `{event_id, recorded_at, trigger_ref}`, `trigger_ref` the stored
  TEXT verbatim (the monitor's `{daily_pnl, limit, evaluated_at}`
  sample) or `null`.
- **OS-12. Watchdog liveness — read the truth, never fabricate.**
  Heartbeat receipt lives ONLY in memory on the `safety.Monitor`
  (`lastSeen` under `hbMu`, watchdog.md WD-8), fed by
  `api.Config.Heartbeats` → `Monitor.Beat`. The handler reads it via
  the NEW nil-able `api.Config.Watchdog` seam (§Wiring seams):
  seam nil (paper mode — no monitor; or `CONTROLPLANE_WATCHDOG_
  DISABLED` live mode) ⇒ `{"enabled": false, "last_heartbeat_at":
  null, "seconds_since": null}`. Seam wired ⇒ `enabled: true`;
  `LastBeat` hit ⇒ `last_heartbeat_at` = the beat instant,
  `seconds_since` = `floor(now − beat)` in whole seconds (may be
  negative-clamped to 0 on clock skew); no beat known ⇒ BOTH null.
  The WD-9 baseline (`startedAt`/`firstWatched`) is watchdog-internal
  accounting and MUST NOT be reported as a heartbeat: after a
  restart the endpoint answers nulls until a real beat arrives — the
  honest rendering of the accepted WD-9 liveness gap. `enabled`
  means "watchdog evaluation is running in this deployment", not
  "this strategy is in the watch set": a paper strategy under a live
  server may show a beat (receipt is mode- and state-independent,
  WD-7). One accepted artifact: `pruneWatchSet` deletes `lastSeen`
  when a strategy LEAVES the live watch set (LC-34b), so a
  just-demoted strategy transiently reports `enabled: true` with
  `last_heartbeat_at: null` despite continuous beats — accepted;
  the next beat re-stamps it.
- **OS-13. Read-only; standard rate policy.** The handler performs NO
  store write, NO alert append, NO drive invocation. Rate limiting is
  the standard per-token policy UNCHANGED: the `api/auth.go` guard
  charges the 60/min bucket on non-GET requests only, and this GET
  adds NO self-charge and NO new limiter — it is O(kill rows), a
  point read, not the LC-24 O(window-fills) replay that justified the
  paper-gate's self-charge. Cost is bounded by append-only tables
  that grow by operator acts, not by trading.
- **OS-14. Cross-tier visibility (accepted, reasoned).** A tenant
  viewer sees tenant- and platform-scope kill rows binding its
  strategy, including `actor_id` and the clearing reason. Accepted:
  the standing condition is already observable through gate
  rejections (`KILL_SWITCH_ACTIVE`), actor ids are audit identifiers
  (token ids / `"env-admin"`), never secrets (multi-tenant-rbac.md
  §Audit identity), and hiding the binding kill from the person whose
  book is locked would make the panel lie. No other tenant's
  existence is revealed (tenant rows shown are the strategy's OWN
  tenant's).

## GET /api/v1/strategies/{id}/alerts — per-strategy feed

- **OS-15.** Matrix row: `{Method: "GET", Path:
  "/api/v1/strategies/{id}/alerts", Roles: readers, Classes: [read],
  Requires: ""}` — always registered, both modes (paper deployments
  have `safety_alerts` rows too, e.g. `lifecycle_entry_cancel_failed`).
  Tenant-scoped resolution per OS-6 (404-no-oracle).
- **OS-16.** Response is the standard pagination envelope
  `{items, total, page, limit}` (`page` 1-based, `limit` default 20
  max 100 — persistence-and-api.md §HTTP API), newest first.
  Ordering is PINNED: `ORDER BY recorded_at DESC, alert_id DESC` —
  the tiebreak is `alert_id`, NOT rowid, because second-precision
  timestamps collide and `alert_id` is IN the payload, so the
  ordering is verifiable from what a client sees; rowid is not
  exposed. That ordering is deterministic and stable FOR A FIXED
  DATASET — and no stronger: under newest-first LIMIT/OFFSET over
  an append-only table, EVERY append shifts every page boundary,
  and same-second inserts can interleave within their second under
  the `alert_id` tiebreak. Pages are per-poll snapshots; clients
  MUST NOT assume a row keeps its page across polls. Keyset
  (cursor) pagination is deferred (OS-D7).
- **OS-17. Strategy scoping is exact.** The feed is
  `safety_alerts.strategy_id = {id}` rows ONLY. Platform/NULL-
  strategy rows (`safety_effect_stalled` for platform events,
  NULL-strategy stall alerts) are NOT in this feed — pinned: a
  tenant-visible per-strategy feed must not leak platform operational
  rows, and the empty-matches-NULL dedupe convention
  (`HasSafetyAlertToday`) stays a WRITE-side rule with no read-side
  mirror here. NULL-strategy rows are served by OS-19's global feed.
  Practical consequence, stated plainly: today ALL
  `safety_effect_stalled` alerts are written with NULL `strategy_id`
  (`stallScan` passes `""` for EVERY event, strategy-scope kills
  included), so stall alerts are visible ONLY in the env-only
  global feed — an accepted v1 limitation; write-side strategy
  stamping is deferred (OS-D8).
- **OS-18.** Item shape: `{alert_id, kind, strategy_id, ref_id,
  details_json, recorded_at}` — the `store.SafetyAlert` columns
  verbatim; `ref_id` nullable; `details_json` the stored TEXT
  verbatim (a JSON string, decimals-as-strings inside per ADR-0003),
  never re-parsed or re-shaped server-side. `kind` is the OPEN set
  (SS-25): consumers treat unknown kinds as opaque; the registry
  lives in safety-wiring.md §Alerts and events plus watchdog.md WD-21
  and lifecycle-api.md's additions. Tenant-viewer exposure of
  `details_json` is safe because writers embed only redaction-safe
  material; this relies on the VenueError redaction rule (the
  live-oms spec) staying normative for anything flowing into
  `details_json`.

## GET /api/v1/alerts — global feed (env classes only)

- **OS-19.** Matrix row: `{Method: "GET", Path: "/api/v1/alerts",
  Roles: nil, Classes: [read, env-admin], Requires: ""}`. Class
  decision, from the existing conventions: the env READ class is the
  platform-scoped read credential (persistence-and-api.md §Auth — the
  web dashboard's GET-only token) and env-admin joins platform reads
  per the billing-invoice precedent (`GET /api/v1/billing/*`:
  `[classRead, classEnvAdmin]`). The env OPERATOR class is NOT
  granted: it is a POST-approvals credential by definition (OS-2) and
  granting it reads would break the READ/OPERATOR credential split
  that spec pins. No DB role is granted (OS-20).
- **OS-20. Tenant DB principals get 403 — pinned reasoning.**
  `safety_alerts` has NO tenant column (schema.go): rows are keyed by
  `strategy_id` (nullable) alone, and NULL-strategy rows
  (`safety_effect_stalled` on platform events) are platform
  operational data. A tenant-filtered view would need a join-and-
  filter surface this slice does not build; serving the unfiltered
  feed to tenant roles would leak foreign tenants' strategy ids.
  Empty `Roles` makes every DB principal 403 `FORBIDDEN` via the
  standard matrix check — before any handler logic, no new code path.
- **OS-21.** Envelope, ordering, and item shape are OS-16/OS-18
  verbatim, INCLUDING NULL-`strategy_id` rows. One optional query
  filter: `?kind=<exact>` — exact-match on the open kind set (never
  validated against a registry; an unknown kind returns an empty
  page, not an error). `page`/`limit` as OS-16.

## Web ops panel (strategy detail page)

- **OS-22. Placement and polling.** The panel joins the EXISTING
  strategy detail page (`web/app/strategies/[id]/page.tsx`), reading
  through the existing client conventions: fetchers in
  `web/src/lib/api/client.ts` (READ token via `authHeaders`,
  `cache: "no-store"`), zod `strictObject` schemas in
  `web/src/lib/api/schema.ts`, revalidation via `usePoll`
  (`web/src/lib/api/usePoll.ts`). NO poll may run below the existing
  `POLL_INTERVAL_MS` default (10 000 ms, `client.ts`) — the panel
  adds three polled GETs (safety, alerts, paper-gate) and the rate
  arithmetic is exact: the safety and alerts GETs charge NOTHING
  (the guard charges non-GET requests only, OS-13); ONLY the
  paper-gate GET self-charges the shared READ token's 60/min bucket
  (LC-24), and it polls at 6 × `POLL_INTERVAL_MS` (60 s, OS-25), so
  N concurrent detail pages consume N/min of that bucket
  (invariant 4).
- **OS-23. Safety status card.** Polls `GET .../safety`. Renders:
  a KILL BANNER when `kills` contains any row with `cleared: null` —
  scope, actor, recorded_at, flatten, `kill_epoch`; cleared kills
  render collapsed with their `cleared` info (actor, reason, time);
  `active_kill` drives the banner severity, never a client-side
  re-derivation (invariant 1 reaches the UI too); a BREAKER-TODAY
  badge from `breaker.active_today` with the event time when present;
  WATCHDOG liveness — `enabled: false` renders "watchdog off"
  (paper/disabled), enabled-with-nulls renders "no heartbeat
  observed" (never a fake timestamp), enabled-with-beat renders
  `seconds_since` with staleness styling past 90 s (the WD-15
  threshold, display-only).
- **OS-24. Alerts section.** Polls `GET .../alerts` with the OS-16
  envelope, rendered newest-first with the existing `Pager` control
  (`web/app/strategies/ui.tsx`); `kind` displayed verbatim (open
  set), `details_json` parsed defensively for display and shown raw
  on parse failure. The NULL-strategy global feed is NOT rendered in
  v1 (no page consumes `GET /api/v1/alerts`; the endpoint exists for
  operators' direct use and a later platform dashboard — §Deferred).
- **OS-25. Paper-gate panel.** Polls the EXISTING
  `GET .../paper-gate` (lifecycle-api.md LC-24) and renders the LC-23
  report: per-condition rows (name, passed, measured, required) plus
  `window_started_at`. That GET self-charges the 60/min bucket
  (LC-24), so THIS poll runs at 6 × `POLL_INTERVAL_MS` (60 s), never
  faster — the OS-22 arithmetic. Degradation is PINNED: on a 429 the
  panel keeps the last-rendered report and shows a rate-limited
  note; it never auto-tightens the poll interval.
- **OS-26. Lifecycle controls.** Buttons are rendered per this
  PINNED display table — a DELIBERATE v1 SUBSET of
  strategy-lifecycle.md's transition table (single-step promotion,
  single-step demotion plus demote-to-paper, flat unlock only):

| Verb | Displayed from → to |
|---|---|
| activate | `draft → paper` |
| pause | `paper → paused`, `live_* → paused` |
| resume | `paused → <paused_from>` (see resume rule below) |
| promote | `paper → live_l1`, `live_l1 → live_l2`, `live_l2 → live_l3` |
| demote | `live_l3 → live_l2`, `live_l2 → live_l1`, `live_* → paper` |
| unlock | `killed → paper` |

  Legal transitions the panel deliberately does NOT render in v1
  (still reachable via the API): multi-step promotes
  (`paper → live_l2`, `paper → live_l3`, `live_l1 → live_l3`),
  skip-demote `live_l3 → live_l1`, and `killed → paused`.
  Resume rule: resume sends `to` = the OS-7 `paused_from` field —
  the client can NOT re-derive the paused→? target (provenance is
  server-side, lifecycle-api.md LC-7) — EXCEPT when `paused_from`
  is `"killed"`: a paused-after-kill strategy resumes only to
  `paper` (the machine's sole paused-exit for that provenance), so
  the panel renders the button as "resume to paper" and sends
  `to: "paper"`; `to: "killed"` is NEVER sent. The resume button is
  DISABLED when `paused_from` is `null`. The table is a DISPLAY
  convenience only: the server remains the sole transition
  authority (lifecycle-api.md invariant 9) and any 422
  (`ILLEGAL_TRANSITION`, `PAPER_GATE_FAILED` with its embedded
  report, "kill tier active") is surfaced verbatim, never
  pre-suppressed. An explicit confirm step is REQUIRED before any
  POST whose transition is INTO a `live_*` state. The `reason`
  input is shown for every transition and required wherever the
  machine requires it — today that is universal (LC-4: empty reason
  is 400 `SCHEMA_INVALID`), so the UI never sends without one.
  `to: "killed"` is never offered (LC-5: kills flow through the
  kill endpoint — OS-28 is the kill control).
- **OS-27. Lifecycle proxy.** NEW same-origin route
  `web/app/api/strategies/[id]/lifecycle/route.ts`, the approvals-
  proxy pattern verbatim (`approvals/route.ts`): server-side
  `OPERATOR_TOKEN` + `CONTROLPLANE_API_BASE_URL`, body forwarded as
  received, upstream status and body returned verbatim. Unconfigured
  (missing token or base) ⇒ 503 `{code: "OPS_PROXY_UNCONFIGURED",
  message: …}` (the `APPROVAL_PROXY_UNCONFIGURED` shape, one code for
  the three new proxies).
- **OS-28. Kill control (strategy tier only).** A kill button POSTs
  `{"flatten": <checkbox>}` via NEW proxy
  `web/app/api/strategies/[id]/kill/route.ts` (OS-27 pattern). The
  flatten checkbox DEFAULTS CHECKED when the displayed
  `lifecycle_state` is `live_*` and unchecked otherwise —
  safety-wiring.md §Flatten choice: "UI affordances MUST default the
  checkbox/flag to flatten=true for live books" while the wire
  default stays false. The POST requires a typed confirm: the
  operator types the literal `KILL` into a confirm input (client-side
  only — the strategy-tier endpoint has no server ack literal; the
  platform tier's `KILL-PLATFORM` ack is server-side and out of scope
  here). No tenant or platform kill control exists in v1 (§Deferred).
- **OS-29. Clear control.** Shown only when the safety card displays
  an UNCLEARED strategy-scope kill. POSTs `{"reason": <required
  input>, "observed_epoch": N}` via NEW proxy
  `web/app/api/strategies/[id]/kill/clear/route.ts`. PINNED:
  `observed_epoch` = the `kill_epoch` of the displayed NEWEST
  uncleared strategy-scope kill — read from the safety response, the
  CAS token of lifecycle-api.md LC-27/LC-30, never a separate fetch
  or a guess. A 409 `CLEAR_CONFLICT` response re-fetches the safety
  card and surfaces the conflict to the operator; the UI MUST NOT
  auto-retry with the fresh epoch (the CAS exists precisely so a
  human re-observes before sweeping a kill away). Tenant/platform
  kills shown on the card carry no clear control (their clear
  endpoints are other tiers — §Deferred).
- **OS-30. Verbatim passthrough and honest errors (invariant 5).**
  All three proxies return the upstream status and body byte-for-byte
  (including 400/403/404/409/422/429); the client surfaces
  `error.body.code` and `message` from the standard error envelope
  via the existing `ApiError`. In particular the OS-3 403 under an
  operator-class token renders as a visible "credential lacks the
  role" state — never hidden, never retried, never remapped.
- **OS-31. Web schema/client additions.** `schema.ts` gains
  `safetyStatusSchema` (strictObject, nested kill/cleared/breaker/
  watchdog shapes per OS-7 including the nullable `paused_from`;
  `kind`/`details_json` as plain strings), `safetyAlertSchema`,
  `alertsPageSchema = paginated(safetyAlertSchema)`,
  `paperGateReportSchema` (strictObject mirroring the LC-23 report
  JSON exactly — the `internal/papergate` tags: `passed` boolean,
  `window_started_at` nullable string, `evaluated_at` string,
  `conditions` array of strictObject `{name, passed, measured,
  required}`, `measured`/`required` decimal strings), and
  request-payload builders for lifecycle/kill/clear bodies;
  `client.ts` gains `fetchSafety`, `fetchAlerts`, `fetchPaperGate`
  (READ-token GETs) and `postLifecycle`, `postKill`, `postKillClear`
  (same-origin proxy POSTs, no auth header client-side). All
  response schemas are zod `strictObject` per the existing style.

## Wiring seams (normative; NEW markers)

- NEW `api.Config.Watchdog` field of NEW consumer-side interface type
  `WatchdogLiveness { LastBeat(strategyID string) (at time.Time, ok
  bool) }` — declared in `internal/api` (the `HeartbeatSink`/
  `SafetyDriver` precedent: `api` declares, `cmd/controlplane/main.go`
  wires; no `api` → `safety` import). Satisfied by NEW
  `Monitor.LastBeat(strategyID) (time.Time, bool)`: an `hbMu`-guarded
  read of the `lastSeen` map — ok=false when absent; it reads ONLY
  `lastSeen`, never `firstWatched`/`startedAt` (OS-12: baselines are
  not heartbeats). Wired iff the Monitor is constructed (live mode)
  AND the watchdog is not disabled; nil otherwise ⇒
  `watchdog.enabled=false` with nulls. `Beat` and `LastBeat` share
  `hbMu`; a read never blocks a tick beyond the map access.
- NEW read-only store accessors (the `TestStoreSurfaceIsAppendOnly`
  allowlist gains exactly these three; reads never violate the
  append-only surface — the watchdog.md `LatestStrategyKillEvent`
  precedent):
  - `SafetyStatus(strategyID, utcDate string) (SafetyStatusRow,
    error)` — the OS-10a single-snapshot read. ONE read transaction
    evaluates: (a) the OS-8/OS-9 binding-kills join — the 3-clause
    scope match over `kind='kill'` rows with `kill_epoch IS NOT
    NULL` (OS-8b), each LEFT-joined to its newest covering clear
    per the OS-9 scope-column watermark clauses, `kill_epoch DESC`;
    (b) the `activeKillSQL` predicate (OS-10); (c) the
    `BreakerActiveToday` latch plus the OS-11 newest breaker event
    (same 3-clause scope match); (d) the LC-7 paused-provenance
    read (the `PausedProvenance` query) feeding OS-7 `paused_from`.
    Kill rows come back as `BoundKill` — `store.KillBreakerEvent`
    plus a nullable clear struct `{ClearID, ActorID, Reason,
    RecordedAt, ClearedEpoch}` — an INTERNAL struct the handler
    maps to the OS-8a wire DTO; its json tags never reach the
    response.
  - `ListSafetyAlertsByStrategyPage(strategyID string, page, limit
    int) ([]SafetyAlert, int, error)` — OS-16/OS-17: `strategy_id =
    ?` rows, `recorded_at DESC, alert_id DESC`, LIMIT/OFFSET plus
    total (the `ListStrategies(page, limit)` items+total pattern).
  - `ListSafetyAlertsGlobalPage(kind string, page, limit int)
    ([]SafetyAlert, int, error)` — OS-21: all rows including NULL
    `strategy_id`, optional exact `kind` filter, same ordering.
  The existing `ListSafetyAlerts(filter)` (rowid ASC, dedupe/test
  consumer) is UNCHANGED — the paginated reads are new, not a
  re-ordering of an existing read's contract.
- NEW handlers `handleGetSafetyStatus`, `handleGetStrategyAlerts`,
  `handleGetGlobalAlerts` in `internal/api`, plus the three
  permission rows (OS-5, OS-15, OS-19) — registered FROM the table,
  never beside it.
- NEW web files: the three proxy routes (OS-27..OS-29), the ops-panel
  component(s) on the strategy detail page, and the OS-31 schema/
  client additions. No new web env vars (§Config).

## Config (normative)

| Env var | Meaning |
|---|---|
| — (control plane) | NO new control-plane env vars. The three GETs key off existing wiring: the OS-12 seam follows the Monitor's existing live-mode construction and `CONTROLPLANE_WATCHDOG_DISABLED`. |
| `OPERATOR_TOKEN` (web, existing) | Reused by the three new proxies exactly as the approvals proxy uses it: server-only, attached same-origin. Which controls WORK depends on the token's control-plane-resolved principal (OS-3/OS-4). |
| `CONTROLPLANE_API_BASE_URL` (web, existing) | The proxies' upstream base, unchanged. |

- **OS-32.** The `NEXT_PUBLIC_` ban is restated normatively: the
  operator credential MUST NEVER appear in a `NEXT_PUBLIC_*` variable
  or otherwise reach the client bundle (persistence-and-api.md §Auth;
  invariant 3). The READ token remains GET-only; the three new GETs
  are exactly the reads it exists for.

## API surface

| Method + path | Roles | Classes | Requires | Returns / body |
|---|---|---|---|---|
| `GET /api/v1/strategies/{id}/safety` | viewer/trader/admin/owner (own tenant) | read | — (both modes) | 200 OS-7 composite; 401; 403; 404 `UNKNOWN_STRATEGY` (foreign = absent). |
| `GET /api/v1/strategies/{id}/alerts` | viewer/trader/admin/owner (own tenant) | read | — (both modes) | 200 `{items,total,page,limit}` newest-first (OS-16); 401; 403; 404 `UNKNOWN_STRATEGY`. |
| `GET /api/v1/alerts` | — none | read, env-admin | — (both modes) | 200 envelope incl. NULL-strategy rows; optional `?kind=`; 401; 403 (every DB principal). |

- Status semantics per multi-tenant-rbac.md: auth → role/class →
  object resolution. No new error codes server-side; the web-only
  `OPS_PROXY_UNCONFIGURED` (503) joins the approvals proxy's code as
  a Next-server code, never a control-plane one.

## Invariants

1. **Single-source ActiveKill.** `active_kill` is the LC-28
   `activeKillSQL` predicate verbatim — the same SQL the OMS gate,
   hydrator, lifecycle CAS, and watchdog consume — evaluated inside
   the OS-10a single-snapshot transaction; neither the handler nor
   the UI re-derives it from the kills join.
2. **Reads never write.** The three GETs perform no store write, no
   alert append, no drive, no rate-limiter self-charge — the standard
   guard policy applies unchanged.
3. **The operator credential never reaches the client.**
   `OPERATOR_TOKEN` is server-only in the Next proxies; no
   `NEXT_PUBLIC_` variant exists; the READ token authorizes GETs only.
4. **Polling floor.** No panel poll runs below the existing
   `POLL_INTERVAL_MS` default; the panel adds no burst refresh loops.
5. **Verbatim error passthrough.** The proxies return upstream
   status+body byte-for-byte; the UI surfaces `code`/`message`
   honestly — including the pinned OS-3 403 — and never remaps,
   hides, or auto-retries an error (the OS-29 409 rule included).
6. **Deterministic feed ordering.** Alerts order is `recorded_at
   DESC, alert_id DESC`, pinned tiebreak — deterministic and stable
   for a fixed dataset. Pages are per-poll snapshots under
   LIMIT/OFFSET: appends shift page boundaries by design (OS-16);
   clients never assume cross-poll page stability.
7. **Never fabricate liveness.** A missing beat is `null`, in the API
   and in the UI; baselines and process-start instants are never
   rendered as heartbeats.
8. **No RBAC widening.** The permission rows of OS-2 are unchanged;
   `classOperator`'s surface remains exactly `POST .../approvals`;
   the three new rows are read-class reads.
9. **Tenant isolation preserved.** Strategy-scoped reads are
   404-no-oracle via `rootStrategy`; the global feed is 403 for every
   DB principal; no response names a foreign tenant's objects.
10. **UI renders state, server decides.** Lifecycle buttons mirror
    the transition table for display only; every verdict (422s, the
    paper-gate report, CAS 409s) comes from the control plane and is
    shown as received.

## Test obligations

Control plane (`internal/api`, store-level fixtures, injected clock):

| # | Scenario | Test |
|---|---|---|
| OS1 | The three new rows join `TestRBACMatrix` automatically (registration from the table; enumeration equality still holds) | `TestRBACMatrix` (no new test — the registration REQUIREMENT is the obligation) |
| OS2 | Cross-tenant `GET .../safety` and `.../alerts` ⇒ 404 identical to absence; `GET /api/v1/alerts` with viewer/trader/admin/owner DB tokens ⇒ 403 | `TestTenantIsolation_SafetyReads` |
| OS3 | Uncleared strategy kill ⇒ in `kills` with `cleared: null`, `active_kill` true; after the clear ⇒ same row with `cleared` populated (clear_id/actor/reason/epoch), `active_kill` false | `TestSafetyStatus_KillClearedJoin` |
| OS4 | Tenant-scope kill covers a member strategy (`scope: "tenant"`, derived from NULL-ness; Phase-1 global row reports `platform`); a foreign tenant's strategy shows nothing | `TestSafetyStatus_ScopeCover` |
| OS5 | Breaker fired today ⇒ `active_today` true + event populated; yesterday's row ⇒ false + `event: null` (injected clock across the UTC boundary) | `TestSafetyStatus_BreakerToday` |
| OS6 | Watchdog: nil seam ⇒ enabled false/nulls; wired seam, no beat ⇒ enabled true/nulls; wired + beat ⇒ timestamp and `seconds_since` from the injected clock; never a baseline-derived timestamp | `TestSafetyStatus_WatchdogLiveness` |
| OS7 | Alerts ordering and determinism: newest-first, `recorded_at DESC, alert_id DESC` tiebreak on same-second rows, identical results across repeated reads of a FIXED dataset (no cross-append page stability claimed — OS-16); envelope totals correct; NULL-strategy rows absent from the strategy feed, present in the global feed; `?kind=` exact-match filters | `TestAlertsFeed_PaginationAndScope` |
| OS8 | The safety GET writes nothing: row counts across all safety tables identical before/after; no rate-bucket charge on GET | `TestSafetyStatus_ReadOnly` |
| OS9 | Clear scope key (OS-9 counterexample): tenant kill at epoch 3, then a strategy clear at epoch 5 carrying the SAME `tenant_id` ⇒ the tenant kill stays `cleared: null`, `active_kill` true | `TestSafetyStatus_StrategyClearNeverCoversTenantKill` |
| OS10 | Single snapshot (OS-10a): every response is internally consistent — `active_kill` agrees with the OS-8/OS-9 join, the breaker latch agrees with its event, `paused_from` reads in the same transaction; kill/clear appends interleaved between requests never yield a torn banner-vs-predicate response | `TestSafetyStatus_SingleSnapshot` |
| OS11 | `paused_from`: paused strategy reports the provenance from_state; non-paused ⇒ `null`; paused with no provenance row ⇒ `null` (resume button disabled client-side, OS-26) | `TestSafetyStatus_PausedFrom` |
| OS12 | A legacy NULL-epoch `kind='kill'` row (inserted directly) is absent from `kills`, never flips `active_kill`, and renders no banner input (OS-8b) | `TestSafetyStatus_NullEpochHidden` |
| OS13 | Breaker latch/event parity (OS-11): strategy-, tenant-, and platform-scope breaker rows each latch `active_today` true WITH a non-null matching `event`; `event` null only when no breaker row matches today | `TestSafetyStatus_BreakerEventScopeParity` |

Store unit tests: `SafetyStatus` — the binding-kills scope matrix
(strategy/tenant/platform/Phase-1-global × cleared/uncleared, epoch
ordering, newest-covering-clear join on the OS-9 scope-column key),
the NULL-epoch filter, breaker day edges and the 3-clause event
match, and paused provenance, all from ONE call; both paginated
alert reads (ordering, tiebreak, totals, kind filter); allowlist
extension in `TestStoreSurfaceIsAppendOnly`.

Web (the existing gates: `make web-check` = `pnpm install
--no-frozen-lockfile && pnpm typecheck && pnpm test && pnpm build`,
mirrored by CI; `test` is `vitest run`):

- Schema tests: the new zod schemas accept the OS-7/OS-18 shapes
  (nullable `cleared`/`event`/beat fields and `paused_from`; unknown
  alert `kind` accepted as a plain string) and REJECT unknown object
  keys (strictObject pinned); `paperGateReportSchema` accepts the
  LC-23 report (nullable `window_started_at`, decimal-string
  `measured`/`required`) and rejects unknown keys.
- Client tests: `fetchSafety`/`fetchAlerts`/`fetchPaperGate` build
  the right URLs and attach the READ token;
  `postLifecycle`/`postKill`/`postKillClear`
  POST same-origin without an auth header and with the exact bodies —
  including `observed_epoch` threaded from a displayed kill epoch and
  resume's `to` = the displayed `paused_from` (button disabled when
  `null`; `paused_from = "killed"` sends `to: "paper"` and never
  `"killed"`, OS-26); error bodies surface through `ApiError` untouched;
  a 409 `CLEAR_CONFLICT` path re-fetches and does NOT re-POST; a
  paper-gate 429 keeps the last-rendered report with a rate-limited
  note and never tightens the poll interval (OS-25).
- All existing web gates stay green (typecheck, vitest, build).

## Companion edits (listed here; applied in the implementation change,
not by this spec)

- `docs/specs/safety-wiring.md` — §Alerts and events: add the
  one-line pointer "Read surface: `docs/specs/operator-surface.md`
  (per-strategy and global alert feeds, safety-status composite)".
- `docs/specs/persistence-and-api.md` — §HTTP API table: add the
  three GET rows (OS-5/OS-15/OS-19 shapes, normative here).
- `docs/specs/multi-tenant-rbac.md` — §Permission matrix: the three
  new read rows.
- `docs/specs/watchdog.md` — §Deferred WD-D3 (web viewer surface):
  mark landed, pointing here.
- `docs/specs/lifecycle-api.md` — §Deferred LC-D6 (web dashboard
  surface): mark landed for the lifecycle/clear/paper-gate panel,
  pointing here.
- `control-plane/internal/api/rbac_test.go` — `TestRBACMatrixPins`'s
  comment pins "env-admin has no read surface", which becomes wrong
  once `GET /api/v1/alerts` admits env-admin (OS-19): the comment
  (and any assertion pinning it) is updated in the implementation
  change.

## Deferred (recorded, not silent)

- OS-D1 — outbound notification (webhook/email/pager): v1 delivery is
  the log line plus this API feed; revisit with operational evidence
  that polling the feed is insufficient.
- OS-D2 — tenant/platform kill and clear UI (and their ack literals):
  v1 panel is strategy-tier only; the wider tiers stay curl/runbook
  acts.
- OS-D3 — tokens/billing admin UI.
- OS-D4 — SSE/websocket push (the polling deferral of
  persistence-and-api.md stands).
- OS-D5 — a dedicated web-operator DB role/token bootstrap flow (the
  OS-4 deployment note automated); includes revisiting whether the
  env operator class should ever gain read or lifecycle rights —
  today it deliberately does not (OS-2).
- OS-D6 — a platform dashboard page consuming `GET /api/v1/alerts`
  (the endpoint lands in v1; its UI does not).
- OS-D7 — keyset (cursor) pagination for the alert feeds: v1 is
  LIMIT/OFFSET with per-poll page snapshots (OS-16); revisit if page
  shift under append becomes an operational problem.
- OS-D8 — write-side strategy stamping for `safety_effect_stalled`
  alerts (`stallScan` passing the event's `strategy_id` when set),
  so strategy-kill stalls reach the per-strategy feed (OS-17).
