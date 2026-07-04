# AlphaMintX

AlphaMintX is a SaaS platform for **LLM-driven auto trading**: customers "hire
an AI trader" that analyzes markets, explains its reasoning in natural
language, and — at higher autonomy levels — places orders automatically within
hard, human-set risk limits. A multi-agent LLM pipeline produces trade
*proposals*; a deterministic Go risk gate and order management system are the
only components that ever touch an exchange.

## Monorepo map

| Path | Contents |
|---|---|
| `control-plane/` | Go: API, RBAC, strategy lifecycle, Risk Gate, OMS, exchange connectivity, backtest replay (`backtestctl`), billing hooks |
| `agent-plane/` | Python: LangGraph agent pipeline, strategy engine (paper/live), offline backtest emitter (backtest replay runs in control-plane) |
| `web/` | Next.js: dashboard, reasoning viewer, copilot approve/reject UI, risk settings, kill-switch |
| `contracts/` | JSON Schemas (TradeProposal, RiskVerdict) + golden fixtures for cross-language contract tests |
| `docs/` | Architecture, delivery plan, normative specs, ADRs |

## Safety invariants (non-negotiable)

1. **LLMs never place orders directly.** Only the Go OMS talks to exchanges;
   every order passes the deterministic Risk Gate first.
2. **SL/TP live on the exchange**, not in slow LLM loops; no code path may
   leave an open position without an exchange-resident stop-loss while
   `require_stop_loss=true`.
3. **Autonomy ladder per strategy**: L0 Advisor → L1 Copilot (per-order
   approval, timeout → auto-reject) → L2 Semi-auto (envelope + escalation) →
   L3 Full-auto. Promotion to real money requires a code-enforced paper-gate.
4. **Kill-switch 3 tiers** (strategy / tenant / platform): cancel ENTRY orders
   + optional reduce-only flatten + lock, no auto-restart; protective stops
   are preserved while a position is open (canceled only after flatten fills).
   Circuit breaker: daily loss (incl. fees + funding) hit ⇒ reduce-only
   flatten + demote to L0 for the UTC day. Watchdog on agent heartbeats
   cancels entry orders only.
5. **Risk limits are set by humans (Admin)** — a hard ceiling neither Trader
   users nor AI agents can raise. Right-to-set-limits ≠ right-to-trade.
6. **Exchange API keys are write-only** after save (field-level encryption);
   trade-only, never withdrawal-enabled (non-custodial).
7. **Track record is immutable/append-only**; backtests free of lookahead
   bias; strategy code identical across backtest / paper / live.

## Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — planes, boundaries, the only LLM→order path
- [`docs/PLAN.md`](docs/PLAN.md) — Phase 0→3 delivery plan with exit criteria
- [`docs/specs/proposal-contract.md`](docs/specs/proposal-contract.md) — TradeProposal / RiskVerdict semantics
- [`docs/specs/risk-limits.md`](docs/specs/risk-limits.md) — RiskLimits v1, gate order, circuit breaker, kill-switch
- [`docs/specs/strategy-lifecycle.md`](docs/specs/strategy-lifecycle.md) — lifecycle states + autonomy ladder
- [`docs/specs/multi-tenant-rbac.md`](docs/specs/multi-tenant-rbac.md) — tenants, roles, DB tokens, isolation, tenant kill-switch
- [`docs/adr/`](docs/adr/) — ADR-0001 tech stack · ADR-0002 proposals-only · ADR-0003 decimal money · ADR-0004 mintrouter gateway
- Contracts: [`contracts/proposal.schema.json`](contracts/proposal.schema.json) · [`contracts/riskverdict.schema.json`](contracts/riskverdict.schema.json) · [`contracts/fixtures/`](contracts/fixtures/)

## Quickstart (Phase 0)

Component skeletons (`control-plane/`, `agent-plane/`, `web/`) are in place;
see `docs/PLAN.md` for the remaining exit criteria.

```sh
make check            # all of the below (alias: make test)
make go-check         # control-plane: go build + vet + gofmt + go test -race
make py-check         # agent-plane:   uv sync + ruff + mypy + pytest
make web-check        # web:           pnpm install + typecheck + vitest + next build
make contracts-check  # schemas + golden fixtures (uv run --with jsonschema)
```
