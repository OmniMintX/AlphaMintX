"""The live tick scheduler (persistence-and-api.md §checkpoint/resume and scheduler).

One asyncio task per strategy; each tick runs the SYNC pipeline in a worker
thread under the SqliteSaver checkpointer with ``thread_id =
"{strategy_id}#{tick_number}"``. A thread that already has a checkpoint is
RESUMED (never re-executed), so a crash-restart of the same tick re-produces
the same proposal ids and the duplicate POSTs are idempotent no-ops.

Per-tick ordering (normative): pipeline -> proposal POST (client retries; final
failure => trace ``proposal_id`` null, llm-routing §5) -> trace POST (final
failure => logged defect, checkpoint retained) -> ONLY THEN advance and persist
``next_tick_number``. A caught per-tick exception is logged with strategy/tick
and the scheduler resumes at the next tick. Tick pacing is a fixed interval on
a monotonic clock; an overrun logs a warning and starts the next tick
immediately. Cancellation shuts down cleanly.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
import time
from collections.abc import Callable, Coroutine, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any, Protocol

from langgraph.checkpoint.base import BaseCheckpointSaver

from alphamintx_agent_plane.client.controlplane import (
    HEARTBEAT_INTERVAL_SECONDS,
    ControlPlaneClient,
)
from alphamintx_agent_plane.client.errors import (
    ControlPlaneConflictError,
    ControlPlaneError,
)
from alphamintx_agent_plane.contract.models import decimal_to_str, rfc3339_utc
from alphamintx_agent_plane.llm.budget import DailyTokenBudget
from alphamintx_agent_plane.llm.stub import LLMClient
from alphamintx_agent_plane.pipeline.graph import (
    DEFAULT_MAX_DEBATE_ROUNDS,
    PipelineInput,
    PipelineState,
    has_checkpoint,
    run_pipeline,
)
from alphamintx_agent_plane.pipeline.trace import BudgetState, build_trace_envelope
from alphamintx_agent_plane.scheduler.snapshot import MarketSnapshotProvider

logger = logging.getLogger(__name__)

ENV_TICK_INTERVAL_SECONDS = "ALPHAMINTX_TICK_INTERVAL_SECONDS"
ENV_HEARTBEAT_INTERVAL_SECONDS = "ALPHAMINTX_HEARTBEAT_INTERVAL_SECONDS"
ENV_STRATEGY_ID = "ALPHAMINTX_STRATEGY_ID"
ENV_SYMBOL = "ALPHAMINTX_SYMBOL"
DEFAULT_TICK_INTERVAL_SECONDS = 60.0
# WD-25: the interval upper bound is half the watchdog's 90 s silence
# threshold; the default stays HEARTBEAT_INTERVAL_SECONDS (controlplane.py).
MAX_HEARTBEAT_INTERVAL_SECONDS = 45.0
# WD-24: each beat's WAIT is capped at min(interval, 15 s); the transport
# chain itself keeps running on its abandoned per-attempt thread (WD-23).
HEARTBEAT_WAIT_CAP_SECONDS = 15.0

AsyncSleep = Callable[[float], Coroutine[Any, Any, None]]


@dataclass(frozen=True)
class StrategyRuntime:
    """Everything one strategy's tick loop needs (per-strategy token in ``client``)."""

    strategy_id: str
    symbol: str
    client: ControlPlaneClient
    llm: LLMClient
    budget: DailyTokenBudget | None = None


@dataclass
class _DayUsage:
    """In-memory informational accumulation for the trace ``budget_state``."""

    tokens: int = 0
    cost_usd: Decimal = field(default_factory=lambda: Decimal("0"))


class TickStateStore(Protocol):
    """Structural shape of ``scheduler.state.TickState`` (injectable in tests)."""

    def next_tick_number(self, strategy_id: str) -> int: ...

    def advance(self, strategy_id: str, completed_tick: int) -> None: ...


class Scheduler:
    """Runs one fixed-interval tick loop per strategy until cancelled."""

    def __init__(
        self,
        *,
        strategies: Sequence[StrategyRuntime],
        snapshots: MarketSnapshotProvider,
        checkpointer: BaseCheckpointSaver[str],
        tick_state: TickStateStore,
        tick_interval_seconds: float = DEFAULT_TICK_INTERVAL_SECONDS,
        heartbeat_interval_seconds: float = HEARTBEAT_INTERVAL_SECONDS,
        max_debate_rounds: int = DEFAULT_MAX_DEBATE_ROUNDS,
        monotonic: Callable[[], float] = time.monotonic,
        wall_clock: Callable[[], datetime] = lambda: datetime.now(UTC),
        sleep: AsyncSleep = asyncio.sleep,
        heartbeat_monotonic: Callable[[], float] = time.monotonic,
        heartbeat_sleep: AsyncSleep = asyncio.sleep,
    ) -> None:
        if tick_interval_seconds <= 0:
            raise ValueError("tick_interval_seconds must be > 0")
        if not 0 < heartbeat_interval_seconds <= MAX_HEARTBEAT_INTERVAL_SECONDS:
            raise ValueError(
                "heartbeat_interval_seconds must be in "
                f"(0, {MAX_HEARTBEAT_INTERVAL_SECONDS:g}]"
            )
        self._strategies = list(strategies)
        self._snapshots = snapshots
        self._checkpointer = checkpointer
        self._tick_state = tick_state
        self._tick_interval = float(tick_interval_seconds)
        self._heartbeat_interval = float(heartbeat_interval_seconds)
        self._max_debate_rounds = max_debate_rounds
        self._monotonic = monotonic
        self._wall_clock = wall_clock
        self._sleep = sleep
        self._heartbeat_monotonic = heartbeat_monotonic
        self._heartbeat_sleep = heartbeat_sleep
        self._day_usage: dict[tuple[str, str], _DayUsage] = {}

    async def run(self, *, max_ticks: int | None = None) -> None:
        """Run every strategy loop; cancellation propagates for a clean shutdown.

        Heartbeat sender tasks (watchdog.md WD-22) are EXCLUDED from the
        primary gather and cancelled in the shutdown path, so a bounded
        ``run(max_ticks=…)`` terminates once every strategy loop finishes.
        """
        tasks = [
            asyncio.create_task(
                self._run_strategy(strategy, max_ticks),
                name=f"scheduler:{strategy.strategy_id}",
            )
            for strategy in self._strategies
        ]
        heartbeats = [
            asyncio.create_task(
                self._run_heartbeat(strategy),
                name=f"heartbeat:{strategy.strategy_id}",
            )
            for strategy in self._strategies
        ]
        try:
            await asyncio.gather(*tasks)
        finally:
            for task in [*tasks, *heartbeats]:
                task.cancel()
            await asyncio.gather(*tasks, *heartbeats, return_exceptions=True)

    async def _run_heartbeat(self, strategy: StrategyRuntime) -> None:
        """Per-strategy heartbeat sender loop (watchdog.md WD-22..WD-25).

        Start-anchored cadence: the next attempt starts at start + interval,
        NEVER end + interval — an attempt overrunning its slot consumes its
        own slot and the next send fires immediately. Failures never crash
        the scheduler and never touch the checkpoint DB or tick state.
        """
        while True:
            started = self._heartbeat_monotonic()
            await self._attempt_heartbeat(strategy)
            elapsed = self._heartbeat_monotonic() - started
            remaining = self._heartbeat_interval - elapsed
            if remaining > 0:
                await self._heartbeat_sleep(remaining)

    async def _attempt_heartbeat(self, strategy: StrategyRuntime) -> None:
        """One heartbeat POST on its OWN short-lived single-use executor.

        WD-23/WD-24: the blocking transport chain runs on a fresh one-thread
        executor per attempt (never the default executor ticks use) under a
        ``min(interval, 15 s)`` wait cap; on timeout the beat is ABANDONED but
        its thread keeps running to completion, blocking nothing — at most
        ceil(90 s max chain / 30 s cadence) = 3 zombie threads per strategy at
        the default cadence. Any failure is logged WARNING; the loop continues.
        """
        wait_cap = min(self._heartbeat_interval, HEARTBEAT_WAIT_CAP_SECONDS)
        executor = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        try:
            await asyncio.wait_for(
                asyncio.get_running_loop().run_in_executor(
                    executor, strategy.client.heartbeat
                ),
                timeout=wait_cap,
            )
        except TimeoutError:
            logger.warning(
                "heartbeat abandoned after the %.1fs wait cap: strategy=%s "
                "(the transport chain keeps running on its abandoned thread)",
                wait_cap,
                strategy.strategy_id,
            )
        except Exception:
            logger.warning(
                "heartbeat failed: strategy=%s", strategy.strategy_id, exc_info=True
            )
        finally:
            executor.shutdown(wait=False)

    async def _run_strategy(self, strategy: StrategyRuntime, max_ticks: int | None) -> None:
        completed = 0
        while max_ticks is None or completed < max_ticks:
            tick_started = self._monotonic()
            tick_number = self._tick_state.next_tick_number(strategy.strategy_id)
            try:
                await self.run_tick(strategy, tick_number)
            except Exception:
                # Spec: a per-tick exception is caught and recorded (checkpoint
                # retained); the scheduler resumes at the NEXT tick.
                logger.exception(
                    "tick failed: strategy=%s tick=%s (checkpoint retained)",
                    strategy.strategy_id,
                    tick_number,
                )
            self._tick_state.advance(strategy.strategy_id, tick_number)
            completed += 1
            if max_ticks is not None and completed >= max_ticks:
                return
            elapsed = self._monotonic() - tick_started
            remaining = self._tick_interval - elapsed
            if remaining <= 0:
                logger.warning(
                    "tick overrun: strategy=%s tick=%s took %.3fs (interval %.3fs); "
                    "starting the next tick immediately",
                    strategy.strategy_id,
                    tick_number,
                    elapsed,
                    self._tick_interval,
                )
            else:
                await self._sleep(remaining)

    async def run_tick(self, strategy: StrategyRuntime, tick_number: int) -> None:
        """Run one tick end-to-end; the CALLER advances the tick state afterwards."""
        thread_id = f"{strategy.strategy_id}#{tick_number}"
        started_at_dt = self._wall_clock()
        # Checkpoint FIRST: on crash-resume the pipeline replays from the
        # checkpoint with None input, so the snapshot fetch is skipped entirely —
        # a snapshot failure on restart must never drop the checkpointed proposal.
        inputs: PipelineInput | None = None
        if not await asyncio.to_thread(has_checkpoint, self._checkpointer, thread_id):
            snapshot = await asyncio.to_thread(self._snapshots.snapshot, strategy.symbol)
            inputs = PipelineInput(
                strategy_id=strategy.strategy_id,
                symbol=strategy.symbol,
                market_data=snapshot.market_data,
                news=snapshot.news,
                fundamentals=snapshot.fundamentals,
            )
        state = await asyncio.to_thread(
            run_pipeline,
            strategy.llm,
            inputs,
            max_debate_rounds=self._max_debate_rounds,
            checkpointer=self._checkpointer,
            thread_id=thread_id,
        )
        completed_at_dt = self._wall_clock()
        proposal = state["proposal"]
        proposal_id: str | None = None
        if proposal is None:
            logger.error(
                "defect: pipeline ended without a proposal: strategy=%s tick=%s",
                strategy.strategy_id,
                tick_number,
            )
        else:
            try:
                submission = await asyncio.to_thread(
                    strategy.client.submit_proposal, proposal, tick_number=tick_number
                )
                proposal_id = proposal.proposal_id
                if submission.submitted is False:
                    logger.warning(
                        "proposal accepted but not submitted downstream: strategy=%s "
                        "tick=%s submit_error_code=%s",
                        strategy.strategy_id,
                        tick_number,
                        submission.submit_error_code,
                    )
                elif submission.pending_approval:
                    logger.info(
                        "proposal pending approval: strategy=%s tick=%s",
                        strategy.strategy_id,
                        tick_number,
                    )
            except ControlPlaneError:
                # llm-routing §5: proposal_id stays null ONLY here — a defect
                # alert, never a routine skip.
                logger.exception(
                    "defect: proposal POST failed after retries: strategy=%s tick=%s",
                    strategy.strategy_id,
                    tick_number,
                )
        envelope = build_trace_envelope(
            state,
            strategy_id=strategy.strategy_id,
            tick_number=tick_number,
            started_at=rfc3339_utc(started_at_dt),
            completed_at=rfc3339_utc(completed_at_dt),
            proposal_id=proposal_id,
            budget_state=self._budget_state(strategy, state, started_at_dt),
        )
        try:
            await asyncio.to_thread(strategy.client.submit_trace, envelope)
        except ControlPlaneConflictError:
            # Idempotent re-drive: the run's trace is already persisted and
            # append-only wins. A re-built envelope legitimately differs
            # (wall-clock started_at/completed_at, advisory budget counters),
            # so this 409 is expected recovery noise, not a defect.
            logger.warning(
                "trace already persisted for this run; re-driven envelope "
                "differs and was rejected append-only: strategy=%s tick=%s run_id=%s",
                strategy.strategy_id,
                tick_number,
                envelope.get("run_id"),
            )
        except ControlPlaneError:
            logger.exception(
                "defect: trace POST failed after retries: strategy=%s tick=%s "
                "(checkpoint retained)",
                strategy.strategy_id,
                tick_number,
            )

    def _budget_state(
        self, strategy: StrategyRuntime, state: PipelineState, started_at: datetime
    ) -> BudgetState:
        """Informational-only budget report attributed to the UTC day of started_at.

        Token/cost accumulation here is in-memory advisory reporting; the
        authoritative ledger is control-plane, incremented from trace
        model_costs on ingest (llm-routing §4).
        """
        utc_date = started_at.astimezone(UTC).date().isoformat()
        usage = self._day_usage.setdefault((strategy.strategy_id, utc_date), _DayUsage())
        usage.tokens += sum(
            cost.input_tokens + cost.output_tokens for cost in state["model_costs"]
        )
        usage.cost_usd += sum(
            (cost.cost_usd for cost in state["model_costs"]), Decimal("0")
        )
        tokens_used = (
            strategy.budget.tokens_used() if strategy.budget is not None else usage.tokens
        )
        return BudgetState(
            utc_date=utc_date,
            tokens_used=tokens_used,
            cost_usd_used=decimal_to_str(usage.cost_usd),
        )
