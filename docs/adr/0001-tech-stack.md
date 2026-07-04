# ADR-0001: Tech stack per plane

Status: accepted · Date: 2026-07-04

## Context

AlphaMintX needs a low-latency, correctness-critical execution side (OMS, Risk
Gate, exchange connectivity) and a fast-iterating LLM agent side. The sibling
mintrouter project has proven Go patterns (Gin + GORM, layered internal/*
packages, additive-only migrations, field-level encryption) we can reuse. The
strongest agent-orchestration ecosystem (LangGraph, pydantic structured output)
is Python-only.

## Decision

- `control-plane/`: **Go** — Gin + GORM, mintrouter-style 4-layer layout,
  `go vet`, `go test -race`, golangci-lint.
- `agent-plane/`: **Python 3.12+** — LangGraph pipeline, pydantic contract
  models, uv, ruff, mypy, pytest.
- `web/`: **TypeScript / Next.js** App Router, strict TS, pnpm.
- `contracts/`: language-neutral **JSON Schema (draft 2020-12)** + golden
  fixtures; both planes run contract tests against the same fixtures in CI.
- Conventional commits; PRs must pass CI; secrets never committed.

## Consequences

- Two runtimes to operate; the JSON contract boundary (ADR-0002/0003) keeps the
  interface small and testable cross-language.
- Go plane cannot call LLMs (by design); Python plane cannot reach exchanges
  (by design). Skills split accordingly across contributors.
- mintrouter conventions transfer directly (migrations, crypto, billing hooks).
