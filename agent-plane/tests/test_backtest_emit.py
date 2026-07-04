"""Backtest emitter: byte-determinism, M0 == M1, exact meta/line shapes,
independently recomputed snapshot hashes, T+1s created_at, gapped-grid ticks,
schema validity, and the no-HTTP tripwire."""

from __future__ import annotations

import hashlib
import json
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import httpx
import pytest
from jsonschema import Draft202012Validator

from alphamintx_agent_plane.backtest.emit import main
from alphamintx_agent_plane.contract.models import rfc3339_utc
from alphamintx_agent_plane.scheduler.snapshot import format_market_data

HOUR_MS = 3_600_000
FIRST = 1_700_000_000_000
STRATEGY_ID = "00000000-0000-4000-8000-000000000001"
WINDOW = 3
EPOCH = datetime(1970, 1, 1, tzinfo=UTC)

# Exact key set and order of the Go MetaLine struct (control-plane
# service.go); its decoder rejects unknown keys, so nothing else may appear.
META_KEYS = [
    "kind", "strategy_id", "symbol", "interval", "dataset_sha256", "seed",
    "mask_level", "window", "scenario",
]


def _block_network(mp: pytest.MonkeyPatch) -> None:
    def _fail(*args: object, **kwargs: object) -> object:
        raise AssertionError("backtest emit must not make HTTP calls")

    mp.setattr(httpx.Client, "send", _fail)
    mp.setattr(httpx.AsyncClient, "send", _fail)


@pytest.fixture(autouse=True)
def no_network(monkeypatch: pytest.MonkeyPatch) -> None:
    _block_network(monkeypatch)


def _dataset(path: Path, open_times: list[int]) -> Path:
    lines = []
    for i, open_time in enumerate(open_times):
        payload = {
            "symbol": "BTC/USDT",
            "interval": "1h",
            "open_time": open_time,
            "open": f"64{i}00.00",
            "high": f"64{i}50.00",
            "low": f"63{i}50.00",
            "close": f"64{i}20.50",
            "volume": f"10{i}.5",
        }
        lines.append(json.dumps(payload, separators=(",", ":")))
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def _emit(dataset: Path, out: Path, *, mask: str = "M0", scenario: str = "bullish",
          window: int = WINDOW, symbol: str = "BTC/USDT") -> Path:
    argv = [
        "--dataset", str(dataset), "--out", str(out), "--seed", "42",
        "--strategy-id", STRATEGY_ID, "--scenario", scenario,
        "--window", str(window), "--symbol", symbol, "--interval", "1h",
        "--mask", mask,
    ]
    assert main(argv) == 0
    return out


@pytest.fixture(scope="module")
def dataset_path(tmp_path_factory: pytest.TempPathFactory) -> Path:
    path = tmp_path_factory.mktemp("bt") / "dataset.jsonl"
    return _dataset(path, [FIRST + i * HOUR_MS for i in range(5)])


@pytest.fixture(scope="module")
def emitted(dataset_path: Path, tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("bt-out") / "proposals.jsonl"
    with pytest.MonkeyPatch.context() as mp:
        _block_network(mp)
        return _emit(dataset_path, out)


@pytest.fixture(scope="module")
def lines(emitted: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in emitted.read_text(encoding="utf-8").splitlines()]


def test_double_run_is_byte_identical(dataset_path: Path, tmp_path: Path) -> None:
    out_a = _emit(dataset_path, tmp_path / "a.jsonl")
    out_b = _emit(dataset_path, tmp_path / "b.jsonl")
    assert out_a.read_bytes() == out_b.read_bytes()
    assert out_a.read_bytes().endswith(b"\n")
    assert b"\r" not in out_a.read_bytes()


def test_m0_and_m1_are_byte_identical_on_honest_code(
    dataset_path: Path, tmp_path: Path
) -> None:
    out_m0 = _emit(dataset_path, tmp_path / "m0.jsonl", mask="M0")
    out_m1 = _emit(dataset_path, tmp_path / "m1.jsonl", mask="M1")
    bytes_m0 = out_m0.read_bytes()
    bytes_m1 = out_m1.read_bytes()
    # The meta lines differ ONLY in mask_level; every proposal line is identical.
    assert bytes_m0.split(b"\n")[1:] == bytes_m1.split(b"\n")[1:]
    meta_m0 = json.loads(bytes_m0.split(b"\n")[0])
    meta_m1 = json.loads(bytes_m1.split(b"\n")[0])
    assert meta_m0.pop("mask_level") == "M0"
    assert meta_m1.pop("mask_level") == "M1"
    assert meta_m0 == meta_m1


def test_meta_line_exact_shape(
    dataset_path: Path, emitted: Path, lines: list[dict[str, Any]]
) -> None:
    first_line = emitted.read_text(encoding="utf-8").splitlines()[0]
    meta = lines[0]
    assert list(meta) == META_KEYS
    assert first_line == json.dumps(meta, separators=(",", ":"))
    assert meta["kind"] == "backtest_meta"
    assert meta["strategy_id"] == STRATEGY_ID
    assert meta["symbol"] == "BTC/USDT"
    assert meta["interval"] == "1h"
    assert meta["dataset_sha256"] == hashlib.sha256(dataset_path.read_bytes()).hexdigest()
    assert meta["seed"] == 42
    assert meta["mask_level"] == "M0"


def test_one_line_per_grid_tick_from_zero_with_exact_keys(lines: list[dict[str, Any]]) -> None:
    # The Go replay requires tick_number 0..N-1 sequential, one line per tick.
    proposal_lines = lines[1:]
    assert [line["tick_number"] for line in proposal_lines] == [0, 1, 2, 3, 4]
    for line in proposal_lines:
        assert list(line) == ["tick_number", "snapshot_sha256", "proposal"]


def test_proposals_validate_against_schema_and_use_uuid5(
    lines: list[dict[str, Any]], proposal_schema: dict[str, Any]
) -> None:
    validator = Draft202012Validator(proposal_schema)
    for line in lines[1:]:
        validator.validate(line["proposal"])
        assert line["proposal"]["strategy_id"] == STRATEGY_ID
        assert uuid.UUID(line["proposal"]["proposal_id"]).version == 5
        assert uuid.UUID(line["proposal"]["agent_trace_id"]).version == 5


def test_snapshot_sha256_matches_independent_recompute(
    dataset_path: Path, lines: list[dict[str, Any]]
) -> None:
    # Independent slice + hash, straight from the dataset file bytes.
    rows = [json.loads(line) for line in dataset_path.read_text().splitlines()]
    for line in lines[1:]:
        t = line["tick_number"]
        selected = [
            row for row in rows if (row["open_time"] - FIRST) // HOUR_MS <= t
        ][-WINDOW:]
        klines = [
            [r["open_time"], r["open"], r["high"], r["low"], r["close"], r["volume"]]
            for r in selected
        ]
        market_data = format_market_data("BTC/USDT", klines)
        expected = hashlib.sha256(market_data.encode("utf-8")).hexdigest()
        assert line["snapshot_sha256"] == expected


def test_created_at_is_decision_time_plus_one_second(lines: list[dict[str, Any]]) -> None:
    for line in lines[1:]:
        t = line["tick_number"]
        decision_ms = FIRST + (t + 1) * HOUR_MS
        expected = rfc3339_utc(EPOCH + timedelta(milliseconds=decision_ms + 1000))
        assert line["proposal"]["created_at"] == expected


def test_gapped_dataset_emits_one_line_per_grid_tick(tmp_path: Path) -> None:
    # Grid indices 0, 1, 2, 5: ticks 3 and 4 are gapped but still own lines.
    dataset = _dataset(
        tmp_path / "gapped.jsonl",
        [FIRST, FIRST + HOUR_MS, FIRST + 2 * HOUR_MS, FIRST + 5 * HOUR_MS],
    )
    out = _emit(dataset, tmp_path / "gapped-out.jsonl")
    lines = [json.loads(line) for line in out.read_text().splitlines()]
    assert [line["tick_number"] for line in lines[1:]] == [0, 1, 2, 3, 4, 5]
    # Gapped ticks see the same closed rows as the last present tick.
    by_tick = {line["tick_number"]: line["snapshot_sha256"] for line in lines[1:]}
    assert by_tick[2] == by_tick[3] == by_tick[4]
    assert by_tick[5] != by_tick[4]


def test_low_confidence_scenario_emits_schema_valid_hold_every_tick(
    dataset_path: Path, tmp_path: Path, proposal_schema: dict[str, Any]
) -> None:
    out = _emit(dataset_path, tmp_path / "hold.jsonl", scenario="low_confidence")
    lines = [json.loads(line) for line in out.read_text().splitlines()]
    validator = Draft202012Validator(proposal_schema)
    assert [line["tick_number"] for line in lines[1:]] == [0, 1, 2, 3, 4]
    for line in lines[1:]:
        validator.validate(line["proposal"])
        assert line["proposal"]["action"] == "hold"
        assert line["proposal"]["size_quote"] == "0"


def test_window_exceeding_ticks_still_emits_every_tick(
    dataset_path: Path, tmp_path: Path
) -> None:
    # Early/short-dataset ticks see min(window, available) rows — never an error:
    # the Go replay requires one line per dataset tick regardless of window.
    out = _emit(dataset_path, tmp_path / "wide.jsonl", window=6)
    lines = [json.loads(line) for line in out.read_text().splitlines()]
    assert [line["tick_number"] for line in lines[1:]] == [0, 1, 2, 3, 4]


def test_symbol_mismatch_is_an_error(dataset_path: Path, tmp_path: Path) -> None:
    with pytest.raises(ValueError, match="does not match"):
        _emit(dataset_path, tmp_path / "never.jsonl", symbol="ETH/USDT")
