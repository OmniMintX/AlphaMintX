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
import logging
import time
from collections.abc import Callable, Coroutine, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from decimal import Decimal
from typing import Any, Protocol

from langgraph.checkpoint.base import BaseCheckpointSaver

from alphamintx_agent_plane.client.controlplane import ControlPlaneClient
from alphamintx_agent_plane.client.errors import ControlPlaneError
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
ENV_STRATEGY_ID = "ALPHAMINTX_STRATEGY_ID"
ENV_SYMBOL = "ALPHAMINTX_SYMBOL"
DEFAULT_TICK_INTERVAL_SECONDS = 60.0

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
        max_debate_rounds: int = DEFAULT_MAX_DEBATE_ROUNDS,
        monotonic: Callable[[], float] = time.monotonic,
        wall_clock: Callable[[], datetime] = lambda: datetime.now(UTC),
        sleep: AsyncSleep = asyncio.sleep,
    ) -> None:
        if tick_interval_seconds <= 0:
            raise ValueError("tick_interval_seconds must be > 0")
        self._strategies = list(strategies)
        self._snapshots = snapshots
        self._checkpointer = checkpointer
        self._tick_state = tick_state
        self._tick_interval = float(tick_interval_seconds)
        self._max_debate_rounds = max_debate_rounds
        self._monotonic = monotonic
        self._wall_clock = wall_clock
        self._sleep = sleep
        self._day_usage: dict[tuple[str, str], _DayUsage] = {}

    async def run(self, *, max_ticks: int | None = None) -> None:
        """Run every strategy loop; cancellation propagates for a clean shutdown."""
        tasks = [
            asyncio.create_task(
                self._run_strategy(strategy, max_ticks),
                name=f"scheduler:{strategy.strategy_id}",
            )
            for strategy in self._strategies
        ]
        try:
            await asyncio.gather(*tasks)
        finally:
            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

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
