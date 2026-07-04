"""build_trace_envelope output must validate against contracts/agent_trace.schema.json."""

from __future__ import annotations

from decimal import Decimal
from typing import Any

from jsonschema import Draft202012Validator

from alphamintx_agent_plane.contract.models import TraceModelCost
from alphamintx_agent_plane.llm.stub import bullish_scenario, low_confidence_scenario
from alphamintx_agent_plane.pipeline.graph import PipelineInput, PipelineState, run_pipeline
from alphamintx_agent_plane.pipeline.trace import BudgetState, build_trace_envelope

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
STARTED_AT = "2026-07-04T12:00:00Z"
COMPLETED_AT = "2026-07-04T12:00:07Z"


def _state(bullish: bool = True) -> PipelineState:
    llm = bullish_scenario() if bullish else low_confidence_scenario()
    return run_pipeline(
        llm,
        PipelineInput(
            strategy_id=SID,
            symbol="BTC/USDT",
            market_data="close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50",
            news="no news feed in phase 1",
            fundamentals="no fundamentals feed in phase 1",
        ),
    )


def _envelope(state: PipelineState, proposal_id: str | None) -> dict[str, Any]:
    return build_trace_envelope(
        state,
        strategy_id=SID,
        tick_number=3,
        started_at=STARTED_AT,
        completed_at=COMPLETED_AT,
        proposal_id=proposal_id,
        budget_state=BudgetState(
            utc_date="2026-07-04", tokens_used=1234, cost_usd_used="0.001234"
        ),
    )


def test_envelope_validates_against_schema(trace_schema: dict[str, Any]) -> None:
    state = _state()
    proposal = state["proposal"]
    assert proposal is not None
    envelope = _envelope(state, proposal.proposal_id)
    Draft202012Validator(trace_schema).validate(envelope)
    assert envelope["schema_version"] == "1.0"
    assert envelope["run_id"] == proposal.agent_trace_id
    assert envelope["tick_number"] == 3
    assert envelope["started_at"] == STARTED_AT
    assert envelope["completed_at"] == COMPLETED_AT
    assert envelope["proposal_id"] == proposal.proposal_id
    assert len(envelope["debate_rounds"]) == 2
    assert {cost["node"] for cost in envelope["model_costs"]} >= {"trader"}
    assert envelope["budget_state"] == {
        "utc_date": "2026-07-04",
        "tokens_used": 1234,
        "cost_usd_used": "0.001234",
    }


def test_null_proposal_id_validates(trace_schema: dict[str, Any]) -> None:
    # proposal_id is null ONLY when the proposal POST failed after retries.
    envelope = _envelope(_state(), None)
    Draft202012Validator(trace_schema).validate(envelope)
    assert envelope["proposal_id"] is None


def test_forced_hold_state_still_builds_a_valid_envelope(
    trace_schema: dict[str, Any],
) -> None:
    state = _state(bullish=False)
    proposal = state["proposal"]
    assert proposal is not None
    envelope = _envelope(state, proposal.proposal_id)
    Draft202012Validator(trace_schema).validate(envelope)
    assert envelope["debate_summary"]  # never empty: fallback marker when skipped


def test_run_id_present_even_without_proposal(trace_schema: dict[str, Any]) -> None:
    state = _state()
    state["proposal"] = None  # simulate a run that ended without a proposal
    envelope = _envelope(state, None)
    Draft202012Validator(trace_schema).validate(envelope)
    assert envelope["run_id"] == state["agent_trace_id"]


def test_stub_entries_serialize_byte_identical_to_pre_upgrade_shape(
    trace_schema: dict[str, Any],
) -> None:
    """Hash stability (billing-and-metering.md): stub-mode entries carry neither
    request_id nor estimated, so they serialize exactly the pre-upgrade keys."""
    envelope = _envelope(_state(), None)
    Draft202012Validator(trace_schema).validate(envelope)
    assert envelope["model_costs"]
    for entry in envelope["model_costs"]:
        assert set(entry) == {"node", "model", "input_tokens", "output_tokens", "cost_usd"}


def test_entries_with_request_id_and_estimated_serialize_the_fields(
    trace_schema: dict[str, Any],
) -> None:
    measured_id = "0f8fad5b-d9cb-469f-a165-70867728950e"
    estimated_id = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
    state = _state()
    state["model_costs"] = [
        TraceModelCost(
            node="market_analyst",
            model="gpt-4o-mini",
            input_tokens=120,
            output_tokens=0,
            cost_usd=Decimal("0.000018"),
            request_id=estimated_id,
            estimated=True,
        ),
        TraceModelCost(
            node="trader",
            model="gpt-4o-mini",
            input_tokens=100,
            output_tokens=50,
            cost_usd=Decimal("0.000045"),
            request_id=measured_id,
        ),
        TraceModelCost(
            node="legacy",
            model="gpt-4o-mini",
            input_tokens=10,
            output_tokens=5,
            cost_usd=Decimal("0.000005"),
        ),
    ]
    envelope = _envelope(state, None)
    Draft202012Validator(trace_schema).validate(envelope)
    estimated_entry, measured_entry, legacy_entry = envelope["model_costs"]
    assert estimated_entry["request_id"] == estimated_id
    assert estimated_entry["estimated"] is True
    assert measured_entry["request_id"] == measured_id
    assert "estimated" not in measured_entry
    assert set(legacy_entry) == {"node", "model", "input_tokens", "output_tokens", "cost_usd"}
