# 30-Day Design-Partner Beta Protocol

Status: normative for the beta's judgment. Governs the final Phase 3
exit criterion in `docs/PLAN.md` ("≥1 design-partner tenant completes
30 days of live beta within limits with zero invariant violations in
audit review"). On mechanism, `docs/specs/*` and `docs/RUNBOOK.md`
win; on what counts as beta success, failure, or a clock restart,
THIS document wins. Every metric and audit below is measurable from
surfaces that already exist (persisted tables, read APIs, notifier
envelopes, backup artifacts, systemd journal) — a criterion that
cannot be evidenced from those surfaces does not belong here.

## Purpose and non-goals

The beta certifies ONE thing: that a real, non-house tenant operated
a live strategy for 30 days inside the agreed RiskLimits with the
safety machinery demonstrably intact. It does NOT certify
profitability (PnL is the partner's outcome, not a pass/fail input),
does NOT relax any spec invariant, and does NOT grant the partner any
control surface beyond the RBAC matrix already shipped.

## BP-1 Definitions

- **Beta day**: a complete UTC day (00:00:00Z–23:59:59Z), matching the
  breaker/daily-loss day boundary in `risk-limits.md`.
- **Certified strategy**: the beta binds to exactly ONE
  `strategy_id`, recorded in the Day-0 evidence (BP-2 item 9). The
  clock, M1–M8, and the BP-8 audits bind to that id only; other
  strategies in the tenant are out of scope (they still live inside
  their own limits, but they cannot substitute for the certified one).
- **Beta clock**: Day 1 is the first complete UTC day after go-live
  (BP-2 satisfied and the certified strategy in a `live_*` state).
  A day COUNTS only if the certified strategy was in a `live_*`
  state for the entire UTC day; the beta completes when 30 days have
  counted. Any kill (any actor, any tier) suspends counting until
  re-promotion — and because `killed → paper` restarts the 14-day
  paper window (`lifecycle-api.md` LC-16/LC-17, unwaivable), a kill
  that the exit review does not classify as a spec-mandated safety
  fire ends the attempt in practice. A breaker fire does NOT suspend
  counting (it demotes to L0 until the next UTC day without leaving
  `live_*`). Clock-restart conditions: BP-4.
- **Within limits**: every venue order the strategy originated passed
  the Risk Gate (proposal-originated) or was a safety-path submission
  (SL/TP placement, kill/breaker/watchdog cancels and flatten,
  `risk-limits.md` gate step exemptions). Breaker fires, watchdog
  escalations, and kills are NOT failures by themselves — they are
  the product working; only their AUDIT findings (BP-8) can fail the
  beta.
- **Invariant violation**: any confirmed finding of class V1–V9 in
  BP-8. Exactly zero are tolerated across the 30 days.
- **Incident**: any safety event delivered by the alert notifier
  (`alert-notifier.md` — kill/breaker/clear envelopes and every
  `safety_alerts` kind), a notifier heartbeat gap (BP-5), or a
  process crash visible in the systemd journal. Incidents are logged
  in the beta log (BP-9). SLA clocks start at the event's persisted
  `recorded_at` in `control.db` — NOT `delivered_at`, which is
  dispatch time (AN-13) and moves under receiver-side retries.
  **Acknowledgment** = a beta-log entry citing the event's
  `(source, id)`; the receiver's raw request log (each envelope with
  arrival time), copied off-host daily, is the tiebreaker.

## BP-2 Entry criteria (Day-0 gate — ALL must hold, with evidence)

1. **Real-testnet drills green** on the deployed build:
   `TestTestnetDrill_OutageRestart`, `TestTestnetDrill_KillSwitch`,
   `TestTestnetDrill_Breaker`, `TestTestnetDrill_Watchdog` against
   the real Binance testnet (the four REMAINING drill items in
   `docs/PLAN.md` Phase 3 — three exit criteria plus the watchdog
   deliverable). These tests fail on vacuous evidence by
   construction; CI-fake equivalents do not count.
2. **systemd deployment** per RUNBOOK §10 on the beta VM
   (`deploy/install.sh` contract), including the restart-on-kill
   drill that was deferred from the dev container: `kill -9` the
   control-plane, verify systemd restarts it and the startup
   reconcile completes.
3. **Backup + restore drill on the beta VM**: RUNBOOK §2 backup with
   verification, §3 restore of the artifact, restored copy boots
   GATED (503 `RESTORE_GATE`), env-admin ack clears it — the full
   `deploy-and-survive.md` restore-gate path (RUNBOOK-§3 ack ordering
   per DS-14), not just the backup half.
4. **Notifier live + dead-man alarm**: a real operator webhook
   configured per RUNBOOK §9, heartbeat enabled (`heartbeat_hours`
   ≥ 1, recommended 24), one end-to-end delivery observed. The
   operator's receiver MUST implement the receiver-side dead-man
   alarm the specs assign to it (AN-14a; `deploy-and-survive.md`
   DS-13 makes heartbeat silence the EXTERNAL detector for
   control-plane death): it pages the operator when heartbeat
   silence exceeds `heartbeat_hours` + 1h, and its firing timestamp
   is the SLA clock start for the heartbeat-gap and
   control-plane-down classes. One drill of this alarm is Day-0
   evidence.
5. **Partner onboarded self-service** per RUNBOOK §7: tenant →
   strategy (`POST /api/v1/strategies`) → agent token → scheduler
   instance. Zero manual DB edits; the owner token stays with the
   partner, never with the operator.
6. **RiskLimits countersigned**: `symbol_whitelist` non-empty,
   `per_position_notional_cap_quote` and `daily_loss_limit_quote`
   explicitly set (they have no defaults — `risk-limits.md`), values
   recorded in the beta log and acknowledged by the partner in
   writing. Recommended starting caps: per-position ≈ 5% equity,
   daily loss 2–5% equity (the spec's guidance).
7. **Venue**: testnet by default. Prod only via the RUNBOOK §8
   triple opt-in, and only with trade-only, non-custodial API keys —
   withdrawals DISABLED, verified against the venue's API-permission
   screen before go-live and re-verified at every key rotation.
8. **Paper gate passed**: the strategy reached `live_*` exclusively
   through `POST /api/v1/strategies/{id}/lifecycle` and its computed,
   unwaivable paper gate (`lifecycle-api.md`). A copy of the
   `GET .../paper-gate` report at promotion time goes in the beta log.
9. **Certified strategy pinned**: the Day-0 evidence records the one
   `strategy_id` under certification (BP-1); every metric and audit
   below binds to it.
10. **Clock integrity**: NTP slew-mode active and offset < 1 s per
    RUNBOOK §1.1, recorded in the beta log — the UTC-day boundary
    carries the beta clock, the breaker day, M5, and M8.

## BP-3 Roles

- **Operator** (env-admin): runs the VM, backups, upgrades, notifier,
  responds to incidents per RUNBOOK, conducts BP-8 audits, keeps the
  beta log. The operator holds ADMIN/OPERATOR/READ env tokens and the
  venue keys; the partner never sees them.
- **Partner** (tenant owner): owns strategy configuration within the
  countersigned limits, the agent-plane prompts/models, and the
  decision to trade. May kill or pause their own strategy at any time
  (RBAC matrix). A partner-initiated kill is never a violation, but
  the clock effect follows BP-1: counting suspends, and the 14-day
  paper window on unlock means it ends the attempt in practice
  (without prejudice — BP-7). For SLA classing, partner-initiated
  kills of their own strategy are class B (a hostile partner must
  not be able to manufacture class-A load at 3 a.m.); their V4 audit
  treatment is unchanged.
- No LLM touches credentials; no beta procedure may route an operator
  secret through the partner or vice versa.

## BP-4 Change control during the beta

- **Tightening limits** (lower caps, shorter whitelist) is allowed
  any time and does NOT restart the clock. But a day on which the
  effective `per_position_notional_cap_quote` was below 25% of the
  countersigned Day-0 value does not count toward M8's activity
  floor (BP-6) — tightening to dust must not manufacture "activity"
  at zero real risk.
- **Loosening limits or widening the whitelist** restarts the clock:
  the criterion certifies 30 days within ONE agreed envelope, not a
  moving one. Restart = the next complete UTC day becomes Day 1.
  Enforcement is not honor-based: V9 (BP-8) replays the append-only
  limit-change audit trail every week.
- **Binary upgrades** are allowed per RUNBOOK §10 and logged (old →
  new `--version` build ids). An upgrade that crash-loops (start
  limit exhausted) is an incident; the clock continues if recovery
  meets the BP-5 availability bar. If recovery breaches the
  availability bar, either party may declare the attempt ended at
  once: the clock restarts at a fresh Day-0 gate after the root
  cause is fixed.
- **Venue switch testnet → prod** restarts the clock. Prod → testnet
  ends the beta attempt (the run no longer certifies live operation
  at one venue tier).
- Schema/spec changes on the deployed build follow the normal repo
  audit process; they restart the clock only if they alter the Risk
  Gate, OMS, or safety machinery semantics.

## BP-5 Operational SLA (operator commitments; clocks start at the
event's persisted `recorded_at`, or the dead-man alarm / journal
timestamp for the absence classes — BP-1 §Incident)

| Class | Events | Response commitment |
|---|---|---|
| A | kill (any tier, any actor — except partner-initiated kills of their own strategy, class B per BP-3), `sl_deadline_contingency`, `venue_reset`, restore-gate engaged, notifier heartbeat gap > `heartbeat_hours` + 1h, control-plane down | Acknowledge ≤ 4h; act per RUNBOOK §3/§4/§5/§6/§9/§10 as the event dictates; root-cause note in the beta log ≤ 24h |
| B | breaker fired, `watchdog_silence` (rung 1), `backup_failed`, `breaker_mark_stale` (all causes: `stale_mark`, `not_reconciled`, `pnl_error`), partner-initiated kills | Acknowledge ≤ 24h; resolution or explicit accept-and-monitor note ≤ 72h |

- **Backups**: periodic loop enabled with
  `CONTROLPLANE_BACKUP_INTERVAL_HOURS` ≤ 24; retention
  (`CONTROLPLANE_BACKUP_RETAIN`) keeps ≥ 7 artifacts; the newest
  artifact plus its SHA-256 is copied OFF-HOST daily and the copy
  logged (retention deletion means the on-VM list cannot evidence
  early days at Day 30); weekly offline `cmd/backupverify` against
  the newest artifact, result logged. **RPO ≤ 24h** follows from
  the interval.
- **RTO target ≤ 4h** using RUNBOOK §3 including the restore-gate
  ack and the mandatory post-restore safety diff.
- **Availability bar**: no single continuous control-plane outage
  > 4h, and every restart is followed by a completed startup
  reconcile (`GET /api/v1/oms/recon/status`). The watchdog ladder
  makes agent-plane outages safe by construction (ENTRY sweep at
  90 s, kill at 10 min); an agent-plane outage is an incident but
  not an availability breach.

## BP-6 Success metrics (Day-30 evaluation — every row from
persisted or hash-chained, off-host-copied evidence; a bare operator
assertion is not evidence for any row)

| # | Metric | Bar | Evidence source |
|---|---|---|---|
| M1 | Invariant violations (BP-8 classes V1–V9) | 0 | weekly + Day-30 audit reports |
| M2 | Venue orders outside the Risk Gate / safety-path taxonomy | 0 | V1 audit query (orders ↔ verdicts join) |
| M3 | Continuous control-plane outage | none > 4h; startup reconcile completed after every start | systemd journal + recon status |
| M4 | Reconciler convergence | every restart reconcile completes; no unresolved orphan intent > 24h | recon status API, oms tables |
| M5 | Backup discipline | every counted UTC day covered: an artifact was created in it (verified by construction — verify-failed artifacts are renamed `.failed` and never listed, OB-9), OR the loop ran on schedule and the day's gap is ≤ one interval after a logged restart (OB-10 start-anchored loop); every `backup_failed` resolved per SLA | daily off-host copy log (name + SHA-256), `GET /api/v1/ops/backups`, alerts feed |
| M6 | Alert delivery | notifier watermark caught up at audit time; 0 poison-row skips; heartbeats present for every `heartbeat_hours` window | `alert_dispatch_state` table (direct sqlite read — a read API is an explicit non-goal, AN §Non-goals) + receiver log |
| M7 | SLA adherence | 100% class-A, ≥ 90% class-B within commitment | beta log (hash-chained, BP-9) vs persisted `recorded_at`; receiver raw log as tiebreaker |
| M8 | Partner activity floor | ≥ 15 counted days have ≥ 1 gate-passed proposal (verdict decision `approve` or `clip`; `escalate` counts only once human-approved), and ≥ 1 filled ENTRY order over the beta; days under the BP-4 25% cap floor do not count | verdicts/orders tables + limit-change audit trail |

M8 exists because a silent strategy proves nothing: the criterion is
"completes 30 days of live beta", not "keeps a process alive for 30
days". If the partner cannot meet M8 the beta is INCONCLUSIVE, not
failed — see BP-9 outcomes.

## BP-7 Stop criteria (abort NOW, fix, clock restarts after the fix
lands through the normal audit process)

- Any confirmed V1–V9 finding (BP-8).
- Venue credential exposure in any log, response body, alert detail,
  or repo artifact (the `redaction-pinned` taxonomy failed in the
  field).
- Data loss beyond the 24h RPO, or a restore that cannot pass the
  post-restore safety diff.
- Funds movement that trading cannot explain (non-custodial keys
  failed) — also an immediate key revocation.
- The partner requests stop (their capital, their call). This ends
  the attempt without prejudice; restart needs a fresh Day-0 gate.

Safety machinery firing (kill, breaker, watchdog, restore gate) is
NOT a stop criterion; failing to fire when its spec says it must is
(that is V4/V5 territory).

## BP-8 Invariant audit (weekly + Day-30; findings are violations
only when confirmed against the normative spec text)

Each audit runs against the live `control.db` (read APIs) plus the
newest backup artifact, and is logged with the queries used:

- **V1 — LLM→order path** (ARCHITECTURE invariant 1): every
  proposal-originated order row joins to a persisted RiskVerdict via
  `proposal_id`; zero orders lack the join; every non-proposal order
  matches the safety-path taxonomy (SL/TP, kill/breaker/watchdog
  cancels, flatten). Any unexplained order = violation.
- **V2 — protective stops** (invariant 2): for every filled ENTRY,
  exchange-resident SL/TP placed by the OMS within the spec deadline
  or the `sl_deadline_contingency` companion alert fired and the
  position was flattened; no SL/TP mutation originates from
  agent-plane traffic.
- **V3 — plane boundary**: agent-plane host/env holds no venue
  credentials (config inspection); every agent-plane call used the
  strategy-scoped token; zero `STRATEGY_SCOPE_MISMATCH` bypasses.
- **V4 — kill/breaker semantics**: for every kill and breaker row:
  ENTRY orders canceled, protective stops preserved until flatten
  filled, lifecycle locked, NO auto-restart (state stayed `killed`
  until a human `kill_clear_events` row), effects converged across
  any restart in the window (`DriveSafetyEffects`).
- **V5 — watchdog ladder**: every silence > 90 s produced the ENTRY
  sweep on every tick and the `watchdog_silence` alert (deduped
  ≤ 1 per strategy per UTC day per the WD spec — a missing second
  same-day alert is NOT a finding); every silence > 10 min (or 90 s
  unprotected) produced the strategy-tier kill by actor `watchdog`.
- **V6 — credential storage** (invariant 6): venue keys never
  readable back through any API; grep of logs/alert details for key
  material comes back empty (report pass/fail only, never values).
- **V7 — append-only audit trail** (invariant 7): proposal → verdict
  → order → fill linkage intact for every trade; audit tables only
  ever grew versus the previous audit's reference artifact — each
  weekly audit copies its reference artifact off-host so OB-9
  retention cannot rotate the comparison base away; no VACUUM ran
  (rowid stability — the no-VACUUM rule).
- **V8 — decimal + tenant isolation**: money/size fields remain
  decimal strings end-to-end on sampled trades (contract fixtures
  green on the deployed build); cross-tenant reads with the beta
  tenant's tokens return 404 (no existence oracle).
- **V9 — change control**: replay the append-only limit-change audit
  trail (who, when, old → new — `risk-limits.md`) for the certified
  strategy; any loosening or whitelist widening that the beta log
  does not pair with a clock restart is a confirmed violation.

## BP-9 Beta log, exit review, and outcomes

- **Beta log**: an append-only operator file recording: Day-0
  evidence (BP-2 items 1–10), every incident with `recorded_at` →
  acknowledgment → resolution timestamps, every upgrade (build ids),
  every limit change, weekly audit reports. The log is evidence, not
  narrative — every entry cites its source (envelope `(source, id)`,
  alert id, journal line, artifact name). Integrity is not
  honor-based: each entry appends the SHA-256 of the previous entry
  (hash chain), and the log is copied off-host daily alongside the
  backup off-host copy; the exit reviewer verifies the chain and
  diffs against the off-host copies. The entire M7 input rides on
  these timestamps — an unverifiable log fails M7, not the reviewer.
- **Exit review** (within 7 days of the 30th counted day): operator
  + one reviewer who did not operate the beta walk M1–M8 and the
  audit reports. The outcome mapping is TOTAL — every run terminates
  in exactly one of the three, evaluated in this order:
  1. **FAIL** — any stop criterion was hit, or M1/M2 nonzero: fix
     through the normal spec→review→drill process; the clock
     restarts at a fresh Day-0 gate.
  2. **INCONCLUSIVE** — no FAIL condition, and any of M3–M8 missed:
     extend up to 30 more days (ONE extension max, envelope frozen,
     no clock restart). A second miss after the extension is FAIL.
  3. **PASS** — all of M1–M8 met: tick the PLAN.md checkbox with the
     evidence attached. Phase 3 exit is complete.
- The PLAN.md checkbox is ticked only by the exit review, only on
  PASS, and only with the beta log attached — never on operator
  assertion alone.
