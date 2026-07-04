"""E2E emitter tests: byte-determinism, schema validity, and per-scenario coverage."""

from __future__ import annotations

import json
import uuid
from datetime import timedelta
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator

from alphamintx_agent_plane.contract.models import rfc3339_utc
from alphamintx_agent_plane.e2e.emit import (
    SCENARIO_ORDER,
    STALE_OFFSET_SECONDS,
    RunSpec,
    load_runspec,
    main,
    render,
)

REPO_ROOT = Path(__file__).resolve().parents[2]
COMMITTED_RUNSPEC = REPO_ROOT / "e2e" / "runspec.json"


def _inline_runspec() -> dict[str, Any]:
    strategies = [
        {
            "strategy_id": f"00000000-0000-4000-8000-00000000000{n}",
            "token": f"e2e-token-{n}",
            "scenario": scenario,
        }
        for n, scenario in enumerate(SCENARIO_ORDER, start=1)
    ]
    return {
        "clock_start": "2026-07-04T12:00:00Z",
        "tick_seconds": 60,
        "seed": 42,
        "quote_currency": "USDT",
        "strategies": strategies,
        "marks": {
            "BTC/USDT": [
                "64180.1",
                "64210.5",
                "64190.0",
                "64230.2",
                "64200.7",
                "64220.9",
                "64240.3",
            ],
            "ETH/USDT": ["3400.0", "3410.5", "3395.2", "3405.8", "3412.1", "3399.9", "3408.4"],
        },
    }


@pytest.fixture(scope="module")
def runspec_path(tmp_path_factory: pytest.TempPathFactory) -> Path:
    if COMMITTED_RUNSPEC.exists():
        return COMMITTED_RUNSPEC
    path = tmp_path_factory.mktemp("e2e") / "runspec.json"
    path.write_text(json.dumps(_inline_runspec()), encoding="utf-8")
    return path


@pytest.fixture(scope="module")
def runspec(runspec_path: Path) -> RunSpec:
    return load_runspec(runspec_path)


@pytest.fixture(scope="module")
def lines(runspec: RunSpec) -> list[dict[str, Any]]:
    text = render(runspec)
    return [json.loads(line) for line in text.splitlines()]


def test_render_twice_is_byte_identical(runspec: RunSpec) -> None:
    first = render(runspec)
    second = render(runspec)
    assert first == second
    assert first.endswith("\n")
    assert "\r" not in first


def test_cli_writes_byte_identical_files(runspec_path: Path, tmp_path: Path) -> None:
    out_a = tmp_path / "a" / "proposals.jsonl"
    out_b = tmp_path / "b" / "proposals.jsonl"
    assert main(["--runspec", str(runspec_path), "--out", str(out_a)]) == 0
    assert main(["--runspec", str(runspec_path), "--out", str(out_b)]) == 0
    assert out_a.read_bytes() == out_b.read_bytes()
    assert out_a.read_bytes().endswith(b"\n")


def test_every_proposal_validates_against_schema(
    lines: list[dict[str, Any]], proposal_schema: dict[str, Any]
) -> None:
    assert len(lines) == 7
    validator = Draft202012Validator(proposal_schema)
    for envelope in lines:
        assert set(envelope) == {"token", "proposal"}
        validator.validate(envelope["proposal"])


def test_ids_are_deterministic_uuid5(lines: list[dict[str, Any]]) -> None:
    for envelope in lines:
        proposal = envelope["proposal"]
        assert uuid.UUID(proposal["proposal_id"]).version == 5
        assert uuid.UUID(proposal["agent_trace_id"]).version == 5


def test_scenario_coverage(runspec: RunSpec, lines: list[dict[str, Any]]) -> None:
    proposals = [envelope["proposal"] for envelope in lines]

    assert proposals[0]["action"] == "open_long"
    assert proposals[0]["symbol"] == "BTC/USDT"
    assert proposals[0]["entry"]["type"] == "limit"

    assert proposals[1]["action"] == "hold"
    assert proposals[1]["size_quote"] == "0"

    assert proposals[2]["symbol"] == "SOL/USDT"
    assert proposals[2]["action"] == "open_long"

    assert proposals[3]["action"] == "open_long"
    assert float(proposals[3]["size_quote"]) > 2000

    assert proposals[4]["action"] == "close"
    assert proposals[4]["size_quote"] == "0"

    assert proposals[5]["action"] == "open_long"

    for index, (envelope, strategy) in enumerate(zip(lines, runspec.strategies, strict=True)):
        proposal = envelope["proposal"]
        assert proposal["strategy_id"] == strategy.strategy_id
        slot = runspec.clock_start + timedelta(seconds=index * runspec.tick_seconds)
        if index == 5:
            expected = rfc3339_utc(slot - timedelta(seconds=STALE_OFFSET_SECONDS))
        else:
            expected = rfc3339_utc(slot)
        assert proposal["created_at"] == expected
        if index == 6:
            assert envelope["token"] == runspec.strategies[0].token
            assert envelope["token"] != strategy.token
        else:
            assert envelope["token"] == strategy.token
