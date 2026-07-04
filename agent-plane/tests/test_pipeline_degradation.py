"""Pipeline failure-taxonomy tests (docs/specs/llm-routing-and-budget.md §5).

Analyst failures degrade to explicit markers; debate failures cut the debate
short; trader failure, budget exhaustion, rate limiting, and twice-malformed
output resolve to a deterministic, schema-valid forced-hold proposal.
"""

from __future__ import annotations

from dataclasses import replace
from decimal import Decimal
from typing import Any

import httpx
from jsonschema import Draft202012Validator

from alphamintx_agent_plane.contract.models import Action, ModelCost
from alphamintx_agent_plane.llm.costs import MODEL_COSTS_CAP, OVERFLOW_NODE
from alphamintx_agent_plane.llm.errors import (
    BudgetExhaustedError,
    LLMUnavailableError,
    RateLimitedError,
)
from alphamintx_agent_plane.llm.mintrouter import MintRouterLLM
from alphamintx_agent_plane.llm.pricing import PriceTable
from alphamintx_agent_plane.llm.stub import (
    ROLE_BEAR_RESEARCHER,
    ROLE_BULL_RESEARCHER,
    ROLE_DEBATE_JUDGE,
    ROLE_MARKET_ANALYST,
    ROLE_NEWS_ANALYST,
    ROLE_TRADER,
    LLMClient,
    LLMResponse,
    bullish_scenario,
)
from alphamintx_agent_plane.pipeline.graph import PipelineInput, PipelineState, run_pipeline

STRATEGY_ID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"


def _inputs() -> PipelineInput:
    return PipelineInput(
        strategy_id=STRATEGY_ID,
        symbol="BTC/USDT",
        market_data="close=64250.50 range_high_20d=64000.00 rsi=61 volume_ratio=1.8",
        news="ETF inflows continue; no adverse regulatory headlines in the last 24h.",
        fundamentals="On-chain activity flat WoW; funding rates near neutral.",
    )


class ScriptedLLM:
    """Wraps the bullish StubLLM; per-role scripted failures/texts are consumed
    first, then calls fall through to the stub. Counts calls per role."""

    def __init__(self, overrides: dict[str, list[Exception | str]]) -> None:
        self._inner = bullish_scenario()
        self._overrides = {role: list(items) for role, items in overrides.items()}
        self.calls: dict[str, int] = {}

    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
        self.calls[role] = self.calls.get(role, 0) + 1
        queue = self._overrides.get(role)
        if queue:
            item = queue.pop(0)
            if isinstance(item, Exception):
                raise item
            return LLMResponse(
                text=item,
                model="stub-model",
                input_tokens=1,
                output_tokens=1,
                cost_usd=Decimal("0.000003"),
            )
        return self._inner.complete(role=role, symbol=symbol, prompt=prompt)


def _run(llm: LLMClient) -> PipelineState:
    return run_pipeline(llm, _inputs())


def test_unavailable_analyst_degrades_and_pipeline_continues(
    proposal_schema: dict[str, Any],
) -> None:
    llm = ScriptedLLM({ROLE_NEWS_ANALYST: [LLMUnavailableError("relay down")]})
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.OPEN_LONG
    news = proposal.analyst_summaries.news
    assert news.summary.startswith("unavailable:")
    assert news.confidence == 0.0
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_budget_exhausted_analyst_forces_hold_and_skips_downstream(
    proposal_schema: dict[str, Any],
) -> None:
    llm = ScriptedLLM(
        {ROLE_MARKET_ANALYST: [BudgetExhaustedError("BUDGET_EXHAUSTED: daily budget spent")]}
    )
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert "BUDGET_EXHAUSTED" in proposal.reasoning
    assert proposal.to_json_dict()["size_quote"] == "0"
    # No debate/judge/trader tokens are spent once the forced hold is flagged.
    assert llm.calls.get(ROLE_BULL_RESEARCHER, 0) == 0
    assert llm.calls.get(ROLE_DEBATE_JUDGE, 0) == 0
    assert llm.calls.get(ROLE_TRADER, 0) == 0
    assert state["debate_rounds"] == []
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_rate_limited_trader_forces_hold_with_rate_limited_marker(
    proposal_schema: dict[str, Any],
) -> None:
    llm = ScriptedLLM({ROLE_TRADER: [RateLimitedError("429 after retries")]})
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert "RATE_LIMITED" in proposal.reasoning
    assert "BUDGET_EXHAUSTED" not in proposal.reasoning
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_multiple_forced_hold_reasons_are_all_preserved(
    proposal_schema: dict[str, Any],
) -> None:
    """Distinct markers from different nodes (BUDGET_EXHAUSTED analyst AND
    RATE_LIMITED analyst) all reach the forced-hold reasoning, joined in
    deterministic reducer order within the 8000-char bound."""
    llm = ScriptedLLM(
        {
            ROLE_MARKET_ANALYST: [BudgetExhaustedError("BUDGET_EXHAUSTED: daily budget spent")],
            ROLE_NEWS_ANALYST: [RateLimitedError("RATE_LIMITED: 429 after retries")],
        }
    )
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert "BUDGET_EXHAUSTED" in proposal.reasoning
    assert "RATE_LIMITED" in proposal.reasoning
    assert len(proposal.reasoning) <= 8000
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_relay_down_connect_error_resolves_to_forced_hold(
    proposal_schema: dict[str, Any],
) -> None:
    """A relay that is DOWN (connection refused on every call) degrades
    through the taxonomy end to end: analysts degrade, the debate is cut
    short, and the trader failure resolves to a forced hold — the pipeline
    never crashes and never skips the record."""

    def handler(_request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused")

    llm = MintRouterLLM(
        base_url="https://mintrouter.test",
        api_key="sk-test-key",
        price_table=PriceTable.load_default(),
        transport=httpx.MockTransport(handler),
        sleep=lambda _delay: None,
        monotonic=lambda: 0.0,
        rng=lambda: 0.0,
    )
    state = run_pipeline(llm, _inputs())
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert "LLM_UNAVAILABLE" in proposal.reasoning
    assert proposal.analyst_summaries.market.summary.startswith("unavailable:")
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_malformed_trader_output_reprompts_once_then_forces_hold(
    proposal_schema: dict[str, Any],
) -> None:
    llm = ScriptedLLM({ROLE_TRADER: ["not json", "still not json"]})
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.HOLD
    assert "MALFORMED_LLM_OUTPUT" in proposal.reasoning
    assert llm.calls[ROLE_TRADER] == 2
    # Both malformed attempts spent tokens and are accounted for.
    trader_entries = [cost for cost in proposal.model_costs if cost.node == ROLE_TRADER]
    assert len(trader_entries) == 2
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_malformed_output_recovers_after_single_reprompt() -> None:
    llm = ScriptedLLM({ROLE_TRADER: ["not json"]})
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert proposal.action is Action.OPEN_LONG
    assert llm.calls[ROLE_TRADER] == 2
    trader_entries = [cost for cost in proposal.model_costs if cost.node == ROLE_TRADER]
    assert len(trader_entries) == 2


def test_unavailable_bull_researcher_cuts_debate_short_but_still_trades(
    proposal_schema: dict[str, Any],
) -> None:
    llm = ScriptedLLM({ROLE_BULL_RESEARCHER: [LLMUnavailableError("relay down")]})
    state = _run(llm)
    proposal = state["proposal"]
    assert proposal is not None
    assert state["debate_rounds"] == []
    assert "Debate cut short" in proposal.debate_summary
    assert llm.calls.get(ROLE_BEAR_RESEARCHER, 0) == 0
    assert llm.calls.get(ROLE_DEBATE_JUDGE, 0) == 0
    assert llm.calls[ROLE_TRADER] == 1
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


class PaddedCostsLLM:
    """Wraps the bullish StubLLM, attaching ``pad`` synthetic extra cost entries
    to every response so a run overflows the 32-entry model_costs cap."""

    def __init__(self, pad: int) -> None:
        self._inner = bullish_scenario()
        self._pad = pad
        self.emitted: list[ModelCost] = []

    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
        response = self._inner.complete(role=role, symbol=symbol, prompt=prompt)
        extras = tuple(
            ModelCost(
                node=f"{role}_retry_{i}",
                model=response.model,
                input_tokens=10,
                output_tokens=0,
                cost_usd=Decimal("0.0000015"),
            )
            for i in range(self._pad)
        )
        real = ModelCost(
            node=role,
            model=response.model,
            input_tokens=response.input_tokens,
            output_tokens=response.output_tokens,
            cost_usd=response.cost_usd,
        )
        self.emitted.extend([*extras, real])
        return replace(response, extra_costs=extras)


def test_model_costs_overflow_aggregates_to_cap_without_dropping_cost(
    proposal_schema: dict[str, Any],
) -> None:
    llm = PaddedCostsLLM(pad=4)  # 9 calls x 5 entries = 45 raw cost entries.
    state = run_pipeline(llm, _inputs())
    proposal = state["proposal"]
    assert proposal is not None
    assert len(llm.emitted) > MODEL_COSTS_CAP
    assert len(proposal.model_costs) == MODEL_COSTS_CAP
    assert proposal.model_costs[-1].node == OVERFLOW_NODE
    total_emitted = sum((entry.cost_usd for entry in llm.emitted), Decimal("0"))
    total_reported = sum((entry.cost_usd for entry in proposal.model_costs), Decimal("0"))
    assert total_reported == total_emitted
    Draft202012Validator(proposal_schema).validate(proposal.to_json_dict())


def test_estimated_cost_nodes_are_propagated_to_state() -> None:
    inner = bullish_scenario()

    class EstimatingLLM:
        def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
            response = inner.complete(role=role, symbol=symbol, prompt=prompt)
            if role == ROLE_NEWS_ANALYST:
                return replace(response, estimated_cost_nodes=(ROLE_NEWS_ANALYST,))
            return response

    state = run_pipeline(EstimatingLLM(), _inputs())
    assert state["estimated_cost_nodes"] == [ROLE_NEWS_ANALYST]
    assert state["proposal"] is not None
