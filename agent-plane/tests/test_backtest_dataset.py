"""Dataset loader: canonical-form rejections, sha256 stability, grid helpers,
and closed_window slicing (including gapped grids)."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

import pytest

from alphamintx_agent_plane.backtest.dataset import (
    INTERVAL_SECONDS,
    DatasetError,
    close_time,
    closed_window,
    grid_index,
    interval_ms,
    load_dataset,
    tick_count,
)

HOUR_MS = 3_600_000
FIRST = 1_700_000_000_000


def _line(open_time: int, **overrides: Any) -> str:
    payload: dict[str, Any] = {
        "symbol": "BTC/USDT",
        "interval": "1h",
        "open_time": open_time,
        "open": "63900.00",
        "high": "64500.00",
        "low": "63500.00",
        "close": "64250.50",
        "volume": "100.5",
    }
    payload.update(overrides)
    return json.dumps(payload, separators=(",", ":"))


def _write(tmp_path: Path, lines: list[str]) -> Path:
    path = tmp_path / "dataset.jsonl"
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def test_load_valid_dataset_and_sha256_stability(tmp_path: Path) -> None:
    path = _write(tmp_path, [_line(FIRST + i * HOUR_MS) for i in range(3)])
    rows, sha = load_dataset(path)
    assert len(rows) == 3
    assert sha == hashlib.sha256(path.read_bytes()).hexdigest()
    rows_again, sha_again = load_dataset(path)
    assert sha_again == sha
    assert rows_again == rows


@pytest.mark.parametrize(
    ("lines", "match"),
    [
        ([_line(FIRST), _line(FIRST + 2 * HOUR_MS), _line(FIRST + HOUR_MS)], "ascending"),
        ([_line(FIRST), _line(FIRST)], "ascending"),  # duplicate open_time
        ([_line(FIRST), _line(FIRST + HOUR_MS + 1)], "grid"),
        ([_line(FIRST), _line(FIRST + HOUR_MS, symbol="ETH/USDT")], "does not match"),
        ([_line(FIRST), _line(FIRST + HOUR_MS, interval="2h")], "does not match"),
        ([_line(FIRST, close="064.00")], "line 1"),  # leading zero fails DecimalStr
        ([_line(FIRST, close="1e5")], "line 1"),  # exponent fails DecimalStr
        ([_line(FIRST, close=64250.5)], "line 1"),  # JSON number, never a string
        ([_line(FIRST, interval="7m")], "unknown interval"),
        ([_line(FIRST, close_time=FIRST + HOUR_MS)], "line 1"),  # extra key forbidden
        ([_line(FIRST), ""], "blank line"),
    ],
)
def test_invalid_datasets_are_rejected(tmp_path: Path, lines: list[str], match: str) -> None:
    path = _write(tmp_path, lines)
    with pytest.raises(DatasetError, match=match):
        load_dataset(path)


def test_empty_dataset_is_rejected(tmp_path: Path) -> None:
    path = tmp_path / "dataset.jsonl"
    path.write_bytes(b"")
    with pytest.raises(DatasetError, match="empty"):
        load_dataset(path)


def test_interval_table() -> None:
    expected = {
        "1m": 60, "3m": 180, "5m": 300, "15m": 900, "30m": 1800, "1h": 3600,
        "2h": 7200, "4h": 14400, "6h": 21600, "8h": 28800, "12h": 43200, "1d": 86400,
    }
    assert INTERVAL_SECONDS == expected
    for interval, seconds in expected.items():
        assert interval_ms(interval) == seconds * 1000
    with pytest.raises(DatasetError, match="unknown interval"):
        interval_ms("45m")


def test_close_time_and_grid_index() -> None:
    assert close_time(FIRST, HOUR_MS) == FIRST + HOUR_MS
    assert grid_index(FIRST, FIRST, HOUR_MS) == 0
    assert grid_index(FIRST + 3 * HOUR_MS, FIRST, HOUR_MS) == 3


def test_gaps_are_legal_and_own_ticks(tmp_path: Path) -> None:
    path = _write(tmp_path, [_line(FIRST), _line(FIRST + 3 * HOUR_MS)])
    rows, _ = load_dataset(path)
    assert tick_count(rows) == 4  # grid indices 0..3; 1 and 2 are gapped ticks


def test_closed_window_full_grid(tmp_path: Path) -> None:
    path = _write(tmp_path, [_line(FIRST + i * HOUR_MS) for i in range(5)])
    rows, _ = load_dataset(path)
    assert closed_window(rows, 2, 3) == rows[0:3]
    assert closed_window(rows, 4, 3) == rows[2:5]
    # Fewer rows available than the window: trailing min(window, available).
    assert closed_window(rows, 0, 3) == rows[0:1]


def test_closed_window_with_gaps(tmp_path: Path) -> None:
    # Grid indices 0, 1, 4: ticks 2 and 3 are gapped.
    path = _write(
        tmp_path,
        [_line(FIRST), _line(FIRST + HOUR_MS), _line(FIRST + 4 * HOUR_MS)],
    )
    rows, _ = load_dataset(path)
    assert tick_count(rows) == 5
    # Gapped ticks see only the rows at or before them.
    assert closed_window(rows, 2, 3) == rows[0:2]
    assert closed_window(rows, 3, 3) == rows[0:2]
    assert closed_window(rows, 4, 3) == rows[0:3]
    assert closed_window(rows, 4, 2) == rows[1:3]
