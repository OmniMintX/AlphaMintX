"""Versioned local price table (docs/specs/llm-routing-and-budget.md §3).

mintrouter does not return ``cost_usd``; agent-plane computes cost locally from
``usage`` tokens and the checked-in ``prices.json`` (USD per 1M input/output tokens
per model, with an ``as_of`` date). All arithmetic is ``Decimal`` (ADR-0003).
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass
from datetime import UTC, date, datetime
from decimal import Decimal
from pathlib import Path

from alphamintx_agent_plane.llm.errors import LLMConfigError

logger = logging.getLogger(__name__)

DEFAULT_PRICES_PATH = Path(__file__).with_name("prices.json")
STALENESS_DAYS = 90
_ONE_MILLION = Decimal(1_000_000)


@dataclass(frozen=True)
class ModelPrice:
    """USD per 1M input tokens and per 1M output tokens for one model."""

    input_usd_per_1m: Decimal
    output_usd_per_1m: Decimal


class PriceTable:
    """Immutable model→price map with an ``as_of`` date and staleness warning."""

    def __init__(self, as_of: date, prices: dict[str, ModelPrice]) -> None:
        self._as_of = as_of
        self._prices = dict(prices)

    @classmethod
    def load(cls, path: Path) -> PriceTable:
        with path.open(encoding="utf-8") as handle:
            data = json.load(handle)
        try:
            as_of = date.fromisoformat(data["as_of"])
            prices = {
                model: ModelPrice(
                    input_usd_per_1m=Decimal(entry["input_usd_per_1m"]),
                    output_usd_per_1m=Decimal(entry["output_usd_per_1m"]),
                )
                for model, entry in data["models"].items()
            }
        except (KeyError, TypeError, ValueError, ArithmeticError) as exc:
            raise LLMConfigError(f"invalid price table {path}: {exc}") from exc
        if not prices:
            raise LLMConfigError(f"price table {path} contains no models")
        return cls(as_of, prices)

    @classmethod
    def load_default(cls) -> PriceTable:
        return cls.load(DEFAULT_PRICES_PATH)

    @property
    def as_of(self) -> date:
        return self._as_of

    def __contains__(self, model: str) -> bool:
        return model in self._prices

    def models(self) -> frozenset[str]:
        return frozenset(self._prices)

    def cost_usd(self, model: str, input_tokens: int, output_tokens: int) -> Decimal:
        """Exact Decimal cost: tokens × USD-per-1M ÷ 1e6, never float."""
        try:
            price = self._prices[model]
        except KeyError as exc:
            raise LLMConfigError(f"model {model!r} is not in the price table") from exc
        return (
            Decimal(input_tokens) * price.input_usd_per_1m
            + Decimal(output_tokens) * price.output_usd_per_1m
        ) / _ONE_MILLION

    def warn_if_stale(self, today: date | None = None) -> bool:
        """Warn (spec §3, risk R1) when ``as_of`` is older than 90 days."""
        current = today if today is not None else datetime.now(UTC).date()
        age_days = (current - self._as_of).days
        if age_days > STALENESS_DAYS:
            logger.warning(
                "price table is stale: as_of=%s is %d days old (limit %d); "
                "cost accounting may be inaccurate",
                self._as_of.isoformat(),
                age_days,
                STALENESS_DAYS,
            )
            return True
        return False
