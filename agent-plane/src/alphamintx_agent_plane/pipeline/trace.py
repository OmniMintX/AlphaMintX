"""Trace-envelope builder (contracts/agent_trace.schema.json; persistence-and-api.md
§Trace ingestion).

Builds the document agent-plane POSTs to ``/api/v1/strategies/{id}/traces`` from a
finished ``PipelineState``. ``run_id`` == the proposal's ``agent_trace_id`` (the
state carries it even when the run ends without a proposal). ``proposal_id`` is
set by the SCHEDULER: null ONLY when the proposal POST itself failed after
retries (llm-routing §5). ``budget_state`` is informational only and never
writes the control-plane ledger.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any

from alphamintx_agent_plane.llm.costs import aggregate_overflow
from alphamintx_agent_plane.pipeline.graph import PipelineState, _filled_summaries

TRACE_SCHEMA_VERSION = "1.0"


@dataclass(frozen=True)
class BudgetState:
    """Informational budget report attributed to the UTC day of ``started_at``."""

    utc_date: str
    tokens_used: int
    cost_usd_used: str


def build_trace_envelope(
    state: PipelineState,
    *,
    strategy_id: str,
    tick_number: int,
    started_at: str,
    completed_at: str,
    proposal_id: str | None,
    budget_state: BudgetState,
) -> dict[str, Any]:
    """Build a document valid against contracts/agent_trace.schema.json."""
    envelope: dict[str, Any] = {
        "schema_version": TRACE_SCHEMA_VERSION,
        "strategy_id": strategy_id,
        "run_id": state["agent_trace_id"],
        "tick_number": tick_number,
        "started_at": started_at,
        "completed_at": completed_at,
        "analyst_summaries": _filled_summaries(state).to_json_dict(),
        "debate_rounds": [asdict(debate_round) for debate_round in state["debate_rounds"]],
        "debate_summary": (
            state["debate_summary"] or "unavailable: debate skipped (forced hold)"
        )[:4000],
        "proposal_id": proposal_id,
        "model_costs": [
            cost.to_json_dict() for cost in aggregate_overflow(state["model_costs"])
        ],
        "budget_state": {
            "utc_date": budget_state.utc_date,
            "tokens_used": budget_state.tokens_used,
            "cost_usd_used": budget_state.cost_usd_used,
        },
    }
    if state["estimated_cost_nodes"]:
        envelope["estimated_cost_nodes"] = list(state["estimated_cost_nodes"])
    return envelope
