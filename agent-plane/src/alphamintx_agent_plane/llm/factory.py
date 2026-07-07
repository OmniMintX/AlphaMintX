"""LLM mode selection (docs/specs/llm-routing-and-budget.md §6).

``ALPHAMINTX_LLM_MODE=stub`` (default: StubLLM, no network, CI/e2e unchanged) or
``live`` (MintRouterLLM). Live-mode config resolution order:

1. ``MINTROUTER_BASE_URL`` + ``MINTROUTER_API_KEY`` env pair, when BOTH are set —
   explicit operator override, no control-plane call.
2. Otherwise, when ``ALPHAMINTX_CONTROLPLANE_BASE_URL`` and
   ``ALPHAMINTX_STRATEGY_TOKEN`` are both present: one synchronous GET to
   ``/api/v1/agent/llm-config`` fetches the admin-saved config from the
   control-plane vault (``MINTROUTER_TIMEOUT_SECONDS``, if set, still wins over
   the fetched timeout).
3. Neither source available is a startup error; any other mode value is too.

The API key — from env or fetched — is never logged and never echoed in errors.
"""

from __future__ import annotations

import json
import os
from collections.abc import Callable, Mapping
from typing import Any

import httpx

from alphamintx_agent_plane.client.controlplane import TOKEN_ENV_VAR
from alphamintx_agent_plane.client.http import ENV_BASE_URL as ENV_CONTROLPLANE_BASE_URL
from alphamintx_agent_plane.llm.budget import DailyTokenBudget
from alphamintx_agent_plane.llm.errors import LLMConfigError
from alphamintx_agent_plane.llm.mintrouter import (
    DEFAULT_ROLE_MODELS,
    DEFAULT_TIMEOUT_SECONDS,
    MintRouterLLM,
)
from alphamintx_agent_plane.llm.pricing import PriceTable
from alphamintx_agent_plane.llm.stub import (
    DEFAULT_STUB_MODEL_NAME,
    PIPELINE_ROLES,
    ROLE_TRADER,
    LLMClient,
    StubLLM,
    bullish_scenario,
    low_confidence_scenario,
)
from alphamintx_agent_plane.scheduler.loop import ENV_SYMBOL

ENV_LLM_MODE = "ALPHAMINTX_LLM_MODE"
ENV_BASE_URL = "MINTROUTER_BASE_URL"
ENV_API_KEY = "MINTROUTER_API_KEY"  # env var NAME only; the value is read from env
ENV_TIMEOUT_SECONDS = "MINTROUTER_TIMEOUT_SECONDS"
ENV_STUB_SCENARIO = "ALPHAMINTX_STUB_SCENARIO"
ENV_STUB_MODEL_NAME = "ALPHAMINTX_STUB_MODEL_NAME"
ENV_STUB_TRADER_JSON = "ALPHAMINTX_STUB_TRADER_JSON"
ENV_STUB_ROLE_MODELS = "ALPHAMINTX_STUB_ROLE_MODELS"

MODE_STUB = "stub"
MODE_LIVE = "live"

STUB_SCENARIO_BULLISH = "bullish"
STUB_SCENARIO_LOW_CONFIDENCE = "low_confidence"

LLM_CONFIG_PATH = "/api/v1/agent/llm-config"
CONFIG_FETCH_TIMEOUT_SECONDS = 10.0
NOT_CONFIGURED_CODE = "NOT_CONFIGURED"


def _fetch_llm_config(
    controlplane_base_url: str,
    token: str,
    transport: httpx.BaseTransport | None,
) -> tuple[str, str, float, dict[str, str] | None]:
    """Fetch (base_url, api_key, timeout_seconds, role_models) from the vault.

    ``role_models`` is None when the config carries no model fields (payloads
    sealed before they existed) — the caller falls back to DEFAULT_ROLE_MODELS.
    Failure messages never contain the bearer token or any response body — a
    body could hold the API key.
    """
    try:
        with httpx.Client(
            base_url=controlplane_base_url.rstrip("/"),
            transport=transport,
            timeout=CONFIG_FETCH_TIMEOUT_SECONDS,
        ) as client:
            response = client.get(
                LLM_CONFIG_PATH, headers={"Authorization": f"Bearer {token}"}
            )
    except httpx.TimeoutException as exc:
        raise LLMConfigError(
            f"live mode: control-plane LLM-config fetch timed out ({type(exc).__name__})"
        ) from exc
    except httpx.RequestError as exc:
        raise LLMConfigError(
            "live mode: control-plane LLM-config fetch failed with a transport error "
            f"({type(exc).__name__})"
        ) from exc
    status = response.status_code
    if status == 404 and _error_code(response) == NOT_CONFIGURED_CODE:
        raise LLMConfigError(
            "live mode: no LLM config in the control-plane vault and MINTROUTER_* env "
            "not set — save one in the web Settings page or export "
            f"{ENV_BASE_URL}/{ENV_API_KEY}"
        )
    if status != 200:
        raise LLMConfigError(
            f"live mode: control-plane LLM-config fetch returned HTTP {status}"
        )
    try:
        data: Any = response.json()
    except ValueError as exc:
        raise LLMConfigError(
            "live mode: control-plane LLM-config response is not valid JSON"
        ) from exc
    if not isinstance(data, dict):
        raise LLMConfigError(
            "live mode: control-plane LLM-config response is not a JSON object"
        )
    base_url = data.get("base_url")
    api_key = data.get("api_key")
    timeout_seconds = data.get("timeout_seconds")
    if not isinstance(base_url, str) or not base_url:
        raise LLMConfigError(
            "live mode: control-plane LLM config has a missing or empty base_url"
        )
    if not isinstance(api_key, str) or not api_key:
        raise LLMConfigError(
            "live mode: control-plane LLM config has a missing or empty api_key"
        )
    if (
        not isinstance(timeout_seconds, int | float)
        or isinstance(timeout_seconds, bool)
        or timeout_seconds <= 0
    ):
        raise LLMConfigError(
            "live mode: control-plane LLM config has a non-positive or non-numeric "
            "timeout_seconds"
        )
    role_models = _role_models_from_config(data)
    return base_url, api_key, float(timeout_seconds), role_models


def _role_models_from_config(data: dict[str, Any]) -> dict[str, str] | None:
    """Build the role→model map from the config's optional model fields.

    A fully resolved ``role_models`` object (Phase 29 control-plane) wins; it is
    merged OVER the trader_model/default_model-derived map so a partial map from
    an older control-plane still yields a complete 7-role map. With no model
    fields at all, None is returned (the caller falls back to DEFAULT_ROLE_MODELS).
    """
    trader_model = data.get("trader_model")
    default_model = data.get("default_model")
    for name, value in (("trader_model", trader_model), ("default_model", default_model)):
        if value is not None and (not isinstance(value, str) or not value):
            raise LLMConfigError(
                f"live mode: control-plane LLM config has a non-string or empty {name}"
            )
    derived: dict[str, str] | None = None
    if trader_model is not None or default_model is not None:
        trader = trader_model or DEFAULT_ROLE_MODELS[ROLE_TRADER]
        default = default_model or next(
            model for role, model in DEFAULT_ROLE_MODELS.items() if role != ROLE_TRADER
        )
        derived = {
            role: (trader if role == ROLE_TRADER else default) for role in PIPELINE_ROLES
        }
    raw_role_models = data.get("role_models")
    if raw_role_models is None:
        return derived
    if not isinstance(raw_role_models, dict):
        raise LLMConfigError(
            "live mode: control-plane LLM config role_models is not a JSON object"
        )
    for key, value in raw_role_models.items():
        if not isinstance(key, str) or key not in PIPELINE_ROLES:
            raise LLMConfigError(
                "live mode: control-plane LLM config role_models contains a key "
                "outside the pipeline roles"
            )
        if not isinstance(value, str) or not value:
            raise LLMConfigError(
                "live mode: control-plane LLM config role_models has a non-string "
                f"or empty model for role {key!r}"
            )
    base = derived if derived is not None else dict(DEFAULT_ROLE_MODELS)
    return {**base, **raw_role_models}


def _error_code(response: httpx.Response) -> str:
    """Extract the Go error envelope's ``code`` (never the body itself)."""
    try:
        data: Any = response.json()
    except ValueError:
        return "UNKNOWN"
    if isinstance(data, dict) and isinstance(data.get("code"), str):
        return str(data["code"])
    return "UNKNOWN"


def _stub_from_env(env: Mapping[str, str]) -> StubLLM:
    """Build the stub scenario from the ALPHAMINTX_STUB_* env vars (fail-fast)."""
    scenario = env.get(ENV_STUB_SCENARIO, STUB_SCENARIO_BULLISH)
    if scenario == STUB_SCENARIO_BULLISH:
        scenario_fn = bullish_scenario
    elif scenario == STUB_SCENARIO_LOW_CONFIDENCE:
        scenario_fn = low_confidence_scenario
    else:
        raise LLMConfigError(
            f"invalid {ENV_STUB_SCENARIO}={scenario!r}: must be "
            f"{STUB_SCENARIO_BULLISH!r} or {STUB_SCENARIO_LOW_CONFIDENCE!r}"
        )
    model_name = env.get(ENV_STUB_MODEL_NAME)
    if model_name is None:
        model_name = DEFAULT_STUB_MODEL_NAME
    elif not model_name:
        raise LLMConfigError(f"{ENV_STUB_MODEL_NAME} is set but empty")
    trader_overrides: Mapping[str, object] | None = None
    raw_trader = env.get(ENV_STUB_TRADER_JSON)
    if raw_trader is not None:
        snippet = raw_trader if len(raw_trader) <= 64 else raw_trader[:61] + "..."
        try:
            parsed: Any = json.loads(raw_trader)
        except ValueError as exc:
            raise LLMConfigError(
                f"invalid {ENV_STUB_TRADER_JSON}: not valid JSON ({snippet!r})"
            ) from exc
        if not isinstance(parsed, dict):
            raise LLMConfigError(
                f"invalid {ENV_STUB_TRADER_JSON}: must be a JSON object, "
                f"got {type(parsed).__name__} ({snippet!r})"
            )
        trader_overrides = parsed
    role_models: Mapping[str, str] | None = None
    raw_role_models = env.get(ENV_STUB_ROLE_MODELS)
    if raw_role_models is not None:
        snippet = (
            raw_role_models if len(raw_role_models) <= 64 else raw_role_models[:61] + "..."
        )
        try:
            parsed_models: Any = json.loads(raw_role_models)
        except ValueError as exc:
            raise LLMConfigError(
                f"invalid {ENV_STUB_ROLE_MODELS}: not valid JSON ({snippet!r})"
            ) from exc
        if not isinstance(parsed_models, dict):
            raise LLMConfigError(
                f"invalid {ENV_STUB_ROLE_MODELS}: must be a JSON object, "
                f"got {type(parsed_models).__name__} ({snippet!r})"
            )
        for key, value in parsed_models.items():
            if key not in PIPELINE_ROLES:
                raise LLMConfigError(
                    f"invalid {ENV_STUB_ROLE_MODELS}: key {key!r} is not a pipeline "
                    f"role ({snippet!r})"
                )
            if not isinstance(value, str) or not value:
                raise LLMConfigError(
                    f"invalid {ENV_STUB_ROLE_MODELS}: model for role {key!r} must be "
                    f"a non-empty string ({snippet!r})"
                )
        role_models = parsed_models
    symbol = env.get(ENV_SYMBOL)
    if symbol:
        return scenario_fn(
            symbol,
            model_name=model_name,
            trader_overrides=trader_overrides,
            role_models=role_models,
        )
    return scenario_fn(
        model_name=model_name, trader_overrides=trader_overrides, role_models=role_models
    )


def create_llm_client(
    *,
    environ: Mapping[str, str] | None = None,
    role_models: Mapping[str, str] | None = None,
    budget: DailyTokenBudget | None = None,
    stub_factory: Callable[[], LLMClient] = bullish_scenario,
    transport: httpx.BaseTransport | None = None,
    config_transport: httpx.BaseTransport | None = None,
) -> LLMClient:
    """Build the LLM client selected by ``ALPHAMINTX_LLM_MODE`` (fail-fast on defects).

    Stub mode: when any of ``ALPHAMINTX_STUB_SCENARIO``, ``ALPHAMINTX_STUB_MODEL_NAME``,
    ``ALPHAMINTX_STUB_TRADER_JSON`` or ``ALPHAMINTX_STUB_ROLE_MODELS`` is set, the
    scenario is built from env (keyed by ``ALPHAMINTX_SYMBOL`` when set) and takes
    precedence over ``stub_factory``; with none of them set, ``stub_factory()`` is
    called unchanged (back-compat).

    ``transport`` stubs the mintrouter client itself; ``config_transport`` stubs
    the control-plane LLM-config fetch (tests only).
    """
    env = os.environ if environ is None else environ
    mode = env.get(ENV_LLM_MODE, MODE_STUB)
    if mode == MODE_STUB:
        stub_env_vars = (
            ENV_STUB_SCENARIO,
            ENV_STUB_MODEL_NAME,
            ENV_STUB_TRADER_JSON,
            ENV_STUB_ROLE_MODELS,
        )
        if any(name in env for name in stub_env_vars):
            return _stub_from_env(env)
        return stub_factory()
    if mode != MODE_LIVE:
        raise LLMConfigError(
            f"invalid {ENV_LLM_MODE}={mode!r}: must be {MODE_STUB!r} or {MODE_LIVE!r}"
        )
    base_url = env.get(ENV_BASE_URL, "")
    api_key = env.get(ENV_API_KEY, "")
    fetched_timeout: float | None = None
    fetched_models: dict[str, str] | None = None
    if not (base_url and api_key):
        controlplane_url = env.get(ENV_CONTROLPLANE_BASE_URL, "")
        strategy_token = env.get(TOKEN_ENV_VAR, "")
        if controlplane_url and strategy_token:
            base_url, api_key, fetched_timeout, fetched_models = _fetch_llm_config(
                controlplane_url, strategy_token, config_transport
            )
        elif base_url or api_key:
            set_var = ENV_BASE_URL if base_url else ENV_API_KEY
            missing_var = ENV_API_KEY if base_url else ENV_BASE_URL
            raise LLMConfigError(
                f"live mode: {set_var} is set but {missing_var} is missing, and the "
                f"control-plane fetch is unavailable ({ENV_CONTROLPLANE_BASE_URL} and "
                f"{TOKEN_ENV_VAR} are not both set) — export the full "
                f"{ENV_BASE_URL}/{ENV_API_KEY} pair or save an LLM config in the web "
                "Settings page"
            )
        else:
            raise LLMConfigError(
                f"live mode: {ENV_BASE_URL}/{ENV_API_KEY} are not set and the "
                f"control-plane fetch is unavailable ({ENV_CONTROLPLANE_BASE_URL} and "
                f"{TOKEN_ENV_VAR} are not both set) — export the env pair or save an "
                "LLM config in the web Settings page"
            )
    raw_timeout = env.get(ENV_TIMEOUT_SECONDS)
    if raw_timeout is not None:
        try:
            timeout_seconds = float(raw_timeout)
        except ValueError as exc:
            raise LLMConfigError(f"invalid {ENV_TIMEOUT_SECONDS}={raw_timeout!r}") from exc
    elif fetched_timeout is not None:
        timeout_seconds = fetched_timeout
    else:
        timeout_seconds = DEFAULT_TIMEOUT_SECONDS
    price_table = PriceTable.load_default()
    price_table.warn_if_stale()
    return MintRouterLLM(
        base_url=base_url,
        api_key=api_key,
        price_table=price_table,
        role_models=role_models if role_models is not None else fetched_models,
        timeout_seconds=timeout_seconds,
        budget=budget,
        transport=transport,
    )
