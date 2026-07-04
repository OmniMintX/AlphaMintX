"""MintRouterLLM: the only live LLM transport (docs/specs/llm-routing-and-budget.md §1).

POST ``{MINTROUTER_BASE_URL}/v1/chat/completions`` (OpenAI-compatible relay), never
streaming, never a provider hostname or SDK (ADR-0004). At most 2 retries, only on
429/5xx/timeout/transport failure, honoring ``X-MintRouter-*-Reset-After-Seconds``
when present, else exponential backoff with jitter; per-attempt timeout plus a 3×
overall deadline. Every failure resolves inside the LLMError taxonomy (spec §5):
a relay that is down (connection refused / DNS / TLS) degrades to
LLM_UNAVAILABLE, never an escaped httpx exception. The bearer key is a secret:
never logged, never in argv, never in error messages.
"""

from __future__ import annotations

import json
import math
import random
import re
import time
from collections.abc import Callable, Mapping
from typing import Any

import httpx

from alphamintx_agent_plane.contract.models import ModelCost
from alphamintx_agent_plane.llm.budget import DailyTokenBudget
from alphamintx_agent_plane.llm.errors import (
    BudgetExhaustedError,
    LLMConfigError,
    LLMRequestError,
    LLMUnavailableError,
    RateLimitedError,
)
from alphamintx_agent_plane.llm.pricing import PriceTable
from alphamintx_agent_plane.llm.stub import PIPELINE_ROLES, ROLE_TRADER, LLMResponse

CHAT_COMPLETIONS_PATH = "/v1/chat/completions"
DEFAULT_TIMEOUT_SECONDS = 60.0
OVERALL_DEADLINE_FACTOR = 3
MAX_ATTEMPTS = 3  # 1 initial + at most 2 retries; normative cap, not tunable upward.
BACKOFF_BASE_SECONDS = 1.0

_RESET_AFTER_HEADER_RE = re.compile(r"^x-mintrouter-.+-reset-after-seconds$", re.IGNORECASE)

# Cheap model for Tier-1/Tier-2 roles, the expensive model for trader only
# (ARCHITECTURE.md); every model MUST be present in llm/prices.json.
DEFAULT_ROLE_MODELS: dict[str, str] = {
    role: ("gpt-4o" if role == ROLE_TRADER else "gpt-4o-mini") for role in PIPELINE_ROLES
}


def validate_role_models(role_models: Mapping[str, str], price_table: PriceTable) -> None:
    """Startup validation (spec §2): every role mapped, every model priced."""
    missing = [role for role in PIPELINE_ROLES if role not in role_models]
    if missing:
        raise LLMConfigError(f"role→model map is missing pipeline roles: {missing}")
    unknown = [role for role in role_models if role not in PIPELINE_ROLES]
    if unknown:
        raise LLMConfigError(f"role→model map contains unknown roles: {unknown}")
    unpriced = sorted({model for model in role_models.values() if model not in price_table})
    if unpriced:
        raise LLMConfigError(f"role→model map uses models absent from the price table: {unpriced}")


def _reset_after_seconds(response: httpx.Response) -> float | None:
    for name, value in response.headers.items():
        if _RESET_AFTER_HEADER_RE.fullmatch(name):
            try:
                return float(value)
            except ValueError:
                return None
    return None


class MintRouterLLM:
    """LLMClient implementation backed by the mintrouter relay."""

    def __init__(
        self,
        *,
        base_url: str,
        api_key: str,
        price_table: PriceTable,
        role_models: Mapping[str, str] | None = None,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        budget: DailyTokenBudget | None = None,
        transport: httpx.BaseTransport | None = None,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        rng: Callable[[], float] = random.random,
    ) -> None:
        if not base_url:
            raise LLMConfigError("mintrouter base URL must not be empty")
        if not api_key:
            raise LLMConfigError("mintrouter API key must not be empty")
        if timeout_seconds <= 0:
            raise LLMConfigError("mintrouter timeout must be > 0 seconds")
        models = dict(role_models) if role_models is not None else dict(DEFAULT_ROLE_MODELS)
        validate_role_models(models, price_table)
        self._role_models = models
        self._price_table = price_table
        self._timeout_seconds = float(timeout_seconds)
        self._budget = budget
        self._sleep = sleep
        self._monotonic = monotonic
        self._rng = rng
        self._base_url = base_url.rstrip("/")
        self._client = httpx.Client(
            base_url=self._base_url,
            transport=transport,
            headers={"Authorization": f"Bearer {api_key}"},
        )

    def __repr__(self) -> str:
        # The API key is a secret: it MUST NOT appear in reprs or logs (spec §6).
        return f"MintRouterLLM(base_url={self._base_url!r})"

    def _record_tokens(self, tokens: int) -> None:
        if self._budget is not None:
            self._budget.record(tokens)

    def _estimated_cost(self, role: str, model: str, estimated_input: int) -> ModelCost:
        return ModelCost(
            node=role,
            model=model,
            input_tokens=estimated_input,
            output_tokens=0,
            cost_usd=self._price_table.cost_usd(model, estimated_input, 0),
        )

    def _backoff(self, retry_index: int, deadline: float, reset_after: float | None) -> None:
        if reset_after is not None:
            delay = reset_after
        else:
            delay = BACKOFF_BASE_SECONDS * (2.0**retry_index) + self._rng()
        remaining = deadline - self._monotonic()
        delay = min(delay, max(remaining, 0.0))
        if delay > 0:
            self._sleep(delay)

    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
        model = self._role_models.get(role)
        if model is None:
            raise LLMConfigError(f"no model configured for role {role!r}")
        if self._budget is not None:
            self._budget.check()
        body = json.dumps(
            {"model": model, "messages": [{"role": "user", "content": prompt}]},
            sort_keys=True,
        )
        # Timed-out attempts spent upstream tokens but return no usage: estimate
        # input as ceil(request characters / 4), output 0 (spec §3).
        estimated_input = math.ceil(len(body) / 4)
        deadline = self._monotonic() + OVERALL_DEADLINE_FACTOR * self._timeout_seconds
        attempt_costs: list[ModelCost] = []
        estimated_nodes: list[str] = []
        last_failure = "no attempt was made"
        rate_limited = False
        for attempt in range(MAX_ATTEMPTS):
            remaining = deadline - self._monotonic()
            if remaining <= 0:
                last_failure = "overall per-call deadline exceeded"
                break
            try:
                response = self._client.post(
                    CHAT_COMPLETIONS_PATH,
                    content=body,
                    headers={"Content-Type": "application/json"},
                    timeout=min(self._timeout_seconds, remaining),
                )
            except httpx.TimeoutException:
                attempt_costs.append(self._estimated_cost(role, model, estimated_input))
                if role not in estimated_nodes:
                    estimated_nodes.append(role)
                self._record_tokens(estimated_input)
                rate_limited = False
                last_failure = f"attempt {attempt + 1} timed out"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, deadline, None)
                continue
            except httpx.RequestError as exc:
                # Connection-level failure (refused / DNS / TLS / protocol):
                # the request never reached mintrouter, so no tokens were
                # spent and no cost entry is appended — but the failure MUST
                # stay inside the taxonomy (spec §5): retry with backoff,
                # then resolve to LLM_UNAVAILABLE, never an escaped crash.
                rate_limited = False
                last_failure = f"attempt {attempt + 1} transport error ({type(exc).__name__})"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, deadline, None)
                continue
            status = response.status_code
            if status == 200:
                return self._parse_success(role, model, response, attempt_costs, estimated_nodes)
            if status == 402:
                raise BudgetExhaustedError(
                    self._budget_exhausted_detail(),
                    attempt_costs=attempt_costs,
                    estimated_cost_nodes=estimated_nodes,
                )
            if status == 429 or 500 <= status < 600:
                rate_limited = status == 429
                if not rate_limited:
                    # A 5xx after the request reached mintrouter is an
                    # aborted call: upstream tokens may have been spent, so
                    # it appends an estimated cost entry exactly like a
                    # timeout (spec §3: never silently uncounted). A 429 was
                    # rejected pre-generation — zero spend is correct there.
                    attempt_costs.append(self._estimated_cost(role, model, estimated_input))
                    if role not in estimated_nodes:
                        estimated_nodes.append(role)
                    self._record_tokens(estimated_input)
                last_failure = f"attempt {attempt + 1} got HTTP {status}"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, deadline, _reset_after_seconds(response))
                continue
            raise LLMRequestError(
                status,
                f"mintrouter returned HTTP {status} for role {role!r}; not retried",
                attempt_costs=attempt_costs,
                estimated_cost_nodes=estimated_nodes,
            )
        if rate_limited:
            raise RateLimitedError(
                f"RATE_LIMITED: mintrouter returned 429 for role {role!r} after "
                f"{MAX_ATTEMPTS} attempts; a rate limit is not a budget event",
                attempt_costs=attempt_costs,
                estimated_cost_nodes=estimated_nodes,
            )
        raise LLMUnavailableError(
            f"mintrouter unavailable for role {role!r}: {last_failure}",
            attempt_costs=attempt_costs,
            estimated_cost_nodes=estimated_nodes,
        )

    def _budget_exhausted_detail(self) -> str:
        detail = "BUDGET_EXHAUSTED: mintrouter returned 402 (token budget exhausted)"
        if self._budget is not None:
            detail += (
                f" for strategy {self._budget.strategy_id} on UTC date {self._budget.utc_date}"
            )
        return detail

    def _parse_success(
        self,
        role: str,
        model: str,
        response: httpx.Response,
        attempt_costs: list[ModelCost],
        estimated_nodes: list[str],
    ) -> LLMResponse:
        try:
            data: Any = response.json()
            text = str(data["choices"][0]["message"]["content"])
            usage = data["usage"]
            input_tokens = int(usage["prompt_tokens"])
            output_tokens = int(usage["completion_tokens"])
        except (KeyError, IndexError, TypeError, ValueError) as exc:
            raise LLMUnavailableError(
                f"mintrouter returned an invalid response body for role {role!r}",
                attempt_costs=attempt_costs,
                estimated_cost_nodes=estimated_nodes,
            ) from exc
        self._record_tokens(input_tokens + output_tokens)
        return LLMResponse(
            text=text,
            model=model,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cost_usd=self._price_table.cost_usd(model, input_tokens, output_tokens),
            extra_costs=tuple(attempt_costs),
            estimated_cost_nodes=tuple(estimated_nodes),
        )
