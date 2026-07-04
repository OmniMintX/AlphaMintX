"""Lookahead checks over emitted proposals.jsonl (docs/specs/backtest-engine.md).

Two modes:

- ``--mode m1 --a f1 --b f2``: compare two emit outputs (M0 vs M1). The meta
  lines legitimately differ in ``mask_level`` (that IS the M0-vs-M1 pair), so
  metas are compared as JSON objects with ``mask_level`` removed; every
  following line must be byte-identical. Reports the first divergence.
- ``--mode m2 --dataset F --proposals F``: per proposal line, rebuild the
  market_data string from the dataset sliced at that tick with an
  INDEPENDENT slicing implementation and compare its sha256 against the
  recorded ``snapshot_sha256``; also validates the meta ``dataset_sha256``
  against the dataset file bytes. The emit window is read from the meta
  line's ``window`` field (recorded for exactly this reproducibility).

Exit 0 = all pass; exit 1 = failure, with a per-tick report on stderr.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from collections.abc import Sequence
from pathlib import Path

from alphamintx_agent_plane.backtest.dataset import (
    binance_klines,
    interval_ms,
    load_dataset,
)
from alphamintx_agent_plane.scheduler.snapshot import format_market_data


def run_m1(a: Path, b: Path) -> int:
    """Compare two proposals.jsonl files, ignoring only the meta mask_level."""
    lines_a = a.read_bytes().split(b"\n")
    lines_b = b.read_bytes().split(b"\n")
    if not lines_a[0] or not lines_b[0]:
        print("m1 FAIL: missing meta line", file=sys.stderr)
        return 1
    meta_a = json.loads(lines_a[0])
    meta_b = json.loads(lines_b[0])
    meta_a.pop("mask_level", None)
    meta_b.pop("mask_level", None)
    if meta_a != meta_b:
        print("m1 FAIL: meta lines differ beyond mask_level", file=sys.stderr)
        print(f"  a: {meta_a!r}", file=sys.stderr)
        print(f"  b: {meta_b!r}", file=sys.stderr)
        return 1
    for number, (line_a, line_b) in enumerate(
        zip(lines_a[1:], lines_b[1:], strict=False), start=2
    ):
        if line_a != line_b:
            print(f"m1 FAIL: first divergence at line {number}", file=sys.stderr)
            print(f"  a: {line_a[:200]!r}", file=sys.stderr)
            print(f"  b: {line_b[:200]!r}", file=sys.stderr)
            return 1
    if len(lines_a) != len(lines_b):
        number = min(len(lines_a), len(lines_b)) + 1
        print(f"m1 FAIL: files diverge in length at line {number}", file=sys.stderr)
        return 1
    total = len(lines_a) - 1
    print(f"m1 PASS: {a} and {b} agree ({total} lines, mask_level excluded)")
    return 0


def run_m2(dataset_path: Path, proposals_path: Path) -> int:
    """Recompute every snapshot_sha256 from the dataset and compare per tick."""
    rows, dataset_sha256 = load_dataset(dataset_path)
    first_open_time = rows[0].open_time
    d_ms = interval_ms(rows[0].interval)
    lines = proposals_path.read_text(encoding="utf-8").splitlines()
    failures: list[str] = []
    if not lines:
        print("m2 FAIL: proposals file is empty (missing meta line)", file=sys.stderr)
        return 1
    meta = json.loads(lines[0])
    if meta.get("kind") != "backtest_meta":
        failures.append(f'meta: kind {meta.get("kind")!r}, want "backtest_meta"')
    window = meta.get("window")
    if not isinstance(window, int) or window < 1:
        print(f"m2 FAIL: meta window {window!r} is not a positive int", file=sys.stderr)
        return 1
    if meta.get("dataset_sha256") != dataset_sha256:
        failures.append(
            f"meta: dataset_sha256 {meta.get('dataset_sha256')} does not match "
            f"dataset file {dataset_sha256}"
        )
    if meta.get("symbol") != rows[0].symbol or meta.get("interval") != rows[0].interval:
        failures.append(
            f"meta: ({meta.get('symbol')}, {meta.get('interval')}) does not match "
            f"dataset ({rows[0].symbol}, {rows[0].interval})"
        )
    symbol = rows[0].symbol
    checked = 0
    for lineno, line in enumerate(lines[1:], start=2):
        payload = json.loads(line)
        try:
            t = int(payload["tick_number"])
            recorded = str(payload["snapshot_sha256"])
        except KeyError as exc:
            failures.append(f"line {lineno}: missing key {exc}")
            continue
        # INDEPENDENT slicing, deliberately duplicated inline: this MUST NOT
        # import dataset.closed_window — if the emitter and this check shared
        # one slicing implementation, a common lookahead bug (reading rows past
        # tick t) would recompute the same wrong snapshot and pass blindly.
        selected = [
            row for row in rows if (row.open_time - first_open_time) // d_ms <= t
        ]
        if len(selected) > window:
            selected = selected[len(selected) - window :]
        market_data = format_market_data(symbol, binance_klines(selected))
        recomputed = hashlib.sha256(market_data.encode("utf-8")).hexdigest()
        if recomputed != recorded:
            failures.append(
                f"tick {t}: recorded snapshot_sha256 {recorded} != recomputed "
                f"{recomputed} (dataset sliced at tick {t})"
            )
        checked += 1
    if failures:
        for failure in failures:
            print(f"m2 FAIL: {failure}", file=sys.stderr)
        return 1
    print(f"m2 PASS: {checked} proposal lines verified against {dataset_path}")
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m alphamintx_agent_plane.backtest.check",
        description="Backtest lookahead checks: m1 byte-compare, m2 snapshot-hash recheck.",
    )
    parser.add_argument("--mode", required=True, choices=("m1", "m2"))
    parser.add_argument("--a", type=Path, help="m1: first proposals.jsonl")
    parser.add_argument("--b", type=Path, help="m1: second proposals.jsonl")
    parser.add_argument("--dataset", type=Path, help="m2: canonical dataset JSONL")
    parser.add_argument("--proposals", type=Path, help="m2: emitted proposals.jsonl")
    args = parser.parse_args(argv)
    if args.mode == "m1":
        if args.a is None or args.b is None:
            parser.error("--mode m1 requires --a and --b")
        return run_m1(args.a, args.b)
    if args.dataset is None or args.proposals is None:
        parser.error("--mode m2 requires --dataset and --proposals")
    return run_m2(args.dataset, args.proposals)


if __name__ == "__main__":
    raise SystemExit(main())
