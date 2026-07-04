# Spec: Billing and metering reconciliation (Phase 2)

Normative. Defines the LLM-attempt join key (`request_id`), the mintrouter
metering-export ingest, UTC-month billing periods, append-only invoices, and
the reconciliation procedure for the PLAN.md Phase 2 exit criterion "Billing
invoices reconcile with mintrouter metering". Companion to
`docs/specs/llm-routing-and-budget.md` (§3 cost accounting, §4 ledger — this
spec extends both), `docs/specs/persistence-and-api.md` (store rules, trace
ingest), `docs/specs/multi-tenant-rbac.md` (roles, tenancy, permission
matrix), ADR-0003 (decimal-as-string), ADR-0004 (mintrouter gateway).

## Goals and non-goals

- Goal: every invoice is generated deterministically from `model_costs` (the
  client-side billable truth), and a reconciliation run proves, with exact
  decimal arithmetic, that the invoice agrees with a mintrouter spend-log
  export — every divergence enumerated and classified, never absorbed.
- **Truth model (normative).** AlphaMintX does not control mintrouter and
  this deployment cannot call a live mintrouter spend API. `model_costs`
  (computed locally from `usage` tokens × the versioned `llm/prices.json`,
  llm-routing §3) is the BILLABLE source; the imported gateway export is the
  CHECK. A cost disagreement never changes an invoice: it becomes a
  `MISMATCH_COST` discrepancy documenting price-table drift.
- Non-goals (v1): no payment collection; no subscription plans or pricing
  tiers — the PLAN.md "subscription plans" scope item is explicitly
  deferred: v1 invoices meter raw LLM cost and plan pricing multipliers land
  on top later; no live mintrouter API pull (import is file-based via the
  ingest endpoint); no credit-note ISSUANCE flow (the line shape is pinned
  below, the endpoint is v1.1); no PDF/rendering; no Postgres migration.

## Join key: per-attempt `request_id`

- Agent-plane generates a UUIDv4 `request_id` **per LLM ATTEMPT**, not per
  logical call: retried and timed-out attempts each spend tokens at the
  gateway and each produce their own spend-log row, so a per-call id could
  never join 1:1. Each attempt in `MintRouterLLM.complete` sends its id as
  the `X-Request-Id` header and records it on the `ModelCost` entry that
  attempt appends (the success entry carries the successful attempt's id;
  each timeout/5xx estimated entry carries its own attempt's id).
- Attempts that append no cost entry keep their ids out of the trace: a
  transport error never reached mintrouter (no spend either side); a 429 was
  rejected pre-generation (llm-routing §3) — if the gateway logs zero-spend
  rows for 429s they surface as zero-cost `ORPHAN_GATEWAY`, enumerated and
  expected.
- **Contract change: a trace-only `trace_model_cost` (proposal untouched).**
  `model_cost` is a SHARED definition: `contracts/proposal.schema.json` and
  `contracts/agent_trace.schema.json` both pin it with
  `additionalProperties: false`, and the Go `contract.ModelCost` / Pydantic
  `ModelCost` types back BOTH contracts — adding fields to `model_cost`
  would silently widen the PROPOSAL contract. The TRACE contract therefore
  gets its OWN definition: `agent_trace.schema.json` gains
  `$defs/trace_model_cost` = the proposal `model_cost` fields plus OPTIONAL
  `request_id` (uuid `$def`) and OPTIONAL `estimated` (boolean, default
  false — set on timeout/5xx estimated entries), and the envelope's
  `model_costs[]` items reference it. `proposal.schema.json` is NOT touched;
  `TradeProposal.model_costs` items NEVER carry the new fields
  (`additionalProperties: false` keeps rejecting them). `estimated` is a
  row-level flag because `estimated_cost_nodes[]` is node-granular and a
  node can carry BOTH estimated attempts and a final measured entry —
  node-level data cannot classify rows.
- **Artifacts to change (exhaustive).** (1)
  `contracts/agent_trace.schema.json`: add `$defs/trace_model_cost` and
  point `model_costs.items` at it. (2) control-plane Go: the trace envelope
  struct (`store.TraceEnvelope`) and `validateTrace` switch `model_costs` to
  a NEW trace-only type; the shared `contract.ModelCost` stays
  proposal-shaped, unchanged. (3) agent-plane Python: the trace envelope
  model gains a separate `TraceModelCost` Pydantic model; the proposal's
  `ModelCost` is unchanged. (4) agent-plane `LLMResponse` gains
  `request_id`, so the pipeline can build trace entries from attempt
  results.
- **Deploy order (normative).** control-plane MUST deploy before ANY agent
  that emits the new fields: an old control-plane 400-rejects a new-field
  trace (`DisallowUnknownFields`), and that trace is lost PERMANENTLY —
  `agent_traces.run_id` is UNIQUE and the run is never re-traced. WITH this
  ordering clause, `schema_version` stays `"1.0"`: purely optional
  additions; every previously valid trace remains valid.
- **Hash stability (normative).** The new Go envelope fields marshal
  omitted-when-absent (pointer types with `omitempty`), so a pre-upgrade
  envelope re-POSTed after the upgrade re-marshals byte-identical and its
  `payload_sha256` is unchanged — trace-ingest idempotency stays stable (no
  spurious 409 `IDEMPOTENCY_CONFLICT` on checkpoint re-drives of old runs).
- **Overflow aggregation (llm-routing §3, pinned interaction).** The
  `node="overflow_aggregate"` entry merges ≥ 2 calls and carries **NO
  `request_id`** and no `estimated` flag: per-row join keys are dropped for
  merged calls (cost is never dropped — sums stay exact). Consequence,
  accepted: gateway rows for merged calls classify `ORPHAN_GATEWAY`, and the
  run reconciles at aggregate level only. Overflow requires > 32 calls in
  one run and is rare by construction.
- Stub mode appends entries without `request_id` (no network, nothing to
  reconcile). Pre-upgrade traces likewise have none: such rows are
  "unattributed" and participate only in the invoice identity (§Reconciliation).
- **Ingest fan-out.** Trace ingest copies `request_id` and `estimated` onto
  the `model_costs` row; an ABSENT `estimated` field means `is_estimated =
  0` — the flag is NEVER inferred from `estimated_cost_nodes[]` (node-level
  data; a node can hold both estimated and measured entries). On a
  `request_id` UNIQUE-index conflict (agent defect, UUID collision, or a
  squatted id — §Reconciliation) the row is stored with `request_id` NULL —
  a join-key defect MUST NOT drop cost or fail the trace; the resulting
  gateway orphan surfaces at reconciliation.

## Tables (DDL, normative — control.db)

```sql
-- model_costs gains request_id + is_estimated via guarded ALTER (§Migration).
CREATE UNIQUE INDEX idx_model_costs_request_id
  ON model_costs (request_id) WHERE request_id IS NOT NULL;
CREATE TABLE metering_records (record_id TEXT PRIMARY KEY,   -- append-only gateway import
  source TEXT NOT NULL,                                      -- export label, e.g. filename
  request_id TEXT NOT NULL UNIQUE, strategy_id TEXT NOT NULL REFERENCES strategies,
  model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
  cost_usd TEXT NOT NULL, metered_at TEXT NOT NULL, imported_at TEXT NOT NULL);
CREATE TABLE billing_periods (period_id TEXT PRIMARY KEY,    -- append-only; absence = open
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL,      -- YYYY-MM
  period_start TEXT NOT NULL, period_end TEXT NOT NULL,      -- inclusive UTC dates
  status TEXT NOT NULL CHECK (status IN ('closed')),         -- row insertion IS the close
  closed_at TEXT NOT NULL,                                   -- informational ONLY (§Billing)
  watermark_rowid INTEGER NOT NULL,                          -- exactly-once window key (§Billing)
  UNIQUE (tenant_id, period));
CREATE TABLE invoices (invoice_id TEXT PRIMARY KEY,          -- append-only, immutable
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL,
  total_usd TEXT NOT NULL, line_count INTEGER NOT NULL, generated_at TEXT NOT NULL,
  UNIQUE (tenant_id, period));
CREATE TABLE invoice_lines (line_id TEXT PRIMARY KEY,        -- append-only
  invoice_id TEXT NOT NULL REFERENCES invoices, strategy_id TEXT NOT NULL,
  model TEXT NOT NULL, entry_type TEXT NOT NULL CHECK (entry_type IN ('usage','carry_over','credit_note')),
  original_period TEXT,                                      -- non-null iff carry_over
  input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, amount_usd TEXT NOT NULL);
CREATE TABLE reconciliation_runs (recon_id TEXT PRIMARY KEY, -- append-only
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL, invoice_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pass','fail')),
  matched_count INTEGER NOT NULL, discrepancy_count INTEGER NOT NULL,
  matched_client_cost_usd TEXT NOT NULL, orphan_client_cost_usd TEXT NOT NULL,
  estimated_client_cost_usd TEXT NOT NULL, unattributed_client_cost_usd TEXT NOT NULL,
  invoice_total_usd TEXT NOT NULL, run_at TEXT NOT NULL);    -- == invoices.total_usd (identity)
CREATE TABLE discrepancies (discrepancy_id TEXT PRIMARY KEY, -- append-only
  recon_id TEXT NOT NULL REFERENCES reconciliation_runs,
  class TEXT NOT NULL CHECK (class IN ('ORPHAN_CLIENT','ORPHAN_GATEWAY','ESTIMATED_CLIENT',
    'MISMATCH_TOKENS','MISMATCH_COST','ATTRIBUTION_MISMATCH')),
  request_id TEXT, strategy_id TEXT, details_json TEXT NOT NULL);
```

All six tables are **INSERT-only** (invariant 7): no UPDATE, no DELETE, ever.
`TestStoreSurfaceIsAppendOnly` gains them with ZERO new mutators. Timestamps
RFC 3339 UTC `Z`; all money TEXT decimal-as-string (ADR-0003).

## Metering ingest (gateway export import)

`POST /api/v1/billing/metering` — **env-admin ONLY**: the import is a
deployer act (mintrouter is patterned on the LiteLLM proxy; its spend-log
export carries request id, api-key alias, model, prompt/completion tokens,
spend, timestamp — the deployer holds that file, no tenant does).

- Body: `{source, alias_map?: {<alias>: <strategy_id>}, records: [...]}`;
  each record `{request_id, strategy_id? | api_key_alias?, model,
  input_tokens, output_tokens, cost_usd, metered_at}`. Each record MUST
  resolve to an existing `strategies` row — directly by `strategy_id`, or
  via `alias_map[api_key_alias]`; the record's tenant is DERIVED from
  `strategies.tenant_id`, never trusted from the body.
- **Records without `request_id` are REJECTED** (400
  `INVALID_METERING_RECORD`): v1 is strict — a row that cannot ever join is
  a defective export, and accepting it (or minting a synthetic id) would
  fabricate reconciliation coverage that does not exist. Same code for an
  unresolvable strategy, malformed decimal, or negative token count. The
  whole batch is atomic: any invalid record rejects the entire POST, nothing
  persisted.
- **Idempotent re-import.** Same `request_id` + identical content ⇒
  per-record no-op, 200. "Identical" is pinned: `cost_usd` compares by
  DECIMAL VALUE (`"0.50"` == `"0.5000"`); `strategy_id` (post-resolution),
  `model`, both token counts, and `metered_at` compare by byte equality
  (`source` and `imported_at` excluded). Same `request_id` + different
  content ⇒ **409 `METERING_CONFLICT`**, whole batch rejected: two exports
  disagreeing about one request is an upstream defect a re-import must
  never paper over.
- **Chunked import.** An export larger than the 1 MiB POST body cap
  (persistence-and-api.md §Limits) is imported as MULTIPLE POSTs — safe by
  per-record idempotency: already-imported records no-op, and a failed
  chunk is simply re-POSTed.
- Metering ingest NEVER writes `token_budget_ledger`, `model_costs`, or any
  invoice: the ledger stays incremented only at trace ingest
  (persistence-and-api.md), and gateway data is a check, never billable.

## Billing periods and attribution

- A billing period is a **UTC calendar month** per tenant, named `YYYY-MM`
  (`period_start` = first day, `period_end` = last day, inclusive).
- A period is **open** iff no `billing_periods` row exists for
  `(tenant_id, period)`; the table is append-only, so the row's insertion
  IS the close and closed is terminal by construction. The `status` enum
  admits ONLY `'closed'`: "open" is the absence of a row, never a row state.
- **Close.** `POST /api/v1/billing/periods/close` — env-admin ONLY, body
  `{tenant_id, period}`. Preconditions: `period_end` < today UTC (400
  `INVALID_PERIOD` — a running month cannot close); not already closed (409
  `PERIOD_CLOSED` — the close is ONE transaction, so an operator retry
  either finds the row and gets the 409 or safely re-runs a close that
  never committed); unknown tenant ⇒ 404 `UNKNOWN_TENANT` (env-admin only —
  no tenant-facing existence oracle). Closes need NOT be in calendar order:
  closing 2026-03 before 2026-02 is LEGAL — the watermark below keeps
  billing exactly-once regardless of close order. The close INSERTS the
  `billing_periods` row, the `invoices` row, and all `invoice_lines` in
  **ONE transaction** — an invoice can never exist half-generated, and a
  closed period always has exactly one invoice.
- **Exactly-once billing window: the rowid watermark (normative).** The
  close transaction snapshots `MAX(model_costs.rowid)` over the whole table
  (`0` if none) as `watermark_rowid`, persisted on the `billing_periods`
  row. Let `prev_watermark` := `MAX(watermark_rowid)` over ALL existing
  closes of the tenant (`0` for the tenant's first close). The billable set
  of this close is EXACTLY the rows matching
  `strategies.tenant_id = ? AND model_costs.rowid > prev_watermark AND
  model_costs.rowid <= watermark_rowid` (join `agent_traces USING (run_id)`
  for `started_at`; join `strategies` for the tenant). The rowid window is
  the SOLE billing partition. Exactly-once needs no chronology claim: rowid
  assignment is monotonic with commit order under SQLite's single writer,
  so successive watermark windows partition the tenant's rows and every
  `model_costs` row is billed **exactly once**, whatever calendar order the
  closes run in. Timestamps — `closed_at` included — are informational only
  and play NO part in the window. **No-VACUUM invariant:** `control.db`
  MUST NOT be VACUUMed (and `auto_vacuum` MUST remain off) while billing
  watermarks are in use — VACUUM may renumber the implicit rowids that
  `watermark_rowid` windows reference, corrupting every stored window. If
  VACUUM is ever needed, the designated future migration is promoting the
  window key to an explicit monotonic column first.
- **Usage-day labeling (labels lines; never partitions).** A `model_costs`
  row's usage day is the UTC date of its run's `agent_traces.started_at` —
  the SAME attribution the `token_budget_ledger` uses (llm-routing §4: one
  ledger day per run, by `started_at`); `recorded_at` is server-assigned
  ingest time and is NOT the attribution key. The join is total:
  `model_costs` rows are created only by trace ingest, so the trace always
  exists. Within the billable set, a row's line is `entry_type='usage'` iff
  its usage day ∈ P, else `entry_type='carry_over'` with `original_period`
  = the usage day's own period — EARLIER (a trace ingested only after its
  period closed; that invoice is immutable, so the cost rides this close)
  or LATER (a future-dated `started_at`: agent-reported and TRUSTED,
  exactly as the ledger trusts it — the rowid window prevents
  double-billing no matter what the agent reports).

## Invoices (normative)

- `invoice_id = "inv-{tenant_id}-{period}"` — deterministic from the natural
  key; UNIQUE `(tenant_id, period)` makes a second invoice for a period
  unrepresentable. **A closed invoice is never mutated**; corrections are
  future `credit_note` lines on a LATER invoice, never edits.
- Line granularity: one line per `(strategy_id, model, entry_type[,
  original_period])`, token counts and `amount_usd` the exact sums of the
  covered rows. Deterministic order: `entry_type` (`usage` < `carry_over` <
  `credit_note`), then `strategy_id`, `model`, `original_period`, all
  ascending; `line_id = "{invoice_id}#{n}"` with `n` the 0-based position in
  that order. Closing the same DB state twice (hypothetically) would emit
  byte-identical lines.
- **NO ROUNDING in v1.** `amount_usd` and `total_usd` are full-precision
  decimal strings; `total_usd` = the exact decimal sum of all lines. This
  platform stores sub-cent LLM costs; forcing 2 dp would create
  irreconcilable drift between invoice, ledger, and reconciliation identity.
  Display rounding is a presentation concern, out of scope.
- `entry_type='credit_note'` is pinned but DEFERRED (v1.1): the ONLY line
  type whose `amount_usd` may be negative, using the signed decimal variant
  `^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`; `usage` and `carry_over` amounts MUST
  match the unsigned ADR-0003 form. v1 emits no credit notes and MUST reject
  any negative amount elsewhere.

## Reconciliation (normative, testable definition)

`POST /api/v1/billing/reconcile` — env-admin ONLY, body `{tenant_id,
period}`. The period MUST be closed (else 409 `PERIOD_OPEN`): the invoice is
the comparison target. Each run appends a `reconciliation_runs` row plus its
`discrepancies` rows in one transaction; re-running appends a new run
(append-only — later imports can turn a FAIL into a PASS on a fresh run).

- **Client set** = exactly the `model_costs` rows the invoice covers (the
  §Billing rowid window: its `usage` + `carry_over` lines). Classification
  is a strict PRECEDENCE — rules applied in order, first match wins, every
  row lands in EXACTLY ONE class:
  1. `request_id` IS NULL ⇒ **unattributed** (pre-upgrade rows, stub rows,
     overflow aggregates, conflict-nulled rows — aggregate level only);
  2. `is_estimated = 1` ⇒ **estimated_client** (timeout/5xx attempts — the
     gateway may have logged real spend for a timed-out request or nothing
     at all, and estimated token counts never promise equality);
  3. `request_id` joins a `metering_records` row with the SAME
     `strategy_id` — and hence the same tenant, the record's tenant being
     derived from `strategies` — ⇒ **matched**;
  4. else ⇒ **orphan_client** (no gateway row, or a gateway row failing the
     strategy constraint — see `ATTRIBUTION_MISMATCH`).
- **ATTRIBUTION_MISMATCH (defect severity).** The matched join REQUIRES
  equal `strategy_id` on both sides; a `request_id` joining a metering
  record with a DIFFERENT `strategy_id` appends an `ATTRIBUTION_MISMATCH`
  discrepancy (the client row itself classifies `orphan_client`, rule 4).
  Squatting risk, owned: `request_id` is agent-supplied and the client
  partial UNIQUE index is first-writer-wins, so a forged id NULLs the
  VICTIM's attribution — the victim's row stores `request_id` NULL and
  classifies `unattributed`; the victim is never mis-billed (each row bills
  only on its own tenant's invoice) — while the forger's row joins the
  victim's gateway record only as a strategy mismatch:
  `ATTRIBUTION_MISMATCH` surfaces the forgery.
- **Gateway set** = the `metering_records` matched by the client set, plus
  unmatched records for the tenant's strategies with `metered_at` UTC date
  within the period ⇒ **ORPHAN_GATEWAY**. Expected sources, enumerated:
  (a) crash after the LLM call, before the trace POST — real gateway spend,
  no trace ever lands; (b) a checkpoint re-drive rebuilds the envelope,
  trace ingest answers 409 `IDEMPOTENCY_CONFLICT` and the rebuilt trace is
  dropped, while the re-driven attempts spent real gateway tokens; (c) an
  HTTP-200 response with an unparseable body appends no client cost entry
  though the gateway logged spend; plus zero-spend 429 log rows and
  overflow-merged calls. A gateway record can be `ORPHAN_GATEWAY` on period
  P's run and then MATCHED on P+1's run (its client row arrived late and
  carried over) — expected and harmless: each run is a standalone snapshot.
- **Matched pairs** (all `is_estimated = 0`, by precedence): `input_tokens`
  and `output_tokens` MUST match exactly — inequality ⇒ `MISMATCH_TOKENS`.
  `cost_usd` is compared as exact decimals — inequality ⇒ `MISMATCH_COST`,
  EXPECTED whenever the gateway price table drifts from `llm/prices.json`:
  the client cost stays billable and the discrepancy documents the drift.
- **No DUPLICATE class.** A duplicated `request_id` is unrepresentable by
  construction on BOTH sides: the client partial UNIQUE index nulls
  conflicts at ingest; the gateway side has `request_id` UNIQUE plus
  idempotent import. Uniqueness is enforced by the schema, never audited by
  reconciliation.
- **Arithmetic identity (exact, zero tolerance).** Over the client set:
  `matched_cost + orphan_client_cost + estimated_client_cost +
  unattributed_cost == invoices.total_usd` for the period (the exact sum of
  its `usage` + `carry_over` lines; v1 emits no credit notes), as exact
  decimals. This MUST hold on every run: the four classes partition
  precisely the rows the invoice sums, so any violation is an
  implementation defect, never data drift.
- **Aggregate check.** For every `(strategy_id, usage day)` group of matched
  pairs, client token sums MUST equal gateway token sums with tolerance
  **±0** — a cross-check that the row-level join and the sums agree.
- **PASS definition (verbatim).** A reconciliation run PASSES iff (a) every
  matched pair has exact token equality, (b) the arithmetic identity above
  holds exactly, (c) the aggregate check holds at ±0, and (d) no
  `ATTRIBUTION_MISMATCH` exists. `MISMATCH_COST`, `ORPHAN_CLIENT`,
  `ORPHAN_GATEWAY`, and `ESTIMATED_CLIENT` do NOT fail a run: they are
  enumerated and classified — the discrepancy LIST is the tolerance
  mechanism; the arithmetic has none.

## Permission matrix additions (normative)

Every route below gets its OWN `Permissions()` entry in
`control-plane/internal/api/permissions.go` — SEVEN entries, each `/{id}`
detail route a distinct row (never folded into its list route); routes
register FROM that table, so `TestRBACMatrix` covers them automatically
(multi-tenant-rbac.md §Test requirements).

| Endpoint | viewer | trader | admin | owner | agent | env classes |
|---|---|---|---|---|---|---|
| `POST /api/v1/billing/metering` | ✗ | ✗ | ✗ | ✗ | ✗ | env-admin only |
| `POST /api/v1/billing/periods/close` | ✗ | ✗ | ✗ | ✗ | ✗ | env-admin only |
| `POST /api/v1/billing/reconcile` | ✗ | ✗ | ✗ | ✗ | ✗ | env-admin only |
| `GET /api/v1/billing/invoices` | ✗ | ✗ | ✓ own | ✓ own | ✗ | read, env-admin |
| `GET /api/v1/billing/invoices/{invoice_id}` | ✗ | ✗ | ✓ own | ✓ own | ✗ | read, env-admin |
| `GET /api/v1/billing/reconciliations` | ✗ | ✗ | ✓ own | ✓ own | ✗ | read, env-admin |
| `GET /api/v1/billing/reconciliations/{recon_id}` | ✗ | ✗ | ✓ own | ✓ own | ✗ | read, env-admin |

- Invoices are financial records: DB-token reads are **admin/owner only** —
  viewer and trader get 403 `FORBIDDEN` (viewer's "read-only over the
  tenant's data" grant covers strategy/run data, not the tenant's bill; the
  admin role already owns the money surfaces: limits, kill). The env `read`
  class keeps its platform-scoped legacy semantics (deployer credential,
  multi-tenant-rbac.md §Principals) and reads every tenant's invoices, as it
  reads every tenant's runs.
- **Tenant isolation.** Invoices and reconciliations are tenant-scoped; list
  endpoints filter to the principal's tenant; a foreign or absent
  `invoice_id`/`recon_id` ⇒ **404** `UNKNOWN_INVOICE` /
  `UNKNOWN_RECONCILIATION`, indistinguishable from absence
  (multi-tenant-rbac.md §Tenancy rules — no cross-tenant existence oracle).
- Agent tokens are accepted by NO billing route (existing invariant: agents
  never leave their two ingestion routes).
- Error codes: `INVALID_METERING_RECORD` (400), `METERING_CONFLICT` (409),
  `INVALID_PERIOD` (400), `PERIOD_CLOSED` (409), `PERIOD_OPEN` (409),
  `UNKNOWN_TENANT` (404), `UNKNOWN_INVOICE` (404),
  `UNKNOWN_RECONCILIATION` (404). Mirrored in the persistence-and-api.md
  error-code listing.

## Money discipline (normative)

- Every sum in this spec — invoice lines, totals, identity terms, aggregate
  checks — is computed in `shopspring/decimal` values parsed from ADR-0003
  contract strings. **No float ever touches the path**: not in parsing, not
  in summing, not in comparison, not in serialization.
- Serialization is canonical and MUST round-trip: `parse(serialize(d)) == d`
  exactly by decimal value, output matching the ADR-0003 regex (no
  exponent, no sign). Serialized decimal OUTPUTS — invoice `amount_usd` and
  `total_usd`, the `reconciliation_runs` cost sums — are
  shopspring-normalized: `"0"` for zero, trailing zeros trimmed. Stored
  INPUTS (trace `cost_usd`, metering `cost_usd`) keep their ORIGINAL
  strings verbatim: normalization applies to computed outputs, never to
  stored evidence.
- Negative amounts are FORBIDDEN everywhere except the deferred
  `credit_note` line type (§Invoices, signed regex variant); stores and
  handlers MUST reject them.

## Migration note (normative)

`store.Open` stays additive-idempotent (multi-tenant-rbac.md §Migration):

1. Append the six new tables to `schemaDDL` as `CREATE TABLE IF NOT EXISTS`.
2. `model_costs` exists: after applying `schemaDDL`, inspect
   `PRAGMA table_info(model_costs)` and run `ALTER TABLE model_costs ADD
   COLUMN request_id TEXT` and `ALTER TABLE model_costs ADD COLUMN
   is_estimated INTEGER NOT NULL DEFAULT 0` iff absent — guarded, idempotent
   on the single WAL connection.
3. `CREATE UNIQUE INDEX IF NOT EXISTS idx_model_costs_request_id ON
   model_costs (request_id) WHERE request_id IS NOT NULL` — the partial
   index (SQLite ≥ 3.8.0, supported by `modernc.org/sqlite`) enforces
   uniqueness while leaving NULLs unconstrained.
4. NO data backfill: the existing soak rows (2 988 at spec time) read
   `request_id` NULL / `is_estimated` 0 — the "unattributed" class,
   reconciled at aggregate level only via the identity. An existing soak
   `control.db` opens and serves unchanged.

## Test requirements

- **Store.** `TestStoreSurfaceIsAppendOnly` extended with the six tables and
  no new mutators. Watermark window unit tests: exactly-once (a late trace
  bills as `carry_over` on the next close and NEVER twice); OUT-OF-ORDER
  closes (a later period closed before an earlier one — every row still
  billed exactly once, `carry_over`/`original_period` labels correct);
  close-vs-ingest boundary (a trace ingest committing around a concurrent
  close lands entirely inside or entirely outside the watermark window —
  SQLite's single writer serializes the two transactions); empty-period
  close is LEGAL (zero lines, `total_usd` = `"0"`); ledger-vs-`model_costs`
  drift is impossible by same-transaction construction (pinned by a test
  summing the ledger against the rows). Close determinism (same DB state ⇒
  byte-identical invoice id, line order, totals); decimal round-trip on
  every new money column.
- **API.** `TestRBACMatrix` covers the seven new routes by construction
  (registered from the table). Isolation:
  `TestBillingIsolation_ForeignInvoice404`, `_ForeignReconciliation404`,
  `_ListsExcludeForeignRows`, and viewer/trader ⇒ 403 on invoice reads.
  Metering: idempotent re-import no-op (incl. `cost_usd` differing only in
  trailing zeros), conflicting re-import ⇒ 409 batch reject, missing
  `request_id` ⇒ 400, atomic batch (one bad record persists nothing),
  chunked multi-POST import. Periods: duplicate close of the same period ⇒
  409 `PERIOD_CLOSED` (close is one transaction; operator retry safe),
  running month ⇒ 400, reconcile-before-close ⇒ 409. Forged `request_id`
  (cross-tenant squat): the victim's row ingests conflict-nulled ⇒
  unattributed, the victim's invoice total is unchanged, and reconciliation
  appends `ATTRIBUTION_MISMATCH` against the forger's join.
- **Reconcile golden test.** A synthetic gateway export against known
  `model_costs`: (a) full-match ⇒ PASS with zero discrepancies and the exact
  identity; (b) one injected discrepancy per class — `ORPHAN_CLIENT`,
  `ORPHAN_GATEWAY`, `ESTIMATED_CLIENT`, `MISMATCH_TOKENS`, `MISMATCH_COST`,
  `ATTRIBUTION_MISMATCH` — each classified exactly once, with
  `MISMATCH_TOKENS` and `ATTRIBUTION_MISMATCH` flipping status to `fail`
  and the others not.
- **Exit-criterion evidence (non-vacuous).** The recorded evidence for the
  PLAN.md criterion "Billing invoices reconcile with mintrouter metering"
  is (1) the golden reconcile test above AND (2) a non-vacuous e2e run: an
  agent run that emits `request_id`s (`MintRouterLLM` against a stubbed
  gateway — NOT `llm.stub` mode, which emits none), a matching synthetic
  gateway export imported, period closed, reconciled ⇒ `matched_cost > 0`
  with the identity exact. SUPPLEMENTARY migration evidence only: against a COPY of
  the real soak `control.db`, migrate, close a period, reconcile with an
  empty export — all rows unattributed, identity holds exactly
  (`unattributed == total`). The soak run alone exercises no join and MUST
  NOT stand as the criterion's sole evidence.
- Agent-plane: unit tests that every attempt sends a fresh `X-Request-Id`,
  the success entry and each timeout/5xx estimated entry carry their own
  attempt's id with `estimated` set correctly, 429/transport attempts append
  nothing, and the overflow aggregate carries no `request_id` while its sums
  stay exact (existing `aggregate_overflow` tests extended).
