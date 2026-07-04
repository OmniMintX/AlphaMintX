"""Validate contracts/*.schema.json and all golden fixtures.

Valid fixtures MUST pass; proposal_invalid_no_sl.json MUST fail for exactly the
missing-stop_loss conditional. Run: uv run --with jsonschema python scripts/validate_contracts.py
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


def main() -> int:
    proposal_schema = load(ROOT / "proposal.schema.json")
    verdict_schema = load(ROOT / "riskverdict.schema.json")
    for schema in (proposal_schema, verdict_schema):
        Draft202012Validator.check_schema(schema)
    print("OK   schemas are valid draft 2020-12")

    proposal_v = Draft202012Validator(proposal_schema)
    verdict_v = Draft202012Validator(verdict_schema)

    for name in ("proposal_open_long.json", "proposal_hold.json"):
        proposal_v.validate(load(FIXTURES / name))
        print(f"OK   {name} validates")

    verdict_v.validate(load(FIXTURES / "verdict_reject_daily_loss.json"))
    print("OK   verdict_reject_daily_loss.json validates")

    errors = list(proposal_v.iter_errors(load(FIXTURES / "proposal_invalid_no_sl.json")))
    assert errors, "proposal_invalid_no_sl.json unexpectedly validated"
    assert all("stop_loss" in e.message for e in errors), [e.message for e in errors]
    print(f"OK   proposal_invalid_no_sl.json rejected on stop_loss ({len(errors)} error(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main())
