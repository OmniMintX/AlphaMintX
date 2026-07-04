"""Control-plane client failure taxonomy (docs/specs/persistence-and-api.md §HTTP API).

Mirrors the mintrouter retry policy (llm-routing-and-budget.md §1): 429/5xx/timeouts
and transport failures are retried (at most 2 retries) and resolve to
``ControlPlaneUnavailableError`` when exhausted; 401/403 and 409 conflicts are
non-retryable defects with their own types; any other 4xx is never retried.
Messages never contain the per-strategy bearer token.
"""

from __future__ import annotations


class ControlPlaneError(Exception):
    """Base for control-plane API failures."""


class ControlPlaneAuthError(ControlPlaneError):
    """401/403: bad or out-of-scope bearer token — a configuration defect, no retry."""


class ControlPlaneConflictError(ControlPlaneError):
    """409: IDEMPOTENCY_CONFLICT or RUN_TICK_CONFLICT — a defect, no retry."""

    def __init__(self, error_code: str, message: str) -> None:
        super().__init__(message)
        self.error_code = error_code


class ControlPlaneRequestError(ControlPlaneError):
    """Any other non-retryable 4xx (400/404/413/422)."""

    def __init__(self, status_code: int, message: str) -> None:
        super().__init__(message)
        self.status_code = status_code


class ControlPlaneUnavailableError(ControlPlaneError):
    """429/5xx/timeout/transport failure persisting after retries."""


class ControlPlaneContractError(ControlPlaneError):
    """A 200 response body that violates the cross-plane response contract.

    Proposal POSTs must return the envelope ``{"verdict": {...}, ...}`` (never a
    bare verdict object) for both fresh and duplicate submissions.
    """
