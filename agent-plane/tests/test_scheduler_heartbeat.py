"""Heartbeat sender (docs/specs/watchdog.md WD-22..WD-26, invariant 11):
start-anchored cadence, the per-attempt wait cap on per-attempt executors,
failure isolation from ticks, bounded-run termination, and shutdown."""

from __future__ import annotations

import asyncio
import threading
import time
from collections.abc import Callable, Mapping
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

from alphamintx_agent_plane.client.controlplane import heartbeat_path
from alphamintx_agent_plane.client.errors import ControlPlaneUnavailableError
from alphamintx_agent_plane.scheduler.checkpoint import open_checkpointer
from alphamintx_agent_plane.scheduler.loop import AsyncSleep, Scheduler
from alphamintx_agent_plane.scheduler.snapshot import (
    MarketSnapshot,
    MarketSnapshotProvider,
)
from alphamintx_agent_plane.scheduler.state import TickState
from test_scheduler_loop import (
    FakeMonotonic,
    RecordingTransport,
    ScriptedSnapshots,
    _runtime,
)

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
LOOP_LOGGER = "alphamintx_agent_plane.scheduler.loop"


class FailingHeartbeatTransport(RecordingTransport):
    """Records like RecordingTransport but every heartbeat POST is unavailable."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        if path.endswith("/heartbeat"):
            raise ControlPlaneUnavailableError("control-plane unavailable after retries")
        return super().post(path, headers, body)


class HangingFirstHeartbeatTransport(RecordingTransport):
    """The FIRST heartbeat POST blocks its thread until ``release`` is set."""

    def __init__(self) -> None:
        super().__init__()
        self.release = threading.Event()
        self.heartbeat_calls = 0

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        if path.endswith("/heartbeat"):
            self.heartbeat_calls += 1
            if self.heartbeat_calls == 1:
                self.release.wait()  # a hung transport chain (WD-23)
        return super().post(path, headers, body)


class SlowSnapshots:
    """Blocks the tick's worker thread to simulate a slow tick."""

    def snapshot(self, symbol: str) -> MarketSnapshot:
        time.sleep(0.3)
        return MarketSnapshot(
            market_data="close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50",
            news="no news feed in phase 1",
            fundamentals="no fundamentals feed in phase 1",
        )


def _scheduler(
    tmp_path: Path,
    transport: RecordingTransport,
    *,
    snapshots: MarketSnapshotProvider | None = None,
    heartbeat_interval_seconds: float = 30.0,
    heartbeat_monotonic: Callable[[], float] | None = None,
    heartbeat_sleep: AsyncSleep | None = None,
) -> tuple[Scheduler, TickState]:
    async def _tick_sleep(delay: float) -> None:
        return None

    kwargs: dict[str, Any] = {}
    if heartbeat_monotonic is not None:
        kwargs["heartbeat_monotonic"] = heartbeat_monotonic
    if heartbeat_sleep is not None:
        kwargs["heartbeat_sleep"] = heartbeat_sleep
    tick_state = TickState(tmp_path / "ticks.json")
    scheduler = Scheduler(
        strategies=[_runtime(transport)],
        snapshots=snapshots if snapshots is not None else ScriptedSnapshots(),
        checkpointer=open_checkpointer(str(tmp_path / "checkpoints.sqlite3")),
        tick_state=tick_state,
        tick_interval_seconds=60.0,
        heartbeat_interval_seconds=heartbeat_interval_seconds,
        wall_clock=lambda: datetime(2026, 7, 4, 12, 0, tzinfo=UTC),
        sleep=_tick_sleep,
        **kwargs,
    )
    return scheduler, tick_state


def _cancel_after(sleeps: list[float], count: int) -> AsyncSleep:
    async def _sleep(delay: float) -> None:
        sleeps.append(delay)
        if len(sleeps) >= count:
            raise asyncio.CancelledError

    return _sleep


def test_heartbeat_cadence_is_start_anchored(tmp_path: Path) -> None:
    # WD-24 rule 2: the next attempt starts at start + interval, NEVER
    # end + interval — the sleep is interval - elapsed, not the full interval.
    transport = RecordingTransport()
    sleeps: list[float] = []
    scheduler, _ = _scheduler(
        tmp_path,
        transport,
        heartbeat_interval_seconds=30.0,
        heartbeat_monotonic=FakeMonotonic([0.0, 5.0, 30.0, 36.0]),
        heartbeat_sleep=_cancel_after(sleeps, 2),
    )
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(scheduler._run_heartbeat(_runtime(transport)))
    assert sleeps == [25.0, 24.0]
    assert [path for path, _ in transport.records] == [heartbeat_path(SID)] * 2
    assert transport.bodies("/heartbeat") == [{}, {}]  # WD-4: body is {}


def test_overrun_attempt_consumes_its_own_slot(tmp_path: Path) -> None:
    # WD-24: an attempt overrunning its slot makes the next send fire
    # IMMEDIATELY (no sleep); the abandoned attempt consumed its own slot.
    transport = RecordingTransport()
    sleeps: list[float] = []
    scheduler, _ = _scheduler(
        tmp_path,
        transport,
        heartbeat_interval_seconds=30.0,
        heartbeat_monotonic=FakeMonotonic([0.0, 40.0, 40.0, 41.0]),
        heartbeat_sleep=_cancel_after(sleeps, 1),
    )
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(scheduler._run_heartbeat(_runtime(transport)))
    # Attempt 1 took 40 s of its 30 s slot: no sleep before attempt 2;
    # attempt 2 (elapsed 1 s) sleeps the remaining 29 s.
    assert sleeps == [29.0]
    assert len(transport.bodies("/heartbeat")) == 2


def test_hung_attempt_is_abandoned_and_never_blocks_the_next(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    # WD-23/WD-24: the wait cap is min(interval, 15 s) and each attempt runs
    # on its OWN single-use executor — a hung transport chain is abandoned at
    # the cap and the NEXT attempt still executes on a fresh thread.
    transport = HangingFirstHeartbeatTransport()
    scheduler, _ = _scheduler(tmp_path, transport, heartbeat_interval_seconds=0.05)

    async def scenario() -> None:
        task = asyncio.create_task(scheduler._run_heartbeat(_runtime(transport)))
        for _ in range(500):
            await asyncio.sleep(0.01)
            if transport.heartbeat_calls >= 2:
                break
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        transport.release.set()  # let the zombie thread self-terminate
        await asyncio.sleep(0.05)

    with caplog.at_level("WARNING", logger=LOOP_LOGGER):
        asyncio.run(scenario())
    assert transport.heartbeat_calls >= 2  # the second attempt executed
    assert any("heartbeat abandoned" in r.message for r in caplog.records)


def test_heartbeat_failure_never_propagates_to_ticks(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    # WD-23: any POST exception is a WARNING; the loop continues on cadence,
    # never crashes the scheduler, and ticks run unaffected.
    transport = FailingHeartbeatTransport()
    scheduler, tick_state = _scheduler(
        tmp_path, transport, heartbeat_interval_seconds=0.05
    )
    with caplog.at_level("WARNING", logger=LOOP_LOGGER):
        asyncio.run(scheduler.run(max_ticks=2))
    assert [body["tick_number"] for body in transport.bodies("/proposals")] == [0, 1]
    assert [body["tick_number"] for body in transport.bodies("/traces")] == [0, 1]
    assert tick_state.next_tick_number(SID) == 2
    heartbeat_records = [r for r in caplog.records if "heartbeat" in r.message]
    assert heartbeat_records  # failures were logged ...
    assert all(r.levelname == "WARNING" for r in heartbeat_records)  # ... as WARNING


def test_slow_tick_does_not_delay_beats(tmp_path: Path) -> None:
    # WD-22: the sender is a SEPARATE task, not a per-tick piggyback — a slow
    # tick blocking its worker thread never delays the cadence.
    transport = RecordingTransport()
    scheduler, _ = _scheduler(
        tmp_path, transport, snapshots=SlowSnapshots(), heartbeat_interval_seconds=0.05
    )
    asyncio.run(scheduler.run(max_ticks=1))
    assert len(transport.bodies("/heartbeat")) >= 3


def test_bounded_run_terminates_with_heartbeat_tasks_active(tmp_path: Path) -> None:
    # WD-22: heartbeat tasks are excluded from the primary gather and
    # cancelled once every strategy loop finishes — run(max_ticks=1) exits
    # even though the 30 s-cadence heartbeat task never ends on its own.
    transport = RecordingTransport()
    scheduler, _ = _scheduler(tmp_path, transport)

    async def scenario() -> None:
        await asyncio.wait_for(scheduler.run(max_ticks=1), timeout=60.0)
        current = asyncio.current_task()
        assert [t for t in asyncio.all_tasks() if t is not current] == []

    asyncio.run(scenario())
    assert len(transport.bodies("/heartbeat")) >= 1  # the sender did run
    assert [body["tick_number"] for body in transport.bodies("/proposals")] == [0]


def test_shutdown_cancellation_leaves_no_pending_tasks(tmp_path: Path) -> None:
    # WD-23: SIGTERM/SIGINT cancel the run task (the __main__ path); the
    # heartbeat tasks are cancelled with the others and nothing lingers.
    transport = RecordingTransport()
    scheduler, _ = _scheduler(tmp_path, transport)

    async def scenario() -> None:
        run_task = asyncio.ensure_future(scheduler.run())
        while not transport.bodies("/heartbeat"):
            await asyncio.sleep(0.01)
        run_task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await run_task
        current = asyncio.current_task()
        assert [t for t in asyncio.all_tasks() if t is not current] == []

    asyncio.run(scenario())


class _Poison:
    """Raises on ANY attribute access — the heartbeat task must never touch it."""

    def __getattr__(self, name: str) -> Any:
        raise AssertionError(f"heartbeat task must not touch {name}")


def test_heartbeat_never_touches_checkpoint_db_or_tick_state(tmp_path: Path) -> None:
    # Invariant 11 / invariant 1: a heartbeat writes no checkpoint and no
    # tick state — the sender loop runs against poisoned seams untouched.
    transport = RecordingTransport()
    sleeps: list[float] = []
    scheduler = Scheduler(
        strategies=[_runtime(transport)],
        snapshots=ScriptedSnapshots(),
        checkpointer=_Poison(),
        tick_state=_Poison(),
        heartbeat_interval_seconds=30.0,
        heartbeat_sleep=_cancel_after(sleeps, 1),
    )
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(scheduler._run_heartbeat(_runtime(transport)))
    assert len(transport.bodies("/heartbeat")) == 1


@pytest.mark.parametrize("interval", [0.0, -1.0, 45.1, 46.0])
def test_out_of_bounds_heartbeat_interval_is_rejected(
    tmp_path: Path, interval: float
) -> None:
    with pytest.raises(ValueError, match="heartbeat_interval_seconds"):
        _scheduler(tmp_path, RecordingTransport(), heartbeat_interval_seconds=interval)
