# AlphaMintX Operator Runbook

Operator procedures, per `docs/specs/ops-backup.md` OB-14. This document is
a companion, not a spec: every rule cited here (OB-*, LC-*, WD-*, OS-*,
SW-*, AN-*) is normative in its own spec under `docs/specs/`; on any
conflict the spec wins. It contains NO secret values — only variable names and
placeholder syntax (`$CONTROLPLANE_ADMIN_TOKEN`, `<strategy-id>`, ...).

Conventions used below:

- `$CP` — the control-plane base URL, e.g. `http://localhost:8080`
  (`CONTROLPLANE_ADDR`, default `:8080`).
- `curl` examples authenticate with `-H "authorization: Bearer $TOKEN"`;
  substitute the token class each step names. Tokens are secrets: export
  them from your secret store, never paste values into shells with history
  or into this file.
- All responses are JSON; error bodies are `{code, message}`.

## 1. Deployment (three processes)

### 1.1 Environment inventories

Control-plane (unit `alphamintx-controlplane.service`, env file
`/etc/alphamintx/controlplane.env` — §10; dev mode
`go run ./cmd/controlplane` from `control-plane/`):

| Variable | Required | Secret | Meaning |
|---|---|---|---|
| `CONTROLPLANE_DB` | yes (serve mode) | no | Path to `control.db`. Unset = Phase-0 demo loop, no server. |
| `CONTROLPLANE_ADDR` | no | no | Listen address; default `:8080`. |
| `CONTROLPLANE_MARK_MAX_AGE_SECONDS` | yes (serve mode) | no | Mark staleness bound (market-data.md §Staleness); no default, startup fails without it. |
| `CONTROLPLANE_READ_TOKEN` | recommended | yes | Env read class — GETs only, never authorizes a POST. |
| `CONTROLPLANE_OPERATOR_TOKEN` | recommended | yes | Env operator class — `POST .../approvals` only. |
| `CONTROLPLANE_OPERATOR_PRINCIPAL` | no | no | `approvals.decided_by` attribution; default `operator`. |
| `CONTROLPLANE_ADMIN_TOKEN` | recommended | yes | Env-admin class (deployer acts: tenants, tokens, limits, platform kill/clear, billing POSTs, recon run, backups). |
| `CONTROLPLANE_AGENT_TOKENS` | no | yes | Legacy env agent tokens, `strategy_id=token,...` (DB agent tokens via §7 are preferred). |
| `CONTROLPLANE_RISK_LIMITS` | for ingestion | no | RiskLimits v1 JSON (risk-limits.md); unset = proposal ingestion disabled. |
| `CONTROLPLANE_FILL_MODEL` | paper OMS only | no | Paper fill-model JSON; mutually exclusive with `CONTROLPLANE_OMS_MODE=live`. |
| `CONTROLPLANE_SYMBOLS` | live mode | no | Comma list of canonical `BASE/QUOTE` symbols (market-data feed; required in live mode). |
| `CONTROLPLANE_BINANCE_MARKET` | no | no | `spot` (default) or `futures`. |
| `CONTROLPLANE_OMS_MODE` | no | no | `paper` (default) or `live` (live-oms-and-reconciler.md §Config). |
| `CONTROLPLANE_BINANCE_ENV` | live mode | no | `testnet` (default) or `prod` — see §8. |
| `CONTROLPLANE_BINANCE_API_KEY` | live mode | yes | Venue API key (env-only; never stored in the DB). |
| `CONTROLPLANE_BINANCE_API_SECRET` | live mode | yes | Venue API secret (env-only). |
| `CONTROLPLANE_LIVE_PROD_ACK` | prod only | no | Exact ack literal — see §8. |
| `CONTROLPLANE_LIVE_OMS_TUNING` | no | no | Optional live-OMS tuning JSON. |
| `CONTROLPLANE_BINANCE_REST_URL` / `_WS_URL` | no | no | Endpoint overrides; testnet overrides are refused in prod (§8). |
| `CONTROLPLANE_BREAKER_INTERVAL_ACTIVE` / `_IDLE` | no | no | Monitor cadence seconds, live mode only; bounds [1,10] / [ACTIVE,600]. |
| `CONTROLPLANE_WATCHDOG_DISABLED` | no | no | `1`/`true` disables watchdog EVALUATION (watchdog.md §Config); logs loudly; testnet drills only. |
| `CONTROLPLANE_BACKUP_DIR` | for backups | no | Enables the backup surface (ops-backup.md OB-8); must exist, be writable, SHOULD be `0700` local disk. |
| `CONTROLPLANE_BACKUP_RETAIN` | no | no | Keep newest N artifacts (OB-9); unset/0 = keep everything. |
| `CONTROLPLANE_BACKUP_INTERVAL_HOURS` | no | no | Periodic backup loop (OB-10); unset/0 = manual only. |
| `CONTROLPLANE_ALERT_WEBHOOK` | no | yes (the JSON may embed a secret URL and bearer) | Alert-notifier config JSON (alert-notifier.md AN-10); unset = notifier disabled — see §9. |
| `CONTROLPLANE_MAX_STRATEGIES_PER_TENANT` | no | no | Per-tenant cap on `POST /api/v1/strategies` (strategy-provisioning.md SP-4b); unset = 100; must be ≥ 1. |

Agent-plane scheduler (unit `alphamintx-scheduler@<strategy-id>.service`,
env file `/etc/alphamintx/scheduler-<strategy-id>.env`, one instance per
strategy — §10; dev mode `python -m alphamintx_agent_plane.scheduler` from
`agent-plane/`):

| Variable | Required | Secret | Meaning |
|---|---|---|---|
| `ALPHAMINTX_STRATEGY_ID` | yes | no | The strategy this scheduler drives. |
| `ALPHAMINTX_SYMBOL` | yes | no | Canonical `BASE/QUOTE` symbol. |
| `ALPHAMINTX_STRATEGY_TOKEN` | yes | yes | Agent bearer token (env entry or DB agent token, §7). |
| `ALPHAMINTX_CONTROLPLANE_BASE_URL` | yes | no | Control-plane origin for proposals/traces/heartbeats. |
| `ALPHAMINTX_CONTROLPLANE_TIMEOUT_SECONDS` | no | no | Per-attempt HTTP timeout; default 10. |
| `ALPHAMINTX_CHECKPOINT_DB` | yes | no | LangGraph checkpoint SQLite path (disposable; losing it costs one full-price LLM tick). |
| `ALPHAMINTX_SCHEDULER_STATE` | yes | no | Tick-state JSON path (`next_tick_number`); an exclusive `.lock` beside it enforces one scheduler per file. |
| `ALPHAMINTX_TICK_INTERVAL_SECONDS` | no | no | Tick cadence; default 60; must be > 0. |
| `ALPHAMINTX_HEARTBEAT_INTERVAL_SECONDS` | no | no | Heartbeat cadence (WD-25); default 30; bounds (0, 45]. |
| `ALPHAMINTX_BINANCE_BASE_URL` | no | no | Market-data-only endpoint override; default `https://api.binance.com`. |
| `ALPHAMINTX_LLM_MODE` | no | no | `stub` (default, no network) or `live`. |
| `MINTROUTER_BASE_URL` | LLM live mode | no | LLM router origin. |
| `MINTROUTER_API_KEY` | LLM live mode | yes | LLM router credential; read from env only, never logged. |
| `MINTROUTER_TIMEOUT_SECONDS` | no | no | LLM HTTP timeout override. |

Web dashboard (unit `alphamintx-web.service`, env file
`/etc/alphamintx/web.env`, runtime vars only — §10; dev mode
`pnpm build && pnpm start` from `web/`):

| Variable | Required | Secret | Meaning |
|---|---|---|---|
| `CONTROLPLANE_API_BASE_URL` | yes | no | Control-plane origin as seen from the Next SERVER; baked into rewrites at `next build` time. |
| `NEXT_PUBLIC_API_BASE_URL` | no | no | Cross-origin escape hatch for the browser client; empty = same origin. |
| `NEXT_PUBLIC_READ_TOKEN` | yes | yes | READ token, INLINED into the public JS bundle at build time (GETs only by design — persistence-and-api.md §Auth); use a per-tenant viewer DB token for tenant-facing deployments. |
| `OPERATOR_TOKEN` | for ops controls | yes | Server-only; attached by the approvals/lifecycle/kill/clear proxy routes; never `NEXT_PUBLIC_`. |

Host requirements (deploy-and-survive.md DS-13):

- **NTP in slew mode** (chrony's default, or ntpd `-x`). A wall-clock step
  BACKWARD is fatal to the breaker latch and the watchdog timers; never
  run step-mode sync on a live host.
- **30-day disk projection.** Provision for the sum of: soak-measured
  `control.db` growth/day × 30; artifact size ×
  `CONTROLPLANE_BACKUP_RETAIN`; checkpoint-DB growth/day × 30 × strategy
  count; the journald cap (`SystemMaxUse` in `journald.conf` — set it
  explicitly; the control-plane unit disables journald rate limiting,
  §9.3).
- **Reverse-proxy TLS for any non-loopback exposure** — bearer tokens
  NEVER travel cleartext off-host (same rule as AN-10). nginx sketch:
  `server { listen 443 ssl; ssl_certificate ...; ssl_certificate_key ...;`
  `location / { proxy_pass http://127.0.0.1:3000; } }` (web; same pattern
  for `$CP` if it must be reachable off-host — otherwise keep it
  loopback-bound).
- **Ops-panel token class.** The panel's mutating buttons proxy
  approvals/lifecycle/kill/clear with `OPERATOR_TOKEN`. An ENV
  operator-class token authorizes ONLY `POST .../approvals` — the
  kill/clear/lifecycle buttons 403 with it. For full panel function set
  `OPERATOR_TOKEN` to a DB token with role trader or above (§7).

### 1.2 Start order

1. Start the control-plane: `systemctl start alphamintx-controlplane`.
   Confirm `systemctl status alphamintx-controlplane` is
   `active (running)` and `curl -sS $CP/health` returns
   `{"status":"ok"}`. In live mode the mandatory startup reconcile runs
   before any submission is accepted (`RECONCILE_PENDING` until then).
2. Start one scheduler per strategy:
   `systemctl start alphamintx-scheduler@<strategy-id>`. It fails fast on
   any missing variable and refuses to start if another instance holds
   `$ALPHAMINTX_SCHEDULER_STATE.lock`.
3. Start the web server: `systemctl start alphamintx-web`.

Dev mode (local development ONLY — production runs built artifacts under
systemd, deploy-and-survive.md DS-11): control-plane
`CONTROLPLANE_DB=... go run ./cmd/controlplane` from `control-plane/`;
scheduler `python -m alphamintx_agent_plane.scheduler` from `agent-plane/`
(env per §1.1); web `pnpm build && pnpm start` from `web/`
(`CONTROLPLANE_API_BASE_URL` set at BUILD time).

### 1.3 Graceful stop

1. Stop schedulers FIRST: `systemctl stop 'alphamintx-scheduler@*'`
   (SIGTERM; the run task cancels cleanly and the in-flight tick is
   abandoned — its checkpoint survives. SIGKILL after `TimeoutStopSec` is
   checkpoint-safe, DS-11b). Stopping the scheduler before the
   control-plane avoids orphaned ticks.
2. Stop the web server: `systemctl stop alphamintx-web` (any time; it
   holds no state).
3. Stop the control-plane: `systemctl stop alphamintx-controlplane`
   (SIGTERM); it drains in-flight requests with a 5-second shutdown
   timeout.

## 2. Backup (ops-backup.md OB-2..OB-10)

Prerequisite: `CONTROLPLANE_BACKUP_DIR` set at startup (OB-8). Without it
the two endpoints below do not exist (404). Works identically in paper and
live modes (OB-6).

### 2.1 Trigger a backup

1. `curl -sS -X POST "$CP/api/v1/ops/backups/run" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN"`
   (env-admin ONLY; empty body).
2. Read the `200` response (OB-6):
   `{"artifact": "control-<YYYYMMDD>T<HHMMSS>Z.db", "bytes": ..., "sha256": "<64 hex>", "tables": ..., "rows_total": ..., "started_at": ..., "finished_at": ..., "verified": true}`.
   `verified: true` means the artifact passed integrity check, FK check,
   and row-count parity with the source fingerprint (OB-5). Record
   `artifact` and `sha256` in your ops log.
3. Failure handling:
   - `409 BACKUP_IN_PROGRESS` — one at a time (OB-6a); retry after the
     current run.
   - `409 BACKUP_EXISTS` — two backups in the same second (OB-4); retry.
   - `500 BACKUP_FAILED` — checkpoint/copy/fs error; source DB unaffected
     (OB-3); investigate disk/logs, then retry.
   - `500 BACKUP_VERIFY_FAILED` — artifact renamed `<name>.failed` and
     kept for forensics (OB-2 step 6); do NOT use it; retry.
4. Expect API requests to stall for the copy duration (sub-second at beta
   scale); the server logs a warning if the hold exceeds 5 s (OB-2a).

### 2.2 List artifacts

1. `curl -sS "$CP/api/v1/ops/backups" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN"`
   (env-admin ONLY) — `{"items": [{"artifact", "bytes", "modified_at"}]}`,
   newest first by name (lexicographic == chronological, OB-4).

### 2.3 Periodic loop and retention

- With `CONTROLPLANE_BACKUP_INTERVAL_HOURS=N` the server backs up every N
  hours, first run one interval AFTER boot (OB-10). A failed periodic run
  is PUSHED through the alert notifier as a `safety_alerts` row
  `kind=backup_failed`, `details_json.category` one of `verify_failed`,
  `artifact_exists`, `io` (deploy-and-survive.md DS-9; raw error text
  never leaves the host). The `BACKUP FAILED` log line remains and
  carries the full error; the loop continues at cadence.
- With `CONTROLPLANE_BACKUP_RETAIN=N` the oldest artifacts beyond the
  newest N are deleted after each SUCCESSFUL backup (OB-9). `.tmp`,
  `.failed`, and foreign files are never deleted.

### 2.4 Off-host copy

1. Periodically copy the newest artifact off-host (disk-loss coverage —
   OB-8; the control-plane never uploads). Preserve permissions: artifacts
   are `0600` and tenant-confidential (full agent traces, proposals,
   billing amounts — OB-13), e.g.
   `rsync -p "$CONTROLPLANE_BACKUP_DIR/control-<stamp>.db" <off-host-destination>`.
2. Verify the copy landed intact: run `backupverify -db <copied-path>`
   (§3 step 4 — works on read-only media) and compare its report against
   the recorded `sha256`/counts.

## 3. Restore (ops-backup.md OB-12; restore gate per deploy-and-survive.md DS-14)

Restore is OFFLINE. A restore is NOT complete until step 7's safety diff
has been executed — a post-snapshot kill lost by the restore is FAIL-OPEN
until re-issued (ops-backup.md invariant 8).

1. Stop everything in §1.3 order (schedulers first):
   `systemctl stop 'alphamintx-scheduler@*'`, then
   `systemctl stop alphamintx-web`, then
   `systemctl stop alphamintx-controlplane`.
2. Move aside the live files — KEEP them (forensic record and step-7
   input):
   `mv control.db control.db.incident && mv control.db-wal control.db-wal.incident && mv control.db-shm control.db-shm.incident`
   (the `-wal`/`-shm` moves apply only if the files exist).
3. Copy the chosen artifact to the `CONTROLPLANE_DB` path:
   `cp "$CONTROLPLANE_BACKUP_DIR/control-<stamp>.db" "$CONTROLPLANE_DB"`.
   Do NOT copy any `-wal`/`-shm` alongside — the artifact is fully
   checkpointed and standalone (OB-2).
4. Verify: `/opt/alphamintx/bin/backupverify -db "$CONTROLPLANE_DB"` and
   REQUIRE exit code 0. Do NOT substitute a system `sqlite3` binary
   (OB-11 — the tool shares the server's exact driver version). Never
   point it at a live, attached DB.
5. Start the control-plane: `systemctl start alphamintx-controlplane`
   (§1.2 step 1). `store.Open` re-applies the idempotent schema and
   migrations; it must be the first writer.
6. Confirm: `curl -sS $CP/health` is 200;
   `GET $CP/api/v1/strategies/<id>/safety` renders for each strategy; in
   live mode the mandatory startup reconcile completes.
7. **Safety diff (MANDATORY, BEFORE any scheduler restarts).** The restore
   erased every kill, clear, breaker row, and lifecycle transition issued
   after the snapshot; `GET .../safety` on the restored DB shows a CLEAN
   strategy and cannot reveal this.
   1. Run `/opt/alphamintx/bin/backupverify -db control.db.incident` (the
      moved-aside copy is no longer attached) and note the row counts and
      newest timestamps of `kill_breaker_events`, `kill_clear_events`,
      `lifecycle_transitions`, `safety_alerts`; alternatively inspect the
      moved-aside copy with read-only SQL.
   2. Compare against the restored DB
      (`/opt/alphamintx/bin/backupverify -db "$CONTROLPLANE_DB"`).
   3. RE-ISSUE every post-snapshot kill, clear, and lifecycle action
      through the normal endpoints (§4, `POST .../lifecycle`). A kill
      CLEARED after the snapshot coming back ACTIVE is fail-safe: just
      re-clear it (§4.2).
8. **Acknowledge the restore gate (env-admin) — AFTER the step-7 safety
   diff, BEFORE any scheduler restart.** Booting a stamped artifact
   ENGAGED the gate by construction (deploy-and-survive.md DS-2): a
   `restore_gate_engaged` alert was pushed at boot, and proposals and
   approvals return `503 RESTORE_GATE` until you ack:
   `curl -sS -X POST "$CP/api/v1/ops/restore/ack" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN"`
   (empty body) → `200 {"cleared": true}`. Status any time:
   `GET $CP/api/v1/ops/restore` (read or env-admin). Ack BEFORE any
   scheduler restart — a scheduler started under the gate burns real LLM
   spend on ticks whose proposals are 503'd with nothing persisted and
   leaves permanent run gaps (DS-14).
   - An ack with no gate engaged (double ack, wrong deployment) is
     `409 RESTORE_GATE_NOT_ENGAGED`; nothing is written.
   - Acked by mistake (step 7 incomplete)? The gate cannot be re-armed
     (DS-5): kill at the affected tier (§4.1) and redo the restore.
   - DS-8 edge: a pre-restore `pending_approvals` row that survives to
     post-ack inside its window can then be approved against restored
     state — the approval preflight (kill epoch, mark freshness,
     lifecycle) mitigates; review pending approvals before approving.
   - NEVER restore ad-hoc `cp control.db` copies (deploy-and-survive.md
     D6): only engine artifacts engage the gate, and pre-slice artifacts
     restore UNGATED — take a fresh backup right after deploying the
     deploy-and-survive slice.
9. Rewind each strategy's tick state, THEN restart its scheduler
   (OB-12a):
   1. Read the restored DB's `MAX(tick_number)` for the strategy (newest
      run via `GET $CP/api/v1/strategies/<id>/runs?page=1&limit=1`).
   2. Edit the `ALPHAMINTX_SCHEDULER_STATE` file — shape
      `{"strategies": {"<strategy-id>": {"next_tick_number": <N>}}}` — to
      `MAX(tick_number) + 1`.
   3. Restart the scheduler:
      `systemctl start alphamintx-scheduler@<strategy-id>` (§1.2 step 2).

Pre-declared recovery noise (OB-12a): replayed ticks recreate their rows
idempotently from still-present LangGraph checkpoints (original ids,
~zero LLM cost); their trace re-POSTs may return `409` append-only trace
conflicts — expected, ignore. Skipping the rewind leaves permanent gaps in
`runs`; a tick-state file LOST (rather than rewound) restarts ticks at 0
and produces a `RUN_TICK_CONFLICT` crawl until it catches up — recovery is
the same rewind. In live mode, money truth is NEVER restored from a
snapshot: the reconciler re-derives it from the venue (fills backfilled by
trade id; ENTRY-shaped orphans canceled; protective-shaped orphans left
open with an alert; positions re-derived, never auto-zeroed).

Alert notifier after a restore: `alert_dispatch_state` rolled back with
the DB, so events dispatched after the snapshot are REDELIVERED, with
reused `seq` values (alert-notifier.md AN-13). Receivers dedupe on
`(source, id)`; no action needed.

## 4. Kill / clear (safety-wiring.md §Kill endpoints; lifecycle-api.md LC-25..LC-38)

A kill is a standing, persisted condition; the 200 acknowledges the ROW,
never effect completion. `flatten` is always an explicit choice; the wire
default is false (absent body/field never flattens).

### 4.1 Kill

1. Strategy tier (trader/admin/owner own tenant; env-admin any):
   `curl -sS -X POST "$CP/api/v1/strategies/<strategy-id>/kill" -H "authorization: Bearer $TOKEN" -H "content-type: application/json" -d '{"flatten": false}'`
   → `{event_id, strategy_id, kill_epoch, recorded_at, flatten}`.
2. Tenant tier (admin/owner OWN tenant; env-admin any):
   `curl -sS -X POST "$CP/api/v1/tenants/<tenant-id>/kill" ... -d '{"flatten": false}'`
   → `{event_id, tenant_id, kill_epoch, recorded_at, flatten}`.
3. Platform tier (env-admin ONLY; mandatory case-sensitive ack literal —
   anything else is `400 PLATFORM_KILL_ACK_REQUIRED`, nothing written):
   `curl -sS -X POST "$CP/api/v1/platform/kill" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN" -H "content-type: application/json" -d '{"ack": "KILL-PLATFORM", "flatten": false}'`.
4. Set `"flatten": true` only when you intend to destroy positions at
   market; protective stops remain on the venue either way.

### 4.2 Clear (CAS via `observed_epoch`)

1. READ the standing kill's epoch first: `GET $CP/api/v1/strategies/<id>/safety`
   shows the binding kills and `active_kill` with its `kill_epoch` (OS-7).
   That value is your `observed_epoch` — the CAS token proving you saw the
   CURRENT kill state (LC-27).
2. Clear at the matching tier (strategy/tenant: admin/owner own tenant or
   env-admin; platform: env-admin ONLY):
   - `curl -sS -X POST "$CP/api/v1/strategies/<id>/kill/clear" -H "authorization: Bearer $TOKEN" -H "content-type: application/json" -d '{"reason": "<why>", "observed_epoch": <kill_epoch>}'`
   - `POST $CP/api/v1/tenants/<tenant-id>/kill/clear` — same body.
   - `POST $CP/api/v1/platform/kill/clear` — body additionally requires
     `"ack": "CLEAR-PLATFORM"` (else `400 PLATFORM_CLEAR_ACK_REQUIRED`,
     nothing written — LC-30).
3. Read the 200: `{clear_id, scope, cleared_epoch, recorded_at, superseded_event_ids}` (LC-33).
4. Failure handling:
   - `409 CLEAR_CONFLICT` — a kill landed since your read; the CAS exists
     so a HUMAN re-observes: re-run step 1, review the NEW kill, and only
     then re-attempt. NEVER auto-retry with the fresh epoch (LC-29).
   - `422 NO_ACTIVE_KILL` — nothing standing at this scope.
   - `400 SCHEMA_INVALID` — `reason` and `observed_epoch` are both
     required, strictly decoded.
5. A cleared strategy stays in lifecycle state `killed`: resume is a
   separate reviewed lifecycle transition (§5 step 4).

## 5. Breaker fired / watchdog escalation (safety-wiring.md; watchdog.md)

What happened automatically, before you were paged:

- **Breaker**: the monitor appended a kill row and drove its effects —
  ENTRY orders swept, lifecycle locked to `killed`, protective stops left
  resting. Kill state survives restart (persist-then-execute).
- **Watchdog rung 1** (agent silence > 90 s, WD-17): ENTRY orders
  canceled, ONE `watchdog_silence` alert per UTC day — no kill, no
  flatten, protectives untouched.
- **Watchdog rung 2** (silence > 10 min, OR > 90 s with unprotected
  exposure — WD-19): a strategy-tier kill row with `actor_id="watchdog"`
  and `flatten=false`, plus a `watchdog_kill_escalation` alert whose
  `ref_id` is the kill `event_id`.

Operator procedure:

1. Check the composite safety status:
   `curl -sS "$CP/api/v1/strategies/<id>/safety" -H "authorization: Bearer $CONTROLPLANE_READ_TOKEN"`
   — lifecycle state, binding kills and their clears, `active_kill`,
   breaker-today, watchdog liveness (OS-7).
2. Check the alert feeds: per strategy
   `GET $CP/api/v1/strategies/<id>/alerts?page=1&limit=20`; platform-wide
   (includes NULL-strategy rows) `GET $CP/api/v1/alerts` (read or
   env-admin class). Look for `watchdog_silence`,
   `watchdog_kill_escalation`, breaker kinds, and `details_json` causes.
3. Fix the underlying condition first: for watchdog events, the agent
   went silent — check the scheduler process, its logs, its
   `ALPHAMINTX_CONTROLPLANE_BASE_URL`, and `ALPHAMINTX_STRATEGY_TOKEN`
   validity before considering resumption. Reviving the agent is an
   operator act; nothing auto-restarts (safety-wiring.md invariant 9).
4. Resume ONLY after review: clear the kill (§4.2), then transition
   lifecycle out of `killed` via
   `curl -sS -X POST "$CP/api/v1/strategies/<id>/lifecycle" -H "authorization: Bearer $TOKEN" -H "content-type: application/json" -d '{"to": "<state>", "reason": "<why>"}'`
   (trader+ own tenant or env-admin; LC-2). A `422` (illegal transition,
   paper-gate failure, kill tier still active) is authoritative — never
   work around it.

## 6. Venue-reset acknowledgment (live-oms-and-reconciler.md §Venue epochs)

On detecting a venue reset (a previously-ACKED order returns NotFound,
trade-id discontinuity, or gross balance discontinuity) the live OMS
appends a `venue_reset` alert, enters RECONCILE_PENDING, and REFUSES ALL
sends — including safety flattens — until an operator acknowledges.

1. Confirm the state: `GET $CP/api/v1/oms/recon/status` (any reader;
   env-class fields include full counters and the venue epoch).
2. Independently confirm a REAL venue-side reset explains the divergence:
   a testnet data reset, an account migration, a venue-announced event.
   Check the `venue_reset` alert's details in the feeds (§5 step 2).
3. Acknowledge (env-admin ONLY):
   `curl -sS -X POST "$CP/api/v1/oms/recon/run" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN" -H "content-type: application/json" -d '{"accept_venue_reset": true}'`
   — this inserts the next `venue_epochs` row and runs a startup-grade
   reconcile against the new venue world. `409 RECON_RUNNING` means a run
   is in progress; retry after it completes.
4. **When NOT to use it**: if you cannot explain the divergence with a
   confirmed venue-side event — suspected key compromise, unexplained
   missing orders, or mid-incident confusion — do NOT acknowledge. The
   epoch bump re-namespaces fill dedup and the trade-id watermark and
   accepts the venue's current state as the new truth; acknowledging a
   non-reset buries evidence of a real problem. Sends staying refused is
   the safe state. A plain `POST /api/v1/oms/recon/run` (no body) during
   RECONCILE_PENDING-due-to-reset merely re-detects and re-reports —
   harmless, useful for re-checking.

## 7. Tenant onboarding and token rotation (multi-tenant-rbac.md)

Token plaintexts (`amx_` + 64 hex) are returned EXACTLY ONCE at mint; no
endpoint ever returns a plaintext or hash again. Store them immediately in
your secret manager.

1. Create the tenant (env-admin ONLY):
   `curl -sS -X POST "$CP/api/v1/tenants" -H "authorization: Bearer $CONTROLPLANE_ADMIN_TOKEN" -H "content-type: application/json" -d '{"tenant_id": "<tenant-id>", "name": "<display name>"}'`
   — `tenant_id` must match `^[a-z0-9][a-z0-9_-]{0,31}$` (`default`
   reserved; else `400 INVALID_TENANT_ID`); an existing id is
   `409 TENANT_EXISTS`. The 200 embeds `owner_token` with the tenant's
   first owner token — `owner_token.token` is the plaintext, label
   `initial-owner`. Hand it to the tenant over a secure channel.
2. Rotate the initial owner token (SHOULD be the tenant's first act — the
   platform operator saw that plaintext). As the tenant owner:
   1. Mint a replacement:
      `curl -sS -X POST "$CP/api/v1/tokens" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"principal": "user", "role": "owner", "label": "owner-rotated"}'`
      — the 200 carries the new plaintext once.
   2. Find the old token's id: `GET $CP/api/v1/tokens` (metadata only;
      the row labeled `initial-owner`).
   3. Revoke it: `POST $CP/api/v1/tokens/<token_id>/revoke` (idempotent).
3. Create the strategy (admin/owner own tenant, or env-admin with
   `"tenant_id"` in the body — strategy-provisioning.md, zero manual DB
   edits):
   `curl -sS -X POST "$CP/api/v1/strategies" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"name": "<display name>", "lifecycle_state": "paper"}'`
   — `lifecycle_state` is `draft` (default) or `paper` ONLY; live tiers
   go through the lifecycle endpoint and its paper gate. The 200 carries
   the server-generated `strategy_id` — record it, every later step keys
   on it. A duplicate name in the tenant is `409 STRATEGY_NAME_TAKEN`
   (safe to treat as "already created" after a timed-out retry); the
   per-tenant cap answers `409 STRATEGY_LIMIT_REACHED`
   (`CONTROLPLANE_MAX_STRATEGIES_PER_TENANT`, default 100).
4. Mint the strategy's agent token (admin/owner own tenant, or
   env-admin with `"tenant_id"` in the body):
   `curl -sS -X POST "$CP/api/v1/tokens" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"principal": "agent", "strategy_id": "<strategy-id>", "label": "<label>"}'`
   — agent tokens carry a `strategy_id` and no role; the strategy must
   exist in the tenant (else `404 UNKNOWN_STRATEGY`).
5. Deploy the scheduler for that strategy with
   `ALPHAMINTX_STRATEGY_TOKEN=<the minted plaintext>` from the secret
   store — never committed, never logged. On a systemd VM (§10):
   write `/etc/alphamintx/scheduler-<strategy_id>.env`, then
   `systemctl enable --now alphamintx-scheduler@<strategy_id>`;
   otherwise §1.1/§1.2.
6. Rotation/revocation rules: rotation is always mint-new-then-revoke-old
   (no in-place rotation). The mint ceiling binds user roles to at or
   below the creator's own; env-admin mints `owner` only as recovery when
   zero unrevoked owner tokens remain. The revoke ceiling mirrors it;
   revocation is immediate and idempotent.

## 8. Live-prod enablement (live-oms-and-reconciler.md §Config)

Policy: TESTNET FIRST. Every strategy runs the testnet drills before prod
is considered; prod enablement is a deliberate three-setting act.

1. Run on testnet: `CONTROLPLANE_OMS_MODE=live`,
   `CONTROLPLANE_BINANCE_ENV=testnet` (the default),
   `CONTROLPLANE_BINANCE_API_KEY`/`_SECRET` set to TESTNET keys, plus the
   live-mode requirements (`CONTROLPLANE_RISK_LIMITS`,
   `CONTROLPLANE_SYMBOLS`). Testnet trading may — and is recommended to —
   use prod market data.
2. Complete the testnet drills (kill switch, breaker, watchdog, backup +
   restore §2/§3) and the paper-gate review before any prod switch.
3. Enable prod by setting, together:
   - `CONTROLPLANE_BINANCE_ENV=prod`
   - `CONTROLPLANE_LIVE_PROD_ACK=I-UNDERSTAND-THIS-TRADES-REAL-FUNDS`
     (the exact literal; anything else refuses to start)
   - prod `CONTROLPLANE_BINANCE_API_KEY` / `CONTROLPLANE_BINANCE_API_SECRET`.
4. Venue pairing is enforced at startup: prod REQUIRES prod market data —
   remove any testnet `CONTROLPLANE_BINANCE_REST_URL`/`_WS_URL` override
   or the process refuses to start.
5. Verify after start: `GET /health` 200, the startup reconcile completes
   (`GET $CP/api/v1/oms/recon/status`), and `GET .../safety` renders for
   every live strategy.

## 9. Alert notifier (alert-notifier.md AN-1..AN-19)

The notifier PUSHES safety events — kills, breaker trips, clears, and
`safety_alerts` rows — to ONE operator-configured webhook (or, in
log-only mode, to marker lines for log forwarding — §9.3). Each event is
one POST of a versioned envelope:
`{"schema": "alphamintx.safety-event.v1", "source", "id", "seq", "delivered_at", "event"}`
(AN-13). Delivery is at-least-once, per-source in-order: your receiver
MUST dedupe on `(source, id)`. NEVER use `seq` as a receiver cursor — it
is the source table's rowid and REPEATS across restores (§3 note).

### 9.1 Enabling

1. Webhook mode — set `CONTROLPLANE_ALERT_WEBHOOK` at startup (§1.1).
   The JSON is a SECRET (the URL may embed a capability token; the
   bearer always is one): export it from your secret store, e.g.
   `CONTROLPLANE_ALERT_WEBHOOK='{"url": "https://<receiver-host>/<path>", "authorization_bearer": "<from-secret-store>", "timeout_seconds": 5, "poll_seconds": 5, "max_per_tick": 20, "heartbeat_hours": 24}'`.
   Only `url` is required; the numeric fields default as shown and
   `authorization_bearer` may be omitted (AN-10).
2. Log-only mode — no receiver; events become stderr marker lines:
   `CONTROLPLANE_ALERT_WEBHOOK='{"log_only": true}'`.
   `url` and `authorization_bearer` MUST be absent in this mode.
3. A bearer over `http` is refused unless the host is loopback
   (`127.0.0.0/8`, `::1`, `localhost`) — use `https` for anything
   off-host (AN-10). Any config error refuses boot and the message
   names ONLY the offending field, never the URL/bearer/raw JSON
   (AN-11).
4. Unset/empty = disabled entirely: no goroutine, no seeded watermarks.
   First enablement notifies "from now on" — enabling on a long-lived
   deployment never floods the receiver with history (AN-8).

### 9.2 Verify the pipeline

1. FIRST check — the heartbeat (AN-14a): a synthetic envelope with
   `source: "notifier"`, `seq: 0`, `event.kind: "notifier_heartbeat"`
   is delivered ON START (then every `heartbeat_hours`). Receiving it
   at the receiver proves url, auth, and network without touching any
   safety machinery.
2. Check the startup summary (AN-17a): exactly one non-secret log line
   with mode, `poll_seconds`, `max_per_tick`, `heartbeat_hours`, and
   the per-source backlog counts. No line = notifier not enabled.
3. Full test-fire. There is NO synthetic test endpoint — a real kill
   row is the test; use a throwaway strategy so nothing real is killed:
   1. Create a throwaway tenant (§7 step 1; keep the returned
      `$TENANT_OWNER_TOKEN`) and a throwaway strategy in it (§7
      step 3 — `POST /api/v1/strategies`).
   2. Issue a strategy-tier kill (§4.1: trader/admin/owner own tenant,
      or env-admin — the throwaway owner token qualifies):
      `curl -sS -X POST "$CP/api/v1/strategies/<strategy-id>/kill" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"flatten": false}'`
      and note `event_id` in the 200.
   3. Expect at the receiver, within ~`poll_seconds`: one envelope with
      `source: "kill_breaker_events"` and `id` = that `event_id`.
   4. Clear it (§4.2 conventions: READ the `kill_epoch` from
      `GET $CP/api/v1/strategies/<strategy-id>/safety` first — that is
      your CAS `observed_epoch`):
      `curl -sS -X POST "$CP/api/v1/strategies/<strategy-id>/kill/clear" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"reason": "notifier test-fire", "observed_epoch": <kill_epoch>}'`
      and note `clear_id` in the 200.
   5. Expect one envelope with `source: "kill_clear_events"` and `id` =
      that `clear_id`. Both envelopes received = pipeline verified
      end-to-end.

### 9.3 Log forwarding (log-only mode)

Each event is exactly one stderr line: the marker `SAFETY-EVENT `
followed by the AN-13 envelope as a single JSON line. Journald/syslog
prepend their own metadata, so match the marker as a SUBSTRING — never
`^`-anchored (AN-14):

- Tail: `journalctl -u alphamintx-controlplane -f | grep -F 'SAFETY-EVENT '`
- Extract envelopes:
  `journalctl -u alphamintx-controlplane -o cat | grep -F 'SAFETY-EVENT ' | sed 's/.*SAFETY-EVENT //' | jq .`
- NEVER `grep '^SAFETY-EVENT'` — the journald prefix defeats it.

The envelope payload lands in your log aggregation — the §9.6 data
classification applies to it exactly as to the webhook body.

### 9.4 Failure response

- `ALERT DISPATCH DEGRADED` (a source failed 12+ consecutive ticks,
  ~1 min at defaults — AN-17): the RECEIVER is down, unreachable, or
  misconfigured; the log line carries only a derived failure class
  (`dns`/`connect`/`tls`/`timeout`/`redirect`/`status:<code>`/`other`).
  Fix the receiver (or correct url/bearer and restart, §9.5). The
  watermark held at the last success, so the backlog drains in order
  once deliveries succeed — NO events are lost. Heartbeat silence at
  the receiver is the same signal from the other end (AN-14a).
- `ALERT DISPATCH SKIPPED source=<s> id=<id> seq=<n> status=<4xx>`
  (AN-4a): exactly ONE row was poison — the receiver deterministically
  rejected it with a 4xx on 12 consecutive attempts, and the dispatcher
  advanced past it so later kills are not silenced. Inspect that row by
  `id` via the ops read APIs: alerts through the feeds (§5 step 2),
  kills/clears through `GET .../safety` (§4.2 step 1). Everything after
  it flows normally; no operator reset is needed.

### 9.5 Re-enable after a gap; manual reseed

On dispatcher start with existing watermarks, the log reports each
source's backlog size (`MAX(rowid) − last_rowid`, AN-8a) and then
DISPATCHES the backlog — a kill recorded while the notifier was off may
still be active, so silent skipping is forbidden. If "notify me from
now on" is the actual intent, reseed manually — with the control-plane
STOPPED (`systemctl stop alphamintx-controlplane`, §1.3) — running once
per source:

`UPDATE alert_dispatch_state SET last_rowid = (SELECT COALESCE(MAX(rowid),0) FROM <table>), updated_at = '<now RFC 3339 UTC Z>' WHERE source = '<source>';`

for each of `kill_breaker_events`, `kill_clear_events`,
`safety_alerts` (`<table>` = `<source>` — the AN-1 wire names are the
table names). Then `systemctl start alphamintx-controlplane` (§1.2
step 1).

### 9.6 Bearer rotation and data classification

- Rotation is env-only and requires a restart: update
  `CONTROLPLANE_ALERT_WEBHOOK` in `/etc/alphamintx/controlplane.env`
  (value from your secret store), then
  `systemctl restart alphamintx-controlplane` (§1.3, §1.2). No endpoint
  reads or writes this config.
- The receiver sees PnL figures, strategy and actor ids, and UNFILTERED
  error text from `details_json`. Treat the receiver at the ops-panel
  trust tier; `https` is strongly recommended for anything off-host.
  In log-only mode the same payload reaches your log aggregation
  (AN-14) — the same caveat applies there.

## 10. Deployment under systemd (deploy-and-survive.md DS-11..DS-13)

Production runs BUILT artifacts under systemd — never `go run`, a bare
venv python, or `pnpm dev` (DS-11). Units: `alphamintx-controlplane.service`,
`alphamintx-scheduler@<strategy-id>.service` (template; instance name =
strategy ID), `alphamintx-web.service`. All three restart on failure
(`Restart=on-failure`, `RestartSec=5`) and give up after 10 failures in
300 s (`StartLimitBurst`) into a terminal `failed` state — §10.4.

### 10.1 Install

1. From a repo checkout root on the VM: `sudo sh deploy/install.sh`
   (idempotent). It creates the `alphamintx` system user (no login);
   `/var/lib/alphamintx` and `/var/backups/alphamintx`
   (`alphamintx:alphamintx 0700`); builds and installs
   `/opt/alphamintx/bin/controlplane`, `/opt/alphamintx/bin/backupverify`,
   the agent-plane venv at `/opt/alphamintx/agent-plane/.venv`, and the
   web standalone at `/opt/alphamintx/web`; installs the three units to
   `/etc/systemd/system/` and daemon-reloads.
2. Web BUILD-time inputs: export `CONTROLPLANE_API_BASE_URL` and
   `NEXT_PUBLIC_READ_TOKEN` before running the script (`sudo -E` so they
   survive sudo) — they are baked in at `next build` (DS-11c, §10.7).
3. `CONTROLPLANE_DB`, state files, and checkpoints live under
   `/var/lib/alphamintx`; artifacts under `/var/backups/alphamintx` —
   the only paths the hardened units may write
   (`ProtectSystem=strict` + `ReadWritePaths`).

### 10.2 Env files

`/etc/alphamintx/*.env`, `0600 root:root` (systemd reads them as root
before dropping to `User=alphamintx`); units MUST NOT inline tokens.
Plain `KEY=value` lines, no `export`. Variable inventory: §1.1.

- `controlplane.env` — every `CONTROLPLANE_*` variable from the §1.1
  control-plane table.
- `scheduler-<strategy-id>.env` — one per strategy; the `ALPHAMINTX_*` /
  `MINTROUTER_*` variables from the §1.1 scheduler table. MUST set
  `ALPHAMINTX_STRATEGY_ID=<strategy-id>` equal to the unit instance name,
  and PER-INSTANCE `ALPHAMINTX_SCHEDULER_STATE` /
  `ALPHAMINTX_CHECKPOINT_DB` paths — one state file per instance, NEVER
  shared: a shared file dies at the flock and crash-loops (DS-11b).
- `web.env` — RUNTIME vars only: `PORT`, `OPERATOR_TOKEN`.
  `CONTROLPLANE_API_BASE_URL` and `NEXT_PUBLIC_READ_TOKEN` are build-time
  (§10.7); putting them here does nothing.

### 10.3 Enable, start, verify

1. `systemctl enable --now alphamintx-controlplane`. Verify:
   `/opt/alphamintx/bin/controlplane --version` (module version +
   `vcs.revision`/`vcs.time`, DS-12; the same string is logged at
   startup); `curl -sS $CP/health` returns `{"status":"ok"}`;
   `systemctl status alphamintx-controlplane` is `active (running)`.
2. `systemctl enable --now alphamintx-scheduler@<strategy-id>` per
   strategy. Verify:
   `/opt/alphamintx/agent-plane/.venv/bin/python -m alphamintx_agent_plane.scheduler --version`
   (DS-12); `systemctl status alphamintx-scheduler@<strategy-id>`.
3. `systemctl enable --now alphamintx-web`; `systemctl status
   alphamintx-web`; the web logs its build id at startup (DS-12).

`enable` = start at boot; retiring a strategy is
`systemctl disable --now alphamintx-scheduler@<strategy-id>`.

### 10.4 Crash-loop triage

- `systemctl --failed` — a unit in `failed` state exhausted
  `StartLimitBurst` (10 failures / 300 s): a PERSISTENT defect (bad env
  var, missing file, held flock), not a transient. Read
  `journalctl -u <unit> -n 100`, fix the defect,
  `systemctl reset-failed <unit>`, then start.
- Control-plane DEATH has no self-notification — the notifier dies with
  the process. The EXTERNAL detector is receiver-side heartbeat silence
  (AN-14a, §9.2 step 1): no `notifier_heartbeat` within
  `heartbeat_hours` = check the control-plane host.
- Alive-but-failing scheduler (deploy-and-survive.md D7): supervision
  covers process DEATH only. A scheduler heartbeating while every tick
  errors shows as runs advancing with null `proposal_id` plus defect
  log lines in its journal. A restart will not fix a config defect —
  read the journal.
- Scheduler SIGKILL after `TimeoutStopSec` in the journal (long
  in-flight live-LLM tick) is checkpoint-safe by design (DS-11b) — not
  a fault.

### 10.5 Kill-9 restart drill (pre-beta soak)

1. `systemctl show -p MainPID alphamintx-controlplane`, then
   `kill -9 <pid>`.
2. Within ~`RestartSec` (5 s) the unit is `active (running)` again with
   a new PID; confirm `systemctl status`, `curl -sS $CP/health`, and in
   live mode the mandatory startup reconcile.
3. Repeat for one scheduler instance: it must resume from its checkpoint
   and tick state with no rewind (a rewind is only for restores, §3).

### 10.6 Upgrade and rollback

1. Stop in §1.3 order: `systemctl stop 'alphamintx-scheduler@*'`, then
   `alphamintx-web`, then `alphamintx-controlplane`.
2. From the updated checkout: `sudo sh deploy/install.sh`.
3. Start in §1.2 order (control-plane → schedulers → web), running the
   §10.3 `--version` check at each step.
4. Rollback: BINARIES roll back freely (re-run install.sh from the older
   checkout), but a DB touched by newer migrations is NOT certified for
   older binaries — restore from backup (§3) is the rollback of record.

### 10.7 Web rebuild (READ-token / API-base rotation)

`CONTROLPLANE_API_BASE_URL` and `NEXT_PUBLIC_READ_TOKEN` are baked into
the bundle at `next build` (DS-11c). Rotating the READ token or moving
the control-plane origin = export the new values, re-run the web build +
copy (`deploy/install.sh` does both), then
`systemctl restart alphamintx-web`. Editing `web.env` does NOT rotate
them.

## 11. Beta ops tooling (beta-ops-tooling.md; BETA-PROTOCOL BP-8/BP-9)

Three operator binaries make the 30-day beta protocol executable:
`betalog` (hash-chained beta log, BP-9), `betaaudit` (weekly BP-8
audit queries), `deadman` (BP-2 item 4 heartbeat watcher).
`deploy/install.sh` installs the first two on the beta VM;
`deadman` is installed ONLY by `deploy/install-deadman.sh` on the
watcher host (§11.3) — a dead-man on the host it watches dies with
it and proves nothing.

### 11.1 Beta log (betalog)

One append-only JSONL file for the whole beta; every entry carries
the SHA-256 of the previous line (BL-1). Pick the path on Day 0 and
never move it — `$BETA_LOG` below. There is no delete/rewrite/compact
command and no legal in-place edit (BL-5).

- Append a note:
  `/opt/alphamintx/bin/betalog append -log $BETA_LOG -type note -text "..."`
  The tool prints the new entry's SHA-256; `at` is tool-generated,
  never operator-supplied.
- Incident acknowledge / resolve (BP-1 — refs REQUIRED, exit 2
  without them): `-type incident_ack -ref source=<source> -ref id=<id>`
  and later `-type incident_resolve` with the same refs. The
  `(source, id)` pair is the envelope dedupe pair from §9.2. M7 uses
  the FIRST ack for a pair; a duplicate ack is refused.
- Limit change (joins the V9 audit to the log):
  `-type limit_change -ref change_id=<change_id> -text "<why>"` where
  `change_id` comes from the `risk_limit_changes` row (§8 flow).
- Correction (BL-4a — wrong entries are never rewritten):
  `-type correction -ref supersedes=<n> -text "<what was wrong>"`.
- Verify the whole chain: `betalog verify -log $BETA_LOG` — exit 0
  clean; first break reported with its line number. Run it after
  every append session and before every custody copy (§11.2).

### 11.2 Daily off-host custody (BL-6 — applies to the deadman raw log verbatim)

The chain has no secret input: an operator who controls the host can
regenerate the whole file. Custody, not cryptography, defeats that:

1. Daily, copy BOTH `$BETA_LOG` and the watcher's raw log to storage
   the operator CANNOT rewrite (versioned object store with retention
   lock, or a copy held by the design partner / reviewer). The custody
   arrangement itself is Day-0 evidence (BP-2).
2. Append-only means every daily copy is a byte-prefix of every later
   copy and of the final log. The reviewer's check:
   `betalog verify -log <daily-copy> -prefix-of <later-copy-or-final>`
   A prefix failure against ANY pre-existing copy = regeneration
   finding at exit review.
3. Honest limit: daily copies bound `at` to ±24 h and no finer; M7
   verdicts inside that tolerance rest on the receiver raw log and
   journal cross-checks, not on `at` alone.

### 11.3 Dead-man receiver (deadman — WATCHER HOST only)

1. On a host that is NOT the beta VM, from a repo checkout:
   `sudo sh deploy/install-deadman.sh` (it refuses to run on a host
   with the control-plane unit installed — that is a guard, not a
   suggestion).
2. Create `/etc/alphamintx/deadman.env` (0600 root:root):
   `DEADMAN_BEARER=<generated secret>` (mandatory — the unit fails
   fast without it, DM-1) and optionally `DEADMAN_ALARM_URL=<url>`
   (DM-3 alarm POST target).
3. The unit's `-heartbeat-hours` MUST equal the notifier's
   `heartbeat_hours` (§9.1 `CONTROLPLANE_ALERT_WEBHOOK`): larger
   blinds the alarm, smaller manufactures false alarms. Edit both
   together or neither.
4. Point the notifier webhook at the receiver THROUGH the site
   reverse proxy (TLS terminates there — DM-5; the unit listens on
   loopback), with `authorization_bearer` set to the same secret.
5. `systemctl enable --now alphamintx-deadman`, then confirm a
   `receiver_start` mark in the raw log and the §9.2 on-start
   heartbeat arriving from the control-plane.
6. Day-0 drill (DM-4): with `DEADMAN_BEARER` exported from the env
   file, `/opt/alphamintx-deadman/bin/deadman -selftest
   -target http://127.0.0.1:9190`, then confirm a
   `"source":"selftest"` line in the raw log. Selftest can never
   feed the alarm tracker or mask a real gap.
7. Alarm response: a `{"alarm":"heartbeat_silence"}` line (and POST
   to `DEADMAN_ALARM_URL` if set) means no notifier heartbeat for
   H+1 hours — check the control-plane host per §9.4/§10.4. Its
   `fired_at` starts the BP-2 item 4 SLA clock. Receiver downtime
   does NOT stop that clock: a gap in `receiver_alive` marks counts
   against availability from the last successful heartbeat.

### 11.4 Weekly audit (betaaudit)

1. Input is the newest VERIFIED backup artifact (§2), never the live
   DB for the weekly run:
   `/opt/alphamintx/bin/betaaudit -artifact <newest-artifact>
   -ref-artifact <previous-week-artifact> -strategy <certified-id>
   -log $BETA_LOG -json > audit-<date>.json`
   `-ref-artifact` enables V7b append-only growth; both sides must be
   artifacts.
2. `-db <live>` exists for spot checks only and prints a warning: a
   long read on the live WAL blocks the backup's
   `wal_checkpoint(TRUNCATE)` and would MANUFACTURE `backup_failed`
   incidents against the OB-10 loop.
3. Exit 0 = no FAIL. Any FAIL is an incident: ack it in the beta log
   (§11.1) and handle per the alert class (§5, §9.4).
4. MANUAL checks (V2 residency, V3, V5 timing, V6, V8, M6a) print
   their human procedure. Perform it, then discharge EACH with:
   `betalog append -log $BETA_LOG -type audit_manual
   -ref check=<id> -ref result=pass|fail -text "<what was checked>"`
   A weekly report whose MANUAL items lack matching beta-log entries
   is an INCOMPLETE audit at exit review (BA-2).
5. File `audit-<date>.json` with the daily custody copies (§11.2);
   the report header binds it to its inputs (artifact SHA-256,
   `user_version`, per-table max rowid).
