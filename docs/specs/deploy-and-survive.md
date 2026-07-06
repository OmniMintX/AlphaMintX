# Deploy & Survive — process supervision, fail-closed restore, backup alerting

Status: NORMATIVE for the Phase-3 ops-hardening slice. Closes the top
audit gaps: no process supervision, docs-only restore safety diff, and
silent backup failures. Companion to docs/specs/ops-backup.md (OB-*),
docs/specs/alert-notifier.md (AN-*), and docs/RUNBOOK.md §1/§3/§10.

## Problem

1. Nothing restarts a dead control-plane or scheduler: every safety
   engine (watchdog, breaker, reconciler, notifier) dies with the
   process while positions and protective orders stay live on the venue.
2. RUNBOOK §3 step 7 (post-restore safety diff) is "MANDATORY" prose
   only. A restore erases every post-snapshot kill; nothing stops an
   operator from restarting schedulers without the diff — the lost kill
   is FAIL-OPEN (ops-backup.md invariant 8).
3. A failed periodic backup only writes `BACKUP FAILED` to stderr — the
   exact logs-nobody-watches regime the alert notifier replaced.

## Design decisions

- **D1 — restore detection is stamped into the artifact, not entrusted
  to the operator.** The backup engine sets `PRAGMA user_version = 1`
  inside every artifact. A DB booted from an artifact therefore ENGAGES
  the restore gate by construction; no marker file, env var, or runbook
  discipline involved. The live DB's own `user_version` stays untouched
  by backups (it only becomes 1 by BEING a restored artifact) and is
  reset to 0 exactly once, by the explicit operator acknowledgment.
  `user_version` is confirmed unused by the store today.
- **D2 — the gate blocks NEW TRADING INTENT only.** Proposals and
  approvals are rejected while engaged. Kills, clears, lifecycle
  (operator may need pause), heartbeats, traces (OB-12a replay noise),
  reads, backups, tokens, billing, venue-reset ack, and the mandatory
  startup reconcile all proceed: safety and audit must never be gated.
- **D3 — acknowledgment is persisted in the DB itself** (`user_version`
  back to 0), so the gate survives restarts until an env-admin acts.
- **D4 — systemd, not Docker/K8s.** The 30-day beta is one VM; systemd
  units with `Restart=on-failure` + journald are the smallest reviewable
  supervisor. Containerization is a Phase-4 concern.
- **D5 — only the PERIODIC backup loop appends `backup_failed`.** The
  manual `POST /api/v1/ops/backups/run` already returns the failure
  synchronously to the caller and logs it; duplicating it as an alert
  would double-notify the same human act.
- **D6 — pre-slice artifacts restore ungated** (`user_version` 0).
  Accepted, documented; operators SHOULD take a fresh backup right
  after deploying this slice. Same acceptance for ad-hoc `cp control.db`
  copies made OUTSIDE the engine: only engine artifacts engage the gate,
  and the RUNBOOK forbids restoring ad-hoc copies. A stamped artifact
  booted anywhere (a scratch inspection deployment) fires gate alerts
  into whatever webhook THAT deployment configures — scratch inspections
  SHOULD leave `CONTROLPLANE_ALERT_WEBHOOK` unset.
- **D7 — supervision covers process DEATH only.** `Restart=on-failure`
  restarts a crashed process; it cannot detect the alive-but-failing
  mode (scheduler heartbeating while every tick errors, loop.py
  swallows per-tick exceptions). The detection signal for that mode is
  runs advancing with null `proposal_id` / defect log lines — a RUNBOOK
  §10 triage entry, not a code change here.

## Normative requirements — restore gate

- **DS-1**: The stamp happens IN-STREAM during the OB-2 step-4 byte-copy:
  the copy loop substitutes bytes 60–63 (the SQLite header
  `user_version` field, 4-byte big-endian) with `00 00 00 01` as they
  pass through, so the written tmp file IS the stamped file. The
  streaming SHA-256 is therefore natively computed over the stamped
  bytes and OB-5a's copy-digest == re-read-digest property holds
  verbatim — one write pass, one fsync, no post-copy `WriteAt`. The
  stamp never goes through a SQLite connection and never touches the
  live DB. This amends ops-backup.md OB-5a and Invariant 2 to read
  "bit-identical to the source main file at copy time EXCEPT the 4-byte
  DS-1 user_version stamp".
- **DS-1a**: OB-5 verification and `backupverify` (OB-11) MUST pass
  unchanged on stamped artifacts (`user_version` does not participate in
  `integrity_check`, `foreign_key_check`, or table fingerprints).
  `backupverify` additionally prints `user_version: N` (informational);
  this amends the OB-11 report shape.
- **DS-2**: `store.Open` reads `PRAGMA user_version` after migrations
  (migrations never touch `user_version`). A value ≥ 1 puts the store in
  restore-gate mode, exposed as `RestoreGateEngaged() bool`; the server
  logs LOUDLY at boot:
  `RESTORE GATE ENGAGED: proposals and approvals are blocked until POST /api/v1/ops/restore/ack (RUNBOOK §3)`.
- **DS-3**: While engaged, `POST .../proposals` and `POST .../approvals`
  return `503 RESTORE_GATE` with a message naming the ack endpoint and
  RUNBOOK §3. The gate check is the FIRST statement of each handler —
  before body read, decode, strategy resolution, or any lookup — so no
  verdict, no rejected_submissions row, no approval row is persisted,
  the proposal rate limiter is not charged, and the 503 is uniform
  across known/unknown strategies (no new existence oracle). Blocking
  approvals blocks the explicit reject path too — acceptable because
  the expiry sweep is default-deny.
- **DS-4**: On boot with the gate engaged the server appends one
  `safety_alerts` row `kind='restore_gate_engaged'` (strategy_id NULL,
  `details_json` `{"user_version": N}`) — but only when no
  `restore_gate_engaged` row is newer than the newest
  `restore_gate_cleared` row (once per ENGAGEMENT, not per boot: a
  crash-looping gated server under `RestartSec=5` must not flood the
  append-only table or the webhook). The append happens in the
  `cmd/controlplane` serve wiring AFTER the notifier watermark seed
  (AN-8) and before `ListenAndServe` — appending earlier (in
  `store.Open` or `api.New`) would let a first-enable seed swallow the
  one alert this slice exists to deliver. The alert notifier delivers
  it through the existing `safety_alerts` source — zero notifier
  changes.
- **DS-5**: `POST /api/v1/ops/restore/ack` (env-admin ONLY, empty body,
  never parsed) clears the gate. `user_version = 0` and the
  `safety_alerts` `kind='restore_gate_cleared'` append (`details_json`
  `{"actor_id": ...}` per the existing `actorID()` convention — the
  env-admin class renders as the constant `"env-admin"`) commit in ONE
  transaction (the AN-1a precedent: two sequential appends leave a
  crash window that loses the alert). The in-memory flag is
  compare-and-swapped: exactly one concurrent ack wins and persists;
  the loser — and any ack when the gate is not engaged — gets
  `409 RESTORE_GATE_NOT_ENGAGED` (catches acks aimed at the wrong
  deployment). Winner returns `200 {"cleared": true}`. The route is
  always registered when a Store exists. Clearing is one-way: the gate
  cannot be re-armed; a wrong ack is handled by killing at the affected
  tier and redoing the restore (RUNBOOK §3).
- **DS-6**: `GET /api/v1/ops/restore` returns `{"engaged": bool}` with
  permission row `Roles: nil, Classes: [read, env-admin]` (the
  `GET /api/v1/alerts` OS-19 precedent: platform operational data, not
  tenant-confidential — the ops panel's READ token must be able to
  render WHY approvals 503). Unlike the backup routes it does NOT
  require `CONTROLPLANE_BACKUP_DIR`: a restore can happen on a
  deployment whose backup dir is not (yet) configured.
- **DS-7**: Both new routes enter the permission matrix and
  `TestRBACMatrix` (ack: env-admin only; status: read + env-admin). The
  ack records `actorID()` in the cleared alert's details and the server
  log line.
- **DS-8**: The gate blocks EXACTLY the two routes in DS-3. Explicitly
  NOT gated: kill, clear, lifecycle, heartbeat, traces, all reads,
  backups, tokens, billing, venue-reset ack, recon runs, and the live
  OMS startup reconcile (exchange-is-truth proceeds under the gate).
  Known accepted edge: a pre-restore `pending_approvals` row that
  survives to post-ack inside its window can then be approved against
  restored state — the approval preflight (kill epoch, mark freshness,
  lifecycle) mitigates; RUNBOOK §3 notes it.

## Normative requirements — backup failure alert

- **DS-9**: When the periodic backup loop's run fails (any error other
  than `ErrBackupInProgress`, which is a benign skip), it appends one
  `safety_alerts` row `kind='backup_failed'`, strategy_id NULL,
  `details_json` `{"trigger": "periodic", "category": C}` where C is
  `"verify_failed"` (ErrBackupVerifyFailed), `"artifact_exists"`
  (ErrBackupExists), or `"io"` (anything else). Raw error text is NEVER
  stored or dispatched (it can embed filesystem paths); the full error
  still goes to the server log as today. Note: a retention failure
  AFTER a successful artifact surfaces as `"io"` even though a good
  artifact exists — the log line disambiguates; accepted.
- **DS-10**: An append failure of the alert row itself is logged and the
  loop continues at cadence — alerting must never wedge the backup loop.

## Normative requirements — supervision & versioning

- **DS-11 — install contract.** Production runs BUILT artifacts, never
  `go run`: `controlplane` and `backupverify` binaries installed to
  `/opt/alphamintx/bin/`, the agent-plane in a venv at
  `/opt/alphamintx/agent-plane/.venv`. `deploy/install.sh` (run from a
  repo checkout on the VM) builds and installs both, creates the
  dedicated `alphamintx` system user, and creates
  `/var/lib/alphamintx/` (DB, state files, checkpoints) and
  `/var/backups/alphamintx/` owned `alphamintx:alphamintx 0700`. Env
  files live at `/etc/alphamintx/*.env`, `0600 root:root` (systemd
  reads them as root before dropping to `User=`); units MUST NOT
  inline tokens.
- **DS-11a — units.** `deploy/systemd/` ships
  `alphamintx-controlplane.service`, `alphamintx-scheduler@.service`,
  and `alphamintx-web.service`. Common keys: `User=alphamintx`,
  `Restart=on-failure`, `RestartSec=5`,
  `StartLimitIntervalSec=300` / `StartLimitBurst=10` (a persistent
  config defect lands the unit in a terminal `failed` state visible in
  `systemctl --failed`, instead of an infinite silent crash loop),
  journald stdout/stderr, `Wants=network-online.target` +
  `After=network-online.target` (After alone is a no-op), and
  `[Install] WantedBy=multi-user.target`. Hardening
  (`NoNewPrivileges=yes`, `ProtectSystem=strict` with
  `ReadWritePaths=/var/lib/alphamintx /var/backups/alphamintx`,
  `PrivateTmp=yes`, `MemoryMax=` as a commented site-tunable) ships in
  the unit files. The control-plane unit sets
  `LogRateLimitIntervalSec=0` (journald's default per-service rate
  limit can silently drop SAFETY-EVENT lines under burst — RUNBOOK §9.3
  makes journald a delivery substrate for log-only notifier mode).
- **DS-11b — scheduler template.** Instance name = the strategy ID
  (UUID charset needs no systemd escaping);
  `EnvironmentFile=/etc/alphamintx/scheduler-%i.env`. Each instance
  file MUST set `ALPHAMINTX_STRATEGY_ID` equal to the instance name and
  per-instance `ALPHAMINTX_SCHEDULER_STATE` / `ALPHAMINTX_CHECKPOINT_DB`
  paths — one state file per instance, never shared (a shared file dies
  at the flock and crash-loops). The template declares
  `Wants=`/`After=alphamintx-controlplane.service`. Verified exit
  semantics: startup defects and unhandled loop errors exit non-zero
  (restart fires); SIGTERM exits 0 (no restart). SIGKILL after
  `TimeoutStopSec` (a long in-flight live-LLM tick can exceed the 90 s
  default) is checkpoint-safe by design — an operator seeing it in the
  journal is not looking at a fault.
- **DS-11c — web.** The web unit runs the Next.js standalone build
  (`ExecStart=node /opt/alphamintx/web/server.js`;
  `output: "standalone"` added to `next.config.ts`; `deploy/install.sh`
  builds and copies it). `CONTROLPLANE_API_BASE_URL` and
  `NEXT_PUBLIC_READ_TOKEN` are BUILD-time inputs — changing either
  (including READ-token rotation) is rebuild + restart, documented in
  RUNBOOK §10; the unit's `EnvironmentFile=` carries only runtime vars
  (`PORT`, `OPERATOR_TOKEN`).
- **DS-12**: `controlplane --version` prints the module version plus
  `vcs.revision`/`vcs.time` from `debug.ReadBuildInfo` and exits 0; the
  same string is logged at startup. The agent-plane scheduler accepts
  `--version` and prints the package `__version__`. The web logs its
  build id at startup (standalone build stamp).
- **DS-13**: RUNBOOK gains §10 — install (DS-11), enable/disable
  expectations, upgrade procedure, crash-loop triage (unit `failed`
  state; receiver-side heartbeat silence is the EXTERNAL detector for
  control-plane death, since the notifier dies with the process; and
  the D7 alive-but-failing signal), and a kill-9 restart drill. The
  upgrade procedure pins: stop order = §1.3 (schedulers → web →
  control-plane), install new artifacts, start order = §1.2 reversed,
  `--version` check at each step; rollback = binaries roll back freely,
  but a DB that newer migrations touched is NOT certified for older
  binaries — restore from backup is the rollback of record. §1.1 gains:
  NTP slew-mode requirement, 30-day disk projections (inputs:
  soak-measured control.db growth/day, artifact size ×
  `CONTROLPLANE_BACKUP_RETAIN`, checkpoint DB growth × strategies,
  journald caps), reverse-proxy TLS pattern for any non-loopback
  exposure, and the ops-panel token-class note (env operator-class
  tokens 403 the panel's mutating buttons). Existing sections are
  REWRITTEN, not just appended: §1.2/§1.3 (systemctl verbs; `go run`
  survives only as a dev-mode note), §3 steps 1/5 (`systemctl stop
  'alphamintx-scheduler@*'` … `systemctl start`; step 4 uses the
  installed `/opt/alphamintx/bin/backupverify`), §9.5/§9.6 (systemctl
  restart verbs).
- **DS-14**: RUNBOOK §3 inserts the ack as an explicit step after the
  safety diff and BEFORE step 8 (scheduler restarts): a scheduler
  started under the gate burns real LLM spend on ticks whose proposals
  are 503'd with nothing persisted, leaving permanent run gaps.
  Documents the gate's observable behavior (503s until ack;
  `restore_gate_engaged` webhook on boot; DS-8 pending-approval edge;
  wrong-ack recovery per DS-5; never restore ad-hoc file copies per
  D6).

## Test obligations

- Store: artifact is stamped (`user_version` 1) and its recorded SHA-256
  matches the stamped bytes; OB-5 verify passes; live DB `user_version`
  stays 0 after a backup; opening a restored artifact engages the gate;
  clearing persists across reopen.
- API: gated proposals/approvals return 503 `RESTORE_GATE` with nothing
  persisted; kill/clear/reads/lifecycle/heartbeat/traces still work
  under the gate; ack clears (200, then proposals flow) atomically
  (gate clear + cleared alert in one transaction) and re-ack is 409;
  concurrent acks: exactly one 200; both routes appear in
  `TestRBACMatrix` (ack: env-admin only; status: read + env-admin);
  `restore_gate_engaged`/`_cleared`/`backup_failed` rows have the
  specified shapes; boot-alert dedupe: a second gated boot without an
  intervening clear appends no second `restore_gate_engaged` row.
- Verifier: `backupverify` passes on a stamped artifact and prints
  `user_version`.
- Drill (soak, RUNBOOK §10): systemd restart-on-kill; full OB-12 restore
  into a scratch deployment observing gate alert → webhook, 503, ack,
  cleared alert → webhook; `chmod 000` backup dir → `backup_failed`
  webhook within one periodic interval.
