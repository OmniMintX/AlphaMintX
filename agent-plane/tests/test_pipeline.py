"""End-to-end pipeline tests with StubLLM: valid proposals, forced hold, bounded
debate, determinism, and the DryRunTransport client round-trip."""

from __future__ import annotations

from typing import Any

from jsonschema import Draft202012Validator

from alphamintx_agent_plane.client.controlplane import (
    ControlPlaneClient,
    DryRunTransport,
    StrategyAuth,
)
from alphamintx_agent_plane.contract.models import Action, Decision, EntryType
from alphamintx_agent_plane.llm.stub import (
    ROLE_BEAR_RESEARCHER,
    ROLE_BULL_RESEARCHER,
    ROLE_DEBATE_JUDGE,
    ROLE_FUNDAMENTAL_ANALYST,
    ROLE_MARKET_ANALYST,
    ROLE_NEWS_ANALYST,
    ROLE_TRADER,
    bullish_scenario,
    low_confidence_scenario,
)
from alphamintx_agent_plane.pipeline.graph import PipelineInput, run_pipeline

STRATEGY_ID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


def _inputs() -> PipelineInput:
    return PipelineInput(
        strategy_id=STRATEGY_ID,
        symbol="BTC/USDT",
        market_data="close=64250.50 range_high_20d=64000.00 rsi=61 volume_ratio=1.8",
        news="ETF inflows continue; no adverse regulatory headlines in the last 24h.",
        fundamentals="On-chain activity flat WoW; funding rates near neutral.",
    )


def test_bullish_scenario_emits_valid_open_long(proposal_schema: dict[str, Any]) -> None:
    state = run_pipeline(bullish_scenario(), _inputs())
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.OPEN_LONG
    assert proposal.stop_loss is not None
    assert proposal.entry.type is EntryType.LIMIT
    dumped = proposal.to_json_dict()
    Draft202012Validator(proposal_schema).validate(dumped)
    nodes = {cost.node for cost in proposal.model_costs}
    assert nodes == {
        ROLE_MARKET_ANALYST,
        ROLE_NEWS_ANALYST,
        ROLE_FUNDAMENTAL_ANALYST,
        ROLE_BULL_RESEARCHER,
        ROLE_BEAR_RESEARCHER,
        ROLE_DEBATE_JUDGE,
        ROLE_TRADER,
    }


def test_low_confidence_forces_hold(proposal_schema: dict[str, Any]) -> None:
    state = run_pipeline(low_confidence_scenario(), _inputs())
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert proposal.confidence < 0.3
    dumped = proposal.to_json_dict()
    assert dumped["size_quote"] == "0"
    assert "stop_loss" not in dumped
    assert "take_profit" not in dumped
    assert dumped["entry"] == {"type": "market"}
    Draft202012Validator(proposal_schema).validate(dumped)


def test_debate_runs_exactly_two_rounds() -> None:
    state = run_pipeline(bullish_scenario(), _inputs())
    assert len(state["debate_rounds"]) == 2
    assert [debate_round.round_index for debate_round in state["debate_rounds"]] == [0, 1]


def test_two_runs_identical_except_ids_and_timestamps() -> None:
    first = run_pipeline(bullish_scenario(), _inputs())["proposal"]
    second = run_pipeline(bullish_scenario(), _inputs())["proposal"]
    assert first is not None and second is not None
    assert first.proposal_id != second.proposal_id
    assert first.agent_trace_id != second.agent_trace_id
    dumped_first = first.to_json_dict()
    dumped_second = second.to_json_dict()
    for volatile in ("proposal_id", "created_at", "agent_trace_id"):
        dumped_first.pop(volatile)
        dumped_second.pop(volatile)
    assert dumped_first == dumped_second


def test_dry_run_client_round_trip(verdict_schema: dict[str, Any]) -> None:
    state = run_pipeline(bullish_scenario(), _inputs())
    proposal = state["proposal"]
    assert proposal is not None
    auth = StrategyAuth(strategy_id=STRATEGY_ID, bearer_token="phase0-test-token")
    client = ControlPlaneClient(DryRunTransport(), auth)
    submission = client.submit_proposal(proposal, tick_number=0)
    assert submission.verdict.decision is Decision.APPROVE
    assert submission.verdict.proposal_id == proposal.proposal_id
    assert submission.submitted is True
    assert submission.pending_approval is False
    Draft202012Validator(verdict_schema).validate(submission.verdict.to_json_dict())
    client.heartbeat()
