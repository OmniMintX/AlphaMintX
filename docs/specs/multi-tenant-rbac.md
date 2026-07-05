# Spec: Multi-tenant RBAC and tenancy isolation (Phase 2)

Normative. Defines tenants, the fixed role set, DB-issued API tokens, tenant
isolation, runtime limit changes, and the tenant-tier kill-switch endpoint.
Companion to `docs/specs/persistence-and-api.md` (store, §Auth),
`docs/specs/risk-limits.md` (§Authority, §Kill-switch),
`docs/specs/strategy-lifecycle.md` (role guards), and `docs/PLAN.md` Phase 2.

## Goals and non-goals

- Goal: the two PLAN.md Phase 2 exit criteria this spec serves — "RBAC matrix
  tests: Trader cannot change limits; no role reads back API keys" and
  "Tenant A cannot read or affect tenant B data (isolation tests)" — plus the
  Phase 2 scope items: tenant isolation, RBAC enforcement, per-tenant
  kill-switch.
- Non-goals (v1): no `users` table or per-user identity beyond token
  `label` + `created_by`; no OIDC/JWT; no policy engine (roles are a fixed
  enum); no platform-scope kill endpoint (Phase 3 drills); no per-tenant
  billing endpoints (separate billing spec); no Postgres migration.
  `backtest.db` stays single-tenant dev tooling, explicitly OUT of tenancy
  scope: `backtestctl` is an offline operator CLI, not a tenant-facing API.

## Principals and roles

Three principal classes. **Users** carry exactly one role from the fixed
set, scoped to one tenant (DB tokens only). **Agents** are NOT a role: an
agent token is scoped to (`strategy_id`, tenant) — least privilege, POST
proposals + traces on its own strategy only (ARCHITECTURE.md §Plane
authentication), nothing else. **Env tokens** are platform-scoped legacy
credential classes (below), not tenant principals and not roles.

| Role | Meaning |
|---|---|
| `viewer` | Read-only over the tenant's data. Can never mutate anything. |
| `trader` | viewer + L1/escalation approvals, lifecycle transitions ("Trader+", strategy-lifecycle.md). MUST NOT change limits (invariant 5). |
| `admin` | trader + limits changes, tenant kill-switch, token management. |
| `owner` | All admin permissions. v1 defines no owner-only action (tenant deletion / ownership transfer deferred); `owner` exists so admin-minted tokens can never escalate to the top role (§Token lifecycle). |

Roles order `viewer < trader < admin < owner`. There is no cross-tenant role;
the platform-admin principal of risk-limits.md §Kill-switch is Phase 3.

**Env tokens (platform-scoped, backward compat — the soak run MUST NOT
break).** Env-token holders are the DEPLOYER: they already have filesystem
access to `control.db`, so no security boundary between them and any
tenant's data exists or can exist. Env tokens are config-tier (never stored
in `api_tokens`, not API-revocable, no `token_events`), constant-time
compared exactly as Phase 1, and keep their Phase 1 CLASSES verbatim — they
are not tenant-bound:

| Env token | Class (Phase 1 wire behavior unchanged) |
|---|---|
| `CONTROLPLANE_READ_TOKEN` | read — GETs only, over every tenant; never authorizes a POST. |
| `CONTROLPLANE_OPERATOR_TOKEN` | operator — `POST .../approvals` only, any tenant. |
| `CONTROLPLANE_AGENT_TOKENS` entries | agent — per-strategy ingestion POSTs, exactly as Phase 1. |
| `CONTROLPLANE_ADMIN_TOKEN` (NEW) | env-admin — tenant management, token management, limits changes, tenant kill: the platform operator. |

The tenant-isolation exit criterion binds TENANT principals, which are
exclusively DB tokens. NORMATIVE operational rule: a deployment serving
multiple real tenants MUST NOT distribute env tokens to tenants; tenants
receive DB tokens only. Consequence: all Phase 1 wire behavior is unchanged
(read on POST stays 403, operator on GET stays 403; the existing `api`
tests pass unchanged).

**Classification precedence (normative).** Env-token constant-time
comparisons run FIRST; a match short-circuits the DB lookup — which class a
plaintext resolves to is deterministic even if the same string was also
minted as a DB token.

## Tables (DDL, normative — control.db)

```sql
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE api_tokens (token_id TEXT PRIMARY KEY,           -- mutable snapshot; ONLY legal
  tenant_id TEXT NOT NULL REFERENCES tenants,                 -- mutation sets revoked_at once
  principal TEXT NOT NULL CHECK (principal IN ('user','agent')),
  role TEXT CHECK (role IN ('owner','admin','trader','viewer')),
  strategy_id TEXT REFERENCES strategies,
  token_hash TEXT NOT NULL UNIQUE,                            -- hex(SHA-256(plaintext))
  label TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT,
  CHECK ((principal = 'user' AND role IS NOT NULL AND strategy_id IS NULL)
      OR (principal = 'agent' AND role IS NULL AND strategy_id IS NOT NULL)));
CREATE TABLE token_events (event_id TEXT PRIMARY KEY,         -- append-only token audit
  token_id TEXT NOT NULL REFERENCES api_tokens,
  event TEXT NOT NULL CHECK (event IN ('created','revoked')),
  actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);
-- kill_breaker_events gains a nullable tenant_id column (guarded ALTER, §Migration note).
```

- `api_tokens` follows the `pending_approvals`/`positions` pattern: a mutable
  snapshot whose only mutation is `revoked_at` NULL → timestamp, exactly once.
  Every create and revoke ALSO appends a `token_events` row (invariant 7).
- An agent token's `tenant_id` MUST equal its strategy's `strategies.tenant_id`
  at mint time; minting against a foreign-tenant strategy is 404.
- Timestamps RFC 3339 UTC `Z`; no money columns here (ADR-0003 binds
  §Runtime limit changes below).

## Token lifecycle (normative)

- **Mint.** `POST /api/v1/tokens` (admin/owner own tenant; env-admin any
  tenant, body then carries `tenant_id`): body
  `{principal, role | strategy_id, label}`. Plaintext = `amx_` + 64
  lowercase hex chars (32 CSPRNG bytes), regex `^amx_[0-9a-f]{64}$`,
  returned **exactly once** in the create response; the server persists only
  `token_hash`. A `token_hash` UNIQUE violation at mint is retried
  internally with a fresh CSPRNG value, never surfaced; plaintexts are never
  caller-supplied, so minting provides no existence oracle.
- **Mint ceiling.** A creator may mint `user` roles only at or below its own
  role (admin mints admin/trader/viewer; only an owner mints owner) and
  `agent` tokens for own-tenant strategies. Env-admin exceptions, both
  audited: (a) the tenant-creation response mints the tenant's first `owner`
  token (§Tenancy rules — the SOLE routine owner-minting path for the
  platform); (b) recovery — env-admin may mint an `owner` token via
  `POST /api/v1/tokens` ONLY for a tenant with zero unrevoked owner tokens;
  otherwise 403 `FORBIDDEN`.
- **Lookup.** Auth SHA-256s the presented bearer and looks up `token_hash` by
  exact match. This is constant-time-compatible: the compared value is a
  fixed-length digest and the UNIQUE index matches whole values only — no
  prefix oracle, no length leak. Lookup MUST observe `revoked_at`; a revoked
  token is 401 on the next request (no auth result may outlive its request).
- **Revoke.** `POST /api/v1/tokens/{token_id}/revoke` (admin/owner, own
  tenant; foreign or absent `token_id` ⇒ 404 `UNKNOWN_TOKEN`). Revoke
  ceiling: a principal may revoke only tokens whose role is at or below its
  own (owner tokens are revocable by owner only; env-admin may revoke any).
  Sets `revoked_at`, appends `token_events(event='revoked')`.
- **List.** `GET /api/v1/tokens` (admin/owner, own tenant), paginated with
  the standard `{items, total, page, limit}` envelope, returns metadata
  only: `token_id, principal, role, strategy_id, label, created_by,
  created_at, revoked_at` — **NEVER `token_hash` NOR plaintext**.
- **No-read-back invariant (normative).** After creation, no endpoint, log
  line, or error body may return a credential's plaintext or hash. This
  satisfies the PLAN.md criterion "no role reads back API keys" for these
  tokens now, and the SAME invariant binds every credential the platform
  stores — explicitly including Phase 3 venue API keys (invariant 6:
  write-only after save).
- **Audit identity.** For DB-token actors, audit columns
  (`approvals.decided_by`, `risk_limit_changes.actor_id`,
  `kill_breaker_events.actor_id`, `lifecycle_transitions.actor_id`,
  `token_events.actor_id`) record the `token_id` — stable and non-secret;
  `label` is display metadata only. Env tokens keep their Phase 1 principal
  ids (`OperatorPrincipal`, etc.); the env-admin is recorded as
  `"env-admin"` in `created_by` and every actor column it touches.

## Tenancy rules (normative)

- **Isolation rule (two parts).** (1) The ROOT object resolution of every
  request — the strategy named by the path `{id}`, or a chain ending in a
  strategy (verdict → proposal → strategy) — MUST be filtered by the
  principal's tenant; a foreign-tenant root is indistinguishable from
  absence — **404** with the same code and body as a nonexistent object
  (`UNKNOWN_STRATEGY`, `UNKNOWN_RUN`, `UNKNOWN_VERDICT`), NEVER 403: no
  cross-tenant existence oracle. (2) Sub-reads within the same request MAY
  key on identifiers already proven in-tenant by the root check (e.g., run
  detail's proposal/verdict/orders/fills reads keyed by the checked run).
  Platform env tokens pass the root check for every tenant (§Principals).
- **Approvals ordering.** For `POST .../approvals` the path `{id}` strategy
  is tenant-resolved FIRST (404 `UNKNOWN_STRATEGY` for a DB token on a
  foreign/absent strategy), THEN the verdict → proposal `strategy_id` chain
  must equal the path strategy (existing check). Both shapes are attack
  surface: a foreign strategy path, and an own path with a foreign
  `verdict_id` — the isolation tests cover both.
- **Store layer.** Tenant-scoped root queries take an explicit `tenantID`
  parameter, threaded from the authenticated principal — never from request
  input. No blanket `TenantStore` wrapper type is mandated; the two-part
  rule above binds per request.
- **Lists.** List endpoints filter to the principal's tenant; `total` counts
  are tenant-scoped; aggregate responses MUST never contain foreign rows.
- **Agent scope.** The existing check is preserved verbatim: a path/body
  `strategy_id` outside the agent token's scope ⇒ 403
  `STRATEGY_SCOPE_MISMATCH` (compat: the wire behavior of Phase 1 agents is
  unchanged, and an agent's own strategy set is not secret from itself). An
  env agent token whose strategy row is absent fails closed via the existing
  404 `UNKNOWN_STRATEGY` on its ingestion endpoints — no tenant derivation
  is needed (env tokens are platform-scoped).
- **Tenant creation.** `POST /api/v1/tenants` — env-admin ONLY in v1. Body
  `{tenant_id, name}`; `tenant_id` matches `^[a-z0-9][a-z0-9_-]{0,31}$`
  (`default` reserved) else 400 `INVALID_TENANT_ID`; an existing
  `tenant_id` ⇒ 409 `TENANT_EXISTS`. The response atomically mints and
  returns the tenant's first `owner` token (plaintext once,
  `created_by = "env-admin"`) — the documented mint-ceiling exception.
  Thereafter the tenant is self-service via that owner token. The platform
  operator saw that plaintext: rotating the initial owner token (mint a new
  owner, revoke the initial one) SHOULD be the tenant's first act.

## Permission matrix (normative)

Mirrored by an exported Go table in the `api` package; routes are REGISTERED
from that table, and the matrix test iterates it (§Test requirements).

DB principals:

| Endpoint | viewer | trader | admin | owner | agent |
|---|---|---|---|---|---|
| `GET /api/v1/strategies[/{id}[/runs[/{run_id}]]]` (all reads) | ✓ | ✓ | ✓ | ✓ | ✗ |
| `POST .../approvals` | ✗ | ✓ | ✓ | ✓ | ✗ |
| `POST .../proposals`, `POST .../traces` | ✗ | ✗ | ✗ | ✗ | ✓ own strategy only |
| `POST .../limits` (runtime limit change) | ✗ | ✗ | ✓ | ✓ | ✗ |
| `POST /api/v1/tenants/{tenant_id}/kill` | ✗ | ✗ | ✓ own | ✓ own | ✗ |
| `POST /api/v1/strategies/{id}/lifecycle` (lifecycle transition, lifecycle-api.md LC-2) | ✗ | ✓ | ✓ | ✓ | ✗ |
| `GET /api/v1/strategies/{id}/paper-gate` (promotion visibility, LC-24) | ✓ | ✓ | ✓ | ✓ | ✗ |
| `POST /api/v1/strategies/{id}/kill/clear` (unlock is Admin+, LC-29) | ✗ | ✗ | ✓ own | ✓ own | ✗ |
| `POST /api/v1/tenants/{tenant_id}/kill/clear` | ✗ | ✗ | ✓ own | ✓ own | ✗ |
| `POST /api/v1/platform/kill/clear` (env-admin ONLY) | ✗ | ✗ | ✗ | ✗ | ✗ |
| `POST/GET /api/v1/tokens`, `POST .../revoke` | ✗ | ✗ | ✓ own | ✓ own | ✗ |
| `POST /api/v1/tenants` | ✗ | ✗ | ✗ | ✗ | ✗ |
| `GET /health` | unauthenticated | | | | |

Env classes (platform-scoped, §Principals): read ⇒ all strategy-data GETs,
any tenant, incl. `GET .../paper-gate` (NOT the token-metadata routes — the
most-exposed credential gets the least surface); operator ⇒
`POST .../approvals` only, any tenant; agent ⇒ its two ingestion routes
only; env-admin ⇒ `POST .../limits`, `POST .../kill`,
`POST .../lifecycle`, all three `.../kill/clear` routes (the platform
clear is env-admin ONLY), all `/api/v1/tokens` routes, and
`POST /api/v1/tenants` — any tenant, and NO strategy-data reads (the read
class already exists).

- Reads at viewer+ preserve Phase 1 semantics (READ_TOKEN never authorizes a
  POST); `POST .../approvals` at trader+ preserves operator semantics
  (`decided_by` attribution unchanged).
- No user role may POST proposals or traces; agent tokens MUST NOT be
  accepted by any endpoint outside their two ingestion routes — including
  every NEW endpoint.

**Status semantics (normative).** Checks run in this order — auth, then
role/class, then tenant-scoped object resolution — so an insufficient role
answers 403 without revealing whether the object exists:

| Condition | Response |
|---|---|
| Missing / unknown / revoked token | 401 `UNAUTHORIZED` |
| Known principal, insufficient role or class | 403 `FORBIDDEN` |
| Agent principal, strategy outside token scope | 403 `STRATEGY_SCOPE_MISMATCH` |
| Known principal, sufficient role, foreign-tenant object | 404, same code as nonexistent |

**Error codes for the new endpoints (normative).** `UNKNOWN_TOKEN` (404 —
revoke of a foreign or absent `token_id`), `TENANT_EXISTS` (409),
`INVALID_TENANT_ID` (400), `INVALID_ROLE` (400 — malformed
`principal`/`role`/`strategy_id` mint body shape; mint- and revoke-CEILING
violations are 403 `FORBIDDEN` per §Status semantics),
`INVALID_LIMIT_CHANGE` (400 — any non-whitelisted field or invalid value);
`UNAUTHORIZED`, `FORBIDDEN`, `UNKNOWN_STRATEGY` are reused with their
Phase 1 meanings.

## Runtime limit changes (NEW, minimal)

`POST /api/v1/strategies/{id}/limits` — admin/owner only (trader/viewer ⇒
403 `FORBIDDEN`). Body `{"changes": {"<field>": <value>, ...}}`, per-strategy.

- **Runtime-changeable whitelist v1** (validated on the SAME code path as
  startup parsing, PLUS the bounds here): `max_open_positions` (int ≥ 0),
  `max_orders_per_minute` (int ≥ 1), and the decimal fields
  `per_position_notional_cap_quote`, `daily_loss_limit_quote`,
  `max_loss_at_stop_quote` — decimal-as-string in the strict ADR-0003
  unsigned form (the contract regex: negatives and exponents are
  unrepresentable). `symbol_whitelist` and `require_stop_loss` are NOT
  runtime-changeable in v1; `accounting_quote` is NEVER changeable. Any
  non-whitelisted field or invalid value ⇒ 400 `INVALID_LIMIT_CHANGE`, the
  whole request rejected atomically (no partial apply).
- Admin MAY raise or lower any whitelisted limit — invariant 5 says Admin
  owns limits; the protection is attribution, not a ratchet. Every change is
  audited: one `risk_limit_changes` row per field (old → new, actor,
  timestamp), all rows in ONE transaction, appended BEFORE the in-memory
  effect.
- **Effective limits (single provider).** The api server owns a
  `LimitsProvider`: the ONLY read path for effective limits, consulted by
  (a) the risk-gate call site, (b) the approval preflight daily-loss check —
  `Config.DailyLossBreached` MUST take its limit from the provider per
  strategy, never from a startup-captured value — and (c) any future
  consumer. Effective limits per strategy = the startup config base
  (`CONTROLPLANE_RISK_LIMITS`) overlaid with the latest persisted
  `risk_limit_changes` value per whitelisted field.
- **Overlay determinism.** Replay order is `rowid` ascending (last write
  wins — `changed_at` has second precision and MUST NOT be the order key).
  The persisted overlay ALWAYS wins over the startup base, including after
  an env-config change plus restart; resetting a field = posting the desired
  value (no sentinel mechanism in v1). Hydration happens in the SERVER layer
  at startup, after `store.Open` (the store neither knows nor owns
  `RiskLimits`).
- **Atomicity.** Provider snapshots are immutable values swapped atomically;
  the proposal path reads under the existing per-strategy lock and the
  preflight reads the current snapshot — either the old or the new limits,
  never a torn set.

## Tenant kill-switch (NEW)

`POST /api/v1/tenants/{tenant_id}/kill` — admin/owner, own tenant only
(`{tenant_id}` ≠ principal tenant ⇒ 404); env-admin, any tenant. Body `{}`.

- Appends `kill_breaker_events` with `kind='kill'`, `scope='tenant'`,
  `tenant_id` set, `strategy_id` NULL, `flatten` 0 — persisted and
  acknowledged BEFORE any side effect executes (risk-limits.md §Kill-switch
  procedure, unchanged).
- **v1 scope: gate-block ONLY.** The recorded event makes the kill predicate
  fire for every strategy of the tenant — new proposals are rejected
  `KILL_SWITCH_ACTIVE` and the approval preflight blocks pending decisions.
  The effects engine (ENTRY-order cancel, reduce-only flatten, `killed`
  lifecycle transition, resumable re-drive), deferred here to the Phase 3
  drills, has since LANDED per `docs/specs/safety-wiring.md`, which
  EXTENDS this endpoint with the optional `{"flatten": bool}` body.
- **Clearable since SW-2** (supersedes "v1 is irreversible"): the tenant
  kill's standing condition is cleared by its dual,
  `POST /api/v1/tenants/{tenant_id}/kill/clear` (admin/owner own tenant;
  env-admin) — an append-only `kill_clear_events` row whose REQUIRED
  `observed_epoch` is CAS-verified (`docs/specs/lifecycle-api.md`
  LC-27..LC-33); the active-kill predicate (LC-28) then stops binding the
  tenant's strategies, and the strategy-lifecycle.md unlock paths run
  through the lifecycle endpoint (LC-36). An uncleared kill stands,
  exactly as before.
- **Epoch monotonicity across scopes.** `kill_epoch = MAX(kill_epoch) OVER
  the whole table + 1`, computed inside the insert transaction: one global
  counter, so the OMS kill re-check's "stale epoch" comparison stays
  well-defined whatever scope fired last. This is race-free under the
  store's single-connection invariant (`SetMaxOpenConns(1)`); any relaxation
  of that invariant requires `BEGIN IMMEDIATE` or an equivalent.
- **Gate/hydrator predicate (fail closed, most restrictive wins,
  normative SQL).** Kill is active for a strategy iff `MAX(kill_epoch) > 0`
  over rows matching:

  ```sql
  kind = 'kill' AND ((strategy_id IS NULL AND tenant_id IS NULL)  -- Phase 1 global
                  OR strategy_id = :sid                           -- own strategy
                  OR (tenant_id = :tid AND strategy_id IS NULL))  -- own tenant
  ```

  Phase 1 global rows (both columns NULL) keep binding every strategy;
  tenant rows bind ONLY their tenant — the naive extension
  (`strategy_id IS NULL OR ...`) would make any tenant kill global. The
  predicate applies to BOTH `GlobalMaxKillEpoch` (hydrator → gate) and
  `MaxKillEpoch` (approval preflight); `:tid` is resolved from the
  strategy's `strategies.tenant_id`.
- The platform tier and kill-driven token revocation (ARCHITECTURE.md
  "tokens are revoked on kill-switch") land with the Phase 3 drills.

## Web viewer

Unchanged for Phase 1 deployments: the dashboard's env READ token is
platform-scoped and reads every tenant (§Principals — deployer credential).
A tenant-facing web deployment MUST instead bake a per-tenant `viewer` DB
token into the bundle (`NEXT_PUBLIC_READ_TOKEN` accepts either): then a
leaked dashboard credential reads one tenant, never the platform — the
recorded tenancy hardening of the §Auth READ-token exposure risk in
persistence-and-api.md.

## Security rules (normative)

- Token values are NEVER logged (Phase 1 rule retained; `Logf` MUST NOT be
  handed token values).
- `token_hash` is NEVER returned by any endpoint.
- Error bodies NEVER echo the presented token.
- Rate limits apply per token as today (60 req/min POSTs; 30/min per-strategy
  proposals); for DB tokens the limiter key is `token_hash`, so plaintext is
  never held in long-lived maps.
- Agent tokens cannot call any new endpoint (§Permission matrix).
- Every new mutating endpoint appends its audit row (`token_events`,
  `risk_limit_changes`, `kill_breaker_events`) BEFORE side effects.

## Migration note (normative)

`store.Open` executes the embedded `schemaDDL` idempotently
(`CREATE TABLE IF NOT EXISTS`) on every open — additive changes only:

1. Append `tenants`, `api_tokens`, `token_events` to `schemaDDL` as
   `CREATE TABLE IF NOT EXISTS` (new tables, no conflict with existing DBs).
2. `kill_breaker_events.tenant_id` cannot use that mechanism (the table
   exists). After applying `schemaDDL`, `Open` MUST inspect
   `PRAGMA table_info(kill_breaker_events)` and run `ALTER TABLE
   kill_breaker_events ADD COLUMN tenant_id TEXT` iff the column is absent —
   idempotent, safe on the single WAL connection.
3. Seed `INSERT OR IGNORE INTO tenants VALUES ('default', 'default', <now>)`
   AND `INSERT OR IGNORE INTO tenants (tenant_id, name, created_at)
   SELECT DISTINCT tenant_id, tenant_id, <now> FROM strategies` — every
   pre-existing `strategies.tenant_id` value gets a `tenants` row, so DB
   tokens can be minted for grandfathered tenants.
4. NO data backfill on `kill_breaker_events`: existing rows read `tenant_id`
   NULL (global or strategy scope, exactly as before); `strategies.tenant_id`
   already exists (Phase 1). Because env tokens are platform-scoped, an
   existing soak `control.db` opens and serves unchanged regardless of what
   tenant ids its strategies carry.

## Test requirements

- **RBAC matrix test.** The `api` package exports a declarative permissions
  table (route × allowed principal classes/roles) and registers its mux
  routes FROM that table. `TestRBACMatrix` is a single table-driven test
  iterating (endpoint × principal) over it against a FULLY-WIRED server
  (every optional dependency set, so every route is registered — the table
  is the total route set), asserting the expected status per §Status
  semantics; it MUST fail when a route exists without a matrix entry,
  enforced by comparing the registered-route enumeration against the table.
- **PLAN.md exit-criteria mapping (exact sentences):**

| PLAN.md criterion | Tests |
|---|---|
| "RBAC matrix tests: Trader cannot change limits; no role reads back API keys." | `TestRBACMatrix` (includes trader × `POST .../limits` ⇒ 403), `TestTokenNeverReadBack` (mint, then assert list/detail responses and error bodies contain neither plaintext nor `token_hash`, for every role) |
| "Tenant A cannot read or affect tenant B data (isolation tests)." | `TestTenantIsolation_CrossRead404`, `_CrossApproval404` (BOTH shapes: foreign strategy path, and own path + foreign `verdict_id`), `_CrossKill404`, `_KillDoesNotBleedAcrossTenants` (tenant B proposals pass the gate AND tenant B approvals pass the preflight after a tenant A kill), `_AgentCrossStrategy403` (expects `STRATEGY_SCOPE_MISMATCH`, existing behavior), `_ListsExcludeForeignRows` |

- Isolation tests build TWO tenants and assert: cross-tenant GETs and
  approvals ⇒ 404 identical to absence; cross-tenant kill ⇒ 404 (path
  tenant ≠ principal tenant); agent token against a foreign strategy ⇒ 403
  `STRATEGY_SCOPE_MISMATCH`; list endpoints and their `total` never contain
  foreign rows.
- Token lifecycle tests: `TestEnvAdminCannotMintOwnerWhenOwnerExists`
  (recovery mint allowed only at zero unrevoked owner tokens),
  `TestAdminCannotRevokeOwner` (revoke ceiling), revoked token ⇒ 401 on the
  next request.
- Limit-change tests: audit row per field, atomic reject on any invalid
  field, restart replay yields the same effective limits EVEN WHEN the env
  base differs from the value at change time (overlay wins), gate observes
  the new value on the next evaluation, and the approval preflight
  daily-loss check observes a lowered `daily_loss_limit_quote` (the
  provider, not a startup capture).
- Kill tests: tenant kill epoch strictly greater than any prior epoch in the
  table; hydrator reports kill active for every strategy of the killed
  tenant and inactive for other tenants.
- Backward compat: the Phase 1 env-token deployment shape (no
  `CONTROLPLANE_ADMIN_TOKEN`, no DB tokens) passes the existing `api` tests
  unchanged — guaranteed by construction (§Principals: env classes keep
  Phase 1 wire behavior verbatim).
