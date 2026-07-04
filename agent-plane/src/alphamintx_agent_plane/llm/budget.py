"""Local ADVISORY daily token budget counter (docs/specs/llm-routing-and-budget.md §4).

``daily_token_budget`` (input + output tokens, per strategy per UTC day) is
Admin-set control-plane config; the authoritative ledger is the control-plane
``token_budget_ledger``. This counter is an enforcement pre-check only, persisted
to a JSON state file so it survives restart. It resets at UTC midnight by keying
on the UTC date; a run spanning 00:00Z attributes ALL of its usage to the UTC day
of the run's ``started_at`` (captured at construction).

Corruption FAILS CLOSED: an unreadable/garbled state file or a non-integer
counter is treated as an exhausted day (never a silent reset re-arming the full
advisory headroom); the authoritative control-plane 402 remains the backstop.
``record`` is a non-locked read-modify-write (single-process assumption) and
per-day keys are never pruned.
"""

from __future__ import annotations

import json
import logging
import os
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from alphamintx_agent_plane.llm.errors import BudgetExhaustedError

logger = logging.getLogger(__name__)


def utc_today() -> str:
    return datetime.now(UTC).date().isoformat()


class DailyTokenBudget:
    """Advisory per-(strategy_id, utc_date) token counter with JSON persistence."""

    def __init__(
        self,
        *,
        strategy_id: str,
        daily_token_budget: int,
        state_path: Path | str,
        utc_date: str | None = None,
    ) -> None:
        if daily_token_budget < 0:
            raise ValueError("daily_token_budget must be >= 0")
        self._strategy_id = strategy_id
        self._daily_token_budget = daily_token_budget
        self._state_path = Path(state_path)
        # The run's started_at UTC day: all usage of this run is attributed here.
        self._utc_date = utc_date if utc_date is not None else utc_today()

    @property
    def strategy_id(self) -> str:
        return self._strategy_id

    @property
    def utc_date(self) -> str:
        return self._utc_date

    @property
    def daily_token_budget(self) -> int:
        return self._daily_token_budget

    def _load(self) -> tuple[dict[str, Any], bool]:
        """Load the state file; the second value is True when it is corrupt.
        A missing file is a legitimate fresh start; anything unreadable or
        non-dict is corruption and FAILS CLOSED at the callers."""
        try:
            with self._state_path.open(encoding="utf-8") as handle:
                data = json.load(handle)
        except FileNotFoundError:
            return {}, False
        except (json.JSONDecodeError, OSError):
            logger.warning(
                "budget state file %s is unreadable; failing closed", self._state_path
            )
            return {}, True
        if not isinstance(data, dict):
            logger.warning(
                "budget state file %s is not a JSON object; failing closed", self._state_path
            )
            return {}, True
        return data, False

    def _used_from(self, data: dict[str, Any]) -> int | None:
        """Extract the run day's counter; None when its shape is corrupt."""
        per_day = data.get(self._strategy_id, {})
        if not isinstance(per_day, dict):
            return None
        used = per_day.get(self._utc_date, 0)
        if isinstance(used, bool) or not isinstance(used, int) or used < 0:
            return None
        return used

    def tokens_used(self) -> int:
        """The day's recorded usage; a corrupt file or counter reads as the
        FULL budget (fail closed), never as zero."""
        data, corrupt = self._load()
        used = None if corrupt else self._used_from(data)
        return self._daily_token_budget if used is None else used

    def check(self) -> None:
        """Advisory pre-call check; raises BUDGET_EXHAUSTED when the day is
        spent — or when the state is corrupt (fail closed: a crash-truncated
        file must never silently re-arm the day's budget)."""
        data, corrupt = self._load()
        used = None if corrupt else self._used_from(data)
        if used is None:
            raise BudgetExhaustedError(
                f"BUDGET_EXHAUSTED: budget state file {self._state_path} is corrupt for "
                f"strategy {self._strategy_id} on UTC date {self._utc_date} (fail closed); "
                "pre-call check failed, no LLM call was made"
            )
        if used >= self._daily_token_budget:
            raise BudgetExhaustedError(
                f"BUDGET_EXHAUSTED: strategy {self._strategy_id} has used {used} of "
                f"{self._daily_token_budget} daily tokens for UTC date {self._utc_date}; "
                "pre-call check failed, no LLM call was made"
            )

    def record(self, tokens: int) -> None:
        """Add spent (input + output) tokens to the run's UTC day; atomic
        write. A corrupt file or counter restarts the day AT the full budget
        (exhausted, fail closed), never at zero."""
        if tokens < 0:
            raise ValueError("tokens must be >= 0")
        data, corrupt = self._load()
        if corrupt:
            data = {}
        per_day = data.get(self._strategy_id)
        if per_day is not None and not isinstance(per_day, dict):
            corrupt = True
        if not isinstance(per_day, dict):
            per_day = {}
            data[self._strategy_id] = per_day
        used = per_day.get(self._utc_date, 0)
        if corrupt or isinstance(used, bool) or not isinstance(used, int) or used < 0:
            used = self._daily_token_budget
        per_day[self._utc_date] = used + tokens
        self._state_path.parent.mkdir(parents=True, exist_ok=True)
        tmp_path = self._state_path.with_name(self._state_path.name + ".tmp")
        tmp_path.write_text(json.dumps(data, sort_keys=True), encoding="utf-8")
        os.replace(tmp_path, self._state_path)
