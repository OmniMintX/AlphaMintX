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

Control-plane (`go run ./cmd/controlplane`, repo `control-plane/`):

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

Agent-plane scheduler (`python -m alphamintx_agent_plane.scheduler`, repo
`agent-plane/`, one process per strategy):

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

Web dashboard (`pnpm build && pnpm start`, repo `web/`):

| Variable | Required | Secret | Meaning |
|---|---|---|---|
| `CONTROLPLANE_API_BASE_URL` | yes | no | Control-plane origin as seen from the Next SERVER; baked into rewrites at `next build` time. |
| `NEXT_PUBLIC_API_BASE_URL` | no | no | Cross-origin escape hatch for the browser client; empty = same origin. |
| `NEXT_PUBLIC_READ_TOKEN` | yes | yes | READ token, INLINED into the public JS bundle at build time (GETs only by design — persistence-and-api.md §Auth); use a per-tenant viewer DB token for tenant-facing deployments. |
| `OPERATOR_TOKEN` | for ops controls | yes | Server-only; attached by the approvals/lifecycle/kill/clear proxy routes; never `NEXT_PUBLIC_`. |

### 1.2 Start order

1. Start the control-plane: `CONTROLPLANE_DB=... go run ./cmd/controlplane`
   (from `control-plane/`). Confirm `curl -sS $CP/health` returns
   `{"status":"ok"}`. In live mode the mandatory startup reconcile runs
   before any submission is accepted (`RECONCILE_PENDING` until then).
2. Start one scheduler per strategy: `python -m alphamintx_agent_plane.scheduler`
   (from `agent-plane/`, env per §1.1). It fails fast on any missing
   variable and refuses to start if another instance holds
   `$ALPHAMINTX_SCHEDULER_STATE.lock`.
3. Start the web server: `pnpm build && pnpm start` (from `web/`;
   `CONTROLPLANE_API_BASE_URL` must be set at BUILD time).

### 1.3 Graceful stop

1. Stop schedulers FIRST: send SIGTERM (or SIGINT); the run task cancels
   cleanly and the in-flight tick is abandoned (its checkpoint survives).
   Stopping the scheduler before the control-plane avoids orphaned ticks.
2. Stop the web server (any time; it holds no state).
3. Stop the control-plane: SIGTERM/SIGINT; it drains in-flight requests
   with a 5-second shutdown timeout.

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
  hours, first run one interval AFTER boot (OB-10). Failures log with a
  `BACKUP FAILED` prefix and the loop continues — watch for that string.
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

## 3. Restore (ops-backup.md OB-12, verbatim)

Restore is OFFLINE. A restore is NOT complete until step 7's safety diff
has been executed — a post-snapshot kill lost by the restore is FAIL-OPEN
until re-issued (ops-backup.md invariant 8).

1. Stop the control-plane process AND every agent-plane scheduler (§1.3
   order: schedulers first).
2. Move aside the live files — KEEP them (forensic record and step-7
   input):
   `mv control.db control.db.incident && mv control.db-wal control.db-wal.incident && mv control.db-shm control.db-shm.incident`
   (the `-wal`/`-shm` moves apply only if the files exist).
3. Copy the chosen artifact to the `CONTROLPLANE_DB` path:
   `cp "$CONTROLPLANE_BACKUP_DIR/control-<stamp>.db" "$CONTROLPLANE_DB"`.
   Do NOT copy any `-wal`/`-shm` alongside — the artifact is fully
   checkpointed and standalone (OB-2).
4. Verify: from `control-plane/`, `go run ./cmd/backupverify -db "$CONTROLPLANE_DB"`
   and REQUIRE exit code 0. Do NOT substitute a system `sqlite3` binary
   (OB-11 — the tool shares the server's exact driver version). Never
   point it at a live, attached DB.
5. Start the control-plane (§1.2 step 1). `store.Open` re-applies the
   idempotent schema and migrations; it must be the first writer.
6. Confirm: `curl -sS $CP/health` is 200;
   `GET $CP/api/v1/strategies/<id>/safety` renders for each strategy; in
   live mode the mandatory startup reconcile completes.
7. **Safety diff (MANDATORY, BEFORE any scheduler restarts).** The restore
   erased every kill, clear, breaker row, and lifecycle transition issued
   after the snapshot; `GET .../safety` on the restored DB shows a CLEAN
   strategy and cannot reveal this.
   1. Run `backupverify -db control.db.incident` (the moved-aside copy is
      no longer attached) and note the row counts and newest timestamps of
      `kill_breaker_events`, `kill_clear_events`, `lifecycle_transitions`,
      `safety_alerts`; alternatively inspect the moved-aside copy with
      read-only SQL.
   2. Compare against the restored DB (`backupverify -db "$CONTROLPLANE_DB"`).
   3. RE-ISSUE every post-snapshot kill, clear, and lifecycle action
      through the normal endpoints (§4, `POST .../lifecycle`). A kill
      CLEARED after the snapshot coming back ACTIVE is fail-safe: just
      re-clear it (§4.2).
8. Rewind each strategy's tick state, THEN restart its scheduler
   (OB-12a):
   1. Read the restored DB's `MAX(tick_number)` for the strategy (newest
      run via `GET $CP/api/v1/strategies/<id>/runs?page=1&limit=1`).
   2. Edit the `ALPHAMINTX_SCHEDULER_STATE` file — shape
      `{"strategies": {"<strategy-id>": {"next_tick_number": <N>}}}` — to
      `MAX(tick_number) + 1`.
   3. Restart the scheduler (§1.2 step 2).

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
3. Mint the strategy's agent token (admin/owner own tenant, or
   env-admin with `"tenant_id"` in the body):
   `curl -sS -X POST "$CP/api/v1/tokens" -H "authorization: Bearer $TENANT_OWNER_TOKEN" -H "content-type: application/json" -d '{"principal": "agent", "strategy_id": "<strategy-id>", "label": "<label>"}'`
   — agent tokens carry a `strategy_id` and no role; the strategy must
   exist in the tenant (else `404 UNKNOWN_STRATEGY`).
4. Deploy the scheduler for that strategy (§1.1/§1.2) with
   `ALPHAMINTX_STRATEGY_TOKEN=<the minted plaintext>` from the secret
   store — never committed, never logged.
5. Rotation/revocation rules: rotation is always mint-new-then-revoke-old
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
      `$TENANT_OWNER_TOKEN`) with a throwaway strategy in it (the
      strategy must exist in the tenant — the same provisioning §7
      step 3 presumes).
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

- Tail: `journalctl -u <controlplane-unit> -f | grep -F 'SAFETY-EVENT '`
- Extract envelopes:
  `journalctl -u <controlplane-unit> -o cat | grep -F 'SAFETY-EVENT ' | sed 's/.*SAFETY-EVENT //' | jq .`
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
STOPPED (§1.3) — running once per source:

`UPDATE alert_dispatch_state SET last_rowid = (SELECT COALESCE(MAX(rowid),0) FROM <table>), updated_at = '<now RFC 3339 UTC Z>' WHERE source = '<source>';`

for each of `kill_breaker_events`, `kill_clear_events`,
`safety_alerts` (`<table>` = `<source>` — the AN-1 wire names are the
table names). Then start the control-plane (§1.2 step 1).

### 9.6 Bearer rotation and data classification

- Rotation is env-only and requires a restart: update
  `CONTROLPLANE_ALERT_WEBHOOK` in your secret store, then restart the
  control-plane (§1.3, §1.2). No endpoint reads or writes this config.
- The receiver sees PnL figures, strategy and actor ids, and UNFILTERED
  error text from `details_json`. Treat the receiver at the ops-panel
  trust tier; `https` is strongly recommended for anything off-host.
  In log-only mode the same payload reaches your log aggregation
  (AN-14) — the same caveat applies there.
