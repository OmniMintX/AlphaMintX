"""Pydantic v2 mirrors of contracts/proposal.schema.json and contracts/riskverdict.schema.json.

Money/price/size fields parse into ``Decimal`` and serialize back to plain decimal
strings (no exponent, digits preserved exactly), per ADR-0003 and
docs/specs/proposal-contract.md. Unknown fields are a validation error everywhere.
"""

from __future__ import annotations

import re
from datetime import UTC, datetime
from decimal import Decimal
from enum import StrEnum
from typing import Annotated, Any, Literal, Self

from pydantic import (
    BaseModel,
    BeforeValidator,
    ConfigDict,
    Field,
    PlainSerializer,
    StringConstraints,
    model_validator,
)

SCHEMA_VERSION: Literal["1.0"] = "1.0"

_DECIMAL_RE = re.compile(r"^(0|[1-9][0-9]*)(\.[0-9]+)?$")
_SIGNED_DECIMAL_RE = re.compile(r"^-?(0|[1-9][0-9]*)(\.[0-9]+)?$")

_UUID_PATTERN = r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
_UTC_TIMESTAMP_PATTERN = r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$"
_SYMBOL_PATTERN = r"^[A-Z0-9]{2,15}/[A-Z0-9]{2,10}$"
_REASON_CODE_PATTERN = r"^[A-Z][A-Z0-9_]*$"


def utc_now_rfc3339() -> str:
    """Current time as an RFC 3339 UTC timestamp with the mandatory ``Z`` suffix."""
    return datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def decimal_to_str(value: Decimal) -> str:
    """Render a ``Decimal`` as a plain decimal string: no exponent, no digit loss."""
    return format(value, "f")


def _parse_decimal(value: object, pattern: re.Pattern[str], max_length: int) -> Decimal:
    if isinstance(value, Decimal):
        text = decimal_to_str(value)
    elif isinstance(value, str):
        text = value
    else:
        raise ValueError("decimal fields must be decimal strings, never JSON numbers")
    if pattern.fullmatch(text) is None:
        raise ValueError(f"invalid decimal string: {text!r}")
    if len(text) > max_length:
        raise ValueError(f"decimal string exceeds {max_length} characters")
    return Decimal(text)


def _parse_unsigned_decimal(value: object) -> Decimal:
    return _parse_decimal(value, _DECIMAL_RE, 34)


def _parse_signed_decimal(value: object) -> Decimal:
    return _parse_decimal(value, _SIGNED_DECIMAL_RE, 35)


DecimalStr = Annotated[
    Decimal,
    BeforeValidator(_parse_unsigned_decimal),
    PlainSerializer(decimal_to_str, return_type=str),
]
SignedDecimalStr = Annotated[
    Decimal,
    BeforeValidator(_parse_signed_decimal),
    PlainSerializer(decimal_to_str, return_type=str),
]
UuidStr = Annotated[str, StringConstraints(pattern=_UUID_PATTERN)]
UtcTimestamp = Annotated[str, StringConstraints(pattern=_UTC_TIMESTAMP_PATTERN, max_length=35)]
SymbolStr = Annotated[str, StringConstraints(pattern=_SYMBOL_PATTERN)]
ReasonCode = Annotated[str, StringConstraints(pattern=_REASON_CODE_PATTERN, max_length=64)]


class Action(StrEnum):
    OPEN_LONG = "open_long"
    OPEN_SHORT = "open_short"
    CLOSE = "close"
    HOLD = "hold"


class EntryType(StrEnum):
    MARKET = "market"
    LIMIT = "limit"


class TimeInForce(StrEnum):
    GTC = "gtc"
    IOC = "ioc"


class Signal(StrEnum):
    BULLISH = "bullish"
    BEARISH = "bearish"
    NEUTRAL = "neutral"


class Decision(StrEnum):
    APPROVE = "approve"
    REJECT = "reject"
    CLIP = "clip"
    ESCALATE = "escalate"


class ContractModel(BaseModel):
    """Base for contract mirrors: unknown fields are a validation error."""

    model_config = ConfigDict(extra="forbid", protected_namespaces=())

    def to_json_dict(self) -> dict[str, Any]:
        """JSON-mode dump with absent optionals omitted, matching the schemas."""
        return self.model_dump(mode="json", exclude_none=True)


class Entry(ContractModel):
    type: EntryType
    limit_price: DecimalStr | None = None

    @model_validator(mode="after")
    def _limit_price_rule(self) -> Self:
        if self.type is EntryType.LIMIT and self.limit_price is None:
            raise ValueError("limit_price is required when entry.type is 'limit'")
        if self.type is EntryType.MARKET and self.limit_price is not None:
            raise ValueError("limit_price is forbidden when entry.type is 'market'")
        return self


class AnalystSummary(ContractModel):
    signal: Signal
    confidence: float = Field(ge=0, le=1)
    summary: str = Field(max_length=2000)


class AnalystSummaries(ContractModel):
    market: AnalystSummary
    news: AnalystSummary
    fundamental: AnalystSummary


class ModelCost(ContractModel):
    node: str = Field(max_length=64)
    model: str = Field(max_length=64)
    input_tokens: int = Field(ge=0)
    output_tokens: int = Field(ge=0)
    cost_usd: DecimalStr


class TradeProposal(ContractModel):
    """TradeProposal v1 — the single agent-plane -> control-plane interface."""

    schema_version: Literal["1.0"]
    proposal_id: UuidStr
    strategy_id: UuidStr
    agent_trace_id: UuidStr
    created_at: UtcTimestamp
    symbol: SymbolStr
    action: Action
    size_quote: DecimalStr
    entry: Entry
    stop_loss: DecimalStr | None = None
    take_profit: DecimalStr | None = None
    time_in_force: TimeInForce
    confidence: float = Field(ge=0, le=1)
    reasoning: str = Field(max_length=8000)
    analyst_summaries: AnalystSummaries
    debate_summary: str = Field(max_length=4000)
    model_costs: list[ModelCost] = Field(max_length=32)

    @model_validator(mode="after")
    def _stop_loss_take_profit_rules(self) -> Self:
        if self.action in (Action.OPEN_LONG, Action.OPEN_SHORT):
            if self.stop_loss is None:
                raise ValueError(f"stop_loss is required when action is {self.action.value!r}")
            # Rule 3: size positivity for opens.
            if self.size_quote <= 0:
                raise ValueError(f"size_quote must be > 0 for {self.action.value}")
        else:
            if self.stop_loss is not None:
                raise ValueError(f"stop_loss is forbidden when action is {self.action.value!r}")
            if self.take_profit is not None:
                raise ValueError(f"take_profit is forbidden when action is {self.action.value!r}")
            if self.action is Action.HOLD and self.size_quote != 0:
                raise ValueError('size_quote must be "0" for hold')
        return self

    @model_validator(mode="after")
    def _stop_and_target_placement(self) -> Self:
        """Rules 1-2 against a known entry price (limit entries only; market entries
        are checked by the Risk Gate against the current mark)."""
        if self.entry.type is not EntryType.LIMIT or self.entry.limit_price is None:
            return self
        if self.stop_loss is None:
            return self
        entry_price = self.entry.limit_price
        if entry_price <= 0:
            raise ValueError("entry price must be > 0")
        stop = decimal_to_str(self.stop_loss)
        entry = decimal_to_str(entry_price)
        if self.action is Action.OPEN_LONG:
            if self.stop_loss >= entry_price:
                raise ValueError(f"stop_loss {stop} must be below entry {entry} for open_long")
            if self.take_profit is not None and self.take_profit <= entry_price:
                raise ValueError(
                    f"take_profit {decimal_to_str(self.take_profit)} must be above entry "
                    f"{entry} for open_long"
                )
        elif self.action is Action.OPEN_SHORT:
            if self.stop_loss <= entry_price:
                raise ValueError(f"stop_loss {stop} must be above entry {entry} for open_short")
            if self.take_profit is not None and self.take_profit >= entry_price:
                raise ValueError(
                    f"take_profit {decimal_to_str(self.take_profit)} must be below entry "
                    f"{entry} for open_short"
                )
        return self


class Reason(ContractModel):
    code: ReasonCode
    message: str = Field(max_length=500)


class LimitsSnapshot(ContractModel):
    """Configured limits plus the runtime inputs the Risk Gate actually evaluated."""

    symbol_whitelist: list[SymbolStr]
    max_open_positions: int = Field(ge=0)
    per_position_notional_cap_quote: DecimalStr
    daily_loss_limit_quote: DecimalStr
    max_drawdown_pct: float = Field(ge=0)
    max_orders_per_minute: int = Field(ge=0)
    require_stop_loss: bool
    l2_max_size_quote: DecimalStr | None = None
    l2_allowed_symbols: list[SymbolStr] | None = None
    equity_quote: DecimalStr
    peak_equity_quote: DecimalStr
    daily_realized_pnl_quote: SignedDecimalStr
    open_positions_count: int = Field(ge=0)
    pending_entry_orders_count: int = Field(ge=0)
    mark_price: DecimalStr


class RiskVerdict(ContractModel):
    """RiskVerdict v1 — deterministic Risk Gate output for a TradeProposal."""

    schema_version: Literal["1.0"]
    verdict_id: UuidStr
    proposal_id: UuidStr
    decision: Decision
    clipped_size_quote: DecimalStr | None = None
    reasons: list[Reason] = Field(max_length=32)
    limits_snapshot: LimitsSnapshot
    evaluated_at: UtcTimestamp

    @model_validator(mode="after")
    def _decision_rules(self) -> Self:
        if self.decision is Decision.CLIP:
            if self.clipped_size_quote is None:
                raise ValueError("clipped_size_quote is required when decision is 'clip'")
        elif self.clipped_size_quote is not None:
            raise ValueError("clipped_size_quote is forbidden unless decision is 'clip'")
        if self.decision in (Decision.REJECT, Decision.CLIP) and not self.reasons:
            raise ValueError("reject/clip verdicts require at least one reason")
        return self
