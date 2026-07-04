"""Market snapshots for pipeline inputs (read-only public data — explicitly
permitted by docs/ARCHITECTURE.md §Plane boundary rules; no credentials, no
trading endpoints, never an exchange SDK).

``BinanceSnapshotProvider`` GETs public 1h klines and formats a compact,
deterministic ``market_data`` string (``Decimal``, never float — ADR-0003) on a
CLOSED-candle basis: one extra kline is fetched and the last (forming) row is
dropped before formatting (backtest-engine.md §Lookahead migration note), so
live snapshots and backtest snapshots share the same basis. ``format_market_data``
is the shared formatter reused by the backtest emitter.
News/fundamentals are static placeholders in Phase 1. A fetch failure raises
the typed ``SnapshotError``: the tick records the failure and moves on — the
scheduler loop never crashes on market data.
"""

from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal, InvalidOperation
from typing import Any, Protocol

import httpx

from alphamintx_agent_plane.contract.models import decimal_to_str

BINANCE_BASE_URL = "https://api.binance.com"
# Optional override (market-data.md §Endpoint overrides): a market-data-only
# mirror such as https://data-api.binance.vision, or a testnet. Read-only
# public data either way — never a trading endpoint.
ENV_BINANCE_BASE_URL = "ALPHAMINTX_BINANCE_BASE_URL"
KLINES_PATH = "/api/v3/klines"
KLINES_INTERVAL = "1h"
KLINES_LIMIT = 24
# Binance returns the FORMING candle as the last row; fetch one extra so the
# snapshot window still holds KLINES_LIMIT closed candles after dropping it.
KLINES_FETCH_LIMIT = KLINES_LIMIT + 1
DEFAULT_TIMEOUT_SECONDS = 10.0

NO_NEWS_PLACEHOLDER = "no news feed in phase 1"
NO_FUNDAMENTALS_PLACEHOLDER = "no fundamentals feed in phase 1"

_RATIO_QUANTUM = Decimal("0.01")


class SnapshotError(RuntimeError):
    """Market snapshot fetch/parse failure; the tick records it and moves on."""


@dataclass(frozen=True)
class MarketSnapshot:
    market_data: str
    news: str
    fundamentals: str


class MarketSnapshotProvider(Protocol):
    def snapshot(self, symbol: str) -> MarketSnapshot: ...


def binance_symbol(symbol: str) -> str:
    """Map a contract symbol (``BTC/USDT``) to the Binance form (``BTCUSDT``)."""
    base, sep, quote = symbol.partition("/")
    if not sep or not base or not quote:
        raise SnapshotError(f"cannot map symbol {symbol!r} to a Binance symbol")
    return f"{base}{quote}"


class BinanceSnapshotProvider:
    """Public REST klines -> compact deterministic market_data string."""

    def __init__(
        self,
        *,
        base_url: str = BINANCE_BASE_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.BaseTransport | None = None,
    ) -> None:
        self._client = httpx.Client(base_url=base_url.rstrip("/"), transport=transport)
        self._timeout_seconds = timeout_seconds

    def snapshot(self, symbol: str) -> MarketSnapshot:
        params = {
            "symbol": binance_symbol(symbol),
            "interval": KLINES_INTERVAL,
            "limit": str(KLINES_FETCH_LIMIT),
        }
        try:
            response = self._client.get(
                KLINES_PATH, params=params, timeout=self._timeout_seconds
            )
        except httpx.HTTPError as exc:
            raise SnapshotError(
                f"klines fetch failed for {symbol}: {type(exc).__name__}"
            ) from exc
        if response.status_code != 200:
            raise SnapshotError(
                f"klines fetch for {symbol} returned HTTP {response.status_code}"
            )
        try:
            klines: Any = response.json()
        except ValueError as exc:
            raise SnapshotError(f"klines response for {symbol} is not JSON") from exc
        if not isinstance(klines, list):
            raise SnapshotError(f"klines response for {symbol} is empty or malformed")
        if len(klines) < 2:
            raise SnapshotError(
                f"klines response for {symbol} has too few rows for a closed-candle snapshot"
            )
        # Drop the last (forming) row: the snapshot is closed-candle only.
        return MarketSnapshot(
            market_data=format_market_data(symbol, klines[:-1]),
            news=NO_NEWS_PLACEHOLDER,
            fundamentals=NO_FUNDAMENTALS_PLACEHOLDER,
        )


def format_market_data(symbol: str, klines: Any) -> str:
    """``close=... high_24h=... low_24h=... volume_ratio=...`` from CLOSED klines.

    Kline fields (Binance): [open_time, open, high, low, close, volume, ...].
    ``volume_ratio`` is the last candle's volume over the window's average,
    quantized to 2 decimal places — deterministic Decimal formatting throughout.
    Shared by the live provider and the backtest emitter (identical behavior on
    both, so M1/M2 lookahead comparisons are meaningful).
    """
    if not isinstance(klines, list) or not klines:
        raise SnapshotError(f"klines response for {symbol} is empty or malformed")
    try:
        highs = [Decimal(str(entry[2])) for entry in klines]
        lows = [Decimal(str(entry[3])) for entry in klines]
        closes = [Decimal(str(entry[4])) for entry in klines]
        volumes = [Decimal(str(entry[5])) for entry in klines]
    except (IndexError, TypeError, InvalidOperation) as exc:
        raise SnapshotError(f"klines response for {symbol} is malformed") from exc
    average_volume = sum(volumes, Decimal("0")) / Decimal(len(volumes))
    if average_volume == 0:
        volume_ratio = Decimal("0")
    else:
        volume_ratio = volumes[-1] / average_volume
    return (
        f"close={decimal_to_str(closes[-1])} "
        f"high_24h={decimal_to_str(max(highs))} "
        f"low_24h={decimal_to_str(min(lows))} "
        f"volume_ratio={decimal_to_str(volume_ratio.quantize(_RATIO_QUANTUM))}"
    )
