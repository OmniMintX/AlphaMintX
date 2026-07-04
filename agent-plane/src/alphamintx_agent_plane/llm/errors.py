"""LLM failure taxonomy (docs/specs/llm-routing-and-budget.md §4-5).

Each error carries a normative ``marker`` for forced-hold ``reasoning`` — the audit
trail MUST distinguish RATE_LIMITED from BUDGET_EXHAUSTED — plus the ``model_costs``
entries accrued by attempts that spent tokens (including estimated entries for
timed-out attempts, surfaced via ``estimated_cost_nodes``). Messages never contain
the mintrouter API key.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import ClassVar

from alphamintx_agent_plane.contract.models import ModelCost


class LLMConfigError(Exception):
    """Startup configuration defect (bad mode, missing env, unpriced model)."""


class LLMError(Exception):
    """Base for degradable LLM-call failures; carries spent-cost accounting."""

    marker: ClassVar[str] = "LLM_ERROR"

    def __init__(
        self,
        message: str,
        *,
        attempt_costs: Iterable[ModelCost] = (),
        estimated_cost_nodes: Iterable[str] = (),
    ) -> None:
        super().__init__(message)
        self.attempt_costs = list(attempt_costs)
        self.estimated_cost_nodes = list(estimated_cost_nodes)


class BudgetExhaustedError(LLMError):
    """Local pre-call budget check failed, or mintrouter returned 402."""

    marker: ClassVar[str] = "BUDGET_EXHAUSTED"


class RateLimitedError(LLMError):
    """429 persisting after retries — a rate limit is NOT a budget event."""

    marker: ClassVar[str] = "RATE_LIMITED"


class LLMUnavailableError(LLMError):
    """Timeout / 5xx persisting after retries."""

    marker: ClassVar[str] = "LLM_UNAVAILABLE"


class LLMRequestError(LLMError):
    """4xx other than 429 (400/401/403/404/422): configuration/auth defect, no retry."""

    marker: ClassVar[str] = "LLM_REQUEST_ERROR"

    def __init__(
        self,
        status_code: int,
        message: str,
        *,
        attempt_costs: Iterable[ModelCost] = (),
        estimated_cost_nodes: Iterable[str] = (),
    ) -> None:
        super().__init__(
            message, attempt_costs=attempt_costs, estimated_cost_nodes=estimated_cost_nodes
        )
        self.status_code = status_code


class MalformedLLMOutputError(LLMError):
    """LLM output failed JSON parse / schema validation twice (one reprompt used)."""

    marker: ClassVar[str] = "MALFORMED_LLM_OUTPUT"
