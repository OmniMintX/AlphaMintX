"""TickState: monotonic no-gap tick numbers with atomic, fail-fast persistence."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from alphamintx_agent_plane.scheduler.state import TickState, TickStateError

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


def test_missing_file_starts_at_tick_zero(tmp_path: Path) -> None:
    state = TickState(tmp_path / "ticks.json")
    assert state.next_tick_number(SID) == 0


def test_advance_persists_and_reloads(tmp_path: Path) -> None:
    path = tmp_path / "ticks.json"
    state = TickState(path)
    state.advance(SID, 0)
    state.advance(SID, 1)
    assert state.next_tick_number(SID) == 2
    assert TickState(path).next_tick_number(SID) == 2


def test_advance_rejects_gaps_and_repeats(tmp_path: Path) -> None:
    state = TickState(tmp_path / "ticks.json")
    state.advance(SID, 0)
    with pytest.raises(ValueError, match="monotonic"):
        state.advance(SID, 0)  # repeat
    with pytest.raises(ValueError, match="monotonic"):
        state.advance(SID, 2)  # gap
    assert state.next_tick_number(SID) == 1


def test_strategies_are_tracked_independently(tmp_path: Path) -> None:
    other = "0e984725-c51c-4bf5-9c56-dc0f7c7bde11"
    state = TickState(tmp_path / "ticks.json")
    state.advance(SID, 0)
    assert state.next_tick_number(SID) == 1
    assert state.next_tick_number(other) == 0


def test_write_is_atomic_no_tmp_file_left(tmp_path: Path) -> None:
    path = tmp_path / "ticks.json"
    TickState(path).advance(SID, 0)
    assert not path.with_name(path.name + ".tmp").exists()
    assert json.loads(path.read_text(encoding="utf-8")) == {
        "strategies": {SID: {"next_tick_number": 1}}
    }


def test_corrupt_json_fails_fast(tmp_path: Path) -> None:
    path = tmp_path / "ticks.json"
    path.write_text("{not json", encoding="utf-8")
    with pytest.raises(TickStateError, match="unreadable"):
        TickState(path)


@pytest.mark.parametrize(
    "payload",
    [
        {},  # missing strategies map
        {"strategies": []},  # wrong container type
        {"strategies": {SID: {"next_tick_number": -1}}},  # negative tick
        {"strategies": {SID: {"next_tick_number": "3"}}},  # non-int tick
        {"strategies": {SID: {"next_tick_number": True}}},  # bool is not a tick
        {"strategies": {SID: 3}},  # malformed entry
    ],
)
def test_malformed_state_fails_fast(tmp_path: Path, payload: object) -> None:
    path = tmp_path / "ticks.json"
    path.write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(TickStateError):
        TickState(path)
