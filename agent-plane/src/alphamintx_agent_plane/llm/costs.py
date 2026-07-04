"""model_costs cap-overflow aggregation (docs/specs/llm-routing-and-budget.md §3).

``model_costs`` is schema-capped at 32 items. Normative truncation: the first 31
entries are kept verbatim; every later call merges into ONE final aggregate entry
whose token counts and ``cost_usd`` are the exact sums of the merged calls —
truncation never drops cost.
"""

from __future__ import annotations

from collections.abc import Sequence
from decimal import Decimal

from alphamintx_agent_plane.contract.models import TraceModelCost

MODEL_COSTS_CAP = 32
OVERFLOW_NODE = "overflow_aggregate"
OVERFLOW_MODEL = "aggregate"


def aggregate_overflow(costs: Sequence[TraceModelCost]) -> list[TraceModelCost]:
    """Cap a cost list at 32 entries: 31 verbatim + one exact-sum aggregate.

    The aggregate merges >= 2 calls, so it carries NO ``request_id`` and no
    ``estimated`` flag (billing-and-metering.md §Overflow aggregation); the
    kept head entries keep theirs verbatim.
    """
    if len(costs) <= MODEL_COSTS_CAP:
        return list(costs)
    head = list(costs[: MODEL_COSTS_CAP - 1])
    tail = costs[MODEL_COSTS_CAP - 1 :]
    aggregate = TraceModelCost(
        node=OVERFLOW_NODE,
        model=OVERFLOW_MODEL,
        input_tokens=sum(entry.input_tokens for entry in tail),
        output_tokens=sum(entry.output_tokens for entry in tail),
        cost_usd=sum((entry.cost_usd for entry in tail), Decimal("0")),
    )
    return [*head, aggregate]
