"""LLM mode selection (docs/specs/llm-routing-and-budget.md §6).

``ALPHAMINTX_LLM_MODE=stub`` (default: StubLLM, no network, CI/e2e unchanged) or
``live`` (MintRouterLLM). Live mode fails fast at startup when
``MINTROUTER_BASE_URL`` or ``MINTROUTER_API_KEY`` is missing; any other mode value
is a startup error. The API key is read from the environment only and never logged.
"""

from __future__ import annotations

import os
from collections.abc import Callable, Mapping

import httpx

from alphamintx_agent_plane.llm.budget import DailyTokenBudget
from alphamintx_agent_plane.llm.errors import LLMConfigError
from alphamintx_agent_plane.llm.mintrouter import DEFAULT_TIMEOUT_SECONDS, MintRouterLLM
from alphamintx_agent_plane.llm.pricing import PriceTable
from alphamintx_agent_plane.llm.stub import LLMClient, bullish_scenario

ENV_LLM_MODE = "ALPHAMINTX_LLM_MODE"
ENV_BASE_URL = "MINTROUTER_BASE_URL"
ENV_API_KEY = "MINTROUTER_API_KEY"  # env var NAME only; the value is read from env
ENV_TIMEOUT_SECONDS = "MINTROUTER_TIMEOUT_SECONDS"

MODE_STUB = "stub"
MODE_LIVE = "live"


def create_llm_client(
    *,
    environ: Mapping[str, str] | None = None,
    role_models: Mapping[str, str] | None = None,
    budget: DailyTokenBudget | None = None,
    stub_factory: Callable[[], LLMClient] = bullish_scenario,
    transport: httpx.BaseTransport | None = None,
) -> LLMClient:
    """Build the LLM client selected by ``ALPHAMINTX_LLM_MODE`` (fail-fast on defects)."""
    env = os.environ if environ is None else environ
    mode = env.get(ENV_LLM_MODE, MODE_STUB)
    if mode == MODE_STUB:
        return stub_factory()
    if mode != MODE_LIVE:
        raise LLMConfigError(
            f"invalid {ENV_LLM_MODE}={mode!r}: must be {MODE_STUB!r} or {MODE_LIVE!r}"
        )
    base_url = env.get(ENV_BASE_URL, "")
    if not base_url:
        raise LLMConfigError(f"{ENV_BASE_URL} is required in live mode")
    api_key = env.get(ENV_API_KEY, "")
    if not api_key:
        raise LLMConfigError(f"{ENV_API_KEY} is required in live mode")
    raw_timeout = env.get(ENV_TIMEOUT_SECONDS, str(DEFAULT_TIMEOUT_SECONDS))
    try:
        timeout_seconds = float(raw_timeout)
    except ValueError as exc:
        raise LLMConfigError(f"invalid {ENV_TIMEOUT_SECONDS}={raw_timeout!r}") from exc
    price_table = PriceTable.load_default()
    price_table.warn_if_stale()
    return MintRouterLLM(
        base_url=base_url,
        api_key=api_key,
        price_table=price_table,
        role_models=role_models,
        timeout_seconds=timeout_seconds,
        budget=budget,
        transport=transport,
    )
