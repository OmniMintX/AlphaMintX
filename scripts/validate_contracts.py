"""Validate contracts/*.schema.json and all golden fixtures.

Valid fixtures MUST pass; each proposal_invalid_*.json MUST fail for exactly the
single rule it violates. Run: uv run --with jsonschema python scripts/validate_contracts.py
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

from jsonschema import Draft202012Validator

ROOT = Path(__file__).resolve().parent.parent / "contracts"
FIXTURES = ROOT / "fixtures"


def load(path: Path) -> dict:
    with path.open() as f:
        return json.load(f)


EXPECTED_SCHEMAS = frozenset(
    {"proposal.schema.json", "riskverdict.schema.json", "agent_trace.schema.json"}
)


def main() -> int:
    # Pin the schema set: a silently deleted/renamed/added mirror must fail here,
    # and EVERY schema (agent_trace included) must be valid draft 2020-12.
    present = {p.name for p in ROOT.glob("*.schema.json")}
    assert present == EXPECTED_SCHEMAS, f"schema set drift: {sorted(present ^ EXPECTED_SCHEMAS)}"
    for name in sorted(EXPECTED_SCHEMAS):
        Draft202012Validator.check_schema(load(ROOT / name))
    proposal_schema = load(ROOT / "proposal.schema.json")
    verdict_schema = load(ROOT / "riskverdict.schema.json")
    print(f"OK   all {len(EXPECTED_SCHEMAS)} schemas present and valid draft 2020-12")

    proposal_v = Draft202012Validator(proposal_schema)
    verdict_v = Draft202012Validator(verdict_schema)

    for name in (
        "proposal_open_long.json",
        "proposal_hold.json",
        "proposal_decimal_edges.json",
    ):
        proposal_v.validate(load(FIXTURES / name))
        print(f"OK   {name} validates")

    for name in ("verdict_reject_daily_loss.json", "verdict_clip.json"):
        verdict_v.validate(load(FIXTURES / name))
        print(f"OK   {name} validates")

    errors = list(proposal_v.iter_errors(load(FIXTURES / "proposal_invalid_no_sl.json")))
    assert errors, "proposal_invalid_no_sl.json unexpectedly validated"
    assert all("stop_loss" in e.message for e in errors), [e.message for e in errors]
    print(f"OK   proposal_invalid_no_sl.json rejected on stop_loss ({len(errors)} error(s))")

    errors = list(proposal_v.iter_errors(load(FIXTURES / "proposal_invalid_numeric_size.json")))
    assert errors, "proposal_invalid_numeric_size.json unexpectedly validated"
    assert all("size_quote" in e.json_path for e in errors), [e.json_path for e in errors]
    print(f"OK   proposal_invalid_numeric_size.json rejected on size_quote ({len(errors)} error(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main())
