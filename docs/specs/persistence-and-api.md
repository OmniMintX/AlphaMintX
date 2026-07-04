# Spec: Persistence, HTTP API, and checkpoint/resume (Phase 1)

Normative. Defines the control-plane store, the read/approval HTTP API, the
agent-plane trace-ingestion boundary, and agent-plane checkpoint/resume.
Companion to `docs/ARCHITECTURE.md` (plane boundary rules); on conflict, the
lifecycle and proposal-contract specs win for their domains.

## Store

Phase 1 uses **SQLite** via **`modernc.org/sqlite`** (pure Go — no CGO, so CI
and cross-compilation stay a plain `go build`; single-node paper trading needs
no server DB). Postgres is the Phase 2 path; the schema ports without redesign.

- One DB file for the whole control-plane; path comes from config.
- Connection MUST set `journal_mode=WAL` and `busy_timeout` (≥ 5000 ms).
- All money/size/price columns are TEXT decimal-as-string (ADR-0003).
- All timestamps are TEXT, RFC 3339 UTC with `Z` suffix.
- agent-plane has **no access** to this file or any DB credentials.

### Store rules (normative)

- **Payload rule.** Contract objects (TradeProposal, RiskVerdict, agent
  trace) are stored **as canonical JSON** in a `payload_json` column — the
  source of truth, returned verbatim by the API. Extracted columns
  (strategy_id, symbol, action, decision, timestamps) are **for
  indexing/filtering only**; readers MUST NOT reconstruct contracts from them.
- **Append-only (invariant 7).** `proposals`, `verdicts`, `approvals`,
  `fills`, `lifecycle_transitions`, `model_costs`, `rejected_submissions`,
  `kill_breaker_events`, `risk_limit_changes`, and `pending_approvals` are
  INSERT-only: no UPDATE, no DELETE, ever (a pending item is superseded by
  its `approvals` row, never mutated). `positions` is a mutable snapshot;
  `orders` rows mutate only their FSM `status`/`filled_at`.
- **Idempotency.** The UNIQUE constraint on `proposals.proposal_id` is the
  atomic insert backing at-least-once proposal delivery; a duplicate returns
  the stored verdict verbatim (the DB-backed version of `riskgate.Service`'s
  in-memory step 0b, incl. the payload-hash `IDEMPOTENCY_CONFLICT` check).

### Tables (DDL, normative)

```sql
CREATE TABLE strategies (strategy_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL,
  name TEXT NOT NULL, lifecycle_state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE lifecycle_transitions (transition_id TEXT PRIMARY KEY,   -- append-only audit
  strategy_id TEXT NOT NULL REFERENCES strategies, from_state TEXT NOT NULL, to_state TEXT NOT NULL,
  actor_id TEXT NOT NULL, actor_role TEXT NOT NULL, reason TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE runs (run_id TEXT PRIMARY KEY, strategy_id TEXT NOT NULL REFERENCES strategies,
  tick_number INTEGER NOT NULL, created_at TEXT NOT NULL, completed_at TEXT,
  UNIQUE (strategy_id, tick_number));
CREATE TABLE proposals (proposal_id TEXT PRIMARY KEY,   -- payload = contracts/proposal.schema.json
  run_id TEXT REFERENCES runs, strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, action TEXT NOT NULL,
  created_at TEXT NOT NULL, payload_json TEXT NOT NULL, payload_sha256 TEXT NOT NULL);
CREATE TABLE verdicts (verdict_id TEXT PRIMARY KEY,     -- payload = contracts/riskverdict.schema.json
  proposal_id TEXT NOT NULL UNIQUE REFERENCES proposals, decision TEXT NOT NULL,
  evaluated_at TEXT NOT NULL, payload_json TEXT NOT NULL);
CREATE TABLE approvals (approval_id TEXT PRIMARY KEY,   -- append-only ApprovalDecision records
  verdict_id TEXT NOT NULL UNIQUE REFERENCES verdicts, proposal_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('approved','approved_but_blocked','rejected','timeout')),
  preflight_reasons TEXT,                               -- JSON array; non-null iff approved_but_blocked
  decided_by TEXT NOT NULL, decided_at TEXT NOT NULL, timeout_seconds INTEGER NOT NULL);
CREATE TABLE pending_approvals (                        -- restart-safe L1 timer state
  verdict_id TEXT PRIMARY KEY REFERENCES verdicts, strategy_id TEXT NOT NULL,
  created_at TEXT NOT NULL, deadline_at TEXT NOT NULL);
CREATE TABLE orders (order_id TEXT PRIMARY KEY, proposal_id TEXT REFERENCES proposals,
  origin TEXT NOT NULL CHECK (origin IN ('proposal','breaker','kill','watchdog','sl_contingency')),
  strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, class TEXT NOT NULL CHECK (class IN ('ENTRY','PROTECTIVE')),
  side TEXT NOT NULL, type TEXT NOT NULL, reduce_only INTEGER NOT NULL, qty_base TEXT NOT NULL,
  limit_price TEXT, stop_price TEXT, fill_price TEXT, kill_epoch INTEGER NOT NULL,
  status TEXT NOT NULL, submitted_at TEXT NOT NULL, filled_at TEXT);
CREATE TABLE fills (fill_id TEXT PRIMARY KEY,           -- append-only
  order_id TEXT NOT NULL REFERENCES orders, qty_base TEXT NOT NULL,
  fill_price TEXT NOT NULL, fee_quote TEXT NOT NULL, fill_ts TEXT NOT NULL);
CREATE TABLE positions (strategy_id TEXT NOT NULL,      -- mutable snapshot
  symbol TEXT NOT NULL, qty_base TEXT NOT NULL,
  entry_price TEXT NOT NULL,                            -- fee-EXCLUSIVE (see Row rules)
  fees_quote TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY (strategy_id, symbol));
CREATE TABLE agent_traces (trace_id TEXT PRIMARY KEY,   -- payload = trace envelope (below)
  run_id TEXT NOT NULL UNIQUE REFERENCES runs, strategy_id TEXT NOT NULL, proposal_id TEXT,
  started_at TEXT NOT NULL, completed_at TEXT NOT NULL,
  payload_json TEXT NOT NULL, payload_sha256 TEXT NOT NULL);
CREATE TABLE model_costs (cost_id TEXT PRIMARY KEY,     -- append-only billing signal
  run_id TEXT NOT NULL REFERENCES runs, strategy_id TEXT NOT NULL, node TEXT NOT NULL,
  model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
  cost_usd TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE token_budget_ledger (                      -- authoritative usage; daily_token_budget is
  strategy_id TEXT NOT NULL, utc_date TEXT NOT NULL,    -- Admin-set CONFIG, never ledger state
  tokens_used INTEGER NOT NULL, cost_usd_used TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY (strategy_id, utc_date));
CREATE TABLE rejected_submissions (                     -- append-only; malformed, NO verdict
  rejection_id TEXT PRIMARY KEY, strategy_id TEXT, received_at TEXT NOT NULL,
  reason TEXT NOT NULL, payload_json TEXT NOT NULL);
CREATE TABLE kill_breaker_events (event_id TEXT PRIMARY KEY,  -- append-only safety audit
  kind TEXT NOT NULL CHECK (kind IN ('kill','breaker')), scope TEXT NOT NULL, strategy_id TEXT,
  kill_epoch INTEGER, flatten INTEGER, trigger_ref TEXT, actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE risk_limit_changes (change_id TEXT PRIMARY KEY,  -- append-only limit audit
  strategy_id TEXT NOT NULL, field TEXT NOT NULL, old_value TEXT, new_value TEXT NOT NULL,
  actor_id TEXT NOT NULL, changed_at TEXT NOT NULL);
```

### Row rules (normative)

- **`runs`** are created at **proposal ingest**: the submission envelope
  (transport wrapper, NOT the TradeProposal contract) now carries
  `tick_number`; control-plane inserts the run if absent, keyed
  `run_id = proposal.agent_trace_id` + `(strategy_id, tick_number)`.
  Proposals arrive before traces, so the row exists when either FK needs
  it; trace ingest sets `completed_at` (a never-arriving trace ⇒ NULL).
- **`kill_breaker_events`** rows ARE the persisted kill/breaker intent:
  inserted and acknowledged BEFORE any side effect executes (risk-limits.md
  §Kill-switch). `kill_epoch` is monotonic; the OMS kill re-check reads the
  persisted maximum; restarts re-drive incomplete effects from these rows —
  kill state survives restart.
- **`orders.proposal_id`** is NOT NULL iff `origin='proposal'`; safety-path
  orders carry their causing origin, so audit links every order to a cause.
  `stop_price` and `kill_epoch` make protective stops and the kill re-check
  restart-safe.
- **Fees / daily loss** — `positions.entry_price` is **fee-EXCLUSIVE**; fees
  live only in `fills.fee_quote` and the `positions.fees_quote` accumulator
  (never baked into a price — no double counting). `daily_loss`
  (risk-limits.md Definitions) is DERIVED at read time: the day's `fills`
  (realized PnL, fees) plus unrealized PnL at the current mark; Phase 1 has
  no separate daily-PnL table.

## Trace ingestion (trust boundary)

agent-plane has **no DB access** (ARCHITECTURE.md plane boundary rules; CI
greps agent-plane for DB drivers/credentials). It persists its pipeline trace
by `POST /api/v1/strategies/{id}/traces` with its per-strategy bearer token;
a body/path `strategy_id` outside the token scope is rejected with
`STRATEGY_SCOPE_MISMATCH`, exactly as for proposals. Idempotency mirrors
proposals: `run_id` is UNIQUE, `payload_sha256` stored — a duplicate POST
with the same hash is a no-op 200; a different hash is 409
`IDEMPOTENCY_CONFLICT`. Trace insert, `model_costs` fan-out, and the
`token_budget_ledger` increment happen in ONE transaction, so a duplicate
no-op skips all three atomically (no double or lost billing).

Trace envelope (request body; published as `contracts/agent_trace.schema.json`
— the schema, not this table, is the shape authority):

| Field | Semantics |
|---|---|
| `strategy_id` | MUST equal the path `{id}` and the token scope. |
| `run_id` | UUID == the proposal's `agent_trace_id`. UNIQUE per trace. |
| `tick_number` | Scheduler tick that produced the run (≥ 0, monotonic). |
| `started_at` / `completed_at` | RFC 3339 UTC pipeline start/end. |
| `analyst_summaries` | Exactly `market`/`news`/`fundamental`, contract `analyst_summary` shape. |
| `debate_rounds[]` | `{round_index, bull_argument, bull_score, bear_argument, bear_score}` per round, ≤ `max_debate_rounds`. |
| `debate_summary` | Judge summary (≤ 4000 chars) incl. degradation notes (llm-routing §5). |
| `transcripts` | OPTIONAL full LLM transcripts; ≤ 256 KiB serialized. |
| `proposal_id` | UUID of the emitted proposal; null ONLY when the proposal POST itself failed after retries (llm-routing §5). |
| `model_costs[]` | Contract `model_cost` items (≤ 32; overflow aggregation per llm-routing §3); fan out into `model_costs` rows on ingest. Estimated entries (timeouts/aborts) are listed by node in the OPTIONAL `estimated_cost_nodes[]` field. |
| `budget_state` | `{utc_date, tokens_used, cost_usd_used}` — **informational only**, attributed to the UTC day of `started_at`. It carries NO budget value (`daily_token_budget` is Admin-set control-plane config, invariant 5) and never writes the ledger: the ledger is incremented from the ingested `model_costs`, idempotent by `run_id`, never overwritten from a report. |

## HTTP API (Phase 1 minimal)

| Method + path | Returns / body |
|---|---|
| `GET /api/v1/strategies` | Paginated strategy list (`lifecycle_state` included). |
| `GET /api/v1/strategies/{id}` | Strategy detail + lifecycle state. |
| `GET /api/v1/strategies/{id}/runs?page&limit` | Runs, `tick_number` DESC. |
| `GET /api/v1/strategies/{id}/runs/{run_id}` | Run detail embedding proposal, verdict, trace, orders, fills, approvals (contract payloads verbatim). |
| `POST /api/v1/strategies/{id}/approvals` | Body `{verdict_id, approved: bool}`; records the L1 decision (below). **Operator token only.** |
| `POST /api/v1/strategies/{id}/traces` | Trace envelope ingestion (agent-plane token only). |
| `GET /health` | Unauthenticated liveness. |

- **Auth: two static bearer tokens** — read and approve are never the same
  credential, even single-user:
  - `READ_TOKEN` (web dashboard) is valid for GETs ONLY and MUST NOT
    authorize any POST: a leaked or XSS'd dashboard credential cannot
    approve trades.
  - `OPERATOR_TOKEN` (Trader role) is REQUIRED for `POST .../approvals`;
    `approvals.decided_by` = its principal id, so every decision is
    attributed. Phase-2 RBAC maps this to Trader+ (strategy-lifecycle.md).
  - Agent-plane strategy tokens are valid only for their ingestion endpoints.
- **Limits (every POST):** body > 1 MiB ⇒ 413; per-token 60 req/min ⇒ 429.
- Pagination: `{items, total, page, limit}` (`page` 1-based, `limit` default
  20, max 100). Web validates embedded payloads with the existing zod
  mirrors (`web/src/lib/contract/schema.ts`) and **polls** these endpoints;
  SSE/websocket is deferred.

## L0 / L1 execution semantics

- **L0 / `paper`-as-advisor**: proposals and verdicts are persisted and shown;
  nothing is ever submitted to the OMS.
- **L1 (`live_l1`)**: an L1 `approve`/`clip` verdict — or any `escalate`
  verdict — inserts a `pending_approvals` row, `deadline_at = created_at +
  l1_approval_timeout_seconds` (default 600, risk-limits.md). Timers are
  DERIVED from the persisted `deadline_at`, never in-memory only: a
  **startup sweep** resolves every pending item (a `pending_approvals` row
  with no `approvals` row) past its deadline as `timeout` — restart-safe
  default-deny.
- **One decision per verdict.** `approvals.verdict_id` is UNIQUE; every
  outcome — human POST or timer expiry — resolves through a single
  INSERT-or-conflict transaction: the first decision wins, ever. A second
  POST, a double-click, or a human-vs-timeout race returns **409** with the
  recorded outcome in the body (idempotent, mirroring proposals); the OMS
  submits at most once, on the single winning `approved` row.
- Errors: unknown `verdict_id`, or a verdict whose proposal's `strategy_id`
  ≠ path `{id}` ⇒ 404 `UNKNOWN_VERDICT` (verdict→proposal→strategy match is
  REQUIRED); not pending approval ⇒ 422 `NOT_PENDING`; decided ⇒ 409 above.
- **Approval preflight (normative).** `approved:true` does NOT re-run
  `riskgate.Evaluate` (step 0b idempotency would return the original
  verdict verbatim; one-verdict-per-proposal forbids a second). Instead the
  control plane runs a lightweight **preflight** at decision time:
  kill-epoch unchanged since the verdict; strategy still `live_l1` (or its
  verdict-time live state, for escalations); mark available and fresh (mark
  AGE ≤ `max_age_seconds`, market-data.md); daily-loss limit not breached.
  **Freshness is the mark's age, NOT proposal `created_at`**: the 60 s
  `PROPOSAL_STALE` rule applies at gate evaluation only and does NOT kill
  the 600 s approval window. A deployment with no OMS Submitter wired blocks
  with `SUBMITTER_UNAVAILABLE` (an approval that could never be submitted
  must not read as executed). Pass ⇒ append `outcome=approved`, then OMS
  submission (the OMS kill re-check still applies). Fail ⇒ append
  `outcome=approved_but_blocked` with `preflight_reasons`, NO order — the
  verdict is untouched, and audit distinguishes approved-and-executed from
  approved-but-blocked.
- **Submission failure after approval.** When the OMS rejects the winning
  `approved` decision (kill-epoch stale, OMS down), the failure is persisted
  as a `rejected_submissions` row (reason `SUBMIT_FAILED: <error>`) and the
  POST response carries `{"submitted": false, "submit_error_code":
  "SUBMIT_FAILED"}` alongside the recorded approval; a successful submission
  carries `{"submitted": true}`. Stored approvals (GET run detail) carry no
  submission status.
- `approved:false` ⇒ `outcome=rejected`; timer expiry ⇒ `outcome=timeout`,
  `decided_by="timeout"`; neither submits an order. Outcomes are append-only
  ApprovalDecision records referencing the verdict — never a mutation of,
  or second, RiskVerdict; human approval can never override a gate
  rejection (invariant 5).

## Agent-plane checkpoint/resume and scheduler

- Dependency: **`langgraph-checkpoint-sqlite` MUST be added** via `uv add`
  (verified absent from `agent-plane/uv.lock`; langgraph 1.2.7 +
  langgraph-checkpoint 4.1.1 are locked). The graph runs under `SqliteSaver`.
- Checkpoint DB is a **separate SQLite file local to agent-plane** — never
  the control-plane store (trust boundary). A corrupt or unopenable
  checkpoint DB at startup is a fail-fast startup error (operator alert;
  never silently recreated) — safe, because ticks are recomputable.
- `thread_id = "{strategy_id}#{tick_number}"`; a crash at node N resumes that
  thread from the checkpoint after node N-1. Store debate summaries in
  checkpoints; full transcripts travel only in the trace envelope.
- Scheduler: one asyncio loop per strategy; `tick_interval_seconds` config
  (default 60). Tick overrun ⇒ warn and start the next tick immediately.
  `tick_number` is monotonic, **no gaps**. A per-tick exception is caught
  and recorded (checkpoint retained); the scheduler resumes at the next
  tick. Unhandled per-tick failures are defects.
