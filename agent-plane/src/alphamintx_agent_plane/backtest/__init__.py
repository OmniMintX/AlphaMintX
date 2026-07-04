"""Offline backtest tooling (docs/specs/backtest-engine.md): dataset loading,
the stage-1 proposal emitter, and the M1/M2 lookahead checks. Open-loop by
construction — this package makes NO HTTP calls (dataset file in,
proposals.jsonl out)."""
