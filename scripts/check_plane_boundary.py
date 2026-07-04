"""Plane-boundary gate (docs/ARCHITECTURE.md §Plane boundary rules, ADR-0004).

Fails (exit 1, reporting file:line:pattern) on: (a) direct LLM-provider
hostnames/SDK imports in agent-plane — mintrouter is the ONLY LLM gateway;
(b) control-plane DB access from agent-plane/src — the local LangGraph
checkpoint DB is the single sqlite3 allowlist entry; (c) exchange TRADING
endpoints/credentials in agent-plane/src — read-only market data feeds stay
permitted, so plain Binance market-data hosts are NOT banned; (d) any LLM
call surface in control-plane. Run: python3 scripts/check_plane_boundary.py
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Mapping, NamedTuple

REPO = Path(__file__).resolve().parent.parent

SKIP_DIRS = {
    ".git",
    ".venv",
    "__pycache__",
    "node_modules",
    ".mypy_cache",
    ".pytest_cache",
    ".ruff_cache",
}

# The spec-sanctioned LOCAL LangGraph checkpoint DB (persistence-and-api.md
# §Agent-plane checkpoint/resume) — never the control-plane store.
CHECKPOINT_DB = "agent-plane/src/alphamintx_agent_plane/scheduler/checkpoint.py"

# Exact, case-sensitive substrings. Direct provider hostnames/SDKs (ADR-0004,
# llm-routing-and-budget.md §1 tripwire list).
LLM_PROVIDER_PATTERNS: tuple[str, ...] = (
    "api.openai.com",
    "api.anthropic.com",
    "generativelanguage.googleapis.com",
    "api.mistral.ai",
    "api.cohere.",
    "api.groq.com",
    "api.together.",
    "openrouter.ai",
    "bedrock-runtime",
    "openai.azure.com",
    "import openai",
    "from openai",
    "import anthropic",
    "from anthropic",
    "import google.generativeai",
    "langchain_openai",
    "langchain-openai",
    "langchain_anthropic",
    "langchain-anthropic",
    "litellm",
    '__import__("openai"',
    "__import__('openai'",
)


class Rule(NamedTuple):
    name: str
    roots: tuple[str, ...]
    patterns: tuple[str, ...]
    # pattern -> repo-relative POSIX paths where that pattern is sanctioned.
    allow: Mapping[str, frozenset[str]] = {}


RULES: tuple[Rule, ...] = (
    Rule(
        name="agent-plane direct LLM provider (mintrouter is the sole gateway, ADR-0004)",
        roots=("agent-plane/src", "agent-plane/tests"),
        patterns=LLM_PROVIDER_PATTERNS,
    ),
    Rule(
        name="agent-plane control-plane DB access (no DB grants, ARCHITECTURE.md)",
        roots=("agent-plane/src",),
        patterns=(
            "CONTROLPLANE_DB",
            "import sqlite3",
            "from sqlite3",
            "import sqlalchemy",
            "from sqlalchemy",
            "import psycopg",
            "from psycopg",
            "import asyncpg",
            "from asyncpg",
            "import aiosqlite",
            "from aiosqlite",
        ),
        allow={
            "import sqlite3": frozenset({CHECKPOINT_DB}),
            "from sqlite3": frozenset({CHECKPOINT_DB}),
        },
    ),
    Rule(
        name="agent-plane exchange trading surface (read-only market data stays allowed)",
        roots=("agent-plane/src",),
        patterns=(
            "/api/v3/order",
            "/fapi/v1/order",
            "X-MBX-APIKEY",
            "BINANCE_API_KEY",
            "BINANCE_API_SECRET",
            "import ccxt",
            "from ccxt",
        ),
    ),
    Rule(
        name="control-plane LLM call surface (control-plane MUST NOT call LLMs)",
        roots=("control-plane",),
        patterns=LLM_PROVIDER_PATTERNS + ("MINTROUTER_BASE_URL",),
    ),
)


def iter_text_files(root: Path):
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        if any(part in SKIP_DIRS for part in path.relative_to(REPO).parts):
            continue
        raw = path.read_bytes()
        if b"\0" in raw[:8192]:
            continue  # binary
        try:
            yield path, raw.decode("utf-8")
        except UnicodeDecodeError:
            continue


def check(rule: Rule) -> list[str]:
    violations: list[str] = []
    for root in rule.roots:
        base = REPO / root
        if not base.is_dir():
            continue
        for path, text in iter_text_files(base):
            rel = path.relative_to(REPO).as_posix()
            for lineno, line in enumerate(text.splitlines(), start=1):
                for pattern in rule.patterns:
                    if pattern in line and rel not in rule.allow.get(pattern, ()):
                        violations.append(f"{rel}:{lineno}: {pattern!r} — {rule.name}")
    return violations


def main() -> int:
    violations: list[str] = []
    for rule in RULES:
        found = check(rule)
        violations.extend(found)
        if not found:
            print(f"OK   {rule.name}")
    for v in violations:
        print(f"FAIL {v}", file=sys.stderr)
    return 1 if violations else 0


if __name__ == "__main__":
    sys.exit(main())
