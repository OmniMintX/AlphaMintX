# Spec: LLM routing, cost accounting, and token budget (Phase 1)

Normative companion to ADR-0004 (mintrouter is the sole LLM gateway) and
`docs/ARCHITECTURE.md` (agent-plane holds no exchange credentials; control-plane
never calls LLMs). Applies to `agent-plane/` and its LangGraph pipeline
(`pipeline/graph.py`); the `model_costs` field shape is owned by
`contracts/proposal.schema.json` / `docs/specs/proposal-contract.md`.

## 1. mintrouter client (the only LLM transport)

- The live client MUST call `POST {MINTROUTER_BASE_URL}/v1/chat/completions`
  (OpenAI-compatible relay) with `Authorization: Bearer $MINTROUTER_API_KEY`.
- Timeouts are two-level (both normative): a **per-attempt** hard timeout,
  `MINTROUTER_TIMEOUT_SECONDS` (default 60), AND an **overall per-call
  deadline** of `3 × MINTROUTER_TIMEOUT_SECONDS` covering all attempts and
  retry waits. No unbounded waits. **Streaming is FORBIDDEN in Phase 1**
  (`stream` is never set): usage accounting requires the complete response
  `usage` object, and an aborted stream would spend unmetered tokens.
- **Retries: at most 2**, and ONLY on 429, 5xx, or timeout. Any other 4xx
  (400/401/402/403/404/422) MUST NOT be retried. mintrouter has no relay
  idempotency — retried calls execute twice and spend tokens twice, so the cap
  is normative, not tunable upward.
- Retry delay: if the response carries an `X-MintRouter-*-Reset-After-Seconds`
  header, the client MUST wait that long (capped at the remaining overall
  per-call deadline); otherwise exponential backoff with jitter (base 1 s,
  factor 2).
- Direct provider calls are FORBIDDEN anywhere in agent-plane. CI MUST run a
  gate that fails on ANY `https://` LLM-API base URL in `agent-plane/` source
  that is not `MINTROUTER_BASE_URL` (allowlist, not blocklist — blocklists
  rot). A provider hostname/SDK-import list (`api.openai.com`,
  `api.anthropic.com`, `generativelanguage.googleapis.com`, `api.x.ai`,
  `api.mistral.ai`, `api.cohere.com`, `openrouter.ai`, `*.openai.azure.com`,
  Bedrock endpoints) remains as a secondary tripwire only.
- The live client implements the existing `LLMClient` protocol
  (`llm/stub.py`); the pipeline is transport-agnostic.

## 2. Per-role model config

- The strategy config carries a role→model map covering every pipeline role:
  `market_analyst`, `news_analyst`, `fundamental_analyst`, `bull_researcher`,
  `bear_researcher`, `debate_judge`, `trader`. Cheap models for Tier-1/Tier-2
  roles, the expensive model for `trader` only (per ARCHITECTURE.md).
- The map MUST be validated at startup: every role present. A missing or
  unknown role is a startup failure, not a runtime surprise. Any model name is
  allowed — a model absent from the local price table (§3) MUST log a startup
  WARNING that its costs will be recorded as estimated 0, never an error.
- Routing is by the OpenAI `model` field: agent-plane sets `model` per role and
  mintrouter maps it to the upstream provider. Agent-plane never selects
  providers directly.

## 3. Cost accounting (local price table)

mintrouter does NOT return `cost_usd` in the response body (its cost is
internal billing; only quota-percent headers are exposed). Therefore:

- Agent-plane MUST compute cost locally from the response
  `usage.prompt_tokens` / `usage.completion_tokens` and a versioned price
  table checked into the repo as a data file: USD per 1M input tokens and per
  1M output tokens, per model, with an `as_of` date. Startup MUST warn when
  `as_of` is older than 90 days (staleness policy, risk R1).
- `cost_usd = prompt_tokens × input_price/1e6 + completion_tokens ×
  output_price/1e6`, computed in `Decimal` and serialized as a decimal string
  (ADR-0003) — never float.
- Every LLM call MUST append exactly one `model_costs` entry using the
  contract field names from `contracts/proposal.schema.json`:
  `{node, model, input_tokens, output_tokens, cost_usd}` — `node` is the
  pipeline role, `input_tokens` ← `usage.prompt_tokens`, `output_tokens` ←
  `usage.completion_tokens`. Reprompts and retried requests that reached
  mintrouter each append their own entry (tokens were spent).
- **Unpriced models are not an error.** A configured model absent from the
  price table still appends its `model_costs` entry with the real `usage`
  token counts but `cost_usd = 0`, and the node is listed in the trace
  envelope's `estimated_cost_nodes[]` — an estimated $0, never a crash.
- **Timed-out / aborted calls spent upstream tokens but return no `usage`.**
  They MUST still append a `model_costs` entry: `input_tokens` estimated as
  ceil(request characters ÷ 4), `output_tokens = 0`, cost from the price
  table. Estimated entries are listed by node in the trace envelope's
  `estimated_cost_nodes[]` (`docs/specs/persistence-and-api.md`) — never
  silently uncounted.
- **Cap overflow.** `model_costs` is schema-capped at 32 items, and roles ×
  attempts × debate rounds + reprompts can exceed it. Normative truncation:
  the first 31 entries are kept verbatim; every later call merges into ONE
  final aggregate entry (`node="overflow_aggregate"`, `model="aggregate"`)
  whose token counts and `cost_usd` are the exact sums of the merged calls.
  Totals MUST remain exact for billing; truncation never drops cost.
- Local cost is the metering signal reported to control-plane; reconciliation
  against mintrouter billing is out of scope for Phase 1.
- **Stub mode** (`ALPHAMINTX_LLM_MODE=stub`) remains the default for CI and
  e2e: `StubLLM`, no network, `cost_usd` entries of zero cost. Live mode MUST
  be an explicit opt-in.

## 4. Daily token budget

- **Authority (invariant 5).** `daily_token_budget` (input + output tokens
  summed, per strategy per UTC day) is **Admin-set strategy config owned by
  control-plane**, delivered to agent-plane with the rest of the strategy
  config. It NEVER comes from a trace payload: agent-plane cannot set or
  raise its own budget. The **authoritative ledger is the control-plane
  `token_budget_ledger`**, incremented from trace `model_costs` on ingest,
  idempotent by `run_id` (`docs/specs/persistence-and-api.md`).
- Agent-plane keeps a local **advisory counter** per `(strategy_id,
  utc_date)`, checked BEFORE each LLM call (mintrouter quotas are
  per-key/per-group, not per-strategy). It MUST be persisted alongside the
  LangGraph checkpoints so it survives restart, but it is an enforcement
  pre-check only, never authoritative; the trace `budget_state` it reports
  is informational.
- The counter resets at UTC midnight. A run spanning 00:00Z attributes ALL
  of its usage to the UTC day of the run's `started_at` — one ledger day per
  run, by definition.
- **Budget exhausted** — the local pre-call check fails, or mintrouter
  returns 402 — ⇒ the pipeline MUST emit a deterministic **forced-hold
  proposal**: `action=hold`, `size_quote="0"`, `confidence=0.0`, `reasoning`
  stating `BUDGET_EXHAUSTED` with strategy id and UTC date. Never a crash,
  never a skipped record: the hold is persisted like any other proposal and
  carries the `model_costs` accumulated up to the cutoff. A 429 persisting
  after retries is NOT a budget event: same forced-hold mechanics, but the
  reasoning states `RATE_LIMITED` — the audit trail MUST NOT claim the
  budget was exhausted when it was not.
- A forced hold before ANY LLM call carries an empty `model_costs`. This is
  schema-legal (`model_costs` has no `minItems`) and contract-permitted in
  live mode provided `reasoning` states the pre-call cutoff
  (`docs/specs/proposal-contract.md` §model_costs).
- Concurrent runs MAY both pass the pre-call check (no reservation in Phase
  1); a 402 mid-run is therefore normal and MUST resolve to the same forced
  hold, never a partial crash (risk R3).

## 5. Failure taxonomy

After the retry policy in §1 is exhausted, per-node degradation rules apply.
The pipeline MUST always terminate with a schema-valid proposal:

| Failure | Node(s) | Rule |
|---|---|---|
| Timeout / 5xx after retries | `market_analyst`, `news_analyst`, `fundamental_analyst` | That analyst summary is set to the explicit unavailable marker: `{signal: neutral, confidence: 0.0, summary: "unavailable: <role> LLM call failed"}`; pipeline continues. |
| Timeout / 5xx after retries | `bull_researcher`, `bear_researcher`, `debate_judge` | Debate is cut short; `debate_summary` records the degradation; pipeline continues to trader. |
| Timeout / 5xx after retries | `trader` | Forced hold (`action=hold`, `confidence=0.0`, reasoning states the failure). |
| 402 | any | Budget-exhausted forced hold (§4, reasoning `BUDGET_EXHAUSTED`). |
| 429 persisting after retries | any | Forced hold with reasoning `RATE_LIMITED` (§4) — a rate limit is not a budget event. |
| 4xx ≠ 429 (400/401/403/404/422) | any | No retry. Configuration/auth defect ⇒ forced hold with the status code in `reasoning`; alert. |
| Malformed LLM output (JSON parse or schema validation fail) | any | Exactly ONE reprompt (same role, error appended to prompt, counted against budget and `model_costs`). Second failure ⇒ forced hold. |

Forced holds from this table are proposals, not errors: they MUST be
persisted and submitted like any other proposal (invariant 7). The ONLY case
in which a run ends without a proposal (trace `proposal_id = null`,
`docs/specs/persistence-and-api.md`) is when the proposal POST itself failed
after its submission retries — a defect alert, never a routine skip.

## 6. Environment variables

| Variable | Meaning |
|---|---|
| `MINTROUTER_BASE_URL` | Base URL of the mintrouter relay (e.g. `https://mintrouter.internal`). Optional in live mode when the control-plane vault holds an LLM config. |
| `MINTROUTER_API_KEY` | Bearer key. **Secret**: MUST NOT be logged, MUST NOT appear in argv/process lists, MUST NOT be echoed in error messages or traces. Optional in live mode when the control-plane vault holds an LLM config. |
| `MINTROUTER_TIMEOUT_SECONDS` | Per-**attempt** hard timeout; the overall per-call deadline is 3 × this (§1). Default `60`. When set, it wins over a control-plane-fetched `timeout_seconds`. |
| `ALPHAMINTX_LLM_MODE` | `stub` (default; `StubLLM`, no network, zero cost) or `live` (mintrouter client). Any other value is a startup error. |

Live-mode config resolution order (`llm/factory.py`): **env override →
control-plane vault → startup error**. When BOTH `MINTROUTER_BASE_URL` and
`MINTROUTER_API_KEY` are set, they win and no control-plane call is made.
Otherwise, when `ALPHAMINTX_CONTROLPLANE_BASE_URL` and
`ALPHAMINTX_STRATEGY_TOKEN` are both present, the factory issues one
synchronous `GET /api/v1/agent/llm-config` (bearer-authenticated, 10 s timeout)
and uses the returned `{base_url, api_key, timeout_seconds}`; a `404
NOT_CONFIGURED`, any other non-2xx, a timeout, or a transport error is a
startup `LLMConfigError` that never echoes the token or the response body. The
response MAY carry an optional `role_models` JSON object (role→model, resolved
by the control-plane from platform defaults + platform/strategy overrides);
when present it WINS, merged over the `trader_model`/`default_model`-derived
map so a partial map from an older control-plane still yields a complete
7-role map. A non-object `role_models`, a key outside the pipeline roles, or a
non-string/empty model value is a startup `LLMConfigError` (never echoing the
body); when absent, `trader_model`/`default_model` resolve as before. When
neither the full env pair nor a reachable control-plane config exists, live
mode MUST fail fast naming both options. Stub mode requires none of these and
never fetches.

Stub-mode overrides (`llm/factory.py`). Four OPTIONAL env vars shape the
stub: `ALPHAMINTX_STUB_SCENARIO` (`bullish` | `low_confidence`; any other
value is a startup `LLMConfigError`), `ALPHAMINTX_STUB_MODEL_NAME` (the
model name the stub reports; set-but-empty is a startup error),
`ALPHAMINTX_STUB_TRADER_JSON` (a JSON OBJECT shallow-merged into the
trader response; non-JSON or a non-object value is a startup error), and
`ALPHAMINTX_STUB_ROLE_MODELS` (a JSON OBJECT mapping pipeline role→model
name the stub reports for that role, falling back to
`ALPHAMINTX_STUB_MODEL_NAME` for unmapped roles; non-JSON, a non-object
value, a key outside the pipeline roles, or a non-string/empty model name
is a startup error — all validations fail fast at client construction).
When ANY of the four is set, the scenario is built from env (symbol keyed
by `ALPHAMINTX_SYMBOL` when set) and takes precedence over an explicit
`stub_factory` argument; with none set, `stub_factory()` is called
unchanged — CI and e2e behavior is identical to before.
