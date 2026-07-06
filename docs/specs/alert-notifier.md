# Alert notifier — push dispatch of safety events

Status: normative. Companion procedures live in `docs/RUNBOOK.md` §9
(added with this feature); on
any conflict this spec wins. Cross-references: `docs/specs/safety-wiring.md`
(SW-*), `docs/specs/operator-surface.md` (OS-*), `docs/specs/watchdog.md`
(WD-*), `docs/specs/ops-backup.md` (OB-*).

## Motivation

Every safety-relevant fact is already persisted append-only
(`kill_breaker_events`, `kill_clear_events`, `safety_alerts`) and readable
via the operator surface (OS-7, OS-15..18). But nothing PUSHES: a kill,
breaker trip, or watchdog escalation is invisible until a human happens to
poll the ops panel. For a 30-day design-partner beta that is fail-open on
the human side. The notifier closes this: an at-least-once, ordered,
watermark-resumable dispatcher that POSTs each new safety event to one
operator-configured webhook (or, in log-only mode, emits a stable marker
line for syslog forwarding).

Three facts drive the design and are normative context:

- Breaker `fire()` and the kill endpoints append ONLY a
  `kill_breaker_events` row — they do NOT write `safety_alerts`. A
  notifier reading `safety_alerts` alone misses kills and breaker trips
  entirely. The notifier therefore reads MULTIPLE source tables.
- All three source tables are append-only and the control-plane database
  is never VACUUMed (OB-2 context), so `rowid` is stable and insert-order
  monotonic per table. `rowid` watermarks are safe and are the resume
  mechanism.
- Rowid monotonicity-as-commit-order additionally REQUIRES the store's
  single-connection pool (`SetMaxOpenConns(1)`): with concurrent writers
  under WAL, rows can commit out of rowid order and a `rowid > watermark`
  poller would permanently skip the late-committing row. Raising the pool
  size above 1 invalidates this spec; the store MUST carry a tripwire
  test pinning the pool size.

## Sources

**AN-1.** The notifier dispatches rows from exactly three sources, each
with an independent `rowid` watermark:

| source (wire name) | table | id field | delivered DTO |
|---|---|---|---|
| `kill_breaker_events` | `kill_breaker_events` | `event_id` | AN-13 |
| `kill_clear_events` | `kill_clear_events` | `clear_id` | AN-13 |
| `safety_alerts` | `safety_alerts` | `alert_id` | AN-13 |

`oms_recon_events` is out of scope for v1 as a SOURCE (see Non-goals),
but two of its kinds are page-worthy and must not be invisible:

**AN-1a (companion alerts).** The writers of `venue_reset` (the OMS
refuses ALL order sends until a human acknowledges — the single most
page-worthy condition in the system) and `sl_deadline_contingency` (an SL
could not be placed inside its deadline) MUST additionally append a
`safety_alerts` row with the same kind and a `ref_id` of the recon
event's `event_id`, following the existing writer-side precedent of
`kill_effects_superseded` (appended inside the clear transaction). The
companion alert MUST commit in the same transaction as the recon-event
row: the existing writers call `AppendOMSReconEvent`, a standalone
statement, so a combined recon-event+alert store mutator is required —
two sequential appends leave a crash window that loses the alert. These
facts then reach the notifier through the `safety_alerts` source with no
fourth watermark. `tp_deadline_missed` is intentionally excluded (a TP is
profit-taking, not protection).

**AN-2.** Selection per source is `WHERE rowid > ? ORDER BY rowid ASC
LIMIT ?` — insert order, never timestamp order. Timestamps in these
tables come from different writers and MAY interleave non-monotonically;
`rowid` is the only total order the store guarantees per table (under the
single-connection requirement stated in Motivation).

**AN-2a (no connection held across network I/O).** A dispatch pass MUST
fully materialize its batch into memory and release every store resource
(rows closed, no open transaction, no checked-out connection) BEFORE the
first network attempt. The only store writes a pass performs are
single-statement watermark upserts between deliveries, each its own
short-lived statement. Holding the pool-of-one connection across an HTTP
POST would block every safety write (kills, breaker fires, clears) for up
to `timeout_seconds` × `max_per_tick` — the exact failure class this
system exists to prevent — and is therefore forbidden, not merely
discouraged.

## Delivery semantics

**AN-3.** At-least-once, per-source in-order. The watermark for a source
advances to a row's `rowid` only AFTER that row's delivery attempt
succeeded (AN-16). A crash between delivery and watermark persist yields
redelivery, never loss. Receivers MUST dedupe on `(source, id)`.

**AN-4.** Stop-on-failure: within one dispatch pass, the first failed
delivery aborts the pass for that source; the watermark stays at the last
success and the next tick resumes there. No reordering, no
dead-lettering. Skipping is forbidden except for the single, loudly
logged poison-row case of AN-4a.

**AN-4a (poison rows must not silence future kills).** Failure classes
split: transport errors, timeouts, 3xx, and 5xx are treated as transient
and retry forever (the receiver is down or broken; nothing about the ROW
is at fault). A 4xx is a deterministic rejection of THIS request: if the
SAME row fails with 4xx on 12 consecutive ticks, the dispatcher advances
the watermark past that one row and logs `ALERT DISPATCH SKIPPED
source=… id=… seq=… status=…`. Without this, one over-large or
receiver-rejected row wedges its source forever and every subsequent
kill notification is silently lost — the notifier's failure mode would
reproduce the exact problem it was built to close. The skip is per-row
(the next row starts its own count) and MUST be observable in logs.
"Consecutive" counts delivery ATTEMPTS of that row (an AN-6a-suppressed
tick attempts nothing and does not count); the counter is in-memory and
resets on restart — a restart can only delay a skip, never hasten it.
Accepted.

**AN-5.** The notifier NEVER writes to any source table and never blocks
any safety write path. It is a read-only poller in its own goroutine;
kills, clears, breaker fires, and alert appends proceed identically
whether the notifier is enabled, disabled, wedged, or crashed.

**AN-6.** One event per POST (no batching), at most `max_per_tick` events
per source per tick (default 20). Draining a large backlog takes multiple
ticks by design; bounded work per tick keeps the goroutine's DB and
network footprint predictable.

**AN-6a (no overlap; kills first).** A pass that is still running when
the ticker fires suppresses that tick (skip-never-queue, as OB-10's
periodic backup). Within every pass the sources are processed in fixed
order: `kill_breaker_events`, then `kill_clear_events`, then
`safety_alerts` — a kill is never queued behind slow-but-successful
deliveries of lower-urgency sources. Operators sizing the config should
note the worst-case pass duration is
`3 × max_per_tick × timeout_seconds` (defaults: 300 s); the poll tick
does not shorten it, it only spaces pass starts.

## Watermarks

**AN-7.** Watermarks persist in a new mutable-snapshot table:

```sql
CREATE TABLE IF NOT EXISTS alert_dispatch_state (
  source TEXT PRIMARY KEY,          -- AN-1 wire name
  last_rowid INTEGER NOT NULL,      -- last successfully delivered rowid
  updated_at TEXT NOT NULL);        -- RFC 3339 UTC Z
```

Table creation is UNCONDITIONAL at `store.Open` (same additive-migration
pattern as lifecycle bootstrap — config never reaches the store layer);
only SEEDING and the dispatcher goroutine are config-gated. An empty
table on a deployment that never enables the notifier is the intended
state, not a partial effect.

**AN-8.** Seed-at-enable, not seed-at-migrate. Seeding runs SYNCHRONOUSLY
after `store.Open` and BEFORE any goroutine that can write a source table
starts (safety monitor, live OMS, API listener — the listener boundary is
`ListenAndServe`; handler construction is inert) — otherwise a breaker
fired or an alert appended during boot lands below the freshly seeded
watermark and is silently lost. For each source with no watermark row,
seed `last_rowid = COALESCE(MAX(rowid), 0)` at that moment. Notification
coverage therefore begins at first enablement — enabling the notifier on
a long-lived deployment MUST NOT flood the receiver with the entire
event history.

**AN-8a (existing watermarks at start).** When a watermark row EXISTS at
dispatcher start: (a) if `last_rowid > MAX(rowid)` for its source (hand-
mixed DB lineage, operator SQL, implementation bug — unreachable via the
OB-12 restore, which replaces the whole file), clamp it down to
`MAX(rowid)` and log loudly; without the clamp, new events allocate
rowids still below the stale watermark and kills go unnotified silently.
(b) Log the per-source backlog size (`MAX(rowid) − last_rowid`). The
backlog IS dispatched — a kill recorded while the notifier was disabled
may still be active, so silent skipping is forbidden — but an operator
re-enabling after a long gap sees the flood coming in the log line, and
`docs/RUNBOOK.md` §9 documents the manual reseed statement for when
"from now on" is the actual intent.

**AN-9.** Watermark rows are updated in place (`INSERT ... ON CONFLICT
(source) DO UPDATE`); this table is exempt from the append-only invariant
exactly like `strategy_state`. If a watermark upsert FAILS after a
successful delivery, the pass for that source aborts; the next tick
redelivers from the last persisted watermark (duplicates, never loss —
AN-3). The in-memory position is never trusted across a failed persist.

## Configuration

**AN-10.** Single env var `CONTROLPLANE_ALERT_WEBHOOK`, JSON, parsed with
`DisallowUnknownFields`, fail-fast at startup (config-error = refuse to
boot, matching CONTROLPLANE_RISK_LIMITS conventions — structured configs
are JSON env vars; flat vars are for scalars). Unset/empty = notifier
disabled entirely: no goroutine, no seeded watermark rows (the table
itself is created unconditionally — AN-7).

```json
{
  "url": "https://ops.example.com/hook",
  "authorization_bearer": "…",
  "timeout_seconds": 5,
  "poll_seconds": 5,
  "max_per_tick": 20,
  "heartbeat_hours": 24,
  "log_only": false
}
```

| field | required | default | validation |
|---|---|---|---|
| `url` | iff `log_only` is false; MUST be absent otherwise | — | `net/url.Parse`; scheme `http` or `https`; non-empty host; userinfo (`user:pass@`) REJECTED |
| `authorization_bearer` | no | absent | sent as `Authorization: Bearer …`; MUST be absent when `log_only`; with scheme `http` the host MUST be loopback (`127.0.0.0/8`, `::1`, `localhost`) — a bearer on a cleartext non-loopback hop is a config error, not a warning |
| `timeout_seconds` | no | 5 | integer in [1, 60] |
| `poll_seconds` | no | 5 | integer in [1, 300] |
| `max_per_tick` | no | 20 | integer in [1, 500] |
| `heartbeat_hours` | no | 24 | integer in [0, 168]; 0 disables the AN-14a heartbeat |
| `log_only` | no | false | when true: `url` and `authorization_bearer` MUST be absent |

**AN-11 (secret hygiene, implementable form).** The URL and bearer are
secrets (hosted-webhook URLs embed capability tokens in the path). The
rule is not "redact before logging" — Go wraps the URL into `url.Error`,
DNS errors carry hosts, TLS errors carry names, and `json`/`url.Parse`
errors echo input fragments; redaction is whack-a-mole. The implementable
rule: the notifier NEVER logs `err.Error()` from config validation or
delivery. Delivery failures log only a derived class computed via
`errors.As`/inspection — one of `dns`, `connect`, `tls`, `timeout`,
`redirect`, `status:<code>`, `other`. Config validation failures name the
offending FIELD only and never wrap or propagate the decoder/parser error
text. The HTTP transport MUST set `Proxy: nil` (a `HTTP_PROXY`
environment variable would otherwise route the full URL and headers
through a proxy for http-scheme requests), and `CheckRedirect` MUST
return `http.ErrUseLastResponse` so a 3xx surfaces as a failed status
(AN-16) without ever constructing a `url.Error` for the redirect target.

**AN-12.** Mode-independent: the notifier runs identically in paper and
live OMS modes and regardless of ingestion being enabled, mirroring the
backup surface's mode independence (OB-8 context).

## Wire envelope

**AN-13.** Each delivery POSTs one JSON object:

```json
{
  "schema": "alphamintx.safety-event.v1",
  "source": "kill_breaker_events",
  "id": "3f0e…",
  "seq": 4711,
  "delivered_at": "2026-07-06T01:02:03Z",
  "event": { }
}
```

- `schema` is the literal version pin.
- `source` and `id` are the AN-1 dedupe pair. `seq` is the source rowid:
  monotonic per source WITHIN ONE DATABASE LINEAGE only, and gapless
  except at explicitly logged AN-4a skips. After an OB-12 restore, rows
  appended post-restore REUSE rowids of already-delivered pre-restore
  rows — receivers MUST dedupe and cursor on `(source, id)` ONLY and
  MUST NOT treat `seq` as a resume point. `seq` exposing table
  cardinality to the operator's own receiver is accepted (D8 trust
  model).
- `delivered_at` is dispatch time, NOT event time; the event's own
  `recorded_at` is inside `event`.
- `event` for `safety_alerts` is exactly the OS-18 alert DTO shape
  (`alert_id`, `kind`, `strategy_id`, `ref_id`, `details_json` as a raw
  string, `recorded_at`), with one bound: a `details_json` longer than
  8 KiB is truncated to 8 KiB and suffixed with `…[truncated]` (the wire
  value then may not parse as JSON; the DB row stays complete). This
  keeps a pathological details blob from making the envelope
  receiver-rejectable (413 ⇒ AN-4a).
- `event` for the other two sources is the full row, nullable columns as
  JSON null. Explicit wire columns (for `kill_breaker_events`,
  `tenant_id` arrives via the tenancy migration and is NOT in that
  table's DDL text — it is a wire column regardless; `kill_clear_events`
  has it in the DDL):
  - `kill_breaker_events`: `event_id`, `kind`, `scope`, `strategy_id`,
    `tenant_id`, `kill_epoch`, `flatten` (boolean or null), `trigger_ref`
    (raw string or null, same 8 KiB bound), `actor_id`, `recorded_at`.
  - `kill_clear_events`: `clear_id`, `scope`, `strategy_id`, `tenant_id`,
    `cleared_epoch`, `actor_id`, `reason` (same 8 KiB bound — it is
    operator-supplied and otherwise bounded only by the 1 MiB body cap),
    `recorded_at` (no `flatten` column exists on this table).
  The composite/`cleared` join shapes of OS-7 are NOT used — the wire
  carries facts, not views.
- Sources are independent streams: a receiver MUST NOT infer cross-source
  ordering (a clear and its `kill_effects_superseded` alerts commit
  atomically in the store but ride independent watermarks and may arrive
  in either order, or far apart if one source is stalled).

**AN-14.** Log-only mode emits, instead of an HTTP POST, exactly one
line per event: the marker `SAFETY-EVENT ` followed by the AN-13 envelope
serialized as a single JSON line, written via a dedicated zero-flag
logger (`log.New(os.Stderr, "", 0)`) so the marker is a true line prefix
on the process's stderr. Journald/syslog prepend their own metadata, so
the RUNBOOK's forwarding examples MUST match on the marker as a
SUBSTRING, never `^SAFETY-EVENT`. Webhook mode does NOT also emit this
line (it would double-page a log-forwarding operator); it logs only
failures (AN-17) and a one-line non-secret summary per successful
delivery: source, id, seq. Note the envelope payload reaches whatever
log aggregation exists — the same data-classification caveat as the
webhook body applies (see RUNBOOK §9).

**AN-14a (dispatch heartbeat).** Unless `heartbeat_hours` is 0, the
dispatcher delivers a synthetic envelope every `heartbeat_hours` (first
one on start, then interval-anchored) with `source: "notifier"`,
`id`: `heartbeat-` + the delivery timestamp truncated to the UTC hour in
RFC 3339 (e.g. `heartbeat-2026-07-06T01:00:00Z`; a restart inside the
same hour may re-emit an id the receiver dedupes — accepted), `seq: 0`,
and `event: {"kind": "notifier_heartbeat"}`, through the same delivery
path (webhook POST or log line) but with NO watermark involvement. The
heartbeat is attempted at most once per due pass, AFTER the three AN-6a
sources; its failure feeds its own `notifier` degradation counter (AN-17
log line, source=notifier) and never aborts or delays any source's
dispatch or watermark. Without it a healthy-but-quiet system and a
broken pipeline are indistinguishable at the receiver — events are the
only traffic, so receiver-side dead-man alarming would be impossible and
the notifier's own failure would be visible only in the logs nobody
watches (the exact regime it replaces).

## Delivery attempt

**AN-15.** `POST` with `Content-Type: application/json`, the AN-10
timeout, and no redirect following (redirects fail the attempt; a
redirecting receiver is misconfigured, and following would re-send the
body — with the bearer header — to a location the operator never vetted).

**AN-16.** Success is exactly: transport completed AND status is 2xx.
Everything else — timeout, connection error, 3xx/4xx/5xx — is failure;
the response body is read (bounded at 4 KiB) and discarded, never parsed,
never logged verbatim.

**AN-17.** Failures log one line per failed tick per source (the AN-11
derived class only) and retry on subsequent ticks; transient classes
retry forever, 4xx follows AN-4a. The poll cadence IS the retry cadence;
there is no separate backoff ladder. After 12 consecutive failed ticks
for a source (~1 min at defaults) the log line escalates to the prefix
`ALERT DISPATCH DEGRADED` and repeats at most once per minute thereafter
until a success clears the counter. Degradation is visible in logs and
at the receiver via heartbeat silence (AN-14a); it never touches the DB
and never trips any safety machinery.

**AN-17a (startup summary).** On successful startup with the notifier
enabled, the control-plane logs exactly one non-secret summary line —
mode (webhook/log-only), `poll_seconds`, `max_per_tick`,
`heartbeat_hours`, and the AN-8a per-source backlog counts — mirroring
the loud-startup precedent of `CONTROLPLANE_WATCHDOG_DISABLED`. An
operator with a typo'd URL discovers it from the first failed-delivery
log line and the missing heartbeat, not during their first real kill.

## Shutdown and lifecycle

**AN-18.** Seeding happens synchronously before source writers start
(AN-8); the dispatcher goroutine then runs on a start-anchored ticker
(`poll_seconds`) and exits when the serve context is cancelled — the same
lifecycle as the periodic-backup loop. In-flight HTTP uses a context
derived from the serve context, so shutdown cancels a hung POST rather
than waiting on it. Like the backup loop, the goroutine is not joined
before store close: a watermark upsert racing shutdown can hit a closed
store, which is accepted as redelivery-safe (AN-3/AN-9) cosmetic error
noise, never loss.

**AN-19.** Exactly one dispatcher per process. The engine takes no
cross-process lock: running two control-plane processes against one DB is
already forbidden (single-writer store); the notifier adds no new rule.

## Invariants

1. Safety write paths are byte-identical in behavior with the notifier
   on, off, failing, or absent; no store connection is ever held across
   network I/O (AN-2a, AN-5).
2. No event is silently skipped: the watermark advances only to a
   delivered row's rowid, in order, or past a poison row with a
   mandatory `ALERT DISPATCH SKIPPED` log line (AN-3, AN-4, AN-4a).
3. No history flood at first enable; a backlog at re-enable is logged
   before it is dispatched (AN-8, AN-8a).
4. Secrets (URL, bearer) never leave config memory except inside the
   HTTP request itself; no raw `err.Error()` from validation or delivery
   is ever logged (AN-11).
5. Source tables stay append-only; the only new write surface is
   `alert_dispatch_state` (AN-7, AN-9).
6. The wire envelope is versioned; any shape change bumps `schema`
   (AN-13).
7. Disabled means absent: no goroutine, no seeded rows; the empty
   `alert_dispatch_state` table is the sole unconditional artifact
   (AN-7, AN-10).
8. A wedged or slow receiver costs bounded work per tick and cannot
   back-pressure the API server or safety monitor (AN-2a, AN-6, AN-6a,
   AN-15).

## Decisions

- **D1 — poll, not hook.** Alert writes happen in ≥6 call sites, one
  inside the kill-clear transaction. A write-time hook either misses
  writers or entangles a network call with a transaction. A rowid poller
  catches every writer by construction and can never block one.
- **D2 — three sources, not one.** `safety_alerts` alone misses kills
  and breaker trips (see Motivation). Clears are included because an
  un-notified clear leaves a paged operator believing a kill is still
  active.
- **D3 — rowid watermark, not timestamp.** Timestamps interleave across
  writers and repeat within a second; rowid is the store's only per-table
  total order and is stable under the no-VACUUM regime.
- **D4 — seed-at-enable.** Seeding at migration time floods the receiver
  if the notifier is enabled long after upgrade; seeding at zero floods
  always. Enablement time is the only moment that matches operator
  intent ("notify me from now on").
- **D5 — one event per POST.** Batch delivery makes partial failure
  ambiguous (which of 20 events did the receiver process before 500?).
  Per-event POST plus per-event watermark advance keeps at-least-once
  exact; `max_per_tick` bounds the cost.
- **D6 — no backoff ladder.** The poll tick already spaces retries;
  a second backoff mechanism multiplies states for no operational gain
  at these volumes. Degradation is surfaced by log escalation (AN-17).
- **D7 — raw rows on the wire, not view DTOs.** OS-7's joined shapes
  (e.g., `cleared` embedded in a kill) change when views evolve; the
  notifier pins raw facts so receivers parse a stable, minimal shape.
- **D8 — no TLS pinning / no mTLS in v1.** Operators point at their own
  relay (typically localhost or an internal HTTPS endpoint); transport
  policy belongs to the deployment, with `authorization_bearer`
  available for receiver-side authentication. SSRF is not a v1 concern:
  the URL is a single-tenant, operator-set env var no API or web surface
  can write, and redirects are refused.
- **D9 — poison rows skip after a bounded, logged count.** Retry-forever
  on a deterministic 4xx converts one bad row into permanent silence for
  every future kill on that source — strictly worse than a logged
  single-row gap. Transient classes still never skip; at-least-once is
  preserved for everything a healthy receiver would have accepted.
- **D10 — heartbeat over a status API.** A synthetic periodic event lets
  the RECEIVER dead-man alarm on pipeline failure with zero new API
  surface or DB state; an ops-health endpoint remains a separate slice.
- **D11 — companion alerts, not a fourth source.** `venue_reset` and
  `sl_deadline_contingency` become visible by writer-side
  `safety_alerts` appends (existing precedent: `kill_effects_superseded`)
  instead of dispatching `oms_recon_events` wholesale — the recon table
  is high-volume operational noise and its page-worthy kinds are exactly
  two.

## Test obligations

- Dispatch matrix: rows in all three tables ⇒ delivered in rowid order
  per source with correct envelopes (schema pin, seq = rowid, explicit
  AN-13 wire columns incl. nulls, `flatten` bool, `tenant_id` presence).
- Source order within a pass: `kill_breaker_events` strictly before the
  other sources (AN-6a).
- No overlapping passes: a slow pass suppresses the intervening tick
  (skip-never-queue).
- Seed-at-enable: pre-existing rows in all tables, first start ⇒ zero
  deliveries, watermarks at max rowid; a row appended after the seed
  point IS delivered — including one appended concurrently with startup
  (the AN-8 seed-race case).
- Watermark clamp: `last_rowid > MAX(rowid)` at start ⇒ clamped, logged,
  and a subsequently appended row IS delivered (AN-8a).
- Re-enable backlog: existing watermark with a gap ⇒ backlog size logged
  AND fully dispatched (no silent skip).
- Stop-on-failure + resume: receiver 5xx mid-stream ⇒ watermark holds,
  no skip; receiver recovers ⇒ delivery resumes at the failed row;
  duplicates possible, gaps never (assert exact redelivery sequence).
- Poison-row skip: same row 4xx for 12 consecutive ticks ⇒ watermark
  advances past exactly that row with the `ALERT DISPATCH SKIPPED` line;
  a 5xx/timeout row is NEVER skipped regardless of count (AN-4a).
- Oversized `details_json`/`trigger_ref`/`reason`: > 8 KiB ⇒ truncated
  on the wire with the marker suffix; DB row unchanged (AN-13).
- Restart: stop the dispatcher after a delivery and its watermark
  persist but before the next row's POST, reopen store, restart ⇒ no
  gap; and stop it after a POST succeeds but before its watermark
  persist ⇒ that exact row is redelivered (pin both boundaries).
- Watermark-persist failure: injected upsert error after a successful
  delivery ⇒ pass aborts; next tick redelivers from the persisted
  watermark (AN-9).
- Per-source independence: failing source A does not stall source B.
- `max_per_tick` cap: backlog of N > cap drains across ⌈N/cap⌉ ticks.
- Config table: every AN-10 validation row (bad scheme, userinfo,
  log_only+url, log_only+bearer, bearer+http+non-loopback, bounds) fails
  startup with a message that names the field and does NOT contain the
  URL, bearer, or raw JSON; valid configs parse with defaults applied.
- Secret hygiene matrix: DNS failure, connection refusal, TLS failure,
  timeout, and redirect — each against a URL embedding a marker string;
  the marker appears in NO log output, and each failure logs its AN-11
  class (extend beyond a single connection-error case).
- Redirect refusal: 302 ⇒ `status:302` failure, no second request
  (count requests), no `url.Error` in any log.
- 2xx-only success: 200/204 advance; 302/400/500/timeout do not.
- Log-only mode: no HTTP server needed; marker lines carry the exact
  envelope as a `SAFETY-EVENT `-prefixed single line on stderr;
  url+log_only and bearer+log_only each fail startup.
- Heartbeat: synthetic envelope with `source:"notifier"`, `seq:0` at the
  configured cadence; `heartbeat_hours:0` disables; heartbeat failure
  does not block event dispatch (AN-14a).
- Degraded escalation: 12 consecutive failures ⇒ `ALERT DISPATCH
  DEGRADED` appears; success clears and returns to quiet operation.
- Startup summary: enabled ⇒ exactly one AN-17a line with backlog
  counts; disabled ⇒ no notifier lines, no goroutine, no seeded rows,
  table still created (invariant 7).
- Shutdown: cancel serve ctx during a hung POST ⇒ dispatcher exits
  promptly (bounded by test timeout, not by `timeout_seconds`).
- AN-2a/AN-5 non-interference: with a receiver that blocks forever and a
  deterministic hook proving a POST is in flight, a concurrent kill
  append completes at normal latency (mechanism-pinning, not timing
  luck); plus a store tripwire test asserting the pool of one.
- AN-1a companion alerts: a `venue_reset` and an `sl_deadline_contingency`
  recon event each produce a `safety_alerts` row (same kind, `ref_id` =
  recon `event_id`) and are delivered through the `safety_alerts` source.

## Non-goals (v1)

- `oms_recon_events` as a dispatch SOURCE (high-volume operational
  noise; its two page-worthy kinds arrive via AN-1a companion alerts;
  revisit with data from the beta).
- `lifecycle_transitions` dispatch — a transition INTO `live_*` is
  operator-initiated through the confirm-gated ops panel, so the actor
  already knows; still, this is an acknowledged gap for multi-operator
  teams, deliberately deferred.
- Receiver templating (Slack/PagerDuty formats) — operators run a relay;
  the wire shape is stable JSON.
- Delivery to multiple receivers, fan-out, or per-kind routing.
- A read API over `alert_dispatch_state` (heartbeat covers dead-man
  alarming in v1; an ops-health endpoint is a separate slice).
- Web-panel configuration of the webhook (env-only, like every other
  secret-bearing config).
