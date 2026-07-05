"""HttpTransport retry policy and failure taxonomy (persistence-and-api.md §HTTP API)."""

from __future__ import annotations

import json
import re
from collections.abc import Mapping, Sequence
from typing import Any

import httpx
import pytest

from alphamintx_agent_plane.client.controlplane import (
    ControlPlaneClient,
    DryRunTransport,
    StrategyAuth,
    heartbeat_path,
)
from alphamintx_agent_plane.client.errors import (
    ControlPlaneAuthError,
    ControlPlaneConflictError,
    ControlPlaneContractError,
    ControlPlaneRequestError,
    ControlPlaneUnavailableError,
)
from alphamintx_agent_plane.client.http import (
    MAX_ATTEMPTS,
    RETRY_AFTER_MAX_SECONDS,
    HttpTransport,
)
from alphamintx_agent_plane.contract.models import TradeProposal

BASE_URL = "http://control-plane.test"
PATH = "/api/v1/strategies/s/proposals"
HEADERS = {"Authorization": "Bearer secret-token"}
BODY = {"tick_number": 0, "proposal": {}}


class _Recorder:
    """Scripted responses; records every request and every backoff sleep."""

    def __init__(self, script: Sequence[httpx.Response | Exception]) -> None:
        self.script = list(script)
        self.requests: list[httpx.Request] = []
        self.sleeps: list[float] = []

    def handler(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        step = self.script[len(self.requests) - 1]
        if isinstance(step, Exception):
            raise step
        return step

    def transport(self) -> HttpTransport:
        return HttpTransport(
            base_url=BASE_URL,
            transport=httpx.MockTransport(self.handler),
            sleep=self.sleeps.append,
            rng=lambda: 0.5,
        )


def _ok(payload: dict[str, Any] | None = None) -> httpx.Response:
    return httpx.Response(200, json=payload if payload is not None else {"status": "ok"})


def test_success_returns_json_object() -> None:
    recorder = _Recorder([_ok({"verdict": {}})])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"verdict": {}}
    assert len(recorder.requests) == 1


def test_429_retried_honoring_retry_after() -> None:
    recorder = _Recorder([httpx.Response(429, headers={"Retry-After": "7"}), _ok()])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert len(recorder.requests) == 2
    assert recorder.sleeps == [7.0]


def test_5xx_retried_with_exponential_backoff() -> None:
    recorder = _Recorder([httpx.Response(500), httpx.Response(503), _ok()])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert len(recorder.requests) == 3
    # base * 2**retry_index + rng(): 1*1+0.5 then 1*2+0.5.
    assert recorder.sleeps == [1.5, 2.5]


def test_timeout_retried_then_unavailable_after_max_attempts() -> None:
    recorder = _Recorder([httpx.ConnectTimeout("boom")] * MAX_ATTEMPTS)
    with pytest.raises(ControlPlaneUnavailableError):
        recorder.transport().post(PATH, HEADERS, BODY)
    assert len(recorder.requests) == MAX_ATTEMPTS
    assert len(recorder.sleeps) == MAX_ATTEMPTS - 1


def test_transport_error_retried_then_recovers() -> None:
    recorder = _Recorder([httpx.ConnectError("refused"), _ok()])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert len(recorder.requests) == 2


@pytest.mark.parametrize("status", [401, 403])
def test_auth_failures_are_not_retried(status: int) -> None:
    recorder = _Recorder([httpx.Response(status)])
    with pytest.raises(ControlPlaneAuthError):
        recorder.transport().post(PATH, HEADERS, BODY)
    assert len(recorder.requests) == 1
    assert recorder.sleeps == []


def test_409_conflict_is_typed_and_not_retried() -> None:
    # The Go wire shape is {"code": ..., "message": ...} (respond.go).
    recorder = _Recorder(
        [httpx.Response(409, json={"code": "IDEMPOTENCY_CONFLICT", "message": "dup"})]
    )
    with pytest.raises(ControlPlaneConflictError) as excinfo:
        recorder.transport().post(PATH, HEADERS, BODY)
    assert excinfo.value.error_code == "IDEMPOTENCY_CONFLICT"
    assert len(recorder.requests) == 1


def test_409_legacy_error_key_is_tolerated() -> None:
    recorder = _Recorder([httpx.Response(409, json={"error": "RUN_TICK_CONFLICT"})])
    with pytest.raises(ControlPlaneConflictError) as excinfo:
        recorder.transport().post(PATH, HEADERS, BODY)
    assert excinfo.value.error_code == "RUN_TICK_CONFLICT"


def test_other_4xx_is_not_retried() -> None:
    recorder = _Recorder([httpx.Response(400, json={"error": "bad"})])
    with pytest.raises(ControlPlaneRequestError) as excinfo:
        recorder.transport().post(PATH, HEADERS, BODY)
    assert excinfo.value.status_code == 400
    assert len(recorder.requests) == 1


def test_error_messages_never_contain_the_bearer_token() -> None:
    recorder = _Recorder([httpx.Response(500)] * MAX_ATTEMPTS)
    with pytest.raises(ControlPlaneUnavailableError) as excinfo:
        recorder.transport().post(PATH, HEADERS, BODY)
    assert "secret-token" not in str(excinfo.value)


def test_negative_retry_after_does_not_raise_and_sleeps_zero() -> None:
    recorder = _Recorder([httpx.Response(429, headers={"Retry-After": "-5"}), _ok()])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert len(recorder.requests) == 2
    assert recorder.sleeps == []  # clamped to 0.0 => no sleep call at all


def test_huge_retry_after_is_capped_at_the_maximum() -> None:
    recorder = _Recorder([httpx.Response(429, headers={"Retry-After": "86400"}), _ok()])
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert recorder.sleeps == [RETRY_AFTER_MAX_SECONDS] == [30.0]


def test_non_numeric_retry_after_falls_back_to_exponential_backoff() -> None:
    recorder = _Recorder(
        [httpx.Response(429, headers={"Retry-After": "Fri, 04 Jul 2026 12:00:00 GMT"}), _ok()]
    )
    assert recorder.transport().post(PATH, HEADERS, BODY) == {"status": "ok"}
    assert recorder.sleeps == [1.5]  # base * 2**0 + rng()


# --- Cross-plane response contract (Go handler envelope) regression tests ----

SID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
PROPOSAL_ID = "0f4a2d66-9c1e-4d2b-8a3f-5b6c7d8e9f01"


def _proposal() -> TradeProposal:
    summary = {"signal": "bullish", "confidence": 0.8, "summary": "test summary"}
    return TradeProposal.model_validate(
        {
            "schema_version": "1.0",
            "proposal_id": PROPOSAL_ID,
            "strategy_id": SID,
            "agent_trace_id": "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
            "created_at": "2026-07-04T12:00:00Z",
            "symbol": "BTC/USDT",
            "action": "open_long",
            "size_quote": "100.00",
            "entry": {"type": "market"},
            "stop_loss": "60000.00",
            "take_profit": "70000.00",
            "time_in_force": "gtc",
            "confidence": 0.8,
            "reasoning": "test reasoning",
            "analyst_summaries": {
                "market": summary,
                "news": summary,
                "fundamental": summary,
            },
            "debate_summary": "test debate",
            "model_costs": [],
        }
    )


# A verbatim Go-shaped verdict object as the control-plane handler emits it.
_GO_VERDICT_JSON = (
    '{"schema_version":"1.0",'
    '"verdict_id":"7c1d9e2f-3a4b-4c5d-8e6f-9a0b1c2d3e4f",'
    f'"proposal_id":"{PROPOSAL_ID}",'
    '"decision":"approve",'
    '"reasons":[],'
    '"limits_snapshot":{"symbol_whitelist":["BTC/USDT"],"max_open_positions":3,'
    '"per_position_notional_cap_quote":"2000.00","daily_loss_limit_quote":"500.00",'
    '"max_drawdown_pct":10,"max_orders_per_minute":6,"require_stop_loss":true,'
    '"equity_quote":"10000.00","peak_equity_quote":"10000.00",'
    '"daily_realized_pnl_quote":"0","open_positions_count":0,'
    '"pending_entry_orders_count":0,"mark_price":"64180.10"},'
    '"evaluated_at":"2026-07-04T12:00:01Z"}'
)


def _client(recorder: _Recorder) -> ControlPlaneClient:
    return ControlPlaneClient(
        recorder.transport(), StrategyAuth(strategy_id=SID, bearer_token="tok")
    )


def _json_response(body: str) -> httpx.Response:
    return httpx.Response(200, content=body, headers={"Content-Type": "application/json"})


def test_go_envelope_response_parses_end_to_end() -> None:
    body = (
        '{"verdict":' + _GO_VERDICT_JSON + ',"submitted":true,"pending_approval":false}'
    )
    recorder = _Recorder([_json_response(body)])
    submission = _client(recorder).submit_proposal(_proposal(), tick_number=0)
    assert submission.verdict.proposal_id == PROPOSAL_ID
    assert submission.verdict.decision.value == "approve"
    assert submission.submitted is True
    assert submission.pending_approval is False
    assert submission.submit_error_code is None


def test_go_envelope_duplicate_replay_with_submit_error_code_parses() -> None:
    body = (
        '{"verdict":' + _GO_VERDICT_JSON + ',"submitted":false,'
        '"submit_error_code":"EXCHANGE_REJECTED","pending_approval":false}'
    )
    recorder = _Recorder([_json_response(body)])
    submission = _client(recorder).submit_proposal(_proposal(), tick_number=0)
    assert submission.submitted is False
    assert submission.submit_error_code == "EXCHANGE_REJECTED"


def test_bare_verdict_body_without_envelope_fails_loudly() -> None:
    # A bare verdict at the top level (no {"verdict": ...} envelope) must be a
    # typed contract error, pinning that the envelope is REQUIRED.
    recorder = _Recorder([_json_response(_GO_VERDICT_JSON)])
    with pytest.raises(ControlPlaneContractError):
        _client(recorder).submit_proposal(_proposal(), tick_number=0)


def _trace_envelope() -> dict[str, Any]:
    return {"strategy_id": SID, "run_id": "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}


def test_trace_ingest_go_response_parses_end_to_end() -> None:
    # The Go traces handler answers 200 {"run_id": ...} for fresh AND
    # duplicate ingests (persistence-and-api.md HTTP API table).
    body = '{"run_id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}'
    recorder = _Recorder([_json_response(body)])
    _client(recorder).submit_trace(_trace_envelope())
    assert len(recorder.requests) == 1


def test_trace_ingest_201_fails_loudly() -> None:
    # Regression (live smoke run): a 201 Created from the traces endpoint
    # broke the wire — only 200 is success; anything else is a typed error.
    body = '{"run_id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}'
    resp = httpx.Response(201, content=body, headers={"Content-Type": "application/json"})
    recorder = _Recorder([resp])
    with pytest.raises(ControlPlaneRequestError):
        _client(recorder).submit_trace(_trace_envelope())


# --- Heartbeat contract (docs/specs/watchdog.md WD-1/WD-5/WD-26) --------------


def test_heartbeat_path_is_api_v1() -> None:
    # Pins the WD-1 stub fix: the repo-wide path convention is /api/v1/...
    assert heartbeat_path(SID) == f"/api/v1/strategies/{SID}/heartbeat"


def test_heartbeat_go_envelope_parses_end_to_end() -> None:
    # A verbatim Go-shaped receipt body as the control-plane handler emits it.
    body = '{"received_at":"2026-07-04T12:00:01Z"}'
    recorder = _Recorder([_json_response(body)])
    _client(recorder).heartbeat()
    assert len(recorder.requests) == 1
    assert recorder.requests[0].url.path == f"/api/v1/strategies/{SID}/heartbeat"
    assert json.loads(recorder.requests[0].content) == {}  # WD-4: body is {}


def test_heartbeat_requires_no_more_than_a_json_object() -> None:
    # WD-26: the client MAY ignore received_at — a bare {} 200 is success.
    recorder = _Recorder([_json_response("{}")])
    _client(recorder).heartbeat()
    assert len(recorder.requests) == 1


def test_heartbeat_ignores_received_at_type_drift() -> None:
    # WD-26: the client MUST NOT require more than a JSON object; a drifted
    # received_at type never turns a recorded beat into a per-30s error storm.
    recorder = _Recorder([_json_response('{"received_at":1234567890}')])
    _client(recorder).heartbeat()
    assert len(recorder.requests) == 1


def test_heartbeat_non_object_response_is_a_contract_error() -> None:
    class ListTransport:
        def post(
            self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
        ) -> Any:
            return ["not", "an", "object"]

    client = ControlPlaneClient(
        ListTransport(), StrategyAuth(strategy_id=SID, bearer_token="tok")
    )
    with pytest.raises(ControlPlaneContractError):
        client.heartbeat()


def test_dry_run_heartbeat_stub_matches_the_wd5_envelope() -> None:
    response = DryRunTransport().post(
        heartbeat_path(SID), {"Authorization": "Bearer tok"}, {}
    )
    assert set(response) == {"received_at"}
    assert re.fullmatch(
        r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z",
        response["received_at"],
    )
