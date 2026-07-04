"""Plane-boundary guard: agent-plane must not import exchange SDKs or credential
tooling, in loaded modules or anywhere in its source tree (docs/ARCHITECTURE.md)."""

from __future__ import annotations

import ast
import sys
from pathlib import Path

import alphamintx_agent_plane
import alphamintx_agent_plane.client.controlplane  # noqa: F401
import alphamintx_agent_plane.contract.models  # noqa: F401
import alphamintx_agent_plane.llm.stub  # noqa: F401
import alphamintx_agent_plane.pipeline.graph  # noqa: F401

FORBIDDEN_TOP_LEVEL = {
    "ccxt",
    "binance",
    "bybit",
    "okx",
    "kraken",
    "krakenex",
    "coinbase",
    "web3",
}


def test_no_exchange_modules_loaded() -> None:
    loaded = {name.split(".")[0] for name in sys.modules}
    assert not loaded & FORBIDDEN_TOP_LEVEL


def _imported_top_levels(tree: ast.AST) -> set[str]:
    names: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            names.update(alias.name.split(".")[0] for alias in node.names)
        elif isinstance(node, ast.ImportFrom) and node.module and node.level == 0:
            names.add(node.module.split(".")[0])
    return names


def test_no_exchange_or_credential_imports_in_source() -> None:
    src_root = Path(alphamintx_agent_plane.__file__).resolve().parent
    forbidden = FORBIDDEN_TOP_LEVEL | {"hmac"}
    for module_path in sorted(src_root.rglob("*.py")):
        imported = _imported_top_levels(ast.parse(module_path.read_text(encoding="utf-8")))
        offending = imported & forbidden
        assert not offending, f"{module_path} imports forbidden modules: {offending}"
