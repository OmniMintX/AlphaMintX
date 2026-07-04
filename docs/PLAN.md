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

Exit criteria:
- [ ] A strategy runs continuously ≥7 days in paper against live market data with
      zero unhandled pipeline failures (checkpoint/resume verified).
- [ ] Every run's full agent trace is persisted and viewable end-to-end in the web UI.
- [ ] LLM cost per strategy metered and visible; daily token budget enforced.
- [ ] No direct provider calls anywhere (verified by CI check / egress policy).

## Phase 2 — Backtest tooling, multi-tenant, billing

Scope:
- Backtest engine sharing the exact strategy/pipeline code with paper (parity).
- Lookahead-bias detection (progressive data-masking re-runs, freqtrade pattern).
- Multi-tenant: tenant isolation, RBAC enforcement, per-tenant kill-switch.
- Billing on metered LLM cost + subscription plans (mintrouter patterns).

Exit criteria:
- [ ] Backtest vs paper parity test: same code, same data window ⇒ same trades.
- [ ] Lookahead check passes on all shipped strategy templates.
- [ ] RBAC matrix tests: Trader cannot change limits; no role reads back API keys.
- [ ] Tenant A cannot read or affect tenant B data (isolation tests).
- [ ] Billing invoices reconcile with mintrouter metering.

## Phase 3 — Live trading beta

Scope:
- Live OMS: testnet-first defaults, live endpoints behind explicit flag; small
  notional caps; strict symbol whitelist; trade-only (non-custodial) API keys.
- Paper-gate enforcement for promotion (see `docs/specs/strategy-lifecycle.md`).
- Full audit trail; watchdog (heartbeat loss ⇒ cancel strategy ENTRY orders
  only; protective stops preserved — `docs/specs/risk-limits.md` §Watchdog);
  kill-switch drills at all 3 tiers.

Exit criteria:
- [ ] Reconciler proves exchange-is-truth: orphan adoption and gap detection
      tested against testnet outage/restart scenarios.
- [ ] Kill-switch drill executed at strategy, tenant, and platform tier: ENTRY
      orders canceled, protective stops preserved (canceled only after flatten
      fills), optional reduce-only flatten, no auto-restart, effects resumable
      across a control-plane restart.
- [ ] Circuit breaker fires from the PnL monitor in a live-testnet scenario:
      reduce-only flatten + demote to L0 until next UTC day.
- [ ] ≥1 design-partner tenant completes 30 days of live beta within limits with
      zero invariant violations in audit review.
