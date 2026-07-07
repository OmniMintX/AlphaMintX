"""LLM client interface and the deterministic StubLLM used in CI (no network).

The ``LLMClient`` protocol is deliberately minimal so a mintrouter-backed client
(ADR-0004: mintrouter is the sole LLM gateway) can be swapped in without touching
the pipeline.
"""

from __future__ import annotations

import json
from collections.abc import Mapping
from dataclasses import dataclass, field
from decimal import Decimal
from typing import Protocol

from alphamintx_agent_plane.contract.models import TraceModelCost

ROLE_MARKET_ANALYST = "market_analyst"
ROLE_NEWS_ANALYST = "news_analyst"
ROLE_FUNDAMENTAL_ANALYST = "fundamental_analyst"
ROLE_BULL_RESEARCHER = "bull_researcher"
ROLE_BEAR_RESEARCHER = "bear_researcher"
ROLE_DEBATE_JUDGE = "debate_judge"
ROLE_TRADER = "trader"

PIPELINE_ROLES: tuple[str, ...] = (
    ROLE_MARKET_ANALYST,
    ROLE_NEWS_ANALYST,
    ROLE_FUNDAMENTAL_ANALYST,
    ROLE_BULL_RESEARCHER,
    ROLE_BEAR_RESEARCHER,
    ROLE_DEBATE_JUDGE,
    ROLE_TRADER,
)

_COST_PER_INPUT_TOKEN = Decimal("0.000001")
_COST_PER_OUTPUT_TOKEN = Decimal("0.000002")

DEFAULT_STUB_MODEL_NAME = "stub-model"


@dataclass(frozen=True)
class LLMResponse:
    text: str
    model: str
    input_tokens: int
    output_tokens: int
    cost_usd: Decimal
    # The SUCCESSFUL attempt's per-attempt billing join key (X-Request-Id,
    # billing-and-metering.md); None in stub mode (no network, nothing to join).
    request_id: str | None = None
    # Cost entries spent by failed attempts within the same call (e.g. estimated
    # entries for timed-out attempts before a successful retry), and the nodes
    # whose entries are estimates (llm-routing-and-budget.md §3).
    extra_costs: tuple[TraceModelCost, ...] = field(default=())
    estimated_cost_nodes: tuple[str, ...] = field(default=())


class LLMClient(Protocol):
    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse: ...


class StubLLM:
    """Deterministic canned responses keyed by ``(role, symbol)``; for CI, no network."""

    def __init__(
        self, responses: Mapping[tuple[str, str], str], model_name: str = DEFAULT_STUB_MODEL_NAME
    ) -> None:
        self._responses = dict(responses)
        self._model_name = model_name

    def complete(self, *, role: str, symbol: str, prompt: str) -> LLMResponse:
        try:
            text = self._responses[(role, symbol)]
        except KeyError as exc:
            raise KeyError(f"no canned response for role={role!r} symbol={symbol!r}") from exc
        input_tokens = max(1, len(prompt) // 4)
        output_tokens = max(1, len(text) // 4)
        cost = input_tokens * _COST_PER_INPUT_TOKEN + output_tokens * _COST_PER_OUTPUT_TOKEN
        return LLMResponse(
            text=text,
            model=self._model_name,
            input_tokens=input_tokens,
            output_tokens=output_tokens,
            cost_usd=cost,
        )


def _json(payload: Mapping[str, object]) -> str:
    return json.dumps(payload, sort_keys=True)


def bullish_scenario(
    symbol: str = "BTC/USDT",
    *,
    model_name: str = DEFAULT_STUB_MODEL_NAME,
    trader_overrides: Mapping[str, object] | None = None,
) -> StubLLM:
    """Canned scenario whose trader output is a confident open_long.

    ``trader_overrides`` shallow-merges into the trader-role response dict
    before JSON serialization (overrides replace existing keys and may add new ones).
    """
    trader: dict[str, object] = {
        "action": "open_long",
        "size_quote": "1500.00",
        "entry_type": "limit",
        "limit_price": "64250.50",
        "stop_loss": "62965.49",
        "take_profit": "66820.52",
        "time_in_force": "gtc",
        "confidence": 0.72,
        "reasoning": "Momentum breakout long with a 2% stop below the breakout level.",
    }
    responses = {
        (ROLE_MARKET_ANALYST, symbol): _json({
            "signal": "bullish",
            "confidence": 0.78,
            "summary": "Breakout above the 20-day range high on 1.8x average volume; RSI 61.",
        }),
        (ROLE_NEWS_ANALYST, symbol): _json({
            "signal": "bullish",
            "confidence": 0.65,
            "summary": "Net positive sentiment on ETF inflow coverage; no adverse headlines.",
        }),
        (ROLE_FUNDAMENTAL_ANALYST, symbol): _json({
            "signal": "neutral",
            "confidence": 0.55,
            "summary": "On-chain activity flat week-over-week; funding rates near neutral.",
        }),
        (ROLE_BULL_RESEARCHER, symbol): _json({
            "argument": "Momentum breakout with volume confirmation and supportive flows.",
            "score": 0.74,
        }),
        (ROLE_BEAR_RESEARCHER, symbol): _json({
            "argument": "Macro tightening risk and thin liquidity argue for caution.",
            "score": 0.41,
        }),
        (ROLE_DEBATE_JUDGE, symbol): _json({
            "summary": "Bull case stronger for a short-horizon long with a tight stop.",
        }),
        (ROLE_TRADER, symbol): _json({**trader, **(trader_overrides or {})}),
    }
    return StubLLM(responses, model_name=model_name)


def low_confidence_scenario(
    symbol: str = "BTC/USDT",
    *,
    model_name: str = DEFAULT_STUB_MODEL_NAME,
    trader_overrides: Mapping[str, object] | None = None,
) -> StubLLM:
    """Canned scenario where the trader's conviction is below the 0.3 hold threshold.

    ``trader_overrides`` shallow-merges into the trader-role response dict
    before JSON serialization (overrides replace existing keys and may add new ones).
    """
    trader: dict[str, object] = {
        "action": "open_long",
        "size_quote": "500.00",
        "entry_type": "market",
        "stop_loss": "61000.00",
        "time_in_force": "gtc",
        "confidence": 0.22,
        "reasoning": "Weak long idea in a rangebound market; conviction is low.",
    }
    responses = {
        (ROLE_MARKET_ANALYST, symbol): _json({
            "signal": "neutral",
            "confidence": 0.4,
            "summary": "Price inside a multi-day range; no actionable setup.",
        }),
        (ROLE_NEWS_ANALYST, symbol): _json({
            "signal": "bearish",
            "confidence": 0.35,
            "summary": "Mildly negative headlines, low volume of coverage.",
        }),
        (ROLE_FUNDAMENTAL_ANALYST, symbol): _json({
            "signal": "neutral",
            "confidence": 0.5,
            "summary": "No material changes since the previous run.",
        }),
        (ROLE_BULL_RESEARCHER, symbol): _json({
            "argument": "A range breakout could develop, but confirmation is absent.",
            "score": 0.3,
        }),
        (ROLE_BEAR_RESEARCHER, symbol): _json({
            "argument": "Sentiment is soft yet not weak enough to short the range.",
            "score": 0.35,
        }),
        (ROLE_DEBATE_JUDGE, symbol): _json({
            "summary": "Neither side established an edge; wait for range resolution.",
        }),
        (ROLE_TRADER, symbol): _json({**trader, **(trader_overrides or {})}),
    }
    return StubLLM(responses, model_name=model_name)
