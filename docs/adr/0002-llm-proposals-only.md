# ADR-0002: LLMs emit proposals only; never orders

Status: accepted · Date: 2026-07-04

## Context

LLM outputs are non-deterministic and can be manipulated (prompt injection via
news/social feeds, hallucinated prices/sizes, unit confusion). Customer funds
are at stake. Research (TradingAgents, ai-hedge-fund) converges on the same
shape: LLM layers propose, a deterministic final gate disposes.

## Decision

The only path from an LLM to an exchange order is:

```
TradeProposal (agent-plane) → deterministic Risk Gate (Go) → OMS (Go) → exchange
```

- agent-plane produces `TradeProposal` JSON (`contracts/proposal.schema.json`)
  and nothing else; it holds no exchange credentials and cannot write to order
  tables (safety invariant 1; boundary rules in `docs/ARCHITECTURE.md`).
- The Risk Gate is plain Go code — no LLM calls — and emits a persisted
  `RiskVerdict` (approve/reject/clip/escalate) for every schema-valid proposal.
- Only the OMS talks to exchanges; SL/TP are placed exchange-resident and
  reduce-only (invariant 2), never managed by LLM loops.
- Every proposal/verdict/order is persisted append-only (invariant 7).

## Consequences

- A fooled or hallucinating LLM can waste tokens but cannot exceed hard limits.
- Latency-sensitive protection (stops) does not depend on LLM availability.
- Cost: an extra serialization hop and a contract to maintain (ADR-0003,
  `docs/specs/proposal-contract.md`); accepted as the price of safety.
