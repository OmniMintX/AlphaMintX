"""MintRouterLLM, pricing, daily budget, cost-cap, and factory tests.

Deterministic and network-free: every HTTP interaction goes through
``httpx.MockTransport``; sleeps, clocks, and jitter are injected fakes.
"""

from __future__ import annotations

import json
import logging
import math
import re
from collections.abc import Callable
from datetime import date
from decimal import Decimal
from pathlib import Path

import httpx
import pytest

from alphamintx_agent_plane.contract.models import TraceModelCost
from alphamintx_agent_plane.llm.budget import DailyTokenBudget
from alphamintx_agent_plane.llm.costs import (
    MODEL_COSTS_CAP,
    OVERFLOW_NODE,
    aggregate_overflow,
)
from alphamintx_agent_plane.llm.errors import (
    BudgetExhaustedError,
    LLMConfigError,
    LLMError,
    LLMRequestError,
    LLMUnavailableError,
    RateLimitedError,
)
from alphamintx_agent_plane.llm.factory import create_llm_client
from alphamintx_agent_plane.llm.mintrouter import MintRouterLLM, validate_role_models
from alphamintx_agent_plane.llm.pricing import STALENESS_DAYS, PriceTable
from alphamintx_agent_plane.llm.stub import PIPELINE_ROLES, ROLE_MARKET_ANALYST, StubLLM

TEST_API_KEY = "sk-test-key-that-must-never-leak"
BASE_URL = "https://mintrouter.test"
STRATEGY_ID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")

Handler = Callable[[httpx.Request], httpx.Response]


def _ok_response(
    text: str = '{"ok": true}', prompt_tokens: int = 100, completion_tokens: int = 50
) -> httpx.Response:
    return httpx.Response(
        200,
        json={
            "choices": [{"message": {"content": text}}],
            "usage": {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens},
        },
    )


def _make_llm(
    handler: Handler,
    *,
    budget: DailyTokenBudget | None = None,
    sleeps: list[float] | None = None,
) -> MintRouterLLM:
    recorded = sleeps if sleeps is not None else []
    return MintRouterLLM(
        base_url=BASE_URL,
        api_key=TEST_API_KEY,
        price_table=PriceTable.load_default(),
        budget=budget,
        transport=httpx.MockTransport(handler),
        sleep=recorded.append,
        monotonic=lambda: 0.0,
        rng=lambda: 0.0,
    )


def _estimated_input_tokens(prompt: str, model: str = "gpt-4o-mini") -> int:
    body = json.dumps(
        {"model": model, "messages": [{"role": "user", "content": prompt}]}, sort_keys=True
    )
    return math.ceil(len(body) / 4)


def test_success_cost_math_is_decimal_exact(tmp_path: Path) -> None:
    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1_000_000, state_path=tmp_path / "b.json"
    )
    llm = _make_llm(lambda _req: _ok_response(), budget=budget)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    # gpt-4o-mini: (100 * 0.15 + 50 * 0.60) / 1e6, computed in Decimal, never float.
    assert response.cost_usd == Decimal("0.000045")
    assert response.model == "gpt-4o-mini"
    assert response.request_id is not None and UUID_RE.fullmatch(response.request_id)
    assert response.extra_costs == ()
    assert response.estimated_cost_nodes == ()
    assert budget.tokens_used() == 150


def test_x_request_id_header_present_and_unique_per_attempt() -> None:
    """Every attempt sends a FRESH X-Request-Id (the billing join key,
    billing-and-metering.md): the failed attempt's estimated entry carries the
    first id, the successful response carries the second."""
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.headers["X-Request-Id"])
        if len(seen) == 1:
            return httpx.Response(500)
        return _ok_response()

    llm = _make_llm(handler)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert len(seen) == 2
    assert all(UUID_RE.fullmatch(request_id) for request_id in seen)
    assert seen[0] != seen[1]
    assert len(response.extra_costs) == 1
    assert response.extra_costs[0].request_id == seen[0]
    assert response.extra_costs[0].estimated is True
    assert response.request_id == seen[1]


def test_base_url_with_v1_suffix_is_normalized() -> None:
    """An OpenAI-convention base URL already ending in /v1 must not produce a
    doubled /v1/v1/chat/completions request path."""
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request.url.path)
        return _ok_response()

    llm = MintRouterLLM(
        base_url=BASE_URL + "/v1/",
        api_key=TEST_API_KEY,
        price_table=PriceTable.load_default(),
        transport=httpx.MockTransport(handler),
        sleep=lambda _s: None,
        monotonic=lambda: 0.0,
        rng=lambda: 0.0,
    )
    llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert seen == ["/v1/chat/completions"]


def test_retry_on_429_honors_reset_after_header() -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(
                429, headers={"X-MintRouter-Requests-Reset-After-Seconds": "7"}
            )
        return _ok_response("second try")

    sleeps: list[float] = []
    llm = _make_llm(handler, sleeps=sleeps)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert response.text == "second try"
    assert len(calls) == 2
    assert sleeps == [7.0]


def test_retry_backoff_is_exponential_without_reset_header() -> None:
    statuses = iter([500, 503])

    def handler(_request: httpx.Request) -> httpx.Response:
        try:
            return httpx.Response(next(statuses))
        except StopIteration:
            return _ok_response("third try")

    sleeps: list[float] = []
    llm = _make_llm(handler, sleeps=sleeps)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert response.text == "third try"
    assert sleeps == [1.0, 2.0]


def test_429_exhausted_raises_rate_limited_not_budget() -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(429)

    llm = _make_llm(handler)
    with pytest.raises(RateLimitedError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert len(calls) == 3
    assert excinfo.value.marker == "RATE_LIMITED"
    assert "RATE_LIMITED" in str(excinfo.value)
    assert "BUDGET" not in str(excinfo.value)


def test_400_is_not_retried() -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(400)

    sleeps: list[float] = []
    llm = _make_llm(handler, sleeps=sleeps)
    with pytest.raises(LLMRequestError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert len(calls) == 1
    assert sleeps == []
    assert excinfo.value.status_code == 400


def test_402_raises_budget_exhausted_immediately() -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(402)

    llm = _make_llm(handler)
    with pytest.raises(BudgetExhaustedError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert len(calls) == 1
    assert excinfo.value.marker == "BUDGET_EXHAUSTED"
    assert "BUDGET_EXHAUSTED" in str(excinfo.value)


def test_local_budget_precheck_blocks_before_any_call(tmp_path: Path) -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return _ok_response()

    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=0, state_path=tmp_path / "b.json"
    )
    llm = _make_llm(handler, budget=budget)
    with pytest.raises(BudgetExhaustedError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert calls == []
    assert "no LLM call was made" in str(excinfo.value)


def test_timeout_yields_estimated_cost_entry_then_success(tmp_path: Path) -> None:
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if len(calls) == 1:
            raise httpx.ReadTimeout("simulated timeout")
        return _ok_response()

    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1_000_000, state_path=tmp_path / "b.json"
    )
    llm = _make_llm(handler, budget=budget)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    estimated_input = _estimated_input_tokens("p")
    assert len(response.extra_costs) == 1
    entry = response.extra_costs[0]
    assert entry.node == ROLE_MARKET_ANALYST
    assert entry.input_tokens == estimated_input
    assert entry.output_tokens == 0
    assert entry.cost_usd == Decimal(estimated_input) * Decimal("0.15") / Decimal(1_000_000)
    assert entry.estimated is True
    assert entry.request_id is not None and entry.request_id != response.request_id
    assert response.estimated_cost_nodes == (ROLE_MARKET_ANALYST,)
    # The timed-out attempt's estimated tokens count against the daily budget too.
    assert budget.tokens_used() == estimated_input + 150


def test_all_timeouts_raise_unavailable_with_estimated_costs() -> None:
    def handler(_request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("simulated timeout")

    llm = _make_llm(handler)
    with pytest.raises(LLMUnavailableError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert excinfo.value.marker == "LLM_UNAVAILABLE"
    assert len(excinfo.value.attempt_costs) == 3
    assert excinfo.value.estimated_cost_nodes == [ROLE_MARKET_ANALYST]
    assert all(cost.output_tokens == 0 for cost in excinfo.value.attempt_costs)
    assert all(cost.estimated is True for cost in excinfo.value.attempt_costs)
    request_ids = [cost.request_id for cost in excinfo.value.attempt_costs]
    assert all(rid is not None and UUID_RE.fullmatch(rid) for rid in request_ids)
    assert len(set(request_ids)) == 3


def test_connect_error_retries_then_unavailable_without_cost_entries() -> None:
    """A relay that is DOWN (connection refused / DNS / TLS) stays inside the
    taxonomy: retried with backoff, then LLM_UNAVAILABLE — never an escaped
    httpx exception. The request never reached mintrouter, so no cost entry."""
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        raise httpx.ConnectError("connection refused")

    sleeps: list[float] = []
    llm = _make_llm(handler, sleeps=sleeps)
    with pytest.raises(LLMUnavailableError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert excinfo.value.marker == "LLM_UNAVAILABLE"
    assert len(calls) == 3
    assert sleeps == [1.0, 2.0]
    assert excinfo.value.attempt_costs == []
    assert excinfo.value.estimated_cost_nodes == []
    assert "ConnectError" in str(excinfo.value)


def test_5xx_attempt_appends_estimated_cost_entry(tmp_path: Path) -> None:
    """A 5xx reached mintrouter (an aborted call, spec §3): its spend is
    estimated like a timeout — never silently uncounted."""
    calls: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        if len(calls) == 1:
            return httpx.Response(502)
        return _ok_response()

    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1_000_000, state_path=tmp_path / "b.json"
    )
    llm = _make_llm(handler, budget=budget)
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    estimated_input = _estimated_input_tokens("p")
    assert len(response.extra_costs) == 1
    entry = response.extra_costs[0]
    assert entry.node == ROLE_MARKET_ANALYST
    assert entry.input_tokens == estimated_input
    assert entry.output_tokens == 0
    assert entry.estimated is True
    assert entry.request_id is not None and entry.request_id != response.request_id
    assert response.estimated_cost_nodes == (ROLE_MARKET_ANALYST,)
    # The aborted attempt's estimated tokens hit the advisory budget too.
    assert budget.tokens_used() == estimated_input + 150


def test_429_attempts_append_no_cost_entries() -> None:
    """A 429 is rejected pre-generation: zero upstream spend, no cost entry."""

    def handler(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(429)

    llm = _make_llm(handler)
    with pytest.raises(RateLimitedError) as excinfo:
        llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    assert excinfo.value.attempt_costs == []
    assert excinfo.value.estimated_cost_nodes == []


def test_api_key_never_appears_in_repr_errors_or_logs(
    caplog: pytest.LogCaptureFixture,
) -> None:
    # One call each: 402, 400, then a call whose 3 attempts all time out.
    behaviors: list[int | str] = [402, 400, "timeout", "timeout", "timeout"]
    scripted = iter(behaviors)

    def handler(_request: httpx.Request) -> httpx.Response:
        action = next(scripted)
        if action == "timeout":
            raise httpx.ReadTimeout("simulated timeout")
        return httpx.Response(int(action))

    llm = _make_llm(handler)
    assert TEST_API_KEY not in repr(llm)
    with caplog.at_level(logging.DEBUG):
        for _ in range(3):
            with pytest.raises(LLMError) as ei:
                llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
            assert TEST_API_KEY not in str(ei.value)
            assert TEST_API_KEY not in repr(ei.value)
    assert TEST_API_KEY not in caplog.text


def test_budget_ledger_utc_rollover(tmp_path: Path) -> None:
    path = tmp_path / "budget.json"
    day_one = DailyTokenBudget(
        strategy_id=STRATEGY_ID,
        daily_token_budget=100,
        state_path=path,
        utc_date="2026-07-04",
    )
    day_one.record(60)
    day_one.check()
    day_one.record(40)
    with pytest.raises(BudgetExhaustedError):
        day_one.check()
    # New run after UTC midnight: fresh allowance; day-one usage stays attributed
    # to the day-one started_at date (a run spanning 00:00Z never splits usage).
    day_two = DailyTokenBudget(
        strategy_id=STRATEGY_ID,
        daily_token_budget=100,
        state_path=path,
        utc_date="2026-07-05",
    )
    day_two.check()
    assert day_two.tokens_used() == 0
    assert day_one.tokens_used() == 100


def test_budget_ledger_persists_across_instances(tmp_path: Path) -> None:
    path = tmp_path / "budget.json"
    first = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=100, state_path=path, utc_date="2026-07-04"
    )
    first.record(70)
    reloaded = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=100, state_path=path, utc_date="2026-07-04"
    )
    assert reloaded.tokens_used() == 70


def test_budget_state_corruption_fails_closed(tmp_path: Path) -> None:
    """A crash-truncated state file reads as the FULL budget and blocks the
    pre-call check — never a silent reset re-arming the day's headroom."""
    path = tmp_path / "budget.json"
    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1000, state_path=path, utc_date="2026-07-04"
    )
    budget.record(99)
    assert budget.tokens_used() == 99
    path.write_text('{"truncated', encoding="utf-8")
    assert budget.tokens_used() == 1000
    with pytest.raises(BudgetExhaustedError, match="corrupt"):
        budget.check()
    # A write after corruption restarts the day AT the budget, never at zero.
    budget.record(1)
    assert budget.tokens_used() == 1001
    with pytest.raises(BudgetExhaustedError):
        budget.check()


def test_budget_non_integer_counter_fails_closed(tmp_path: Path) -> None:
    path = tmp_path / "budget.json"
    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1000, state_path=path, utc_date="2026-07-04"
    )
    path.write_text(json.dumps({STRATEGY_ID: {"2026-07-04": "99"}}), encoding="utf-8")
    assert budget.tokens_used() == 1000
    with pytest.raises(BudgetExhaustedError, match="corrupt"):
        budget.check()


def test_price_table_cost_is_decimal_exact() -> None:
    table = PriceTable.load_default()
    assert table.cost_usd("gpt-4o", 1, 1) == Decimal("0.0000125")
    assert table.cost_usd("gpt-4o-mini", 3, 7) == Decimal("0.00000465")
    assert table.cost_usd("gpt-4o-mini", 0, 0) == Decimal("0")
    with pytest.raises(LLMConfigError):
        table.cost_usd("unpriced-model", 1, 1)


def test_validate_role_models_accepts_unpriced_model_with_warning(
    caplog: pytest.LogCaptureFixture,
) -> None:
    models = {role: "custom-provider-model" for role in PIPELINE_ROLES}
    with caplog.at_level(logging.WARNING):
        validate_role_models(models, PriceTable.load_default())
    assert "not in the price table" in caplog.text
    assert "custom-provider-model" in caplog.text


def test_success_with_unpriced_model_costs_zero_and_is_estimated(tmp_path: Path) -> None:
    budget = DailyTokenBudget(
        strategy_id=STRATEGY_ID, daily_token_budget=1_000_000, state_path=tmp_path / "b.json"
    )
    llm = MintRouterLLM(
        base_url=BASE_URL,
        api_key=TEST_API_KEY,
        price_table=PriceTable.load_default(),
        role_models={role: "custom-provider-model" for role in PIPELINE_ROLES},
        budget=budget,
        transport=httpx.MockTransport(lambda _req: _ok_response()),
        sleep=lambda _delay: None,
        monotonic=lambda: 0.0,
        rng=lambda: 0.0,
    )
    response = llm.complete(role=ROLE_MARKET_ANALYST, symbol="BTC/USDT", prompt="p")
    # Real token counts are kept; only the cost is the estimated 0.
    assert response.model == "custom-provider-model"
    assert response.input_tokens == 100
    assert response.output_tokens == 50
    assert response.cost_usd == Decimal("0")
    assert response.estimated_cost_nodes == (ROLE_MARKET_ANALYST,)
    assert budget.tokens_used() == 150


def test_price_table_staleness_warning(caplog: pytest.LogCaptureFixture) -> None:
    table = PriceTable.load_default()
    fresh_day = table.as_of
    stale_day = date.fromordinal(table.as_of.toordinal() + STALENESS_DAYS + 1)
    assert table.warn_if_stale(today=fresh_day) is False
    with caplog.at_level(logging.WARNING):
        assert table.warn_if_stale(today=stale_day) is True
    assert "stale" in caplog.text


def _cost_entry(index: int) -> TraceModelCost:
    return TraceModelCost(
        node=f"node_{index}",
        model="m",
        input_tokens=index,
        output_tokens=index * 2,
        cost_usd=Decimal("0.000001") * index,
        request_id=f"00000000-0000-4000-8000-{index:012d}",
        estimated=index % 2 == 0,
    )


def test_aggregate_overflow_is_noop_at_or_under_cap() -> None:
    costs = [_cost_entry(i) for i in range(MODEL_COSTS_CAP)]
    assert aggregate_overflow(costs) == costs


def test_aggregate_overflow_keeps_31_and_sums_the_rest_exactly() -> None:
    costs = [_cost_entry(i) for i in range(40)]
    capped = aggregate_overflow(costs)
    assert len(capped) == MODEL_COSTS_CAP
    assert capped[: MODEL_COSTS_CAP - 1] == costs[: MODEL_COSTS_CAP - 1]
    aggregate = capped[-1]
    tail = costs[MODEL_COSTS_CAP - 1 :]
    assert aggregate.node == OVERFLOW_NODE
    # The aggregate merges >= 2 gateway calls: no join key, never estimated.
    assert aggregate.request_id is None
    assert aggregate.estimated is False
    assert aggregate.input_tokens == sum(entry.input_tokens for entry in tail)
    assert aggregate.output_tokens == sum(entry.output_tokens for entry in tail)
    assert aggregate.cost_usd == sum((entry.cost_usd for entry in tail), Decimal("0"))
    # Truncation never drops cost: totals before and after are identical.
    assert sum((entry.cost_usd for entry in capped), Decimal("0")) == sum(
        (entry.cost_usd for entry in costs), Decimal("0")
    )


def test_factory_defaults_to_stub_mode() -> None:
    assert isinstance(create_llm_client(environ={}), StubLLM)


def test_factory_rejects_unknown_mode() -> None:
    with pytest.raises(LLMConfigError):
        create_llm_client(environ={"ALPHAMINTX_LLM_MODE": "prod"})


def test_factory_live_mode_fails_fast_on_missing_config() -> None:
    with pytest.raises(LLMConfigError, match="MINTROUTER_BASE_URL"):
        create_llm_client(environ={"ALPHAMINTX_LLM_MODE": "live"})
    with pytest.raises(LLMConfigError, match="MINTROUTER_API_KEY"):
        create_llm_client(
            environ={"ALPHAMINTX_LLM_MODE": "live", "MINTROUTER_BASE_URL": BASE_URL}
        )


def test_factory_live_mode_builds_mintrouter_client() -> None:
    client = create_llm_client(
        environ={
            "ALPHAMINTX_LLM_MODE": "live",
            "MINTROUTER_BASE_URL": BASE_URL,
            "MINTROUTER_API_KEY": TEST_API_KEY,
        },
        transport=httpx.MockTransport(lambda _req: _ok_response()),
    )
    assert isinstance(client, MintRouterLLM)
    assert TEST_API_KEY not in repr(client)
