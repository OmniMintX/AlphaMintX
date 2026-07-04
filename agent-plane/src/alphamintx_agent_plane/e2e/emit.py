"""Deterministic proposal emitter for the E2E paper loop (interface: e2e/runspec.json).

Reads the Go-owned runspec, builds one TradeProposal per scenario (scenarios 1-2 run
the real LangGraph pipeline with StubLLM; 3-7 are hand-constructed schema-valid
proposals that exercise the Risk Gate), and writes one compact-JSON envelope per
line: ``{"token": ..., "proposal": ...}``. Two runs with the same runspec are
byte-identical: all ids are uuid5 in NAMESPACE_E2E and all timestamps derive from
``clock_start + index * tick_seconds``.
"""

from __future__ import annotations

import argparse
import json
import uuid
from collections.abc import Callable, Sequence
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any

from pydantic import AwareDatetime, BaseModel, ConfigDict, Field

from alphamintx_agent_plane.contract.models import (
    SCHEMA_VERSION,
    Action,
    AnalystSummaries,
    AnalystSummary,
    Entry,
    EntryType,
    Signal,
    TimeInForce,
    TradeProposal,
    UuidStr,
    rfc3339_utc,
)
from alphamintx_agent_plane.llm.stub import LLMClient, bullish_scenario, low_confidence_scenario
from alphamintx_agent_plane.pipeline.graph import PipelineInput, run_pipeline

NAMESPACE_E2E = uuid.uuid5(uuid.NAMESPACE_URL, "https://alphamintx.dev/e2e")

STALE_OFFSET_SECONDS = 120

SCENARIO_BULLISH = "bullish_btc_l3"
SCENARIO_LOW_CONFIDENCE = "low_confidence_hold"
SCENARIO_WHITELIST = "whitelist_violation"
SCENARIO_NOTIONAL_CLIP = "notional_clip"
SCENARIO_CLOSE_EXEMPT = "close_exempt"
SCENARIO_STALE = "stale_proposal"
SCENARIO_SCOPE_MISMATCH = "scope_mismatch"


class RunSpecStrategy(BaseModel):
    """One strategy entry in the runspec; extra fields are a validation error."""

    model_config = ConfigDict(extra="forbid")

    strategy_id: UuidStr
    token: str = Field(min_length=1)
    scenario: str = Field(min_length=1)


class RunSpec(BaseModel):
    """Strict mirror of e2e/runspec.json (owned by the Go builder)."""

    model_config = ConfigDict(extra="forbid")

    clock_start: AwareDatetime
    tick_seconds: int = Field(gt=0)
    seed: int
    quote_currency: str = Field(min_length=1)
    strategies: list[RunSpecStrategy]
    marks: dict[str, list[str]]


def load_runspec(path: str | Path) -> RunSpec:
    """Load and strictly validate a runspec JSON file."""
    with Path(path).open(encoding="utf-8") as handle:
        data = json.load(handle)
    return RunSpec.model_validate(data)


def _id_factory_for(scenario: str, seed: int) -> Callable[[str], uuid.UUID]:
    def factory(field: str) -> uuid.UUID:
        return uuid.uuid5(NAMESPACE_E2E, f"{scenario}/{field}/{seed}")

    return factory


def _slot_time(spec: RunSpec, index: int) -> datetime:
    return spec.clock_start + timedelta(seconds=index * spec.tick_seconds)


def _summaries(text: str) -> AnalystSummaries:
    summary = AnalystSummary(signal=Signal.NEUTRAL, confidence=0.5, summary=text)
    return AnalystSummaries(market=summary, news=summary, fundamental=summary)


def _pipeline_proposal(
    spec: RunSpec, strategy: RunSpecStrategy, index: int, llm: LLMClient
) -> TradeProposal:
    slot = _slot_time(spec, index)
    state = run_pipeline(
        llm,
        PipelineInput(
            strategy_id=strategy.strategy_id,
            symbol="BTC/USDT",
            market_data="close=64250.50 range_high_20d=64000.00 rsi=61 volume_ratio=1.8",
            news="ETF inflows continue; no adverse regulatory headlines in the last 24h.",
            fundamentals="On-chain activity flat WoW; funding rates near neutral.",
        ),
        id_factory=_id_factory_for(strategy.scenario, spec.seed),
        clock=lambda: slot,
    )
    proposal = state["proposal"]
    if proposal is None:
        raise RuntimeError(f"pipeline produced no proposal for scenario {strategy.scenario!r}")
    return proposal


def _bullish_btc_l3(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _pipeline_proposal(spec, strategy, index, bullish_scenario())


def _low_confidence_hold(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _pipeline_proposal(spec, strategy, index, low_confidence_scenario())


def _manual_proposal(
    spec: RunSpec,
    strategy: RunSpecStrategy,
    index: int,
    *,
    symbol: str,
    action: Action,
    size_quote: str,
    stop_loss: str | None,
    reasoning: str,
    created_at_offset_seconds: int = 0,
) -> TradeProposal:
    ids = _id_factory_for(strategy.scenario, spec.seed)
    created_at = _slot_time(spec, index) + timedelta(seconds=created_at_offset_seconds)
    return TradeProposal(
        schema_version=SCHEMA_VERSION,
        proposal_id=str(ids("proposal_id")),
        strategy_id=strategy.strategy_id,
        agent_trace_id=str(ids("agent_trace_id")),
        created_at=rfc3339_utc(created_at),
        symbol=symbol,
        action=action,
        size_quote=size_quote,
        entry=Entry(type=EntryType.MARKET),
        stop_loss=stop_loss,
        time_in_force=TimeInForce.GTC,
        confidence=0.7,
        reasoning=reasoning,
        analyst_summaries=_summaries(f"Hand-constructed E2E scenario: {strategy.scenario}."),
        debate_summary=f"Hand-constructed E2E scenario: {strategy.scenario}.",
        model_costs=[],
    )


def _whitelist_violation(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _manual_proposal(
        spec,
        strategy,
        index,
        symbol="SOL/USDT",
        action=Action.OPEN_LONG,
        size_quote="500.00",
        stop_loss="100.00",
        reasoning="Open long on SOL/USDT, which is outside the whitelist; the gate must reject.",
    )


def _notional_clip(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _manual_proposal(
        spec,
        strategy,
        index,
        symbol="BTC/USDT",
        action=Action.OPEN_LONG,
        size_quote="5000.00",
        stop_loss="60000.00",
        reasoning="Open long sized above the 2000 per-position cap; the gate must clip.",
    )


def _close_exempt(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _manual_proposal(
        spec,
        strategy,
        index,
        symbol="BTC/USDT",
        action=Action.CLOSE,
        size_quote="0",
        stop_loss=None,
        reasoning="Full close of the pre-seeded position; exempt from the breached daily loss.",
    )


def _stale_proposal(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _manual_proposal(
        spec,
        strategy,
        index,
        symbol="BTC/USDT",
        action=Action.OPEN_LONG,
        size_quote="150.00",
        stop_loss="60000.00",
        reasoning="Created 120s before the loop clock; the gate must reject PROPOSAL_STALE.",
        created_at_offset_seconds=-STALE_OFFSET_SECONDS,
    )


def _scope_mismatch(spec: RunSpec, strategy: RunSpecStrategy, index: int) -> TradeProposal:
    return _manual_proposal(
        spec,
        strategy,
        index,
        symbol="BTC/USDT",
        action=Action.OPEN_LONG,
        size_quote="200.00",
        stop_loss="60000.00",
        reasoning="Envelope carries strategy 1's token; ingestion must reject with no verdict.",
    )


ScenarioBuilder = Callable[[RunSpec, RunSpecStrategy, int], TradeProposal]

SCENARIO_BUILDERS: dict[str, ScenarioBuilder] = {
    SCENARIO_BULLISH: _bullish_btc_l3,
    SCENARIO_LOW_CONFIDENCE: _low_confidence_hold,
    SCENARIO_WHITELIST: _whitelist_violation,
    SCENARIO_NOTIONAL_CLIP: _notional_clip,
    SCENARIO_CLOSE_EXEMPT: _close_exempt,
    SCENARIO_STALE: _stale_proposal,
    SCENARIO_SCOPE_MISMATCH: _scope_mismatch,
}

SCENARIO_ORDER: tuple[str, ...] = tuple(SCENARIO_BUILDERS)


def build_envelopes(spec: RunSpec) -> list[dict[str, Any]]:
    """Build the 7 scenario envelopes in runspec order (validated against the contract)."""
    scenarios = tuple(strategy.scenario for strategy in spec.strategies)
    if scenarios != SCENARIO_ORDER:
        raise ValueError(
            f"runspec scenarios must be exactly {list(SCENARIO_ORDER)} in order, got "
            f"{list(scenarios)}"
        )
    envelopes: list[dict[str, Any]] = []
    for index, strategy in enumerate(spec.strategies):
        proposal = SCENARIO_BUILDERS[strategy.scenario](spec, strategy, index)
        token = strategy.token
        if strategy.scenario == SCENARIO_SCOPE_MISMATCH:
            token = spec.strategies[0].token
        envelopes.append({"token": token, "proposal": proposal.to_json_dict()})
    return envelopes


def render(spec: RunSpec) -> str:
    """Render proposals.jsonl: compact JSON, insertion order, LF endings, trailing LF."""
    lines = [json.dumps(envelope, separators=(",", ":")) for envelope in build_envelopes(spec)]
    return "\n".join(lines) + "\n"


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m alphamintx_agent_plane.e2e.emit",
        description="Emit deterministic E2E TradeProposal envelopes from a runspec.",
    )
    parser.add_argument("--runspec", required=True, type=Path, help="path to e2e/runspec.json")
    parser.add_argument("--out", required=True, type=Path, help="path to write proposals.jsonl")
    args = parser.parse_args(argv)
    spec = load_runspec(args.runspec)
    text = render(spec)
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_bytes(text.encode("utf-8"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
