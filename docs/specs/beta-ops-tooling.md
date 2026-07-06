# Beta Ops Tooling: betalog, betaaudit, deadman

Status: normative for the three operator tools that make
`docs/BETA-PROTOCOL.md` executable. BETA-PROTOCOL stays normative on
the judgment; this spec is normative on the tools' behavior. All
three are offline/operator-side binaries under `control-plane/cmd/`
(same module; stdlib plus the module's pinned `modernc.org/sqlite`
driver — the backupverify precedent, which shares the exact driver
version with the server via go.mod); none of them is reachable from
the agent plane and none of them ever writes to `control.db`.

Trust model (read this first): every one of these tools RUNS ON A
HOST THE GRADED OPERATOR CONTROLS, under a system clock the operator
controls. No tool here can make self-produced evidence trustworthy;
what they make is evidence whose tampering is DETECTABLE by the exit
reviewer, and only in combination with the off-host custody rules
below (BL-6, DM-5). Where a residual gaming window remains, this
spec says so explicitly rather than pretending otherwise.

Why tools instead of manual procedure: BP-9's hash chain, BP-8's
weekly V1–V9 queries, and BP-2 item 4's receiver-side dead-man alarm
are all obligations the protocol places on the operator. Done by
hand they are exactly the evidence that a hostile exit review will
discount — mistyped hash chains, ad-hoc SQL, and an alarm that only
exists as intent. Each tool produces the artifact the exit review
already demands, in the shape it demands.

## Shared conventions (NORMATIVE)

- **BT-1.** Each binary follows the `backupverify` pattern:
  `main()` delegates to `run(args, stdout, stderr) int` for
  testability. Exit codes: `0` success/no findings, `1` findings or
  runtime failure, `2` usage error. Timestamps RFC 3339 UTC `Z`.
  Hashes are lowercase-hex SHA-256 (`hex.EncodeToString`).
- **BT-2.** Secrets (bearer tokens) enter via environment only —
  never argv, never printed, never logged. Output containing
  attacker-controlled text (alert details, log entries) is written
  verbatim only to files, never interpolated into shell commands.

## betalog — hash-chained beta log (BP-9)

- **BL-1.** File format: UTF-8 JSON Lines. Entry `n` (1-based) is a
  single line, a JSON object with exactly these fields, serialized
  with Go `encoding/json` field order as declared:
  `{"n": <int>, "prev": "<64 hex>", "at": "<RFC3339Z>",
    "type": "<string>", "text": "<string>", "refs": {<string:string>}}`.
  `prev` of entry 1 is 64 `'0'` characters; `prev` of entry n>1 is
  SHA-256 over the exact bytes of line n−1 EXCLUDING its trailing
  `\n`. The chain hashes bytes, not parsed values — any byte edit to
  a committed line breaks every subsequent `prev`.
- **BL-2.** `betalog append -log <path> -type <type> -text <text>
  [-ref k=v]...` takes an advisory `flock` on the log (two
  concurrent appends must not both read the same tail — a duplicate
  `n` would brick the chain by the tool's own race), reads the LAST
  LINE ONLY, computes `prev`, appends one line with `O_APPEND`,
  prints the new entry's own SHA-256 to stdout. `at` is
  tool-generated `time.Now().UTC()` — never operator-supplied. If
  the last line is corrupt append REFUSES (exit 1). Append verifies
  the tail only, NOT the whole file: a mid-file tamper is caught by
  the next `verify`, not at append time. Appending to an absent
  file starts a fresh chain (entry 1); a fresh chain appearing
  mid-beta is itself an exit-review finding (the daily off-host
  copies make it visible — BL-6).
- **BL-3.** Types are an open set, but two are enforced because
  BP-1 §Incident binds them: `incident_ack` and `incident_resolve`
  REQUIRE `-ref source=<source> -ref id=<id>` (the notifier dedupe
  pair). `limit_change` REQUIRES `-ref change_id=<change_id>`
  (joins V9 to the log). Missing required refs = exit 2, nothing
  appended.
- **BL-4.** `betalog verify -log <path>` re-walks the whole file:
  every line parses, `n` is 1,2,3,… with no gap, every `prev`
  matches, no duplicate `(type=incident_ack, refs.source, refs.id)`
  triple. Exit 0 clean, 1 on any break (first break reported with
  line number). A timestamp earlier than its predecessor is a
  WARNING on stderr, not a failure (NTP slew is legal; the chain is
  the integrity claim, `at` is the SLA claim).
- **BL-4a.** Corrections: a wrong entry is never rewritten (BL-5)
  and a duplicate ack is never appended (BL-4). The correction
  pattern is a NEW entry, `type=correction`, with
  `-ref supersedes=<n>` naming the corrected entry. M7 evaluation
  always uses the FIRST `incident_ack` for a `(source, id)` pair;
  corrections adjust narrative, never the SLA clock.
- **BL-5.** Non-goals: no delete/rewrite/compact command exists; no
  in-place edit is ever legal. betalog makes tampering DETECTABLE,
  not impossible: the chain has no secret input, so an operator who
  controls the host can regenerate the whole file under a slewed
  clock. The countermeasure is custody, not cryptography — BL-6.
- **BL-6.** Off-host custody and the reviewer's diff (NORMATIVE —
  this is what defeats regeneration): the log is copied off-host
  daily (BP-9) to storage the operator CANNOT rewrite (versioned
  object store with retention lock, or a copy held by the partner
  or reviewer); the custody arrangement is itself Day-0 evidence
  (BP-2). Because the log is append-only, each daily copy MUST be a
  byte-prefix of every later copy and of the final log —
  `betalog verify -log <daily-copy> -prefix-of <final>` checks
  exactly that. A regenerated chain fails the prefix property
  against any pre-regeneration copy. Honest residual limit: daily
  copies bound `at` to ±24h and NO FINER — an operator can slew the
  clock a few hours within a day before appending. M7 verdicts
  inside that tolerance rest on the receiver raw log (BP-1
  tiebreaker) and journal cross-checks, not on `at` alone.

## betaaudit — BP-8 audit queries (V1–V9, DB-verifiable subset)

- **BA-1.** `betaaudit -artifact <path> | -db <path>
  [-ref-artifact <path>] [-strategy <id>] [-log <path>]
  [-stall-seconds <n>] [-json]`. The RECOMMENDED and default
  posture is `-artifact`: the newest verified backup artifact,
  opened `mode=ro&immutable=1` (backupverify pattern) — artifacts
  are stable files the reviewer can tie to the BP-5 off-host copy
  log by SHA-256. `-db` (live DB, `mode=ro` + `busy_timeout`)
  exists for spot checks and prints a warning: long reads on the
  live WAL file prevent the backup's zero-triple
  `wal_checkpoint(TRUNCATE)` from succeeding (store/backup.go), so
  a weekly audit overlapping the OB-10 loop would MANUFACTURE
  `backup_failed` incidents. `V7b` therefore requires BOTH sides to
  be artifacts. The tool performs ZERO writes and never runs VACUUM
  or any mutating PRAGMA (rowid stability — the no-VACUUM rule).
- **BA-2.** Output is one block per check: id, verdict
  `PASS|FAIL|MANUAL`, finding count, and for FAIL the first ≤ 20
  offending ids. The SQL text of every executed query is printed
  (BP-8: audits "logged with the queries used"). The report header
  binds the report to its inputs — without this the report is
  unfalsifiable: input path and mode, file SHA-256 for every
  artifact input (the live DB is not stably hashable and the header
  says so instead), `PRAGMA user_version`, per-table `max(rowid)`
  for the V7b table set, and audit start time. `-json` emits the
  same as a single JSON object for the weekly report; every MANUAL
  check appears with status `OPEN` plus the human procedure text.
  A MANUAL item is discharged only by a beta-log entry
  (`type=audit_manual`, `-ref check=<id> -ref result=<pass|fail>`)
  for that week; a weekly report whose MANUAL items lack matching
  beta-log entries is an INCOMPLETE audit at exit review. Exit 0
  iff no FAIL; MANUAL never affects the exit code.
- **BA-3.** Checks (verdicts only from persisted rows; anything
  requiring config inspection, API probes, or host access is
  emitted as MANUAL with the BP-8 text of what the human must do):
  - `V1a` orders with `origin='proposal'` and (`proposal_id` NULL
    or no `verdicts` join) → FAIL rows.
  - `V1b` proposal-originated orders whose verdict decision is
    `reject`, or `escalate` without an `approvals` row with outcome
    exactly `approved` → FAIL rows. `approved_but_blocked` means
    the preflight blocked submission and NO order exists
    (persistence-and-api.md §Approvals); an order paired with
    `approved_but_blocked`, `rejected`, or `timeout` is precisely
    the unexplained order V1 exists to catch.
  - `V1c` orders with origin outside
    `('proposal','breaker','kill','watchdog','sl_contingency')`
    (defense-in-depth behind the CHECK) → FAIL rows.
  - `V2a` `protective_obligations` with `satisfied_at` NULL, past
    `due_at` (vs audit start time), and no
    `oms_recon_events.kind='sl_deadline_contingency'` row matching
    on `(strategy_id, symbol)` — via the obligation's entry order —
    with `recorded_at ≥ due_at` → FAIL rows. The contingency event
    carries no order id (oms/live/protective.go), so the match is
    per `(strategy_id, symbol)` and is AMBIGUOUS when multiple
    entries share a symbol; the report says so on affected rows.
    Exchange-side SL/TP residency AND the no-agent-plane-mutation
    clause of BP-8 V2 = MANUAL.
  - `V4a` `kill_breaker_events` rows older than the deployment's
    configured stall threshold (`-stall-seconds`, default 600 —
    `safety_effect_stall_seconds` default) with no `safety_effects`
    completion row → FAIL rows. Valid only against live-OMS
    deployments (paper mode never drives effects — LC-38); the
    beta's DBs are live-OMS by definition.
  - `V4b` `lifecycle_transitions` with `from_state='killed'` and no
    scope-covering `kill_clear_events` row recorded at-or-before
    the transition → FAIL rows. Scope cover is the LC-28
    three-clause match (store/killclear.go): a `strategy`-scope
    clear with that `strategy_id`, OR a `tenant`-scope clear with
    the strategy's `tenant_id` (via `strategies`), OR a `platform`
    clear — tenant/platform kills lock per covered strategy but
    their clears carry `strategy_id` NULL.
  - `V4c` every kill row whose strategy was in a `live_*` state
    pairs with a `lifecycle_transitions` row to `killed`, actor
    role `system` (the lifecycle lock actually engaged) → FAIL
    rows otherwise.
  - `V5a` every `safety_alerts.kind='watchdog_kill_escalation'`
    pairs with a `kill_breaker_events` kill row, actor `watchdog`;
    silence-timing reconstruction = MANUAL.
  - `V7a` linkage: every `fills.order_id` exists in `orders`; every
    `verdicts.proposal_id` exists in `proposals`; every
    proposal-originated order joins both → FAIL rows.
  - `V7b` (only with `-ref-artifact`; both sides artifacts): for
    each table in the NORMATIVE append-only set — `proposals`,
    `verdicts`, `approvals`, `fills`, `lifecycle_transitions`,
    `kill_breaker_events`, `kill_clear_events`, `safety_alerts`,
    `safety_effects`, `oms_recon_events`, `risk_limit_changes`,
    `rejected_submissions`, `token_events`, `venue_epochs`,
    `agent_traces`, `model_costs` (NOT `orders`/`runs`/
    `protective_obligations`/`order_intents` etc., whose columns
    legally mutate) — every rowid present in the reference exists
    in the current artifact with byte-identical column values
    (per-row SHA-256 compare) and current `max(rowid)` ≥ reference;
    shrink or mutation → FAIL.
  - `V9a` (only with `-strategy`): replay `risk_limit_changes` for
    the certified strategy, printing who/when/old→new and
    classifying each numeric-cap change as tightening/loosening
    (new > old = loosening). `symbol_whitelist` is NOT
    runtime-changeable in v1 (api/limits.go field whitelist) so it
    never appears in this table; whitelist widening happens via
    env + restart and is a MANUAL item: diff the effective
    whitelist (newest verdict's persisted `limits_snapshot`)
    against the Day-0 countersigned value. Classification is a
    report; pairing loosenings with a clock restart stays the
    reviewer's judgment. With `-log`, every `change_id` lacking a
    `limit_change` beta-log entry → FAIL rows. HONEST LIMIT: this
    is a COVERAGE check only — the chain proves order, not time,
    so a paper-over append five minutes before the audit passes
    it. Timeliness comes from BL-6: the reviewer locates each
    `change_id` entry's first appearance across the daily off-host
    copies; a `limit_change` entry first appearing ≥ 48h after its
    DB row's `changed_at` is an exit-review finding.
  - `M6a` per `alert_dispatch_state` source: watermark vs current
    `max(rowid)`; lag > 0 reported (informational, PASS/FAIL per
    the M6 bar is the audit-time reviewer's call → MANUAL verdict
    with the numbers printed). Poison-row skips ADVANCE the
    watermark (AN-4a) and are invisible here; the MANUAL text
    directs the human to grep the journal for
    `ALERT DISPATCH SKIPPED` lines — M6's "0 poison-row skips" bar
    is NOT covered by this query.
  - `V3`, `V6`, `V8` → MANUAL (host/config/API-probe checks, per
    BP-8 text).
- **BA-4.** Non-goals: betaaudit does not compute M1–M8 outcomes,
  does not write the weekly report file, and does not read the
  systemd journal. It is the query half of the audit; judgment
  stays human.

## deadman — reference receiver + dead-man alarm (BP-2 item 4, AN-14a)

- **DM-1.** `deadman -listen <addr> -raw-log <path>
  -heartbeat-hours <H>` runs an HTTP server for the notifier
  webhook. Auth is MANDATORY: env `DEADMAN_BEARER` must be
  non-empty or the server refuses to start (exit 2) — an
  internet-facing evidence recorder with anonymous writes would
  let ANYONE pollute the BP-1 tiebreaker file. Requests must carry
  exactly `Authorization: Bearer <value>` (constant-time compare)
  or get `401` with no body detail; each rejected attempt appends
  an `{"auth_reject": ...}` line (remote addr, time, NO header
  echo) so brute-force attempts are themselves evidence. Server
  hardening: `http.MaxBytesReader` at 1 MiB per body (an unbounded
  body is a disk-exhaustion attack ON the evidence file),
  `ReadHeaderTimeout`/`ReadTimeout`/`WriteTimeout` set, and all
  raw-log appends serialized through a single mutex-guarded writer
  (concurrent `O_APPEND` JSONL interleaving would corrupt the
  evidence). It is a separate trust domain: it holds NO
  control-plane tokens and no venue keys.
- **DM-2.** Every accepted POST is appended (single-writer, JSONL)
  to the raw log BEFORE the `200` is written:
  `{"received_at": "<RFC3339Z>", "envelope": <verbatim body>}` —
  this file is the BP-1 acknowledgment tiebreaker. Body that is not
  valid JSON is still logged (base64 under `"raw_b64"`) and answered
  `400`; the notifier never legally sends it, so it must be visible,
  not dropped. Failure to append = `500` (at-least-once: the
  notifier will redeliver; losing evidence is worse than a retry).
  Disk-full is therefore the RIGHT failure direction by design: the
  notifier retries, its watermark stalls, and M6 degrades — a full
  evidence disk becomes a visible metric breach, not silent loss.
- **DM-2a.** Lifecycle marks: on start the receiver appends
  `{"mark": "receiver_start", "at": ...}` and every 10 minutes
  `{"mark": "receiver_alive", "at": ...}`. This makes
  silence-of-the-receiver distinguishable from
  silence-of-the-notifier in the raw log: a gap in `receiver_alive`
  marks is receiver downtime, and BP-2 item 4 counts receiver
  downtime as availability breach from the last successful
  heartbeat — stopping deadman does not stop the clock.
- **DM-3.** Dead-man alarm: the receiver tracks the last arrival
  with `source=="notifier"` (heartbeat envelopes, AN-14a: id
  `heartbeat-<RFC3339Z>`, seq 0). A background ticker (30 s) fires
  when silence exceeds `H+1` hours: it appends
  `{"alarm": "heartbeat_silence", "silent_since": ...,
  "fired_at": ...}` to the raw log (this `fired_at` is the SLA
  clock start for the heartbeat-gap and control-plane-down classes,
  BP-2 item 4) and, if env `DEADMAN_ALARM_URL` is set, POSTs that
  same JSON there (5 s timeout). The alarm line is appended ONCE
  per silence episode; the URL POST is retried each tick while
  silent (best-effort). The alarm re-arms after the next heartbeat
  arrival. On start the tracker seeds from the raw log's newest
  notifier arrival, NOT from process start: a receiver restarted
  after long silence must alarm immediately, not grant a fresh
  `H+1` grace window. If the log has no notifier arrival, seed =
  receiver start (first boot only).
- **DM-4.** Drill hook (Day-0 evidence): `deadman -selftest
  -target <url>` sends one synthetic envelope to a running
  instance and exits (`-target` without `-selftest` = exit 2). The
  selftest envelope carries `"source": "selftest"` so it can never
  satisfy DM-3's tracker or mask a real gap. HONEST LIMIT
  (NORMATIVE to state in Day-0 evidence): the operator configured
  `DEADMAN_BEARER`, so the operator can forge
  `source=="notifier"` envelopes with curl and keep the dead-man
  silent while the control-plane is down. The raw log alone
  therefore cannot prove M3 availability; the exit review
  cross-checks forgery-resistant evidence — `alert_dispatch_state`
  watermarks and append-only table growth across daily artifacts
  (V7b), and journal continuity. A raw log whose heartbeats
  continue across a window where daily artifacts show no
  watermark/rowid movement is itself an exit-review finding.
- **DM-5.** Non-goals: no TLS termination (operators front it with
  their reverse proxy), no dedupe (the raw log is evidence of every
  delivery, duplicates included), no queryable API, no PagerDuty
  templating (AN §Non-goals). The raw log is NOT hash-chained:
  its trust rests on the same daily off-host prefix custody as the
  beta log (BL-6 applies to it verbatim).

## Acceptance (all enforced by `go test -race`)

- betalog: append/verify round-trip; tamper (byte edit, line
  delete, line reorder, truncation) each detected with line number;
  broken-tail append refusal; required-refs enforcement;
  `-prefix-of` accepts a true prefix and rejects a regenerated
  chain; concurrent appends (two goroutines × N) never produce a
  duplicate `n` (flock); correction entry appends with
  `supersedes` ref.
- betaaudit: fixture DB with seeded violations for every FAIL check
  (V1a–V9a) catches each; clean fixture exits 0; `-ref-artifact`
  shrink and mutation both FAIL V7b; report header carries input
  SHA-256 + user_version + per-table max(rowid); read-only enforced
  (db file bytes identical before/after run); `-db` prints the
  live-WAL warning.
- deadman: refuses to start without `DEADMAN_BEARER`; bearer
  accept/reject with auth_reject line; oversized body rejected;
  raw-log append order vs 200; invalid-JSON logging; concurrent
  POSTs produce valid JSONL (no interleaving); alarm fires after
  simulated silence, appends once per episode, re-arms; tracker
  seeds from existing raw log on restart (immediate alarm after
  long-silence restart); selftest cannot reset the tracker.
