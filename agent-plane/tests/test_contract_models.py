"""Golden-fixture contract tests: pydantic models must round-trip contracts/fixtures/*.json
and their JSON output must validate against the actual JSON Schemas (belt and braces)."""

from __future__ import annotations

from typing import Any

import pytest
from jsonschema import Draft202012Validator
from pydantic import ValidationError

from alphamintx_agent_plane.contract.models import Decision, RiskVerdict, TradeProposal
from conftest import FIXTURES_DIR, load_json


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
