"""Entrypoint env-var validation: fail fast on any missing/invalid setting."""

from __future__ import annotations

from pathlib import Path

import pytest

from alphamintx_agent_plane.client.controlplane import TOKEN_ENV_VAR
from alphamintx_agent_plane.client.http import ENV_BASE_URL
from alphamintx_agent_plane.scheduler.__main__ import build_scheduler
from alphamintx_agent_plane.scheduler.checkpoint import ENV_CHECKPOINT_DB
from alphamintx_agent_plane.scheduler.loop import (
    ENV_STRATEGY_ID,
    ENV_SYMBOL,
    ENV_TICK_INTERVAL_SECONDS,
    Scheduler,
)
from alphamintx_agent_plane.scheduler.state import ENV_STATE_PATH

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


def _env(tmp_path: Path) -> dict[str, str]:
    return {
        ENV_STRATEGY_ID: SID,
        ENV_SYMBOL: "BTC/USDT",
        TOKEN_ENV_VAR: "test-token",
        ENV_BASE_URL: "http://control-plane.test",
        ENV_CHECKPOINT_DB: str(tmp_path / "checkpoints.sqlite3"),
        ENV_STATE_PATH: str(tmp_path / "ticks.json"),
    }


def test_full_env_builds_a_scheduler(tmp_path: Path) -> None:
    assert isinstance(build_scheduler(_env(tmp_path)), Scheduler)


@pytest.mark.parametrize(
    "missing",
    [ENV_STRATEGY_ID, ENV_SYMBOL, TOKEN_ENV_VAR, ENV_BASE_URL, ENV_CHECKPOINT_DB, ENV_STATE_PATH],
)
def test_missing_required_env_fails_fast(tmp_path: Path, missing: str) -> None:
    env = _env(tmp_path)
    del env[missing]
    with pytest.raises(RuntimeError, match=missing):
        build_scheduler(env)


@pytest.mark.parametrize("raw", ["zero", "0", "-5"])
def test_invalid_tick_interval_fails_fast(tmp_path: Path, raw: str) -> None:
    env = _env(tmp_path)
    env[ENV_TICK_INTERVAL_SECONDS] = raw
    with pytest.raises(RuntimeError, match=ENV_TICK_INTERVAL_SECONDS):
        build_scheduler(env)
