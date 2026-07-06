"""``python -m alphamintx_agent_plane.scheduler`` — the live scheduler entrypoint.

Fail-fast configuration from ``ALPHAMINTX_*`` env vars (a missing/invalid value
is a startup error, never a guessed default), then one fixed-interval tick loop
per strategy until SIGINT/SIGTERM. The LLM mode defaults to stub
(llm-routing-and-budget.md §6); live mode is an explicit opt-in.
"""

from __future__ import annotations

import asyncio
import fcntl
import logging
import os
import signal
import sys
from collections.abc import Mapping
from typing import TextIO

from alphamintx_agent_plane import __version__
from alphamintx_agent_plane.client.controlplane import (
    HEARTBEAT_INTERVAL_SECONDS,
    TOKEN_ENV_VAR,
    ControlPlaneClient,
    StrategyAuth,
)
from alphamintx_agent_plane.client.http import HttpTransport
from alphamintx_agent_plane.llm.factory import create_llm_client
from alphamintx_agent_plane.scheduler.checkpoint import ENV_CHECKPOINT_DB, open_checkpointer
from alphamintx_agent_plane.scheduler.loop import (
    DEFAULT_TICK_INTERVAL_SECONDS,
    ENV_HEARTBEAT_INTERVAL_SECONDS,
    ENV_STRATEGY_ID,
    ENV_SYMBOL,
    ENV_TICK_INTERVAL_SECONDS,
    MAX_HEARTBEAT_INTERVAL_SECONDS,
    Scheduler,
    StrategyRuntime,
)
from alphamintx_agent_plane.scheduler.snapshot import (
    BINANCE_BASE_URL,
    ENV_BINANCE_BASE_URL,
    BinanceSnapshotProvider,
)
from alphamintx_agent_plane.scheduler.state import ENV_STATE_PATH, TickState

logger = logging.getLogger(__name__)


def _require(env: Mapping[str, str], name: str) -> str:
    value = env.get(name, "")
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def _tick_interval(env: Mapping[str, str]) -> float:
    raw = env.get(ENV_TICK_INTERVAL_SECONDS, str(DEFAULT_TICK_INTERVAL_SECONDS))
    try:
        value = float(raw)
    except ValueError as exc:
        raise RuntimeError(f"invalid {ENV_TICK_INTERVAL_SECONDS}={raw!r}") from exc
    if value <= 0:
        raise RuntimeError(f"invalid {ENV_TICK_INTERVAL_SECONDS}={raw!r}: must be > 0")
    return value


def _heartbeat_interval(env: Mapping[str, str]) -> float:
    """WD-25: optional cadence override; default 30; bounds (0, 45]; fail-fast."""
    raw = env.get(ENV_HEARTBEAT_INTERVAL_SECONDS, str(HEARTBEAT_INTERVAL_SECONDS))
    try:
        value = float(raw)
    except ValueError as exc:
        raise RuntimeError(f"invalid {ENV_HEARTBEAT_INTERVAL_SECONDS}={raw!r}") from exc
    if not 0 < value <= MAX_HEARTBEAT_INTERVAL_SECONDS:
        raise RuntimeError(
            f"invalid {ENV_HEARTBEAT_INTERVAL_SECONDS}={raw!r}: must be in "
            f"(0, {MAX_HEARTBEAT_INTERVAL_SECONDS:g}]"
        )
    return value


def build_scheduler(environ: Mapping[str, str] | None = None) -> Scheduler:
    """Build the scheduler from env vars; any defect raises before the loop starts."""
    env = os.environ if environ is None else environ
    strategy_id = _require(env, ENV_STRATEGY_ID)
    symbol = _require(env, ENV_SYMBOL)
    token = _require(env, TOKEN_ENV_VAR)
    checkpoint_db = _require(env, ENV_CHECKPOINT_DB)
    state_path = _require(env, ENV_STATE_PATH)
    tick_interval_seconds = _tick_interval(env)
    heartbeat_interval_seconds = _heartbeat_interval(env)
    client = ControlPlaneClient(
        HttpTransport.from_env(env),
        StrategyAuth(strategy_id=strategy_id, bearer_token=token),
    )
    runtime = StrategyRuntime(
        strategy_id=strategy_id,
        symbol=symbol,
        client=client,
        llm=create_llm_client(environ=env),
    )
    return Scheduler(
        strategies=[runtime],
        snapshots=BinanceSnapshotProvider(
            base_url=env.get(ENV_BINANCE_BASE_URL, "") or BINANCE_BASE_URL
        ),
        checkpointer=open_checkpointer(checkpoint_db),
        tick_state=TickState(state_path),
        tick_interval_seconds=tick_interval_seconds,
        heartbeat_interval_seconds=heartbeat_interval_seconds,
    )


async def _main_async(scheduler: Scheduler) -> None:
    """Run until cancelled; SIGINT/SIGTERM cancel the run task for a clean shutdown."""
    loop = asyncio.get_running_loop()
    run_task = asyncio.ensure_future(scheduler.run())
    for signum in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(signum, run_task.cancel)
    try:
        await run_task
    except asyncio.CancelledError:
        logger.info("scheduler stopped by signal")


def acquire_instance_lock(state_path: str) -> TextIO:
    """Exclusive advisory lock keyed to the tick-state file.

    Two schedulers sharing one tick-state file + checkpoint DB race every
    tick (live smoke finding: the loser resumes the winner's checkpoint and
    its divergent trace is 409-rejected). Exactly one instance may own a
    tick-state file; the second fails fast at startup.
    """
    lock_path = state_path + ".lock"
    try:
        handle = open(lock_path, "w")
    except OSError as exc:
        raise RuntimeError(f"cannot open scheduler lock file {lock_path}: {exc}") from exc
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        handle.close()
        raise RuntimeError(
            f"another scheduler instance holds {lock_path}; exactly one "
            "scheduler may own a tick-state file"
        ) from exc
    return handle


def main() -> None:
    # DS-12: --version prints the package version and exits 0, before any
    # env validation or lock acquisition.
    if "--version" in sys.argv[1:]:
        print(__version__)
        raise SystemExit(0)
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )
    lock = acquire_instance_lock(_require(os.environ, ENV_STATE_PATH))
    try:
        asyncio.run(_main_async(build_scheduler()))
    finally:
        lock.close()


if __name__ == "__main__":
    main()
