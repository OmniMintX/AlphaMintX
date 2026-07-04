"""Checkpoint DB factory (fail-fast on corruption) and LangGraph resume semantics:
a thread with a checkpoint replays to the same proposal without new LLM calls."""

from __future__ import annotations

from pathlib import Path

import pytest

from alphamintx_agent_plane.llm.stub import LLMResponse, StubLLM, bullish_scenario
from alphamintx_agent_plane.pipeline.graph import (
    PipelineInput,
    has_checkpoint,
    run_pipeline,
)
from alphamintx_agent_plane.scheduler.checkpoint import (
    CheckpointCorruptionError,
    open_checkpointer,
)

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


class CountingLLM:
    """Delegates to StubLLM while counting calls (proves resume skips nodes)."""

    def __init__(self, inner: StubLLM) -> None:
        self._inner = inner
        self.calls = 0

    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
        self.calls += 1
        return self._inner.complete(role=role, symbol=symbol, prompt=prompt)


def _inputs() -> PipelineInput:
    return PipelineInput(
        strategy_id=SID,
        symbol="BTC/USDT",
        market_data="close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50",
        news="no news feed in phase 1",
        fundamentals="no fundamentals feed in phase 1",
    )


def test_open_checkpointer_creates_fresh_db(tmp_path: Path) -> None:
    path = tmp_path / "state" / "checkpoints.sqlite3"
    saver = open_checkpointer(str(path))
    assert path.exists()
    assert not has_checkpoint(saver, f"{SID}#0")


def test_corrupt_db_fails_fast(tmp_path: Path) -> None:
    path = tmp_path / "checkpoints.sqlite3"
    path.write_bytes(b"this is not a sqlite database at all, padded to pass the header")
    with pytest.raises(CheckpointCorruptionError):
        open_checkpointer(str(path))


def test_checkpointed_run_records_the_thread(tmp_path: Path) -> None:
    saver = open_checkpointer(str(tmp_path / "checkpoints.sqlite3"))
    thread_id = f"{SID}#0"
    state = run_pipeline(
        bullish_scenario(), _inputs(), checkpointer=saver, thread_id=thread_id
    )
    assert state["proposal"] is not None
    assert has_checkpoint(saver, thread_id)
    assert not has_checkpoint(saver, f"{SID}#1")


def test_resume_replays_without_new_llm_calls(tmp_path: Path) -> None:
    saver = open_checkpointer(str(tmp_path / "checkpoints.sqlite3"))
    thread_id = f"{SID}#0"
    llm = CountingLLM(bullish_scenario())
    first = run_pipeline(llm, _inputs(), checkpointer=saver, thread_id=thread_id)
    calls_first_run = llm.calls
    assert calls_first_run > 0

    second = run_pipeline(llm, _inputs(), checkpointer=saver, thread_id=thread_id)
    assert llm.calls == calls_first_run  # replayed from the checkpoint, no re-execution
    proposal_first = first["proposal"]
    proposal_second = second["proposal"]
    assert proposal_first is not None and proposal_second is not None
    # Crash-resume of the same tick re-produces the SAME ids => idempotent re-POST.
    assert proposal_second.proposal_id == proposal_first.proposal_id
    assert proposal_second.to_json_dict() == proposal_first.to_json_dict()


def test_distinct_threads_run_independently(tmp_path: Path) -> None:
    saver = open_checkpointer(str(tmp_path / "checkpoints.sqlite3"))
    first = run_pipeline(
        bullish_scenario(), _inputs(), checkpointer=saver, thread_id=f"{SID}#0"
    )
    second = run_pipeline(
        bullish_scenario(), _inputs(), checkpointer=saver, thread_id=f"{SID}#1"
    )
    proposal_first = first["proposal"]
    proposal_second = second["proposal"]
    assert proposal_first is not None and proposal_second is not None
    assert proposal_first.proposal_id != proposal_second.proposal_id


def test_checkpointer_and_thread_id_must_travel_together(tmp_path: Path) -> None:
    saver = open_checkpointer(str(tmp_path / "checkpoints.sqlite3"))
    with pytest.raises(ValueError, match="together"):
        run_pipeline(bullish_scenario(), _inputs(), checkpointer=saver)
    with pytest.raises(ValueError, match="together"):
        run_pipeline(bullish_scenario(), _inputs(), thread_id=f"{SID}#0")
