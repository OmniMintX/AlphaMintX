"""httpx transport for the control-plane API (docs/specs/persistence-and-api.md §HTTP API).

Retry policy mirrors llm-routing-and-budget.md §1: at most 2 retries, ONLY on
429/5xx/timeouts/transport errors, exponential backoff with jitter, honoring a
``Retry-After`` header when present. A 200 whose body is not valid JSON is a
transport-class failure and retried the same way (the body is never echoed in
errors). 401/403 and 409 conflicts are non-retryable
defects (typed errors); any other 4xx is never retried. The per-strategy bearer
token arrives in the request headers and is never logged or echoed in errors.
"""

from __future__ import annotations

import math
import os
import random
import time
from collections.abc import Callable, Mapping
from typing import Any

import httpx

from alphamintx_agent_plane.client.errors import (
    ControlPlaneAuthError,
    ControlPlaneConflictError,
    ControlPlaneRequestError,
    ControlPlaneUnavailableError,
)

ENV_BASE_URL = "ALPHAMINTX_CONTROLPLANE_BASE_URL"
ENV_TIMEOUT_SECONDS = "ALPHAMINTX_CONTROLPLANE_TIMEOUT_SECONDS"

DEFAULT_TIMEOUT_SECONDS = 10.0
MAX_ATTEMPTS = 3  # 1 initial + at most 2 retries (mirrors llm-routing §1).
BACKOFF_BASE_SECONDS = 1.0
# Retry-After is server-controlled input: clamp to [0, 30] so a hostile or
# misconfigured header can neither block a strategy thread for hours nor make
# time.sleep raise ValueError on a negative value.
RETRY_AFTER_MAX_SECONDS = 30.0


def _retry_after_seconds(response: httpx.Response) -> float | None:
    value = response.headers.get("Retry-After")
    if value is None:
        return None
    try:
        seconds = float(value)
    except ValueError:
        return None
    if not math.isfinite(seconds):
        return None
    return min(max(seconds, 0.0), RETRY_AFTER_MAX_SECONDS)


class HttpTransport:
    """Real POST transport behind ``client.controlplane.Transport``."""

    def __init__(
        self,
        *,
        base_url: str,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.BaseTransport | None = None,
        sleep: Callable[[float], None] = time.sleep,
        rng: Callable[[], float] = random.random,
    ) -> None:
        if not base_url:
            raise ValueError("control-plane base URL must not be empty")
        if timeout_seconds <= 0:
            raise ValueError("control-plane timeout must be > 0 seconds")
        self._timeout_seconds = float(timeout_seconds)
        self._sleep = sleep
        self._rng = rng
        self._client = httpx.Client(base_url=base_url.rstrip("/"), transport=transport)

    @classmethod
    def from_env(
        cls,
        environ: Mapping[str, str] | None = None,
        *,
        transport: httpx.BaseTransport | None = None,
    ) -> HttpTransport:
        """Build from ``ALPHAMINTX_CONTROLPLANE_*`` env vars (fail-fast on defects)."""
        env = os.environ if environ is None else environ
        base_url = env.get(ENV_BASE_URL, "")
        if not base_url:
            raise RuntimeError(f"{ENV_BASE_URL} is required")
        raw_timeout = env.get(ENV_TIMEOUT_SECONDS, str(DEFAULT_TIMEOUT_SECONDS))
        try:
            timeout_seconds = float(raw_timeout)
        except ValueError as exc:
            raise RuntimeError(f"invalid {ENV_TIMEOUT_SECONDS}={raw_timeout!r}") from exc
        return cls(base_url=base_url, timeout_seconds=timeout_seconds, transport=transport)

    def _backoff(self, retry_index: int, retry_after: float | None) -> None:
        if retry_after is not None:
            delay = retry_after
        else:
            delay = BACKOFF_BASE_SECONDS * (2.0**retry_index) + self._rng()
        if delay > 0:
            self._sleep(delay)

    def post(
        self, path: str, headers: Mapping[str, str], body: Mapping[str, Any]
    ) -> dict[str, Any]:
        last_failure = "no attempt was made"
        for attempt in range(MAX_ATTEMPTS):
            try:
                response = self._client.post(
                    path,
                    json=dict(body),
                    headers=dict(headers),
                    timeout=self._timeout_seconds,
                )
            except httpx.TimeoutException:
                last_failure = f"attempt {attempt + 1} timed out"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, None)
                continue
            except httpx.RequestError as exc:
                last_failure = f"attempt {attempt + 1} transport error ({type(exc).__name__})"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, None)
                continue
            status = response.status_code
            if status == 200:
                try:
                    data: Any = response.json()
                except ValueError:
                    # A malformed/truncated 200 body is a transport-class
                    # failure; the body (like the token) is never echoed.
                    last_failure = f"attempt {attempt + 1} received invalid JSON in 200 response"
                    if attempt < MAX_ATTEMPTS - 1:
                        self._backoff(attempt, None)
                    continue
                if not isinstance(data, dict):
                    raise ControlPlaneRequestError(
                        status, f"control-plane returned a non-object body for {path}"
                    )
                return data
            if status in (401, 403):
                raise ControlPlaneAuthError(
                    f"control-plane returned HTTP {status} for {path}: bearer token "
                    "rejected or out of scope; not retried"
                )
            if status == 409:
                error_code = self._error_code(response)
                raise ControlPlaneConflictError(
                    error_code, f"control-plane returned 409 {error_code} for {path}; not retried"
                )
            if status == 429 or 500 <= status < 600:
                last_failure = f"attempt {attempt + 1} got HTTP {status}"
                if attempt < MAX_ATTEMPTS - 1:
                    self._backoff(attempt, _retry_after_seconds(response))
                continue
            raise ControlPlaneRequestError(
                status, f"control-plane returned HTTP {status} for {path}; not retried"
            )
        raise ControlPlaneUnavailableError(f"control-plane unavailable for {path}: {last_failure}")

    @staticmethod
    def _error_code(response: httpx.Response) -> str:
        # The Go error body is {"code": ..., "message": ...} (respond.go);
        # "error" is tolerated as a legacy alias.
        try:
            data: Any = response.json()
        except ValueError:
            return "UNKNOWN"
        if isinstance(data, dict):
            for key in ("code", "error"):
                if isinstance(data.get(key), str):
                    return str(data[key])
        return "UNKNOWN"
