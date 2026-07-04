"""Scheduler tick state: the per-strategy ``next_tick_number`` JSON file.

``tick_number`` is monotonic with NO gaps (persistence-and-api.md §scheduler),
advanced and persisted ONLY after a tick's POST attempts conclude: a crash
before the advance restarts the SAME tick, whose checkpoint replay re-produces
the same ids for an idempotent re-POST. Writes are atomic (tmp + os.replace,
the llm/budget.py pattern). Unlike the budget's fail-closed policy, a
corrupt/unparseable state file is a FAIL-FAST startup error: a guessed tick
number would violate monotonicity.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

ENV_STATE_PATH = "ALPHAMINTX_SCHEDULER_STATE"


class TickStateError(RuntimeError):
    """The tick-state file is corrupt or malformed — fail fast, never guess."""


def _validate(data: Any, path: Path) -> dict[str, dict[str, int]]:
    if not isinstance(data, dict) or not isinstance(data.get("strategies"), dict):
        raise TickStateError(f"tick state file {path} must be {{'strategies': {{...}}}}")
    strategies: dict[str, dict[str, int]] = {}
    for strategy_id, entry in data["strategies"].items():
        if not isinstance(strategy_id, str) or not isinstance(entry, dict):
            raise TickStateError(f"tick state file {path} has a malformed strategies map")
        tick = entry.get("next_tick_number")
        if isinstance(tick, bool) or not isinstance(tick, int) or tick < 0:
            raise TickStateError(
                f"tick state file {path} has an invalid next_tick_number for "
                f"strategy {strategy_id}"
            )
        strategies[strategy_id] = {"next_tick_number": tick}
    return strategies


class TickState:
    """``{"strategies": {sid: {"next_tick_number": n}}}`` with atomic persistence."""

    def __init__(self, path: Path | str) -> None:
        self._path = Path(path)
        try:
            with self._path.open(encoding="utf-8") as handle:
                data = json.load(handle)
        except FileNotFoundError:
            self._strategies: dict[str, dict[str, int]] = {}
            return
        except (json.JSONDecodeError, OSError) as exc:
            raise TickStateError(f"tick state file {self._path} is unreadable: {exc}") from exc
        self._strategies = _validate(data, self._path)

    def next_tick_number(self, strategy_id: str) -> int:
        entry = self._strategies.get(strategy_id)
        return 0 if entry is None else entry["next_tick_number"]

    def advance(self, strategy_id: str, completed_tick: int) -> None:
        """Persist ``next_tick_number = completed_tick + 1``; monotonic, no gaps."""
        expected = self.next_tick_number(strategy_id)
        if completed_tick != expected:
            raise ValueError(
                f"completed tick {completed_tick} for strategy {strategy_id} does not "
                f"match the expected next tick {expected} (monotonic, no gaps)"
            )
        self._strategies[strategy_id] = {"next_tick_number": completed_tick + 1}
        self._persist()

    def _persist(self) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        tmp_path = self._path.with_name(self._path.name + ".tmp")
        tmp_path.write_text(
            json.dumps({"strategies": self._strategies}, sort_keys=True), encoding="utf-8"
        )
        os.replace(tmp_path, self._path)
