# AlphaMintX Delivery Plan (Phase 0 → 3)

Each phase MUST meet all exit criteria before the next phase starts.
v1 sells paper trading + reasoning; real money only after the Risk Gate is proven.

## Phase 0 — Skeleton, specs, deterministic paper loop

Scope:
- Monorepo skeleton (`control-plane/`, `agent-plane/`, `web/`, `contracts/`, `docs/`).
- This spec set: architecture, contracts, risk limits, strategy lifecycle, ADRs.
- Agent pipeline running with **StubLLM** (canned responses per role, no network).
- Deterministic paper loop: stub pipeline → TradeProposal → Risk Gate → paper OMS
  (fixed fill model) → persisted proposal/verdict/order records.

Exit criteria:
- [x] `contracts/*.schema.json` validate; Go and Python contract tests both pass on
      all `contracts/fixtures/` (valid fixtures accepted, invalid fixture rejected).
- [x] End-to-end paper run in CI with StubLLM is bit-deterministic (same inputs ⇒
      same proposals, verdicts, paper fills) and requires no network — `make
      e2e-check` double-runs both planes and diffs against committed
      `e2e/golden/*.jsonl`, pinning reason codes and clip sizes per scenario.
- [x] Risk Gate unit tests cover every limit in `docs/specs/risk-limits.md`,
      including kill-switch precedence, circuit breaker, the `close` exit
      exemption, per-strategy serialization (concurrent proposals), and
      cross-strategy token-scope rejection.
- [x] `make test` green at repo root (go vet + go test -race, ruff + mypy + pytest).

## Phase 1 — Paper trading with real data + real LLMs + reasoning viewer

Scope:
- Real market data feeds (testnet/public endpoints); paper OMS with slippage +
  taker-fee fill model; SL/TP simulated as exchange-resident.
- Real LLM calls via mintrouter only; per-role model config; per-node cost
  accounting into `model_costs`; token budgets per strategy per day.
- Web reasoning viewer: analyst summaries, debate, proposal, verdict, trace.
- L0/L1 semantics in UI (advisor feed; copilot approve/reject with timeout).

Progress (specs + foundations landed; specs in `docs/specs/market-data.md`,
`llm-routing-and-budget.md`, `persistence-and-api.md`):
- [x] `marketdata` package: Binance WS/REST feed + deterministic ReplayFeed +
      staleness store (fail-closed `MARK_PRICE_UNAVAILABLE`).
- [x] Paper OMS fill model v2: directional slippage, taker/maker fees, clip-notional
      invariant, gap-through-stop, queued zero-mark exits, per-tick trigger sweep.
- [x] MintRouter client: retry/backoff taxonomy, Decimal cost accounting + price
      table, advisory daily token budget (fail-closed on corruption), forced-hold
      degradation paths; StubLLM remains the CI default.
- [x] SQLite persistence (`modernc.org/sqlite`): 17-table append-only schema,
      idempotent proposal/trace ingest, authoritative token ledger, restart-safe
      L1 approvals; `contracts/agent_trace.schema.json`.
- [x] Control-plane HTTP API: two-token auth, L1 approve/reject with preflight
      (`approved_but_blocked`), trace ingestion with scope check, rate limiting.
- [x] Web reasoning viewer: strategies → runs → trace (analysts, debate, costs,
      orders/fills, approvals timeline); operator token server-side only.
- [x] Serve-mode live wiring: `POST .../proposals` ingestion (idempotent, per-strategy
      30/min + serialization, envelope response), `runstate` hydrator (unrealized PnL
      folded into equity/daily-loss, persisted peak), `omsbridge` (paper OMS restore,
      transactional sweep persistence, Submitter with action dispatch + escalate cap),
      Binance feed writer firing the trigger sweep on every mark write.
- [x] Agent-plane live scheduler: asyncio loop per strategy, monotonic no-gap ticks,
      LangGraph SqliteSaver checkpoint/resume (resume skips snapshot, re-POST is
      idempotent), httpx control-plane client (retry taxonomy, Retry-After clamped),
      trace envelope builder, Binance read-only snapshot provider.
- [x] CI plane-boundary gate (`scripts/check_plane_boundary.py`, `make boundary-check`,
      CI `boundary` job): no direct LLM providers in agent-plane, no control-plane DB
      access, no exchange trading surface, no LLM calls from control-plane.
- [x] Live smoke run (stub LLM, live Binance marks via the data-only mirror,
      10 s ticks): full loop verified end-to-end — proposals/traces 200,
      paper fills + trigger sweeps persisted, `DAILY_LOSS_LIMIT_BREACHED`
      rejections from hydrated state, crash-resume with no tick gap, and a
      two-scheduler race survived by idempotency (duplicate proposal replayed
      verbatim, divergent trace 409-rejected append-only). Fixes from the run:
      traces endpoint 201→200 (wire break), Binance endpoint override env vars,
      web `orderSchema.take_profit` drift, trace-conflict WARNING on re-drive,
      scheduler single-instance flock, 409 error-code key (`code`).

Exit criteria:
- [ ] A strategy runs continuously ≥7 days in paper against live market data with
      zero unhandled pipeline failures (checkpoint/resume verified). (Soak run
      started 2026-07-04 against the Binance data-only mirror.)
- [x] Every run's full agent trace is persisted and viewable end-to-end in the web UI.
      (2026-07-04: same-origin deployment landed — Next.js rewrites proxy
      `/api/v1/*` from `CONTROLPLANE_API_BASE_URL`, persistence-and-api.md
      §Auth. Evidence against the live soak: all 86 runs had traces parsing
      the viewer's own zod schemas, and a real-browser render check of a live
      run-detail page showed every trace section — analyst summaries, debate,
      trader decision, verdict, model costs — with zero console errors. The
      check also caught and fixed a web drift: `paper` was labeled
      advisory-only, contradicting §L0/L1 execution semantics.)
- [x] LLM cost per strategy metered and visible; daily token budget enforced.
- [x] No direct provider calls anywhere (verified by CI check / egress policy).

## Phase 2 — Backtest tooling, multi-tenant, billing

Scope:
- Backtest engine sharing the exact strategy/pipeline code with paper (parity)
  (spec: `docs/specs/backtest-engine.md` — candle-close replay clock with
  pinned OHLC sub-tick pumping, two-stage recorded-proposal architecture,
  isolated `backtest.db`).
- Lookahead-bias detection (progressive data-masking re-runs, freqtrade pattern).
- Multi-tenant: tenant isolation, RBAC enforcement, per-tenant kill-switch.
- Billing on metered LLM cost + subscription plans (mintrouter patterns).

Exit criteria:
- [x] Backtest vs paper parity test: same code, same data window ⇒ same trades.
      (2026-07-04: parity is defined against candle-driven replay-paper,
      backtest-engine.md §Goals. The replay runs the IDENTICAL
      `riskgate.Evaluate` + paper-OMS fill-model-v2 code as the e2e
      replay-paper harness; `make backtest-check` pins byte-identical
      proposals and records across double runs and against committed
      goldens, incl. intra-candle stop fills, TP fills, a grid gap, and a
      boundary no-lookahead regression — decision-t mark is close(t),
      never open(t+1).)
- [x] Lookahead check passes on all shipped strategy templates.
      (2026-07-04: M1 physical-truncation vs M0 masking byte-agreement and
      M2 independent-slice snapshot-hash recheck pass for both shipped
      templates — bullish and low_confidence — on the golden dataset, and
      for bullish on a 72-candle live-fetched BTC/USDT 1h dataset; M1+M2
      run in CI via `make backtest-check`. Scope: deterministic tier only,
      backtest-engine.md §Lookahead NORMATIVE LIMITATION.)
- [x] RBAC matrix tests: Trader cannot change limits; no role reads back API keys.
      (2026-07-04: `docs/specs/multi-tenant-rbac.md` implemented —
      `TestRBACMatrix` iterates the exported permissions table (routes are
      REGISTERED from it; registered-route enumeration equality enforced)
      over every principal: 4 DB roles, agent, 4 env classes, no-token.
      trader × `POST .../limits` ⇒ 403 pinned; `TestTokenNeverReadBack`
      pins that mint returns the plaintext exactly once and no list/detail/
      error surface ever returns plaintext or `token_hash` — the same
      invariant wording binds Phase 3 venue keys.)
- [x] Tenant A cannot read or affect tenant B data (isolation tests).
      (2026-07-04: `TestTenantIsolation_CrossRead404` (404 identical to
      absence, no existence oracle), `_CrossApproval404` (both shapes:
      foreign path AND own path + foreign verdict_id), `_CrossKill404`,
      `_KillDoesNotBleedAcrossTenants` (tenant B gate AND approval
      preflight unaffected by tenant A kill — normative 3-clause SQL),
      `_AgentCrossStrategy403`, `_ListsExcludeForeignRows`. Env tokens are
      platform-scoped deployer credentials by spec; tenant principals are
      DB tokens only.)
- [x] Billing invoices reconcile with mintrouter metering.
      (2026-07-04: `docs/specs/billing-and-metering.md` implemented —
      model_costs is the billable source, the imported mintrouter
      spend-log export is the check, joined per-attempt by `X-Request-Id`.
      Reconciliation PASS is the pinned exact-decimal identity
      matched + orphan_client + estimated_client + unattributed ==
      invoice total, with every divergence enumerated and classified
      (golden test: full-match PASS + one injected discrepancy per
      class). Non-vacuous live evidence: real `MintRouterLLM` against a
      local stub gateway → trace with request_ids ingested → export
      imported (idempotent re-import skips) → period closed
      (`inv-tenant-smoke-2026-06`, exact total) → reconcile `pass`,
      matched_count 3, matched 0.00084 + unattributed 0.816129 ==
      0.816969 exact, over a copy of the live soak control.db (2988
      legacy rows = the unattributed class; migration additive).
      v1 meters raw LLM cost; plan pricing/payment deferred.)

## Phase 3 — Live trading beta

Scope:
- Live OMS: testnet-first defaults, live endpoints behind explicit flag; small
  notional caps; strict symbol whitelist; trade-only (non-custodial) API keys.
- Paper-gate enforcement for promotion (see `docs/specs/strategy-lifecycle.md`).
  (2026-07-05: landed per `docs/specs/lifecycle-api.md` — lifecycle
  transition endpoint `POST /api/v1/strategies/{id}/lifecycle` (CAS
  persistence, guards from persisted state, kills redirected to the
  kill endpoints), computed unwaivable paper-gate (in-request fill
  replay, `GET .../paper-gate` report), SW-2 kill-clear + unlock
  (`kill_clear_events`, `ActiveKill` predicate, 3 clear endpoints,
  `killed → paper/paused` via the endpoint), LC-16a atomic
  CreateStrategy bootstrap + Open-time migration; unlock/clear/
  lifecycle drills per the spec's §Test obligations.)
- Operator surface (2026-07-05: landed per `docs/specs/operator-surface.md` —
  read APIs `GET .../safety` (single-snapshot kill/clear/breaker/watchdog
  composite), `GET .../alerts` (per-strategy feed), `GET /api/v1/alerts`
  (env-only global feed), and the web ops panel: safety card, alerts feed,
  paper-gate report, lifecycle controls, strategy-tier kill/clear via
  server-side OPERATOR_TOKEN proxies).
- Full audit trail; watchdog (heartbeat loss ⇒ cancel strategy ENTRY orders
  only; protective stops preserved — `docs/specs/risk-limits.md` §Watchdog);
  kill-switch drills at all 3 tiers.
  (2026-07-05: watchdog landed per `docs/specs/watchdog.md` — heartbeat
  receiver `POST /api/v1/strategies/{id}/heartbeat`, in-memory liveness
  (no per-beat persistence), escalation ladder (90 s ⇒ ENTRY sweep +
  `watchdog_silence` alert; 10 min or unprotected exposure ⇒ strategy-tier
  kill by actor `watchdog`), agent-plane 30 s start-anchored sender task;
  drills WD1–WD12 green. REMAINING: `TestTestnetDrill_Watchdog` against
  the REAL Binance testnet.)

Exit criteria:
- [ ] Reconciler proves exchange-is-truth: orphan adoption and gap detection
      tested against testnet outage/restart scenarios.
      (2026-07-05: implementation complete per
      `docs/specs/live-oms-and-reconciler.md` — `internal/exchange`
      (Binance spot adapter + deterministic fake, 4-class error taxonomy,
      redaction-pinned), `internal/oms/live` (write-ahead intent journal +
      transactional send claims, monotone FSM, Reconciler R1–R7, venue
      epochs, protective SL/TP lifecycle with contingency flatten,
      safety-engine ops), additive store DDL (5 tables + 5 guarded ALTERs;
      soak `control.db` copy opens unchanged, 590 runs intact), recon API
      behind `requiresLiveOMS` in the RBAC matrix. Scenario matrix
      S1–S23 + `TestFakeDrill_OutageRestart` (the 5-step drill twin:
      ≥1 intent adopted, ≥1 fill backfilled by trade id, cum-qty identity,
      second-restart zero-dup + watermark>0) all green in CI.
      REMAINING for the checkbox: run `TestTestnetDrill_OutageRestart`
      against the REAL Binance testnet — needs operator-supplied
      `CONTROLPLANE_BINANCE_API_KEY`/`_SECRET` (testnet); the test fails
      on vacuous evidence by construction.)
- [ ] Kill-switch drill executed at strategy, tenant, and platform tier: ENTRY
      orders canceled, protective stops preserved (canceled only after flatten
      fills), optional reduce-only flatten, no auto-restart, effects resumable
      across a control-plane restart.
      (2026-07-05: implementation complete per `docs/specs/safety-wiring.md`
      — all 3 kill endpoints (strategy NEW, tenant extended with flatten,
      platform NEW behind the `KILL-PLATFORM` ack literal), derive-from-state
      `DriveSafetyEffects` re-driver (unserved kill/breaker rows re-drive
      after every reconcile; served markers require a strictly-post-event
      reconcile), standing-kill check in `submitEntry`
      (`KILL_SWITCH_ACTIVE`), `AppendKillLifecycleLock` (live_* → killed,
      in-tx), additive `safety_effects`/`safety_alerts` tables (soak
      `control.db` copy opens unchanged, 590 runs intact). Fake-venue
      drills KD1–KD10 green in CI incl. crash-resume restart, tenant
      no-bleed, platform coverage, no-double-flatten, dust carve-out.
      REMAINING: run `TestTestnetDrill_KillSwitch` against the REAL
      Binance testnet (operator keys required; fails on vacuous evidence).)
- [ ] Circuit breaker fires from the PnL monitor in a live-testnet scenario:
      reduce-only flatten + demote to L0 until next UTC day.
      (2026-07-05: `internal/safety` breaker Monitor implemented — ≤10s
      ticks with open exposure (`CONTROLPLANE_BREAKER_INTERVAL_ACTIVE`,
      default 5s), `Poke` on every booked fill, fires at
      `DailyPnL <= -daily_loss_limit_quote` exactly once per strategy per
      UTC day (derived `BreakerActiveToday` latch — survives restart,
      auto-re-arms at 00:00 UTC), persist-then-execute breaker row +
      flatten via the shared safety driver; limit-unset guard,
      not-reconciled skip, stale-mark fail-open-loud alerts, tick-panic
      recovery. Breaker drills BD1–BD5 green in CI incl. latch-across-
      restart and next-UTC-day re-arm. REMAINING: run
      `TestTestnetDrill_Breaker` against the REAL Binance testnet.)
- [ ] ≥1 design-partner tenant completes 30 days of live beta within limits with
      zero invariant violations in audit review.
