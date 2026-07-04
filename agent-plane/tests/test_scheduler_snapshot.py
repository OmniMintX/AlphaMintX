"""BinanceSnapshotProvider: klines parsing, deterministic Decimal formatting on
a closed-candle basis (one extra kline fetched, forming row dropped — spec
backtest-engine.md §Lookahead migration note), symbol mapping, and the typed
SnapshotError on any fetch/parse failure."""

from __future__ import annotations

import json

import httpx
import pytest

from alphamintx_agent_plane.scheduler.snapshot import (
    KLINES_FETCH_LIMIT,
    KLINES_LIMIT,
    NO_FUNDAMENTALS_PLACEHOLDER,
    NO_NEWS_PLACEHOLDER,
    BinanceSnapshotProvider,
    SnapshotError,
    binance_symbol,
)


def _kline(high: str, low: str, close: str, volume: str) -> list[object]:
    # Binance kline: [open_time, open, high, low, close, volume, ...trailing fields].
    return [1700000000000, "64000.00", high, low, close, volume, 1700003599999]


def _provider(transport: httpx.MockTransport) -> BinanceSnapshotProvider:
    return BinanceSnapshotProvider(base_url="http://binance.test", transport=transport)


def test_symbol_mapping() -> None:
    assert binance_symbol("BTC/USDT") == "BTCUSDT"
    with pytest.raises(SnapshotError):
        binance_symbol("BTCUSDT")


def test_snapshot_formats_market_data_from_closed_candles_only() -> None:
    # The LAST row is the FORMING candle: its extreme values would change every
    # field below if it were included — the provider must drop it.
    klines = [
        _kline("64500.10", "63000.00", "63500.00", "100.5"),
        _kline("65000.00", "63900.50", "64250.50", "301.5"),
        _kline("99999.00", "1.00", "50000.00", "9999"),
    ]
    requests: list[httpx.Request] = []

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(200, json=klines)

    snapshot = _provider(httpx.MockTransport(handler)).snapshot("BTC/USDT")
    # volume_ratio = 301.5 / ((100.5 + 301.5) / 2) = 1.50, quantized to 2 dp.
    assert snapshot.market_data == (
        "close=64250.50 high_24h=65000.00 low_24h=63000.00 volume_ratio=1.50"
    )
    assert snapshot.news == NO_NEWS_PLACEHOLDER
    assert snapshot.fundamentals == NO_FUNDAMENTALS_PLACEHOLDER
    assert len(requests) == 1
    params = dict(requests[0].url.params)
    # One extra kline fetched so the window keeps KLINES_LIMIT closed candles.
    assert KLINES_FETCH_LIMIT == KLINES_LIMIT + 1
    assert params == {"symbol": "BTCUSDT", "interval": "1h", "limit": str(KLINES_FETCH_LIMIT)}


def test_repeated_snapshots_are_identical() -> None:
    klines = [
        _kline("65000.00", "63000.00", "64250.50", "200"),
        _kline("66000.00", "64000.00", "65000.00", "300"),
    ]
    transport = httpx.MockTransport(lambda _: httpx.Response(200, json=klines))
    provider = _provider(transport)
    assert provider.snapshot("BTC/USDT") == provider.snapshot("BTC/USDT")


@pytest.mark.parametrize(
    "body",
    [
        [],  # no rows at all
        [_kline("65000.00", "63000.00", "64250.50", "200")],  # only the forming candle
    ],
)
def test_too_few_rows_raise_snapshot_error(body: object) -> None:
    transport = httpx.MockTransport(lambda _: httpx.Response(200, content=json.dumps(body)))
    with pytest.raises(SnapshotError, match="too few rows"):
        _provider(transport).snapshot("BTC/USDT")


def test_non_200_raises_snapshot_error() -> None:
    transport = httpx.MockTransport(lambda _: httpx.Response(500))
    with pytest.raises(SnapshotError, match="HTTP 500"):
        _provider(transport).snapshot("BTC/USDT")


def test_transport_failure_raises_snapshot_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused")

    with pytest.raises(SnapshotError, match="fetch failed"):
        _provider(httpx.MockTransport(handler)).snapshot("BTC/USDT")


def test_non_json_body_raises_snapshot_error() -> None:
    transport = httpx.MockTransport(lambda _: httpx.Response(200, content=b"<html>"))
    with pytest.raises(SnapshotError, match="not JSON"):
        _provider(transport).snapshot("BTC/USDT")


@pytest.mark.parametrize(
    "body",
    [
        {"klines": []},  # not a list of rows
        [[1700000000000, "64000.00"], [1700003600000, "64000.00"]],  # rows too short
        [
            [1700000000000, "x", "y", "z", "w", "v"],  # non-numeric fields
            [1700003600000, "x", "y", "z", "w", "v"],
        ],
    ],
)
def test_malformed_klines_raise_snapshot_error(body: object) -> None:
    transport = httpx.MockTransport(lambda _: httpx.Response(200, content=json.dumps(body)))
    with pytest.raises(SnapshotError):
        _provider(transport).snapshot("BTC/USDT")
