# AlphaMintX Architecture

Status: normative, Phase 0. On conflict, `docs/specs/*` win over this document.

## Monorepo layout

```
AlphaMintX/
├── control-plane/   Go — API, OMS, Risk Gate, exchange connectivity, billing hooks
├── agent-plane/     Python — LangGraph agent pipeline, strategy engine, backtest/paper
├── web/             TypeScript / Next.js — dashboard, reasoning viewer, copilot UI
├── contracts/       JSON Schemas + golden fixtures (cross-language contract tests)
└── docs/            Specs and ADRs (this directory)
```

## Component responsibilities

### control-plane/ (Go, Gin + GORM, mintrouter layering)
- Tenant management, RBAC (Owner / Admin / Trader / Viewer / Platform Admin), auth.
- Strategy lifecycle state machine (`docs/specs/strategy-lifecycle.md`).
- **Risk Gate**: deterministic (no LLM) evaluation of every TradeProposal against
  RiskLimits (`docs/specs/risk-limits.md`). Emits a persisted RiskVerdict.
- **OMS**: the ONLY component that talks to exchanges. Order FSM, idempotent
  clientOrderId, fill reconciliation (ws primary, REST audit), orphan recovery.
- Exchange credential storage: field-level encrypted, write-only (invariant 6).
- Kill-switch endpoints (strategy / tenant / platform) and watchdog.
- Multi-tenant RBAC (`docs/specs/multi-tenant-rbac.md`): fixed roles
  viewer/trader/admin/owner on DB-issued hashed tokens, tenant isolation
  (foreign = 404, no existence oracle), runtime limit changes behind a
  single `LimitsProvider`, tenant-tier kill-switch (Phase 2).
- Backtest engine (`internal/backtest` + `cmd/backtestctl`): historical
  kline replay through the identical Risk Gate + paper OMS path into an
  isolated `backtest.db`; `backtestctl fetch` materializes canonical
  datasets from Binance REST klines (`docs/specs/backtest-engine.md`).
- Billing hooks: meters LLM cost (from `model_costs`) per strategy/tenant.
- Billing + reconciliation (`docs/specs/billing-and-metering.md`): append-only
  invoices generated at period close from `model_costs` (rowid-watermark
  exactly-once window); imported mintrouter spend-log exports are the check,
  joined per-attempt by `X-Request-Id`; reconciliation runs pin an exact
  decimal identity and classify every divergence (Phase 2).

### agent-plane/ (Python 3.12+, LangGraph, pydantic)
- Tier 1: Market / News / Fundamental analysts (parallel fan-out, cheap models).
- Tier 2: Bull vs Bear researcher debate, bounded rounds (default 2).
- Tier 3: Trader agent synthesizes the **TradeProposal** (`contracts/proposal.schema.json`).
- Paper/live strategy engine; strategy code identical across backtest/paper/live.
  Backtest replay/execution is control-plane (`internal/backtest`, Phase 2);
  agent-plane contributes the same pipeline code, run offline by the backtest
  emitter (`docs/specs/backtest-engine.md`).
- Untrusted external text (news/social) is wrapped as data, never as instructions.
- `StubLLM` mode: deterministic canned responses per role for CI (no network).

### web/ (Next.js App Router, strict TS)
- Dashboard, reasoning viewer (proposal + analyst summaries + debate + verdict),
  L1 approve/reject copilot UI, immutable track record, risk settings (Admin),
  kill-switch controls.

### contracts/
- `proposal.schema.json`, `riskverdict.schema.json`, `fixtures/`. Both planes MUST
  validate against these schemas in CI using the shared golden fixtures.

## The ONLY allowed data path: LLM → order

```
                        agent-plane (Python)                    control-plane (Go)
┌──────────────────────────────────────────────────┐   ┌────────────────────────────────┐
│  market data      news feeds      fundamentals   │   │                                │
│      │                │                │         │   │                                │
│      ▼                ▼                ▼         │   │                                │
│  ┌─────────┐    ┌──────────┐    ┌────────────┐   │   │                                │
│  │ Market  │    │  News    │    │Fundamental │   │   │                                │
│  │ Analyst │    │ Analyst  │    │  Analyst   │   │   │                                │
│  └────┬────┘    └────┬─────┘    └─────┬──────┘   │   │                                │
│       └───────┬──────┴────────────────┘          │   │                                │
│               ▼                                  │   │                                │
│      ┌─────────────────┐                         │   │                                │
│      │ Bull ⇄ Bear     │  (≤ max_rounds)         │   │                                │
│      │ Debate          │                         │   │                                │
│      └────────┬────────┘                         │   │                                │
│               ▼                                  │   │                                │
│      ┌─────────────────┐   TradeProposal (HTTP)  │   │  ┌───────────┐   ┌─────┐       │
│      │  Trader Agent   │ ────────────────────────┼───┼─▶│ Risk Gate │──▶│ OMS │──▶ exchange
│      └─────────────────┘   contracts/proposal    │   │  │ (determ.) │   └──┬──┘       │
│               ▲                                  │   │  └─────┬─────┘      │          │
│               │ all LLM calls                    │   │        ▼            ▼          │
│         ┌───────────┐                            │   │   RiskVerdict     orders/fills │
│         │ mintrouter│ (sole LLM gateway)         │   │   (persisted)     (persisted)  │
│         └───────────┘                            │   └────────────────────────────────┘
└──────────────────────────────────────────────────┘
```

`Proposal → Risk Gate → OMS` is the only path from an LLM to an order. Any code
path that lets agent-plane output reach an exchange without a persisted
RiskVerdict is a defect (invariant 1). SL/TP rest on the exchange, placed by the
OMS, never managed by LLM loops (invariant 2).

## Plane boundary rules (normative)

- agent-plane MUST NOT hold exchange credentials, in any form.
- agent-plane MUST NOT open exchange connections (REST or ws) for trading;
  read-only market data feeds are permitted.
- agent-plane MUST NOT write to control-plane DB tables for orders, positions,
  verdicts, or track record. It has no DB grants on those tables.
- agent-plane talks to control-plane exclusively via the control-plane HTTP API
  (submit proposal, fetch strategy config/limits, heartbeat).
- control-plane MUST NOT call LLMs. The Risk Gate is deterministic code only.
- The TradeProposal / RiskVerdict JSON contracts are the single interface
  between planes. Unknown `schema_version` MUST be rejected by both planes.
- Every proposal, verdict, and resulting order MUST be persisted append-only
  (invariant 7) and linked via `proposal_id` / `agent_trace_id`.

## Plane authentication, delivery, and heartbeats (normative)

- Each agent-plane worker holds a **per-strategy bearer token** issued by
  control-plane, scoped to (`strategy_id`, tenant). Control-plane MUST reject
  any request whose `strategy_id` does not match the token scope
  (`STRATEGY_SCOPE_MISMATCH`). Tokens are revoked on kill-switch.
- Control-plane enforces a **per-strategy proposal rate limit** at the API
  layer (default 30/min); excess ⇒ HTTP 429, no persisted verdict.
- Delivery model: proposal submission is **at-least-once**; control-plane
  ingestion is idempotent by `proposal_id` (atomic unique insert). A duplicate
  returns the original verdict verbatim — never re-evaluated, never a second
  order.
- Heartbeat: agent-plane POSTs an authenticated heartbeat **per strategy every
  30 s** (endpoint deferred to the watchdog slice — Phase 3 reaction,
  risk-limits.md §Watchdog; the client stub exists). The watchdog reacts
  after 90 s of silence: it cancels ENTRY orders
  only and alerts; protective reduce-only stops stay on the exchange
  (`docs/specs/risk-limits.md` §Watchdog).

## mintrouter as the sole LLM gateway

All LLM calls from agent-plane route through mintrouter (sibling project) via
`base_url`; direct provider calls are forbidden (ADR-0004). Per-role model
config: cheap models for Tier-1 analysts, expensive model for the Trader agent
only. mintrouter provides metering (token budgets per strategy per day) and the
billing signal consumed by control-plane.
