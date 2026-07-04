"""Typed client for the control-plane ingestion API: proposals, traces, heartbeat.

Plane boundary (docs/ARCHITECTURE.md): this module holds NO exchange credentials
and places NO orders. Its only secret is the per-strategy bearer token issued by
the control-plane (revoked on kill-switch). ``DryRunTransport`` validates the
outbound payload and echoes a canned verdict — no network; the live
``client.http.HttpTransport`` slots in behind the same ``Transport`` shape.
"""

from __future__ import annotations

import os
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, Protocol

from pydantic import ValidationError

from alphamintx_agent_plane.client.errors import ControlPlaneContractError
from alphamintx_agent_plane.contract.models import (
    SCHEMA_VERSION,
    Decision,
    LimitsSnapshot,
    RiskVerdict,
    TradeProposal,
    utc_now_rfc3339,
)

HEARTBEAT_INTERVAL_SECONDS = 30
TOKEN_ENV_VAR = "ALPHAMINTX_STRATEGY_TOKEN"


def proposals_path(strategy_id: str) -> str:
    return f"/api/v1/strategies/{strategy_id}/proposals"


def traces_path(strategy_id: str) -> str:
    return f"/api/v1/strategies/{strategy_id}/traces"


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


@dataclass(frozen=True)
class ProposalSubmission:
    """Parsed proposal-POST response envelope (cross-plane contract).

    The control-plane returns HTTP 200 with ``{"verdict": {...}, "submitted"?:
    bool, "submit_error_code"?: str, "pending_approval"?: bool}`` for BOTH fresh
    and duplicate submissions; the optional flags surface downstream submission
    state to the caller.
    """

    verdict: RiskVerdict
    submitted: bool | None = None
    submit_error_code: str | None = None
    pending_approval: bool | None = None


class Transport(Protocol):
    """POST transport; a real HTTP implementation slots in behind the same shape."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]: ...


class DryRunTransport:
    """Dry-run transport: validates outbound payloads and echoes canned responses."""

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        authorization = headers.get("Authorization", "")
        if not authorization.startswith("Bearer ") or authorization == "Bearer ":
            raise PermissionError("missing or malformed bearer token")
        if path.endswith("/proposals"):
            tick_number = body.get("tick_number")
            if not isinstance(tick_number, int) or isinstance(tick_number, bool):
                raise ValueError("proposal envelope tick_number must be an integer")
            if tick_number < 0:
                raise ValueError("proposal envelope tick_number must be >= 0")
            proposal = TradeProposal.model_validate(body["proposal"])
            return {
                "verdict": self._canned_verdict(proposal).to_json_dict(),
                "submitted": True,
                "pending_approval": False,
            }
        if path.endswith("/traces"):
            return {"status": "ok"}
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
    """Submits TradeProposals, trace envelopes, and heartbeats over an injected transport.

    Delivery is at-least-once and idempotent by ``proposal_id`` / ``run_id``
    (docs/ARCHITECTURE.md §Plane authentication): a duplicate re-POST of the same
    payload returns the stored response verbatim, so the scheduler may safely
    re-submit after a crash-resume of the same tick.
    """

    def __init__(self, transport: Transport, auth: StrategyAuth, base_url: str = "") -> None:
        self._transport = transport
        self._auth = auth
        self._base_url = base_url.rstrip("/")

    def _headers(self) -> dict[str, str]:
        return {
            "Authorization": f"Bearer {self._auth.bearer_token}",
            "Content-Type": "application/json",
        }

    def submit_proposal(
        self, proposal: TradeProposal, *, tick_number: int
    ) -> ProposalSubmission:
        """POST the submission envelope ``{tick_number, proposal}``; parse the response.

        The response MUST be the ``{"verdict": {...}, ...}`` envelope — a bare
        verdict object (or any other shape) is a cross-plane contract violation
        and raises ``ControlPlaneContractError``.
        """
        if proposal.strategy_id != self._auth.strategy_id:
            raise ValueError("proposal strategy_id does not match the token scope")
        if tick_number < 0:
            raise ValueError("tick_number must be >= 0")
        response = self._transport.post(
            self._base_url + proposals_path(self._auth.strategy_id),
            self._headers(),
            {"tick_number": tick_number, "proposal": proposal.to_json_dict()},
        )
        return self._parse_submission(response)

    @staticmethod
    def _parse_submission(response: Mapping[str, Any]) -> ProposalSubmission:
        verdict_raw = response.get("verdict")
        if not isinstance(verdict_raw, Mapping):
            raise ControlPlaneContractError(
                "proposal response violates the cross-plane contract: expected the "
                '{"verdict": {...}} envelope, got no object "verdict" field'
            )
        try:
            verdict = RiskVerdict.model_validate(dict(verdict_raw))
        except ValidationError as exc:
            raise ControlPlaneContractError(
                f"proposal response envelope carries an invalid verdict: {exc}"
            ) from exc
        submitted = response.get("submitted")
        if submitted is not None and not isinstance(submitted, bool):
            raise ControlPlaneContractError("proposal response 'submitted' must be a bool")
        submit_error_code = response.get("submit_error_code")
        if submit_error_code is not None and not isinstance(submit_error_code, str):
            raise ControlPlaneContractError(
                "proposal response 'submit_error_code' must be a string"
            )
        pending_approval = response.get("pending_approval")
        if pending_approval is not None and not isinstance(pending_approval, bool):
            raise ControlPlaneContractError(
                "proposal response 'pending_approval' must be a bool"
            )
        return ProposalSubmission(
            verdict=verdict,
            submitted=submitted,
            submit_error_code=submit_error_code,
            pending_approval=pending_approval,
        )

    def submit_trace(self, envelope: Mapping[str, Any]) -> None:
        """POST the agent_trace envelope (contracts/agent_trace.schema.json)."""
        strategy_id = envelope.get("strategy_id")
        if strategy_id != self._auth.strategy_id:
            raise ValueError("trace strategy_id does not match the token scope")
        self._transport.post(
            self._base_url + traces_path(self._auth.strategy_id), self._headers(), envelope
        )

    def heartbeat(self) -> None:
        """POST the per-strategy heartbeat; call every ``HEARTBEAT_INTERVAL_SECONDS``.

        Endpoint deferred; watchdog reaction is Phase 3 (ARCHITECTURE.md §Plane
        authentication keeps the 30 s heartbeat normative).
        """
        self._transport.post(
            self._base_url + heartbeat_path(self._auth.strategy_id), self._headers(), {}
        )
