"""Canonical backtest dataset (JSONL klines) parsing, validation, and slicing.

The dataset file is produced by the Go ``backtestctl fetch`` step and consumed
read-only here (docs/specs/backtest-engine.md): one kline per LF-terminated
compact-JSON line, a single (symbol, interval) per file, strictly ascending
grid-aligned ``open_time`` (ms epoch), OHLCV as decimal strings (contract
DecimalStr rules, ADR-0003). Gaps in the grid are LEGAL; duplicates,
out-of-order, misaligned, or mixed rows are validation errors.
``close_time`` is never stored — it is derived as ``open_time + d_ms``.
``dataset_sha256`` is the sha256 hex of the ENTIRE file bytes.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from alphamintx_agent_plane.contract.models import DecimalStr, SymbolStr, decimal_to_str

# The only legal dataset intervals (cross-plane contract interval table).
INTERVAL_SECONDS: dict[str, int] = {
    "1m": 60,
    "3m": 180,
    "5m": 300,
    "15m": 900,
    "30m": 1800,
    "1h": 3600,
    "2h": 7200,
    "4h": 14400,
    "6h": 21600,
    "8h": 28800,
    "12h": 43200,
    "1d": 86400,
}


class DatasetError(ValueError):
    """Dataset file violates the canonical-form contract."""


class KlineRow(BaseModel):
    """One dataset line; extra keys are a validation error (canonical form)."""

    model_config = ConfigDict(extra="forbid")

    symbol: SymbolStr
    interval: str
    open_time: int = Field(strict=True)
    open: DecimalStr
    high: DecimalStr
    low: DecimalStr
    close: DecimalStr
    volume: DecimalStr


def interval_ms(interval: str) -> int:
    """Candle duration in ms; unknown intervals are a validation error."""
    try:
        return INTERVAL_SECONDS[interval] * 1000
    except KeyError:
        raise DatasetError(f"unknown interval {interval!r}") from None


def close_time(open_time: int, d_ms: int) -> int:
    """close_time is DERIVED, never stored: ``open_time + d_ms``."""
    return open_time + d_ms


def grid_index(open_time: int, first_open_time: int, d_ms: int) -> int:
    """Grid index of a row: ``(open_time - first_open_time) // d_ms``."""
    return (open_time - first_open_time) // d_ms


def tick_count(rows: list[KlineRow]) -> int:
    """Total grid ticks N = last grid index + 1 (gapped indices own ticks)."""
    d_ms = interval_ms(rows[0].interval)
    return grid_index(rows[-1].open_time, rows[0].open_time, d_ms) + 1


def load_dataset(path: str | Path) -> tuple[list[KlineRow], str]:
    """Load and strictly validate a dataset file; returns (rows, dataset_sha256)."""
    raw = Path(path).read_bytes()
    sha256 = hashlib.sha256(raw).hexdigest()
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise DatasetError(f"dataset is not UTF-8: {exc}") from exc
    rows: list[KlineRow] = []
    d_ms = 0
    for lineno, line in enumerate(text.splitlines(), start=1):
        if not line:
            raise DatasetError(f"dataset line {lineno}: blank line")
        try:
            payload = json.loads(line)
        except ValueError as exc:
            raise DatasetError(f"dataset line {lineno}: not valid JSON") from exc
        try:
            row = KlineRow.model_validate(payload)
        except ValidationError as exc:
            raise DatasetError(f"dataset line {lineno}: {exc}") from exc
        if not rows:
            d_ms = interval_ms(row.interval)
        else:
            first = rows[0]
            if row.symbol != first.symbol or row.interval != first.interval:
                raise DatasetError(
                    f"dataset line {lineno}: ({row.symbol}, {row.interval}) does not "
                    f"match dataset ({first.symbol}, {first.interval})"
                )
            if row.open_time <= rows[-1].open_time:
                raise DatasetError(
                    f"dataset line {lineno}: open_time {row.open_time} not strictly "
                    f"ascending (previous {rows[-1].open_time})"
                )
            if (row.open_time - first.open_time) % d_ms != 0:
                raise DatasetError(
                    f"dataset line {lineno}: open_time {row.open_time} off the "
                    f"{first.interval} grid anchored at {first.open_time}"
                )
        rows.append(row)
    if not rows:
        raise DatasetError("dataset file is empty")
    return rows, sha256


def closed_window(rows: list[KlineRow], t: int, window: int) -> list[KlineRow]:
    """Trailing min(window, available) rows with grid index <= t (the M0 mask path)."""
    if window < 1:
        raise ValueError(f"window must be >= 1, got {window}")
    first_open_time = rows[0].open_time
    d_ms = interval_ms(rows[0].interval)
    available = [
        row for row in rows if grid_index(row.open_time, first_open_time, d_ms) <= t
    ]
    return available[-window:]


def binance_klines(rows: list[KlineRow]) -> list[list[object]]:
    """Rows as Binance-style kline lists for the shared market_data formatter."""
    return [
        [
            row.open_time,
            decimal_to_str(row.open),
            decimal_to_str(row.high),
            decimal_to_str(row.low),
            decimal_to_str(row.close),
            decimal_to_str(row.volume),
        ]
        for row in rows
    ]
