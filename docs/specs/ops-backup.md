# Spec: Control-plane DB backup and restore (Phase 3 ops readiness)

Normative. Defines the online backup of `control.db`, the offline restore
procedure, the artifact verifier, and the operator runbook obligations.
Companions: `docs/specs/persistence-and-api.md` (store pragmas, append-only
rules), `docs/specs/billing-and-metering.md` (the no-VACUUM constraint this
spec inherits), `docs/specs/multi-tenant-rbac.md` (permission matrix),
`docs/specs/operator-surface.md` (read-surface conventions). On conflict,
billing-and-metering.md wins for the VACUUM constraint; this spec wins for
backup/restore mechanics.

## Goals

- An operator can take a consistent, verified snapshot of `control.db` while
  the control-plane is serving, with one authenticated POST — no shell access
  to the data directory required for day-to-day operation.
- A restored snapshot is provably usable: byte-level integrity, referential
  integrity, and rowid preservation are all checked, not assumed.
- The 30-day design-partner beta has a written, numbered restore procedure
  (RUNBOOK) that has been drilled at least once before the beta starts.

## Non-goals (v1)

- Incremental/streaming backup, off-host upload (S3 etc.), encryption at
  rest: the artifact inherits the host's disk protections; off-host copy is
  an operator step in the RUNBOOK, not a control-plane feature.
- Backup of `backtest.db` (rebuildable fetch cache + re-runnable records) and
  of the agent-plane checkpoint DB (disposable by design; see §Restore
  interplay). Neither file is opened by the control-plane server.
- Point-in-time recovery between snapshots. Snapshot cadence bounds data
  loss; the reconciler re-derives live-OMS truth from the venue on restart.

## Why not VACUUM INTO / naive copy (NORMATIVE rationale)

**OB-1.** The backup artifact MUST be a byte-copy of the main DB file taken
while no writer can run and while the WAL is fully checkpointed. `VACUUM`
and `VACUUM INTO` are FORBIDDEN on `control.db` AND on any copy of it:
billing `watermark_rowid` windows reference `model_costs` implicit rowids
(billing-and-metering.md §Billing), and the store orders several append-only
feeds by implicit `rowid`; both are renumbered by VACUUM, silently corrupting
the artifact. A file copy taken while a writer may commit — or while WAL
frames are not checkpointed — can produce a torn or stale artifact; OB-2
removes both hazards structurally.

## Backup algorithm (NORMATIVE)

**OB-2.** The backup engine runs INSIDE the control-plane process against the
store's `*sql.DB` (which has `SetMaxOpenConns(1)`), in this exact order:

1. Check out THE single pooled connection (`db.Conn(ctx)`, ctx = the serve
   context so shutdown cancels a pending checkout). Holding it excludes
   every other query in the process for the duration of steps 2–4: with a
   pool of one, there is no second connection to write through, and no
   other process opens `control.db` (plane-boundary rule).
2. On that connection run `PRAGMA wal_checkpoint(TRUNCATE)` **via a row
   query** (`QueryRowContext` — `Exec` discards the result row) and REQUIRE
   the returned triple to be exactly `(busy, log, checkpointed) = (0, 0, 0)`:
   after a successful TRUNCATE the WAL is zero bytes and both frame counts
   are measured post-truncation, so success IS the zero triple. `(-1, -1)`
   in log/checkpointed means the file is not in WAL mode — FAIL (the store
   DSN pins WAL; anything else is a mis-wired path). `busy != 0` FAILS on
   the single attempt — no in-place retry loop (the retry is the operator's
   next POST; with the pool-of-one hold there are no in-process readers to
   block TRUNCATE, so busy signals a foreign anomaly worth surfacing).
3. On the same connection collect the source fingerprint: per-table row
   counts for every user table — pinned predicate
   `type = 'table' AND name NOT LIKE 'sqlite_%'` in `sqlite_master`,
   ordered by name.
4. Copy the main DB file (the RAW configured path, not the URL-escaped DSN)
   byte-for-byte to `<dir>/<name>.tmp` computing SHA-256 while copying,
   `fsync` the file, then release the connection.
5. Rename `<name>.tmp` → `<name>` and `fsync` the directory.
6. Verify the ARTIFACT (not the source) per OB-5. On verification failure
   rename the artifact to `<name>.failed` (kept for forensics) and report
   failure.

**OB-2a.** Steps 2–4 stall every API request behind the held connection by
design (they queue on the pool; the server sets only `ReadHeaderTimeout`,
so queued in-flight requests are never killed server-side). The copy is
O(file size) — sub-second for a beta-scale DB on local disk. The engine
MUST NOT hold the connection during step 6, and MUST log a warning when
the total hold exceeds 5 seconds (DB growth or slow disks must surface
long before the hold interacts with agent client timeouts (~10 s/attempt)
or the 90 s watchdog ladder).

**OB-2b.** `.tmp` lifecycle: created `O_CREATE|O_EXCL` mode `0600` (a
pre-existing `.tmp` of the same name fails the run, never silently
clobbered); the engine MUST unlink its own `.tmp` on EVERY failure path
(including request-context cancellation mid-copy); and at the start of
each run (inside the OB-6a mutex) the engine MAY delete orphaned
`control-*.db.tmp` files left by a crash. Retention (OB-9) still never
touches `.tmp`/`.failed` — only this in-mutex cleanup may.

**OB-3.** The backup performs ZERO logical writes to the source DB: no rows,
no schema change, no pragma that changes logical content. The step-2
checkpoint DOES rewrite physical bytes (WAL frames transfer into the main
file; the WAL truncates) — that is a representation change of already-
committed content, explicitly permitted. Rationale: the artifact taken at
time T must BE the logical state at T, and a backup that fails partway must
leave the source logically identical. The trigger evidence is the server
log line (principal, artifact name, sha256, bytes, duration) and the HTTP
response — deliberately NOT a DB row.

**OB-4.** Artifact naming: `control-<YYYYMMDD>T<HHMMSS>Z.db` (UTC, second
precision; fixed-width, so LEXICOGRAPHIC order == chronological order — the
normative ordering key everywhere below). If the target name already exists
(two backups within the same second) the request fails with
`BACKUP_EXISTS`; the operator retries. Only files matching this exact
pattern are ever considered by retention (OB-9) or the list endpoint
(OB-7).

## Verification (NORMATIVE)

**OB-5.** Artifact verification opens the artifact with
`file:<path>?mode=ro&immutable=1` (no schema apply, no migrations).
`immutable=1` is REQUIRED, not an optimization: the artifact's header still
says WAL, and a plain read-only open of a WAL database creates `-shm`/`-wal`
sidecars in the backup directory (or fails outright when the directory is
not writable — exactly the read-only off-host archive case). Immutable mode
creates nothing and locks nothing; it is sound here by construction — the
engine mutex is held, the rename is complete, and nothing else writes the
directory. Consequently the verifier MUST NEVER be pointed at a live,
attached DB (immutable disables change detection). Verification REQUIRES
all of:

1. `PRAGMA integrity_check` returns exactly one row `ok`.
2. `PRAGMA foreign_key_check` returns zero rows.
3. Per-table row counts equal the source fingerprint from OB-2 step 3
   (same table set under the same pinned predicate, same counts — proves
   the copy is the checkpointed snapshot, not a torn or stale file).

**OB-5a.** The SHA-256 recorded in the response is computed from the bytes
written during the copy AND re-read from the renamed artifact; both digests
MUST match, and the re-read MUST complete before the SQLite verify handle
opens (catches post-rename filesystem corruption; with `immutable=1` the
verify open itself cannot write).

## HTTP surface (NORMATIVE)

**OB-6.** `POST /api/v1/ops/backups/run` — env-admin class ONLY (deployer
act, like the billing POSTs and `oms/recon/run`, whose verb-under-prefix
naming this follows; no DB role — tenants never see platform backups).
Registered IFF the backup engine is configured (OB-8), via a `Requires`
wiring flag like `requiresLiveOMS`; unconfigured deployments 404. The
surface is MODE-INDEPENDENT: paper deployments MUST be able to configure
and run backups (the pre-beta soak drill depends on it). Empty request
body. Success `200`:

```json
{
  "artifact": "control-20260705T220000Z.db",
  "bytes": 12345678,
  "sha256": "<64 hex>",
  "tables": 25,
  "rows_total": 98765,
  "started_at": "2026-07-05T22:00:00Z",
  "finished_at": "2026-07-05T22:00:01Z",
  "verified": true
}
```

Failures: `409 BACKUP_IN_PROGRESS` (OB-6a), `409 BACKUP_EXISTS` (OB-4) —
the 409s follow the `RECON_RUNNING`/`TENANT_EXISTS` precedent —
`500 BACKUP_FAILED` (checkpoint/copy/fs error), `500 BACKUP_VERIFY_FAILED`
(OB-5 failed; artifact renamed `.failed`). The two 500s deliberately
BYPASS the uniform `INTERNAL` envelope and use the standard error shape
with these specific codes; their `message` MAY name the artifact basename,
NEVER a filesystem path. v1 has NO web surface by design (the ops panel is
tenant-visible; backups are platform ops).

**OB-6a.** At most one backup runs at a time (engine-level mutex, which
also covers retention and `.tmp` cleanup). A POST arriving while one runs
returns `409 BACKUP_IN_PROGRESS` — it MUST NOT queue (the operator would
stack stalls, OB-2a).

**OB-7.** `GET /api/v1/ops/backups` — env-admin ONLY, same `Requires`
flag. Lists OB-4-matching files in the backup directory:
`{"items": [{"artifact": ..., "bytes": ..., "modified_at": ...}]}`,
newest first BY NAME (OB-4 ordering key). Read-only; charges no rate
bucket (the auth guard charges non-GET requests only).

## Configuration (NORMATIVE)

**OB-8.** Environment:

- `CONTROLPLANE_BACKUP_DIR` — enables the whole surface. MUST exist and be
  a writable directory at startup (fail-fast with a clear error otherwise).
  It MAY share a disk with the live DB (same-disk snapshots still cover the
  dominant failure mode: software corruption / operator error) and SHOULD
  be local disk (network filesystems weaken the fsync durability
  assumptions); the RUNBOOK directs a periodic off-host copy for disk-loss
  coverage.
- `CONTROLPLANE_BACKUP_RETAIN` — optional int ≥ 1 enabling retention
  (OB-9). Unset/0 = keep everything.
- `CONTROLPLANE_BACKUP_INTERVAL_HOURS` — optional int ≥ 1 enabling the
  periodic loop (OB-10). Unset/0 = manual only.

**OB-8a.** All three variables are read in `cmd/controlplane` wiring only
(fail-fast parse, `parseBreakerIntervals` convention); the api/store
packages receive plain values (repo config seam convention).

## Retention (NORMATIVE)

**OB-9.** After every SUCCESSFUL backup — inside the OB-6a mutex, before
release — delete the oldest OB-4-matching files beyond the newest N,
ordered BY NAME (OB-4: lexicographic == chronological; mtime is perturbed
by off-host copy-back and MUST NOT be the key). Retention MUST only ever
delete OB-4-matching names (never `.tmp`, never `.failed`, never foreign
files — the OB-2b in-mutex `.tmp` cleanup is the only other deleter), and
MUST NOT run after a failed backup.

## Periodic loop (NORMATIVE)

**OB-10.** Start-anchored periodic backup loop inside the server (first
run one interval after boot, not at boot — boot is already a
fresh-verified state via migrations), cancelled by the serve context on
shutdown. Failures are logged loudly (`BACKUP FAILED` prefix) and the loop
continues at cadence; a periodic run that finds another backup in progress
skips (never queues, OB-6a).

## Verifier CLI (NORMATIVE)

**OB-11.** `cmd/backupverify` — offline artifact/restore verifier:
`backupverify -db <path>`. Opens the file with
`file:<path>?mode=ro&immutable=1` (no schema apply — the tool creates no
sidecars and takes no locks, so it works on read-only media; the same
immutable caveat as OB-5 applies: NEVER point it at a live, attached DB —
not because it could write, but because immutable mode would misread a
file that changes underneath it). It prints a report:

1. `PRAGMA integrity_check` result (MUST be `ok`).
2. `PRAGMA foreign_key_check` violation count (MUST be 0).
3. `PRAGMA journal_mode` (informational; artifact is checkpointed so any
   value opens cleanly under the store's WAL DSN later).
4. Per-table row counts (every user table under the OB-2 step 3 pinned
   predicate, ordered by name).
5. Newest operational timestamp per table — `runs`/`orders` have no
   `recorded_at`, so the columns are per-table: `runs.created_at`,
   `model_costs.recorded_at`, `safety_alerts.recorded_at`,
   `orders.submitted_at` (a missing table is skipped, never an error) —
   the operator's data-loss bound for a restore.

Exit code 0 iff checks 1–2 pass. The RUNBOOK forbids substituting a system
`sqlite3` binary for this tool in the normative restore check (the tool
shares the exact driver version with the server via one `go.mod`).

## Restore (NORMATIVE procedure, drilled in the RUNBOOK)

**OB-12.** Restore is OFFLINE by design (single-node, single file):

1. Stop the control-plane process (and the agent-plane scheduler).
2. Move aside the live `control.db`, `control.db-wal`, `control.db-shm`
   (KEEP them — they are the forensic record of the incident AND the
   input to the safety diff in step 7).
3. Copy the chosen artifact to the `CONTROLPLANE_DB` path. Do NOT copy any
   `-wal`/`-shm` alongside: the artifact is fully checkpointed (OB-2) and
   standalone; stale sidecar files from the old lineage MUST NOT be present.
4. Run `backupverify -db <path>` and require exit 0.
5. Start the control-plane. `store.Open` re-applies the idempotent schema
   and the additive migrations (verified idempotent —
   `TestOpenAppliesSchemaIdempotently` is the restore-boot guarantee);
   this is the ONLY writer that touches the restored file first.
6. Confirm: `GET /health` 200, `GET .../safety` renders, and in live mode
   the mandatory startup reconcile completes.
7. **Safety diff (MANDATORY, BEFORE the scheduler restarts):** restore
   erases every kill, clear, breaker row, and lifecycle transition issued
   AFTER the snapshot — and a lost kill is FAIL-OPEN (nothing re-arms it;
   effects are driven from DB rows, and the restored DB has no row).
   `GET .../safety` on the restored DB shows a CLEAN strategy and cannot
   reveal this. The operator MUST diff the moved-aside DB's
   `kill_breaker_events` / `kill_clear_events` / `lifecycle_transitions` /
   `safety_alerts` (via `backupverify` counts + newest timestamps, or
   read-only SQL on the moved-aside copy) against the restored DB, and
   RE-ISSUE every post-snapshot kill/clear/lifecycle action through the
   normal endpoints. The opposite direction is fail-safe and acceptable:
   a kill CLEARED after the snapshot comes back ACTIVE and merely needs
   re-clearing.
8. Rewind each strategy's agent tick-state file (`next_tick_number`) to
   the restored DB's `MAX(tick_number)+1` for that strategy, THEN restart
   the scheduler (OB-12a).

**OB-12a. Restore interplay (informative rationale; the procedures live
in OB-12 and the RUNBOOK):**

- Agent-plane state AHEAD of a restored `control.db` does not wedge
  anything, but the recovery is the OB-12 step 8 rewind, not magic:
  proposal ingest is idempotent and runs are (strategy_id,
  tick_number)-unique, so REPLAYED ticks recreate their rows from the
  still-present LangGraph checkpoints (original ids, ~zero LLM cost).
  Without the rewind, completed ticks between snapshot and incident stay
  permanent gaps in `runs` (the scheduler only ever re-POSTs its single
  in-flight tick). Trace re-drives of replayed ticks may 409
  (append-only trace conflict) — expected recovery noise, pre-declared
  in the RUNBOOK.
- The two agent files are distinct: losing the LangGraph checkpoint DB
  alone costs one full-price LLM tick; losing the TICK-STATE file with a
  populated `control.db` restarts ticks at 0 and produces a
  RUN_TICK_CONFLICT crawl (one 409 per interval until it catches up) —
  recovery is the same rewind of `next_tick_number`.
- Live mode, precisely (live-oms-and-reconciler.md R-rules): fills newer
  than the snapshot are backfilled by trade id (R5, from the restored
  watermark); venue orders whose intents are missing from the restored
  journal are NOT adopted — ENTRY-shaped orphans are CANCELED at the
  venue (R3), protective-shaped orphans are left open with an operator
  alert; positions are re-derived, never auto-zeroed; restored intents
  whose orders are absent at the venue are terminalized (R2/R4). Money
  truth is NEVER restored from a snapshot; only platform records are.

## Security (NORMATIVE)

**OB-13.** The artifact contains token HASHES only (plaintext tokens are
never stored — multi-tenant-rbac.md invariant) and NO venue API keys
(env-only). It DOES contain tenant-confidential business content: full
agent traces (LLM debate transcripts — proprietary strategy reasoning),
proposal payloads, billing amounts, and the tenant roster — the artifact
is tenant-confidential, not merely credential-free. Its secrecy class
equals the live DB: artifacts (and their `.tmp`, OB-2b) are written
`0600`, the backup dir SHOULD be `0700`, and the RUNBOOK's off-host copy
step preserves permissions. Backup/list responses never echo filesystem
paths other than the artifact basename.

## Invariants

1. No writer can commit between the checkpoint (OB-2 step 2) and the end
   of the file copy (step 4) — the pool's only connection is held.
2. The artifact is bit-identical to the source main file at copy time
   (OB-5a double-digest) and passes integrity + FK + count parity (OB-5).
3. The source DB is logically unaffected by backup, success or failure
   (OB-3; the step-2 checkpoint may rewrite physical representation).
4. No code path ever VACUUMs `control.db` or an artifact (OB-1).
5. Retention deletes only OB-4-matching artifacts, only after success,
   only inside the engine mutex (OB-9).
6. The backup surface is invisible (404) unless explicitly configured.
7. Verification and the verifier CLI never write to the file they check
   nor create sidecars beside it (`immutable=1`, OB-5/OB-11).
8. A restore is not complete until the OB-12 step 7 safety diff has been
   executed — a lost post-snapshot kill is fail-open until re-issued.

## Wiring seams (informative)

- `internal/store`: `(*Store).Backup(ctx, dir, retain)` implementing
  OB-2..OB-5a + OB-9 (engine + mutex + retention + `.tmp` cleanup),
  `(*Store).path` recording the RAW configured path at Open (the DSN is
  URL-escaped; the copy must not be). Fingerprint/count helpers live
  beside it; no api → store import cycle.
- `internal/api`: two `RoutePermission` rows with `Requires:
  requiresBackup`; handlers call a `BackupEngine` interface declared in
  api (seam pattern identical to `ReconStatusProvider`). The RBAC test
  env MUST wire a fake `BackupEngine` so both rows register and the
  `DeepEqual(routes, Permissions())` pin holds (the live-oms fake
  provider precedent); the `TestRBACMatrixPins` doc-comment on the
  env-admin read surface is amended for `GET /ops/backups`.
- `cmd/controlplane`: reads OB-8 vars, validates the dir, wires the
  engine + optional periodic loop (start-anchored, serve-ctx-cancelled).
- `cmd/backupverify`: standalone main, read-only immutable SQL only
  (keeps the artifact-mutation surface nil).

## Runbook obligations (NORMATIVE for docs/RUNBOOK.md)

**OB-14.** `docs/RUNBOOK.md` is the operator-facing companion (procedures,
not a spec — spec IDs are cited, never re-stated as new rules). It MUST
cover, each as a numbered step-by-step procedure with the exact commands
or endpoints:

1. Deployment: the three processes (control-plane serve, agent-plane
   scheduler, web), their full environment variable inventories with
   secret/non-secret marking, start order, and graceful stop.
2. Backup: triggering (endpoint + periodic loop), reading the response,
   off-host copy, retention behavior.
3. Restore: the OB-12 procedure verbatim — including the step 7 safety
   diff and the step 8 tick-state rewind — plus the OB-12a rationale and
   the pre-declared 409 trace-conflict recovery noise.
4. Kill / clear: strategy, tenant, platform tiers — including the
   `observed_epoch` CAS rule and the platform-clear ack literal.
5. Breaker fired / watchdog escalation: what happened automatically,
   what the operator checks (`GET .../safety`, alerts feed), what they
   may do (resume via lifecycle after review).
6. Venue-reset acknowledgment (`accept_venue_reset`) and when NOT to use
   it.
7. Tenant onboarding end-to-end (create tenant → rotate owner token →
   mint agent token → deploy scheduler env) and token rotation/revoke.
8. Live-prod enablement: the exact `CONTROLPLANE_LIVE_PROD_ACK` gate and
   the testnet-first policy.

The RUNBOOK MUST NOT contain any secret value, token, or key — only
variable NAMES and placeholder syntax.

## Test obligations

- Store: backup of a live store with concurrent writer goroutines (writes
  queue, artifact passes OB-5), count-parity failure injection, `.failed`
  rename on verify failure, retention matrix (matching/non-matching
  names, N boundary, no-delete-on-failure, name-order not mtime-order),
  `.tmp` lifecycle (O_EXCL collision fails, unlink on failure path,
  orphan cleanup), checkpoint-failure path (non-WAL / busy ⇒
  BACKUP_FAILED, single attempt), zero-logical-writes check (sha256 of
  the source main file before/after a backup whose WAL was already
  checkpointed — bit-identical in that state).
- API: RBAC matrix rows registered via a fake `BackupEngine` in the RBAC
  env and covered by `TestRBACMatrix`; 404 when unconfigured; 409
  in-progress; response shape; specific 500 codes (not INTERNAL); no
  rate charge on GET.
- CLI: verifier exit codes on a good artifact, a corrupted artifact
  (bit-flip), and an FK-violating file; read-only proof (file bytes
  unchanged AND no sidecar files created after verify).
- Drill (pre-beta, RUNBOOK): live backup on the running soak deployment,
  then a restore drill into a scratch dir with `backupverify` + server
  boot + `/health` + safety GET against the restored copy.

