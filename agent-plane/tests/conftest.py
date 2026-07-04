"""Shared paths and fixtures: the cross-plane schemas and golden fixtures."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
CONTRACTS_DIR = REPO_ROOT / "contracts"
FIXTURES_DIR = CONTRACTS_DIR / "fixtures"


def load_json(path: Path) -> dict[str, Any]:
    with path.open(encoding="utf-8") as handle:
        data: dict[str, Any] = json.load(handle)
    return data


@pytest.fixture(scope="session")
def proposal_schema() -> dict[str, Any]:
    return load_json(CONTRACTS_DIR / "proposal.schema.json")


@pytest.fixture(scope="session")
def verdict_schema() -> dict[str, Any]:
    return load_json(CONTRACTS_DIR / "riskverdict.schema.json")


@pytest.fixture(scope="session")
def trace_schema() -> dict[str, Any]:
    return load_json(CONTRACTS_DIR / "agent_trace.schema.json")
