# ADR-0003: Decimal strings for all money; no binary floats

Status: accepted · Date: 2026-07-04

## Context

Money crosses three runtimes (Go, Python, TS) plus JSON. JSON numbers are
parsed as IEEE-754 doubles by default; binary floats cannot represent values
like 0.1 exactly, and rounding differs across languages. Exchanges reject
orders with wrong tick/step precision; accounting drift breaks the audit-grade
track record. Freqtrade/nautilus-class systems all use exact decimal types.

## Decision

- In all JSON contracts, every money/price/size field is a **string** matching
  `^[0-9]+(\.[0-9]+)?$` (no exponent, no sign): `size_quote`, `limit_price`,
  `stop_loss`, `take_profit`, `cost_usd`, `clipped_size_quote`,
  quote-denominated limits.
- In code: Go uses `shopspring/decimal`; Python uses `decimal.Decimal`
  (pydantic-validated); TS keeps strings end-to-end for display, using a
  decimal lib only if arithmetic is ever needed client-side.
- float32/float64 for money is forbidden and lint-enforced where practical.
  `confidence` and `max_drawdown_pct` are ratios, not money — plain JSON
  numbers are acceptable there.

## Consequences

- Exact round-trip across planes; contract tests can compare values verbatim.
- Slightly more verbose code (no native operators in Go) — accepted.
- Validators must check the string pattern; schemas already encode it.
