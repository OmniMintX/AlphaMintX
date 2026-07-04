"""BinanceSnapshotProvider: klines parsing, deterministic Decimal formatting,
symbol mapping, and the typed SnapshotError on any fetch/parse failure."""

from __future__ import annotations

import json

import httpx
import pytest

from alphamintx_agent_plane.scheduler.snapshot import (
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


def test_snapshot_formats_market_data_deterministically() -> None:
    klines = [
        _kline("64500.10", "63000.00", "63500.00", "100.5"),
        _kline("65000.00", "63900.50", "64250.50", "301.5"),
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
    assert params == {"symbol": "BTCUSDT", "interval": "1h", "limit": "24"}


def test_repeated_snapshots_are_identical() -> None:
    klines = [_kline("65000.00", "63000.00", "64250.50", "200")]
    transport = httpx.MockTransport(lambda _: httpx.Response(200, json=klines))
    provider = _provider(transport)
    assert provider.snapshot("BTC/USDT") == provider.snapshot("BTC/USDT")


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
        [],  # empty klines
        {"klines": []},  # not a list of rows
        [[1700000000000, "64000.00"]],  # row too short
        [[1700000000000, "x", "y", "z", "w", "v"]],  # non-numeric fields
    ],
)
def test_malformed_klines_raise_snapshot_error(body: object) -> None:
    transport = httpx.MockTransport(lambda _: httpx.Response(200, content=json.dumps(body)))
    with pytest.raises(SnapshotError):
        _provider(transport).snapshot("BTC/USDT")
