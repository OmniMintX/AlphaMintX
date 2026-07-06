"""Scheduler tick loop: monotonic no-gap ticks, pacing/overrun, per-tick failure
isolation, proposal/trace POST ordering, and idempotent crash-resume re-POSTs."""

from __future__ import annotations

import asyncio
from collections.abc import Callable, Mapping
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

from alphamintx_agent_plane.client.controlplane import (
    ControlPlaneClient,
    DryRunTransport,
    StrategyAuth,
)
from alphamintx_agent_plane.client.errors import (
    ControlPlaneConflictError,
    ControlPlaneUnavailableError,
)
from alphamintx_agent_plane.llm.stub import bullish_scenario
from alphamintx_agent_plane.pipeline.graph import PipelineInput, run_pipeline
from alphamintx_agent_plane.scheduler.checkpoint import open_checkpointer
from alphamintx_agent_plane.scheduler.loop import Scheduler, StrategyRuntime
from alphamintx_agent_plane.scheduler.snapshot import (
    MarketSnapshot,
    MarketSnapshotProvider,
    SnapshotError,
)
from alphamintx_agent_plane.scheduler.state import TickState

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


class RecordingTransport:
    """Delegates to DryRunTransport while recording every (path, body) POST."""

    def __init__(self) -> None:
        self._inner = DryRunTransport()
        self.records: list[tuple[str, dict[str, Any]]] = []

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        self.records.append((path, dict(body)))
        return self._inner.post(path, headers, body)

    def bodies(self, suffix: str) -> list[dict[str, Any]]:
        return [body for path, body in self.records if path.endswith(suffix)]


class FailingProposalTransport(RecordingTransport):
    """Records like RecordingTransport but every proposal POST is unavailable."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        if path.endswith("/proposals"):
            raise ControlPlaneUnavailableError("control-plane unavailable after retries")
        return super().post(path, headers, body)


class UnavailableTransport(RecordingTransport):
    """Records like RecordingTransport but EVERY POST is unavailable."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        self.records.append((path, dict(body)))
        raise ControlPlaneUnavailableError("control-plane unavailable after retries")


class ConflictingTraceTransport(RecordingTransport):
    """Records like RecordingTransport but every trace POST 409-conflicts."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        if path.endswith("/traces"):
            self.records.append((path, dict(body)))
            raise ControlPlaneConflictError(
                "IDEMPOTENCY_CONFLICT",
                "run_id already has a trace with a different payload",
            )
        return super().post(path, headers, body)


class ScriptedSnapshots:
    """Fixed snapshot, optionally failing the first ``failures`` calls."""

    def __init__(self, failures: int = 0) -> None:
        self.failures = failures

    def snapshot(self, symbol: str) -> MarketSnapshot:
        if self.failures > 0:
            self.failures -= 1
            raise SnapshotError("scripted snapshot failure")
        return MarketSnapshot(
            market_data="close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50",
            news="no news feed in phase 1",
            fundamentals="no fundamentals feed in phase 1",
        )


class ExplodingSnapshots:
    """Raises on EVERY call while counting them (resume must never fetch)."""

    def __init__(self) -> None:
        self.calls = 0

    def snapshot(self, symbol: str) -> MarketSnapshot:
        self.calls += 1
        raise SnapshotError("snapshot provider down after restart")


class FakeMonotonic:
    """Returns scripted instants, then keeps returning the last one."""

    def __init__(self, values: list[float]) -> None:
        self._values = list(values)
        self._last = 0.0

    def __call__(self) -> float:
        if self._values:
            self._last = self._values.pop(0)
        return self._last


class MutableClock:
    """A settable wall clock so tests can cross a UTC-midnight boundary."""

    def __init__(self, now: datetime) -> None:
        self.now = now

    def __call__(self) -> datetime:
        return self.now


def _runtime(transport: RecordingTransport) -> StrategyRuntime:
    return StrategyRuntime(
        strategy_id=SID,
        symbol="BTC/USDT",
        client=ControlPlaneClient(
            transport, StrategyAuth(strategy_id=SID, bearer_token="tok")
        ),
        llm=bullish_scenario(),
    )


def _scheduler(
    tmp_path: Path,
    transport: RecordingTransport,
    *,
    snapshots: MarketSnapshotProvider | None = None,
    monotonic: FakeMonotonic | None = None,
    sleeps: list[float] | None = None,
    wall_clock: Callable[[], datetime] | None = None,
) -> tuple[Scheduler, TickState]:
    async def _sleep(delay: float) -> None:
        if sleeps is not None:
            sleeps.append(delay)

    tick_state = TickState(tmp_path / "ticks.json")
    scheduler = Scheduler(
        strategies=[_runtime(transport)],
        snapshots=snapshots if snapshots is not None else ScriptedSnapshots(),
        checkpointer=open_checkpointer(str(tmp_path / "checkpoints.sqlite3")),
        tick_state=tick_state,
        tick_interval_seconds=60.0,
        monotonic=monotonic if monotonic is not None else FakeMonotonic([0.0]),
        wall_clock=(
            wall_clock
            if wall_clock is not None
            else lambda: datetime(2026, 7, 4, 12, 0, tzinfo=UTC)
        ),
        sleep=_sleep,
    )
    return scheduler, tick_state


def test_ticks_are_monotonic_with_no_gaps(tmp_path: Path) -> None:
    transport = RecordingTransport()
    scheduler, tick_state = _scheduler(tmp_path, transport)
    asyncio.run(scheduler.run(max_ticks=3))
    proposals = transport.bodies("/proposals")
    traces = transport.bodies("/traces")
    assert [body["tick_number"] for body in proposals] == [0, 1, 2]
    assert [body["tick_number"] for body in traces] == [0, 1, 2]
    for proposal_body, trace_body in zip(proposals, traces, strict=True):
        assert trace_body["proposal_id"] == proposal_body["proposal"]["proposal_id"]
        assert trace_body["run_id"] == proposal_body["proposal"]["agent_trace_id"]
    assert tick_state.next_tick_number(SID) == 3
    assert TickState(tmp_path / "ticks.json").next_tick_number(SID) == 3


def test_tick_pacing_sleeps_the_remaining_interval(tmp_path: Path) -> None:
    sleeps: list[float] = []
    scheduler, _ = _scheduler(
        tmp_path,
        RecordingTransport(),
        monotonic=FakeMonotonic([0.0, 5.0, 60.0]),
        sleeps=sleeps,
    )
    asyncio.run(scheduler.run(max_ticks=2))
    assert sleeps == [55.0]  # interval 60 - elapsed 5


def test_tick_overrun_starts_the_next_tick_immediately(tmp_path: Path) -> None:
    sleeps: list[float] = []
    scheduler, _ = _scheduler(
        tmp_path,
        RecordingTransport(),
        monotonic=FakeMonotonic([0.0, 75.0, 100.0]),
        sleeps=sleeps,
    )
    asyncio.run(scheduler.run(max_ticks=2))
    assert sleeps == []  # overrun: no sleep, next tick starts immediately


def test_tick_failure_is_isolated_and_the_tick_still_advances(tmp_path: Path) -> None:
    transport = RecordingTransport()
    scheduler, tick_state = _scheduler(
        tmp_path, transport, snapshots=ScriptedSnapshots(failures=1)
    )
    asyncio.run(scheduler.run(max_ticks=2))
    # Tick 0 failed before any POST; tick 1 ran normally; NO gap in tick numbers.
    assert [body["tick_number"] for body in transport.bodies("/proposals")] == [1]
    assert [body["tick_number"] for body in transport.bodies("/traces")] == [1]
    assert tick_state.next_tick_number(SID) == 2


def test_failed_proposal_post_yields_null_proposal_id_in_trace(tmp_path: Path) -> None:
    transport = FailingProposalTransport()
    scheduler, tick_state = _scheduler(tmp_path, transport)
    asyncio.run(scheduler.run(max_ticks=1))
    traces = transport.bodies("/traces")
    assert len(traces) == 1
    assert traces[0]["proposal_id"] is None  # null ONLY on POST failure after retries
    assert traces[0]["tick_number"] == 0
    assert tick_state.next_tick_number(SID) == 1  # the tick still concludes


def test_trace_conflict_is_a_warning_not_a_defect(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    # Live smoke finding: an idempotent re-drive re-builds the trace envelope
    # with fresh wall-clock timestamps, so a 409 against the already-persisted
    # trace is expected recovery noise — a WARNING, never a defect ERROR.
    transport = ConflictingTraceTransport()
    scheduler, tick_state = _scheduler(tmp_path, transport)
    with caplog.at_level("WARNING", logger="alphamintx_agent_plane.scheduler.loop"):
        asyncio.run(scheduler.run(max_ticks=1))
    loop_records = [
        r for r in caplog.records if r.name == "alphamintx_agent_plane.scheduler.loop"
    ]
    assert [r.levelname for r in loop_records] == ["WARNING"]
    assert "already persisted" in loop_records[0].message
    assert tick_state.next_tick_number(SID) == 1  # the tick still concludes


def test_crash_resume_reposts_the_identical_proposal(tmp_path: Path) -> None:
    transport = RecordingTransport()
    scheduler, _ = _scheduler(tmp_path, transport)
    strategy = _runtime(transport)
    # Same tick run twice = crash after POST, restart before the tick advanced.
    asyncio.run(scheduler.run_tick(strategy, 0))
    asyncio.run(scheduler.run_tick(strategy, 0))
    proposals = transport.bodies("/proposals")
    assert len(proposals) == 2
    # Checkpoint replay re-produces the same ids: the re-POST is byte-identical,
    # so control-plane idempotency by proposal_id makes it a safe no-op.
    assert proposals[0] == proposals[1]


def test_crash_resume_posts_checkpointed_proposal_even_if_snapshot_fails(
    tmp_path: Path,
) -> None:
    # Phase 1: the tick's pipeline completes (checkpoint written), then the
    # process crashes BEFORE the proposal POST — the tick state never advances.
    saver = open_checkpointer(str(tmp_path / "checkpoints.sqlite3"))
    state = run_pipeline(
        bullish_scenario(),
        PipelineInput(
            strategy_id=SID,
            symbol="BTC/USDT",
            market_data=(
                "close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50"
            ),
            news="no news feed in phase 1",
            fundamentals="no fundamentals feed in phase 1",
        ),
        checkpointer=saver,
        thread_id=f"{SID}#0",
    )
    checkpointed = state["proposal"]
    assert checkpointed is not None

    # Phase 2: restart with a snapshot provider that RAISES on every call. The
    # checkpoint is consulted FIRST, so the snapshot fetch is skipped and the
    # resumed tick still POSTs the identical proposal and its trace.
    exploding = ExplodingSnapshots()
    transport = RecordingTransport()
    scheduler, tick_state = _scheduler(tmp_path, transport, snapshots=exploding)
    asyncio.run(scheduler.run(max_ticks=1))

    assert exploding.calls == 0  # never fetched on resume
    proposals = transport.bodies("/proposals")
    assert len(proposals) == 1
    assert proposals[0]["tick_number"] == 0
    assert proposals[0]["proposal"] == checkpointed.to_json_dict()
    traces = transport.bodies("/traces")
    assert len(traces) == 1
    assert traces[0]["tick_number"] == 0
    assert traces[0]["proposal_id"] == checkpointed.proposal_id
    assert traces[0]["run_id"] == checkpointed.agent_trace_id
    assert tick_state.next_tick_number(SID) == 1


def test_scheduler_survives_consecutive_unavailable_ticks(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    # Control-plane fully down: the proposal AND trace POSTs fail after retries
    # on EVERY tick — each failure is a logged defect, never a crash, and the
    # loop keeps advancing with no gap in tick numbers.
    transport = UnavailableTransport()
    scheduler, tick_state = _scheduler(tmp_path, transport)
    with caplog.at_level("ERROR", logger="alphamintx_agent_plane.scheduler.loop"):
        asyncio.run(scheduler.run(max_ticks=3))
    assert tick_state.next_tick_number(SID) == 3
    defects = [
        r.message
        for r in caplog.records
        if r.name == "alphamintx_agent_plane.scheduler.loop" and r.levelname == "ERROR"
    ]
    assert len(defects) == 6  # one proposal defect + one trace defect per tick
    assert all(m.startswith("defect:") for m in defects)


def test_day_usage_prunes_past_days_on_utc_rollover(tmp_path: Path) -> None:
    # A tick on day one seeds the (strategy, day-one) usage entry, exactly as a
    # long-running process would leave it behind at UTC midnight.
    transport = RecordingTransport()
    clock = MutableClock(datetime(2026, 7, 3, 12, 0, tzinfo=UTC))
    scheduler, _ = _scheduler(tmp_path, transport, wall_clock=clock)
    asyncio.run(scheduler.run(max_ticks=1))
    assert set(scheduler._day_usage) == {(SID, "2026-07-03")}
    day_one_tokens = scheduler._day_usage[(SID, "2026-07-03")].tokens
    assert day_one_tokens > 0

    # Cross UTC midnight: the next tick creates the new day's key, prunes the
    # day-one entry, and the new day's accounting starts fresh (one tick's
    # worth — the deterministic stub pipeline spends identical tokens per tick).
    clock.now = datetime(2026, 7, 4, 0, 0, 5, tzinfo=UTC)
    asyncio.run(scheduler.run(max_ticks=1))
    assert set(scheduler._day_usage) == {(SID, "2026-07-04")}
    assert scheduler._day_usage[(SID, "2026-07-04")].tokens == day_one_tokens
    traces = transport.bodies("/traces")
    assert [t["budget_state"]["utc_date"] for t in traces] == ["2026-07-03", "2026-07-04"]
