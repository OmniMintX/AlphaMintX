"""Factory tests for the control-plane LLM-config fetch (spec §6 resolution order).

Deterministic and network-free: the control-plane fetch is stubbed through the
``config_transport`` hook, the mintrouter client itself through ``transport`` —
both ``httpx.MockTransport``.
"""

from __future__ import annotations

import httpx
import pytest

from alphamintx_agent_plane.llm.errors import LLMConfigError
from alphamintx_agent_plane.llm.factory import create_llm_client
from alphamintx_agent_plane.llm.mintrouter import MintRouterLLM
from alphamintx_agent_plane.llm.stub import StubLLM

STRATEGY_TOKEN = "strategy-token-that-must-never-leak"
FETCHED_API_KEY = "sk-vault-key-that-must-never-leak"
FETCHED_BASE_URL = "https://mintrouter.vault.test"
ENV_BASE_URL = "https://mintrouter.env.test"
ENV_API_KEY = "sk-env-key"

LIVE_ENV = {
    "ALPHAMINTX_LLM_MODE": "live",
    "ALPHAMINTX_CONTROLPLANE_BASE_URL": "https://controlplane.test",
    "ALPHAMINTX_STRATEGY_TOKEN": STRATEGY_TOKEN,
}

_MINTROUTER_TRANSPORT = httpx.MockTransport(lambda _req: httpx.Response(200))


def _config_ok(_request: httpx.Request) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "base_url": FETCHED_BASE_URL,
            "api_key": FETCHED_API_KEY,
            "timeout_seconds": 42,
        },
    )


def test_env_override_wins_without_controlplane_call() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        pytest.fail("control-plane must not be called when the env pair is set")

    client = create_llm_client(
        environ={
            **LIVE_ENV,
            "MINTROUTER_BASE_URL": ENV_BASE_URL,
            "MINTROUTER_API_KEY": ENV_API_KEY,
        },
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(handler),
    )
    assert isinstance(client, MintRouterLLM)
    assert repr(client) == f"MintRouterLLM(base_url={ENV_BASE_URL!r})"


def test_fetch_builds_mintrouter_with_fetched_values() -> None:
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return _config_ok(request)

    client = create_llm_client(
        environ=dict(LIVE_ENV),
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(handler),
    )
    assert isinstance(client, MintRouterLLM)
    assert len(requests) == 1
    assert requests[0].url.path == "/api/v1/agent/llm-config"
    assert requests[0].headers["Authorization"] == f"Bearer {STRATEGY_TOKEN}"
    assert repr(client) == f"MintRouterLLM(base_url={FETCHED_BASE_URL!r})"
    assert FETCHED_API_KEY not in repr(client)
    assert client._timeout_seconds == 42.0


def test_fetched_models_apply_to_role_map() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "base_url": FETCHED_BASE_URL,
                "api_key": FETCHED_API_KEY,
                "timeout_seconds": 42,
                "trader_model": "gpt-4o",
                "default_model": "gpt-4o-mini",
            },
        )

    client = create_llm_client(
        environ=dict(LIVE_ENV),
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(handler),
    )
    assert isinstance(client, MintRouterLLM)
    assert client._role_models["trader"] == "gpt-4o"
    assert client._role_models["market_analyst"] == "gpt-4o-mini"


def test_config_without_models_falls_back_to_defaults() -> None:
    client = create_llm_client(
        environ=dict(LIVE_ENV),
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(_config_ok),
    )
    assert isinstance(client, MintRouterLLM)
    assert client._role_models["trader"] == "gpt-4o"
    assert client._role_models["market_analyst"] == "gpt-4o-mini"


def test_fetched_unpriced_model_fails_fast() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "base_url": FETCHED_BASE_URL,
                "api_key": FETCHED_API_KEY,
                "timeout_seconds": 42,
                "trader_model": "not-in-price-table",
            },
        )

    with pytest.raises(LLMConfigError, match="price table"):
        create_llm_client(
            environ=dict(LIVE_ENV),
            transport=_MINTROUTER_TRANSPORT,
            config_transport=httpx.MockTransport(handler),
        )


def test_fetched_empty_model_raises_config_error() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "base_url": FETCHED_BASE_URL,
                "api_key": FETCHED_API_KEY,
                "timeout_seconds": 42,
                "default_model": "",
            },
        )

    with pytest.raises(LLMConfigError, match="default_model"):
        create_llm_client(
            environ=dict(LIVE_ENV),
            transport=_MINTROUTER_TRANSPORT,
            config_transport=httpx.MockTransport(handler),
        )


def test_env_timeout_beats_fetched_timeout() -> None:
    client = create_llm_client(
        environ={**LIVE_ENV, "MINTROUTER_TIMEOUT_SECONDS": "5"},
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(_config_ok),
    )
    assert isinstance(client, MintRouterLLM)
    assert client._timeout_seconds == 5.0


def test_404_not_configured_raises_with_settings_page_hint() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            404, json={"code": "NOT_CONFIGURED", "message": "no LLM config saved"}
        )

    with pytest.raises(LLMConfigError, match="Settings page"):
        create_llm_client(
            environ=dict(LIVE_ENV), config_transport=httpx.MockTransport(handler)
        )


def test_500_raises_config_error_without_leaking_secrets() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        # A hostile/buggy body could echo the vault key: it must never surface.
        return httpx.Response(500, json={"api_key": FETCHED_API_KEY})

    with pytest.raises(LLMConfigError) as excinfo:
        create_llm_client(
            environ=dict(LIVE_ENV), config_transport=httpx.MockTransport(handler)
        )
    assert "500" in str(excinfo.value)
    assert STRATEGY_TOKEN not in str(excinfo.value)
    assert STRATEGY_TOKEN not in repr(excinfo.value)
    assert FETCHED_API_KEY not in str(excinfo.value)
    assert FETCHED_API_KEY not in repr(excinfo.value)


def test_transport_error_raises_config_error_without_leaking_secrets() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused")

    with pytest.raises(LLMConfigError) as excinfo:
        create_llm_client(
            environ=dict(LIVE_ENV), config_transport=httpx.MockTransport(handler)
        )
    assert "ConnectError" in str(excinfo.value)
    assert STRATEGY_TOKEN not in str(excinfo.value)
    assert STRATEGY_TOKEN not in repr(excinfo.value)


def test_fetch_timeout_raises_config_error_without_leaking_secrets() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("simulated timeout")

    with pytest.raises(LLMConfigError) as excinfo:
        create_llm_client(
            environ=dict(LIVE_ENV), config_transport=httpx.MockTransport(handler)
        )
    assert "timed out" in str(excinfo.value)
    assert STRATEGY_TOKEN not in str(excinfo.value)


def test_stub_mode_never_fetches() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        pytest.fail("stub mode must never fetch the control-plane LLM config")

    env = {key: value for key, value in LIVE_ENV.items() if key != "ALPHAMINTX_LLM_MODE"}
    client = create_llm_client(environ=env, config_transport=httpx.MockTransport(handler))
    assert isinstance(client, StubLLM)


def test_half_set_env_pair_falls_through_to_fetch() -> None:
    client = create_llm_client(
        environ={**LIVE_ENV, "MINTROUTER_BASE_URL": ENV_BASE_URL},
        transport=_MINTROUTER_TRANSPORT,
        config_transport=httpx.MockTransport(_config_ok),
    )
    assert isinstance(client, MintRouterLLM)
    assert repr(client) == f"MintRouterLLM(base_url={FETCHED_BASE_URL!r})"


def test_half_set_env_pair_without_controlplane_mentions_pair() -> None:
    with pytest.raises(LLMConfigError, match="MINTROUTER_BASE_URL is missing"):
        create_llm_client(
            environ={"ALPHAMINTX_LLM_MODE": "live", "MINTROUTER_API_KEY": ENV_API_KEY}
        )
