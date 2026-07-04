"""Golden-fixture contract tests: pydantic models must round-trip contracts/fixtures/*.json
and their JSON output must validate against the actual JSON Schemas (belt and braces)."""

from __future__ import annotations

import json
from typing import Any

import pytest
from jsonschema import Draft202012Validator
from pydantic import ValidationError

from alphamintx_agent_plane.contract.models import (
    Decision,
    ModelCost,
    RiskVerdict,
    TraceModelCost,
    TradeProposal,
)
from conftest import FIXTURES_DIR, load_json

_COST_FIELDS: dict[str, Any] = {
    "node": "trader",
    "model": "gpt-4o-mini",
    "input_tokens": 100,
    "output_tokens": 50,
    "cost_usd": "0.000045",
}
_REQUEST_ID = "0f8fad5b-d9cb-469f-a165-70867728950e"


def test_open_long_fixture_round_trips(proposal_schema: dict[str, Any]) -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    proposal = TradeProposal.model_validate(raw)
    dumped = proposal.to_json_dict()
    assert dumped == raw
    # Decimal strings must be preserved exactly (trailing zeros, no exponent).
    assert dumped["size_quote"] == "1500.00"
    assert dumped["entry"]["limit_price"] == "64250.50"
    assert dumped["stop_loss"] == "62965.49"
    assert dumped["take_profit"] == "66820.52"
    assert dumped["model_costs"][0]["cost_usd"] == "0.000593"
    Draft202012Validator(proposal_schema).validate(dumped)


def test_hold_fixture_round_trips(proposal_schema: dict[str, Any]) -> None:
    raw = load_json(FIXTURES_DIR / "proposal_hold.json")
    proposal = TradeProposal.model_validate(raw)
    dumped = proposal.to_json_dict()
    assert dumped == raw
    assert dumped["size_quote"] == "0"
    assert "stop_loss" not in dumped
    assert "take_profit" not in dumped
    assert dumped["model_costs"] == []
    Draft202012Validator(proposal_schema).validate(dumped)


def test_invalid_no_sl_fixture_fails_on_stop_loss() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_invalid_no_sl.json")
    with pytest.raises(ValidationError, match="stop_loss"):
        TradeProposal.model_validate(raw)


def test_open_long_zero_size_quote_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    raw["size_quote"] = "0"
    with pytest.raises(ValidationError, match="size_quote must be > 0"):
        TradeProposal.model_validate(raw)


def test_open_long_stop_at_or_above_limit_entry_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    # Entry is a limit at 64250.50; a stop at or above it is invalid for a long.
    for bad_stop in ("64250.50", "65000.00"):
        raw["stop_loss"] = bad_stop
        with pytest.raises(ValidationError, match="must be below entry"):
            TradeProposal.model_validate(raw)


def test_open_short_inverted_stop_and_take_profit_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    raw["action"] = "open_short"
    # Fixture stop 62965.49 is below the 64250.50 limit entry: invalid for a short.
    with pytest.raises(ValidationError, match="must be above entry"):
        TradeProposal.model_validate(raw)
    raw["stop_loss"] = "65500.00"
    # Fixture take_profit 66820.52 is above the entry: invalid for a short.
    with pytest.raises(ValidationError, match="must be below entry"):
        TradeProposal.model_validate(raw)


def test_hold_nonzero_size_quote_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_hold.json")
    raw["size_quote"] = "123.45"
    with pytest.raises(ValidationError, match='size_quote must be "0" for hold'):
        TradeProposal.model_validate(raw)


def test_verdict_reject_fixture_round_trips(verdict_schema: dict[str, Any]) -> None:
    raw = load_json(FIXTURES_DIR / "verdict_reject_daily_loss.json")
    verdict = RiskVerdict.model_validate(raw)
    assert verdict.decision is Decision.REJECT
    assert verdict.reasons[0].code == "DAILY_LOSS_LIMIT_BREACHED"
    dumped = verdict.to_json_dict()
    assert dumped == raw
    # Signed decimal variant must round-trip exactly.
    assert dumped["limits_snapshot"]["daily_realized_pnl_quote"] == "-512.40"
    Draft202012Validator(verdict_schema).validate(dumped)


def test_unknown_schema_version_is_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    raw["schema_version"] = "1.1"
    with pytest.raises(ValidationError, match="schema_version"):
        TradeProposal.model_validate(raw)


def test_unknown_fields_are_rejected() -> None:
    raw = load_json(FIXTURES_DIR / "proposal_open_long.json")
    raw["surprise"] = "extension"
    with pytest.raises(ValidationError, match="surprise"):
        TradeProposal.model_validate(raw)


def test_trace_model_cost_defaults_serialize_byte_identical_to_model_cost() -> None:
    """Hash stability (billing-and-metering.md): an entry WITHOUT the new fields
    must serialize byte-identical to the pre-upgrade ModelCost shape."""
    plain = ModelCost.model_validate(_COST_FIELDS)
    trace = TraceModelCost.model_validate(_COST_FIELDS)
    assert json.dumps(trace.to_json_dict(), sort_keys=True) == json.dumps(
        plain.to_json_dict(), sort_keys=True
    )
    assert set(trace.to_json_dict()) == set(_COST_FIELDS)


def test_trace_model_cost_serializes_request_id_and_estimated_when_set() -> None:
    dumped = TraceModelCost.model_validate(
        {**_COST_FIELDS, "request_id": _REQUEST_ID, "estimated": True}
    ).to_json_dict()
    assert dumped["request_id"] == _REQUEST_ID
    assert dumped["estimated"] is True
    # estimated=False is the default: omitted, not serialized as false.
    dumped = TraceModelCost.model_validate(
        {**_COST_FIELDS, "request_id": _REQUEST_ID, "estimated": False}
    ).to_json_dict()
    assert dumped["request_id"] == _REQUEST_ID
    assert "estimated" not in dumped


def test_trace_model_cost_to_model_cost_strips_trace_only_fields() -> None:
    trace = TraceModelCost.model_validate(
        {**_COST_FIELDS, "request_id": _REQUEST_ID, "estimated": True}
    )
    plain = trace.to_model_cost()
    assert type(plain) is ModelCost
    assert plain.to_json_dict() == ModelCost.model_validate(_COST_FIELDS).to_json_dict()


def test_trace_model_cost_rejects_non_uuid_request_id() -> None:
    with pytest.raises(ValidationError, match="request_id"):
        TraceModelCost.model_validate({**_COST_FIELDS, "request_id": "not-a-uuid"})
