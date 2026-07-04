"""Backtest check CLI: m1 byte-compare pass/fail and m2 snapshot-hash recheck
against honest, corrupted, and deliberately lookahead-buggy emits."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

from alphamintx_agent_plane.backtest.check import main
from alphamintx_agent_plane.backtest.emit import main as emit_main
from alphamintx_agent_plane.scheduler.snapshot import format_market_data

HOUR_MS = 3_600_000
FIRST = 1_700_000_000_000
STRATEGY_ID = "00000000-0000-4000-8000-000000000001"
WINDOW = 3


@pytest.fixture(scope="module")
def dataset_path(tmp_path_factory: pytest.TempPathFactory) -> Path:
    path = tmp_path_factory.mktemp("bt-check") / "dataset.jsonl"
    lines = []
    for i in range(5):
        payload = {
            "symbol": "BTC/USDT",
            "interval": "1h",
            "open_time": FIRST + i * HOUR_MS,
            "open": f"64{i}00.00",
            "high": f"64{i}50.00",
            "low": f"63{i}50.00",
            "close": f"64{i}20.50",
            "volume": f"10{i}.5",
        }
        lines.append(json.dumps(payload, separators=(",", ":")))
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


@pytest.fixture(scope="module")
def honest_proposals(dataset_path: Path, tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("bt-check-out") / "proposals.jsonl"
    assert emit_main([
        "--dataset", str(dataset_path), "--out", str(out), "--seed", "42",
        "--strategy-id", STRATEGY_ID, "--scenario", "bullish",
        "--window", str(WINDOW), "--symbol", "BTC/USDT", "--interval", "1h",
        "--mask", "M0",
    ]) == 0
    return out


def _buggy_sha(dataset_path: Path, t: int, lookahead: int) -> str:
    """Snapshot hash for a slice that (wrongly) admits rows up to tick t+lookahead."""
    rows = [json.loads(line) for line in dataset_path.read_text().splitlines()]
    selected = [
        row for row in rows if (row["open_time"] - FIRST) // HOUR_MS <= t + lookahead
    ][-WINDOW:]
    klines = [
        [r["open_time"], r["open"], r["high"], r["low"], r["close"], r["volume"]]
        for r in selected
    ]
    market_data = format_market_data("BTC/USDT", klines)
    return hashlib.sha256(market_data.encode("utf-8")).hexdigest()


def _m2_argv(dataset_path: Path, proposals_path: Path) -> list[str]:
    return [
        "--mode", "m2", "--dataset", str(dataset_path),
        "--proposals", str(proposals_path),
    ]


def test_m2_passes_on_honest_emit(dataset_path: Path, honest_proposals: Path) -> None:
    assert main(_m2_argv(dataset_path, honest_proposals)) == 0


def test_m2_fails_on_corrupted_snapshot_sha256(
    dataset_path: Path, honest_proposals: Path, tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    lines = honest_proposals.read_text(encoding="utf-8").splitlines()
    payload = json.loads(lines[1])
    payload["snapshot_sha256"] = hashlib.sha256(b"corrupted").hexdigest()
    lines[1] = json.dumps(payload, separators=(",", ":"))
    corrupted = tmp_path / "corrupted.jsonl"
    corrupted.write_text("\n".join(lines) + "\n", encoding="utf-8")
    assert main(_m2_argv(dataset_path, corrupted)) == 1
    assert f"tick {payload['tick_number']}" in capsys.readouterr().err


def test_m2_fails_on_lookahead_buggy_emit(
    dataset_path: Path, honest_proposals: Path, tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    # Simulate an emitter whose slice wrongly includes row t+1: every recorded
    # hash is recomputed with the buggy slice. Ticks with a t+1 row must fail.
    lines = honest_proposals.read_text(encoding="utf-8").splitlines()
    buggy_lines = [lines[0]]
    for line in lines[1:]:
        payload = json.loads(line)
        payload["snapshot_sha256"] = _buggy_sha(dataset_path, payload["tick_number"], 1)
        buggy_lines.append(json.dumps(payload, separators=(",", ":")))
    buggy = tmp_path / "buggy.jsonl"
    buggy.write_text("\n".join(buggy_lines) + "\n", encoding="utf-8")
    assert main(_m2_argv(dataset_path, buggy)) == 1
    err = capsys.readouterr().err
    # Ticks 0-3 have a t+1 row and must diverge; the LAST tick (4) has no
    # t+1 row, so its buggy hash equals the honest one — not reported.
    for tick in range(4):
        assert f"tick {tick}" in err
    assert "tick 4" not in err


def test_m2_fails_on_meta_dataset_sha_mismatch(
    dataset_path: Path, honest_proposals: Path, tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
) -> None:
    lines = honest_proposals.read_text(encoding="utf-8").splitlines()
    meta = json.loads(lines[0])
    meta["dataset_sha256"] = hashlib.sha256(b"other dataset").hexdigest()
    lines[0] = json.dumps(meta, separators=(",", ":"))
    tampered = tmp_path / "tampered.jsonl"
    tampered.write_text("\n".join(lines) + "\n", encoding="utf-8")
    assert main(_m2_argv(dataset_path, tampered)) == 1
    assert "dataset_sha256" in capsys.readouterr().err


def test_m1_passes_on_identical_files(honest_proposals: Path, tmp_path: Path) -> None:
    copy = tmp_path / "copy.jsonl"
    copy.write_bytes(honest_proposals.read_bytes())
    assert main(["--mode", "m1", "--a", str(honest_proposals), "--b", str(copy)]) == 0


def test_m1_passes_on_real_m0_vs_m1_pair(
    dataset_path: Path, honest_proposals: Path, tmp_path: Path
) -> None:
    # The actual use case: metas differ ONLY in mask_level, all proposal
    # lines byte-identical on honest code.
    m1_out = tmp_path / "m1.jsonl"
    assert emit_main([
        "--dataset", str(dataset_path), "--out", str(m1_out), "--seed", "42",
        "--strategy-id", STRATEGY_ID, "--scenario", "bullish",
        "--window", str(WINDOW), "--symbol", "BTC/USDT", "--interval", "1h",
        "--mask", "M1",
    ]) == 0
    assert main(["--mode", "m1", "--a", str(honest_proposals), "--b", str(m1_out)]) == 0


def test_m1_fails_when_meta_differs_beyond_mask_level(
    honest_proposals: Path, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    lines = honest_proposals.read_text(encoding="utf-8").splitlines()
    meta = json.loads(lines[0])
    meta["seed"] = 999
    lines[0] = json.dumps(meta, separators=(",", ":"))
    other = tmp_path / "other-meta.jsonl"
    other.write_text("\n".join(lines) + "\n", encoding="utf-8")
    assert main(["--mode", "m1", "--a", str(honest_proposals), "--b", str(other)]) == 1
    assert "meta lines differ" in capsys.readouterr().err


def test_m1_fails_and_reports_first_diverging_line(
    honest_proposals: Path, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    lines = honest_proposals.read_text(encoding="utf-8").splitlines()
    payload = json.loads(lines[2])
    payload["snapshot_sha256"] = hashlib.sha256(b"diverged").hexdigest()
    lines[2] = json.dumps(payload, separators=(",", ":"))
    other = tmp_path / "other.jsonl"
    other.write_text("\n".join(lines) + "\n", encoding="utf-8")
    assert main(["--mode", "m1", "--a", str(honest_proposals), "--b", str(other)]) == 1
    assert "line 3" in capsys.readouterr().err
