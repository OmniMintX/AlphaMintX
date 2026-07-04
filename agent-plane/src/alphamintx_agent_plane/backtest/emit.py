"""Stage-1 backtest proposal emitter (docs/specs/backtest-engine.md).

Runs the REAL pipeline (run_pipeline + StubLLM) once per decision tick over a
canonical dataset file and writes proposals.jsonl: line 1 is the meta line
``{"kind":"backtest_meta","strategy_id":...,"symbol":...,"interval":...,
"dataset_sha256":...,"seed":...,"mask_level":...,"window":...,"scenario":...}``
— the exact key set and order of the Go ``MetaLine`` struct (service.go), whose
decoder REJECTS unknown keys. ``window``/``scenario`` are recorded so the
artifact alone is enough to re-run stage 1 reproducibly; every
following line is ``{"tick_number":...,"snapshot_sha256":...,"proposal":{...}}``
— exactly one per grid tick t in [0, N-1], ascending, INCLUDING gapped ticks
(the Go replay requires tick_number 0..N-1 sequential, one line per dataset
tick). Early ticks see min(window, available) closed rows. Compact JSON, LF
lines, byte-deterministic.

Determinism mirrors e2e/emit.py: all ids are uuid5 in NAMESPACE_BACKTEST
(uuid5 of ``uuid.NAMESPACE_URL`` + ``https://alphamintx.dev/backtest``) with the
pinned name string ``f"{strategy_id}/{seed}/{tick_number}/{field}"`` where
``field`` is the pipeline IdFactory name (``agent_trace_id``, ``proposal_id``).
The decision tick t evaluates at ``T = first_open_time + (t+1)*d_ms`` with
proposal ``created_at = T + 1s`` exactly (contract ``rfc3339_utc`` formatting
via the pipeline clock).

Open-loop tripwire (spec-normative): this module makes NO HTTP calls.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import uuid
from collections.abc import Callable, Sequence
from datetime import UTC, datetime, timedelta
from pathlib import Path

from alphamintx_agent_plane.backtest.dataset import (
    KlineRow,
    binance_klines,
    closed_window,
    grid_index,
    interval_ms,
    load_dataset,
    tick_count,
)
from alphamintx_agent_plane.llm.stub import (
    StubLLM,
    bullish_scenario,
    low_confidence_scenario,
)
from alphamintx_agent_plane.pipeline.graph import (
    Clock,
    IdFactory,
    PipelineInput,
    run_pipeline,
)
from alphamintx_agent_plane.scheduler.snapshot import (
    NO_FUNDAMENTALS_PLACEHOLDER,
    NO_NEWS_PLACEHOLDER,
    format_market_data,
)

NAMESPACE_BACKTEST = uuid.uuid5(uuid.NAMESPACE_URL, "https://alphamintx.dev/backtest")

MASK_M0 = "M0"
MASK_M1 = "M1"
MASK_LEVELS: tuple[str, ...] = (MASK_M0, MASK_M1)

SCENARIO_BULLISH = "bullish"
SCENARIO_LOW_CONFIDENCE = "low_confidence"
# The seed is an id-salt and meta field only: StubLLM responses are canned per
# (role, symbol), so nothing in the emit path is stochastic.
SCENARIOS: dict[str, Callable[[str], StubLLM]] = {
    SCENARIO_BULLISH: bullish_scenario,
    SCENARIO_LOW_CONFIDENCE: low_confidence_scenario,
}

_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)


def _id_factory_for(strategy_id: str, seed: int, tick_number: int) -> IdFactory:
    def factory(field: str) -> uuid.UUID:
        return uuid.uuid5(NAMESPACE_BACKTEST, f"{strategy_id}/{seed}/{tick_number}/{field}")

    return factory


def _fixed_clock(at: datetime) -> Clock:
    return lambda: at


def _masked_window(rows: list[KlineRow], t: int, window: int, mask: str) -> list[KlineRow]:
    """Snapshot window at tick t. M0 and M1 MUST be byte-identical on honest
    code: M1 exists because it masks by PHYSICAL truncation — a structurally
    different mechanism — so an M0 index-mask slicing bug shows as a byte diff."""
    if mask == MASK_M0:
        return closed_window(rows, t, window)
    first_open_time = rows[0].open_time
    d_ms = interval_ms(rows[0].interval)
    last_present = -1
    for index, row in enumerate(rows):
        if grid_index(row.open_time, first_open_time, d_ms) <= t:
            last_present = index
    truncated = rows[: last_present + 1]
    return truncated[-window:]


def render(
    rows: list[KlineRow],
    dataset_sha256: str,
    *,
    seed: int,
    strategy_id: str,
    scenario: str,
    window: int,
    symbol: str,
    interval: str,
    mask: str,
) -> str:
    """Render proposals.jsonl: meta line + one line per decision tick, LF-terminated."""
    if scenario not in SCENARIOS:
        raise ValueError(f"unknown scenario {scenario!r}")
    if mask not in MASK_LEVELS:
        raise ValueError(f"mask_level must be one of {list(MASK_LEVELS)}, got {mask!r}")
    if window < 1:
        raise ValueError(f"window must be >= 1, got {window}")
    if str(uuid.UUID(strategy_id)) != strategy_id:
        raise ValueError(f"strategy_id {strategy_id!r} is not a canonical lowercase UUID")
    if rows[0].symbol != symbol or rows[0].interval != interval:
        raise ValueError(
            f"dataset is ({rows[0].symbol}, {rows[0].interval}), "
            f"does not match requested ({symbol}, {interval})"
        )
    total_ticks = tick_count(rows)
    meta = {
        "kind": "backtest_meta",
        "strategy_id": strategy_id,
        "symbol": symbol,
        "interval": interval,
        "dataset_sha256": dataset_sha256,
        "seed": seed,
        "mask_level": mask,
        "window": window,
        "scenario": scenario,
    }
    lines = [json.dumps(meta, separators=(",", ":"))]
    llm = SCENARIOS[scenario](symbol)
    d_ms = interval_ms(interval)
    first_open_time = rows[0].open_time
    for t in range(total_ticks):
        window_rows = _masked_window(rows, t, window, mask)
        market_data = format_market_data(symbol, binance_klines(window_rows))
        snapshot_sha256 = hashlib.sha256(market_data.encode("utf-8")).hexdigest()
        # Decision time T = first_open_time + (t+1)*d_ms; created_at = T + 1s.
        decision_at = _EPOCH + timedelta(milliseconds=first_open_time + (t + 1) * d_ms + 1000)
        state = run_pipeline(
            llm,
            PipelineInput(
                strategy_id=strategy_id,
                symbol=symbol,
                market_data=market_data,
                news=NO_NEWS_PLACEHOLDER,
                fundamentals=NO_FUNDAMENTALS_PLACEHOLDER,
            ),
            id_factory=_id_factory_for(strategy_id, seed, t),
            clock=_fixed_clock(decision_at),
        )
        proposal = state["proposal"]
        if proposal is None:
            # Every pipeline path (including forced-hold and low-confidence
            # holds) returns a proposal; None would break one-line-per-tick.
            raise RuntimeError(f"pipeline produced no proposal at tick {t}")
        lines.append(
            json.dumps(
                {
                    "tick_number": t,
                    "snapshot_sha256": snapshot_sha256,
                    "proposal": proposal.to_json_dict(),
                },
                separators=(",", ":"),
            )
        )
    return "\n".join(lines) + "\n"


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m alphamintx_agent_plane.backtest.emit",
        description="Emit deterministic backtest TradeProposal lines from a dataset file.",
    )
    parser.add_argument("--dataset", required=True, type=Path, help="canonical dataset JSONL")
    parser.add_argument("--out", required=True, type=Path, help="path to write proposals.jsonl")
    parser.add_argument("--seed", required=True, type=int, help="id-salt, recorded in meta")
    parser.add_argument("--strategy-id", required=True, help="strategy UUID")
    parser.add_argument("--scenario", required=True, choices=sorted(SCENARIOS))
    parser.add_argument("--window", required=True, type=int, help="snapshot window (>= 1)")
    parser.add_argument("--symbol", required=True, help="contract symbol, e.g. BTC/USDT")
    parser.add_argument("--interval", required=True, help="candle interval, e.g. 1h")
    parser.add_argument("--mask", required=True, choices=MASK_LEVELS)
    args = parser.parse_args(argv)
    rows, dataset_sha256 = load_dataset(args.dataset)
    text = render(
        rows,
        dataset_sha256,
        seed=args.seed,
        strategy_id=args.strategy_id,
        scenario=args.scenario,
        window=args.window,
        symbol=args.symbol,
        interval=args.interval,
        mask=args.mask,
    )
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_bytes(text.encode("utf-8"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
