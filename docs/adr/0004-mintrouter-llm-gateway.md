# ADR-0004: mintrouter is the sole LLM gateway

Status: accepted · Date: 2026-07-04

## Context

The agent pipeline calls multiple models per run (cheap Tier-1 analysts,
expensive Trader agent). LLM cost must be metered per strategy and billed to
tenants; token budgets must be enforceable; provider keys must not sprawl
across services. The sibling mintrouter project already provides routing,
metering, quotas, and billing patterns.

## Decision

- All LLM calls from agent-plane go through **mintrouter** via a configured
  `base_url`. Direct provider API calls are forbidden anywhere in the repo.
- Per-role model configuration: cheap models for market/news/fundamental
  analysts and debate; the expensive model only for the Trader agent.
- Per-node token/cost accounting is accumulated into the proposal's
  `model_costs` array; mintrouter metering is the billing source of truth,
  reconciled against `model_costs`.
- Token budgets per strategy per day are enforced at the gateway; budget
  exhaustion fails the pipeline run cleanly (no proposal, no order).
- CI/egress policy verifies no direct provider endpoints are referenced.

## Consequences

- Single choke point for cost, quotas, provider keys, and model swaps.
- mintrouter availability becomes a dependency of live pipelines — acceptable:
  exchange-resident SL/TP (invariant 2) protects open positions regardless.
- StubLLM mode (Phase 0) bypasses the network entirely and needs no gateway.
