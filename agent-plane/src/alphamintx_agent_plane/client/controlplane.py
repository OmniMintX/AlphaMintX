"""Typed client stub for the control-plane HTTP API: POST /v1/proposals + heartbeat.

Plane boundary (docs/ARCHITECTURE.md): this module holds NO exchange credentials
and places NO orders. Its only secret is the per-strategy bearer token issued by
the control-plane (revoked on kill-switch). Phase 0 ships ``DryRunTransport``,
which validates the outbound payload and echoes a canned verdict — no network.
"""

from __future__ import annotations

import os
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, Protocol

from alphamintx_agent_plane.contract.models import (
    SCHEMA_VERSION,
    Decision,
    LimitsSnapshot,
    RiskVerdict,
    TradeProposal,
    utc_now_rfc3339,
)

HEARTBEAT_INTERVAL_SECONDS = 30
PROPOSALS_PATH = "/v1/proposals"
TOKEN_ENV_VAR = "ALPHAMINTX_STRATEGY_TOKEN"


def heartbeat_path(strategy_id: str) -> str:
    return f"/v1/strategies/{strategy_id}/heartbeat"


@dataclass(frozen=True)
class StrategyAuth:
    """Per-strategy bearer token scoped to (strategy_id, tenant)."""

    strategy_id: str
    bearer_token: str

    @classmethod
    def from_env(cls, strategy_id: str, env_var: str = TOKEN_ENV_VAR) -> StrategyAuth:
        token = os.environ.get(env_var, "")
        if not token:
            raise RuntimeError(f"strategy bearer token not found in ${env_var}")
        return cls(strategy_id=strategy_id, bearer_token=token)


class Transport(Protocol):
    """POST transport; a real HTTP implementation slots in behind the same shape."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]: ...


class DryRunTransport:
    """Phase-0 transport: validates the payload and echoes a canned approve verdict."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        authorization = headers.get("Authorization", "")
        if not authorization.startswith("Bearer ") or authorization == "Bearer ":
            raise PermissionError("missing or malformed bearer token")
        if path.endswith(PROPOSALS_PATH):
            proposal = TradeProposal.model_validate(dict(body))
            return self._canned_verdict(proposal).to_json_dict()
        if path.endswith("/heartbeat"):
            return {"status": "ok"}
        raise ValueError(f"DryRunTransport does not know path {path!r}")

    @staticmethod
    def _canned_verdict(proposal: TradeProposal) -> RiskVerdict:
        snapshot = LimitsSnapshot.model_validate(
            {
                "symbol_whitelist": [proposal.symbol],
                "max_open_positions": 3,
                "per_position_notional_cap_quote": "2000.00",
                "daily_loss_limit_quote": "500.00",
                "max_drawdown_pct": 10,
                "max_orders_per_minute": 6,
                "require_stop_loss": True,
                "equity_quote": "10000.00",
                "peak_equity_quote": "10000.00",
                "daily_realized_pnl_quote": "0",
                "open_positions_count": 0,
                "pending_entry_orders_count": 0,
                "mark_price": "64180.10",
            }
        )
        return RiskVerdict(
            schema_version=SCHEMA_VERSION,
            verdict_id=str(uuid.uuid4()),
            proposal_id=proposal.proposal_id,
            decision=Decision.APPROVE,
            reasons=[],
            limits_snapshot=snapshot,
            evaluated_at=utc_now_rfc3339(),
        )


class ControlPlaneClient:
    """Submits TradeProposals and heartbeats over an injected transport."""

    def __init__(self, transport: Transport, auth: StrategyAuth, base_url: str = "") -> None:
        self._transport = transport
        self._auth = auth
        self._base_url = base_url.rstrip("/")

    def _headers(self) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self._auth.bearer_token}",
            "Content-Type": "application/json",
        }

    def submit_proposal(self, proposal: TradeProposal) -> RiskVerdict:
        if proposal.strategy_id != self._auth.strategy_id:
            raise ValueError("proposal strategy_id does not match the token scope")
        response = self._transport.post(
            self._base_url + PROPOSALS_PATH, self._headers(), proposal.to_json_dict()
        )
        return RiskVerdict.model_validate(response)

    def heartbeat(self) -> None:
        """POST the per-strategy heartbeat; call every ``HEARTBEAT_INTERVAL_SECONDS``."""
        self._transport.post(
            self._base_url + heartbeat_path(self._auth.strategy_id), self._headers(), {}
        )
