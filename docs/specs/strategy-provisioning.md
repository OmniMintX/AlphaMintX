# Strategy provisioning — `POST /api/v1/strategies`

Status: normative. Closes the Phase-3 audit gap "partner onboarding requires
manual DB edits": `store.CreateStrategy` (LC-16a) has no production
caller, so every strategy so far was inserted by hand. This spec gives
it an API caller and completes the self-service onboarding chain
(`POST /api/v1/tenants` → **this** → `POST /api/v1/tokens` agent mint).

Normative context: `docs/specs/strategy-lifecycle.md` (LC-10/LC-16/
LC-16a), `docs/specs/multi-tenant-rbac.md` (classes, mint patterns,
tenant resolution), `docs/specs/persistence-and-api.md` (§Tables,
error envelope), `docs/specs/deploy-and-survive.md` (restore gate).

## Requirements

- **SP-1 (route + tier)** `POST /api/v1/strategies` exists in the
  permission matrix as `Roles: admins (owner|admin), Classes:
  [classEnvAdmin]` — the exact tier of `POST /api/v1/tokens`. The
  route is registered FROM the matrix (no matrix entry ⇒ no route).
  It is always on (no `Requires:` feature key).
- **SP-2 (body)** Strict-decoded body
  `{tenant_id?, name, lifecycle_state?, role_models?}` (`decodeStrict`:
  unknown fields, trailing data, malformed JSON ⇒ 400 `SCHEMA_INVALID`;
  oversize ⇒ 413 as everywhere). `name` is required, 1–128 bytes
  after `strings.TrimSpace`; whitespace-only or longer is 400. It
  MUST be valid UTF-8 and MUST NOT contain code points < U+0020,
  U+007F–U+009F, or bidi controls (U+202A–U+202E, U+2066–U+2069) ⇒
  400 — names render in the operator's strategy list, and bidi
  overrides could spoof it. (The bound and content rules are a
  DELIBERATE divergence from the unbounded tenant `name` and token
  `label`: this is the first tenant-writable display string, capped
  at birth. Names never reach shell/systemd — scheduler instances
  are keyed by `strategy_id`.) `lifecycle_state`
  is optional; allowed values are exactly `draft` (the default when
  omitted/empty) and `paper`. Anything else — including every
  `live_*` tier — is 400 `SCHEMA_INVALID`: the paper gate (LC-16,
  unwaivable) cannot be bypassed at birth via provisioning. The
  LC-16a bootstrap transition row for `paper` comes from the existing
  `store.CreateStrategy` transaction semantics, unchanged.
  `role_models` is an optional role→model override map: every key
  MUST be one of the seven pipeline roles
  (`llm-routing-and-budget.md` §2) and every value 1..128 chars
  (else 400 `SCHEMA_INVALID`, no row); a subset of roles is valid.
  It persists in the additive guarded `strategies.role_models` TEXT
  column (raw JSON; `''` for legacy/no-override rows) and echoes
  back in strategy reads (`role_models` omitted when unset). Per-role
  resolution for agents happens in `GET /api/v1/agent/llm-config`
  (`platform-secrets.md` §API).
- **SP-3 (tenant resolution)** Identical semantics to
  `resolveMintTenant`: a tenant-bound user token creates in its OWN
  tenant only (a body `tenant_id` naming another tenant is 403
  `FORBIDDEN`); env-admin MUST name a tenant in the body (missing ⇒
  400 `INVALID_TENANT_ID`) and that tenant must exist (unknown ⇒ 404
  `UNKNOWN_TENANT`, the exact code `resolveMintTenant` already
  returns — env-admin already holds tenant-existence oracles via
  409 `TENANT_EXISTS` on create and 404 on tenant kill, so this
  adds none). Tenants are never deleted, so tenant existence cannot
  race a deletion.
- **SP-4 (identity + idempotency)** `strategy_id` is a
  server-generated UUIDv4. The body carries NO id field —
  client-chosen ids would add an existence oracle and a collision
  surface for zero benefit. Retry safety comes from a per-tenant
  name uniqueness rule instead: an existing strategy whose STORED
  name equals the new name after Go `strings.TrimSpace` on BOTH
  sides (full unicode whitespace — SQLite's `TRIM` strips 0x20
  only, which would let a tab-padded legacy row slip through) ⇒ 409
  `STRATEGY_NAME_TAKEN`, checked INSIDE the insert transaction.
  Name-taken WINS over the SP-4b cap: a timed-out retry of the
  create that filled the cap still gets the deterministic
  name-taken signal.
  Race-freedom rests on the store's single-connection invariant
  (`SetMaxOpenConns(1)`, the same invariant kill-epoch monotonicity
  cites in `multi-tenant-rbac.md` §Tenant kill-switch); any
  relaxation of that invariant requires `BEGIN IMMEDIATE` or an
  equivalent here. A partner script that re-POSTs after a timeout
  gets a deterministic 409, never a silent duplicate. No new
  index (rows predating this spec may hold duplicates; they are
  untouched). The check lives in a NEW store entry point
  (`CreateStrategyProvisioned`) used only by this route —
  `store.CreateStrategy`'s existing callers/tests keep their
  semantics. That entry point MUST itself reject any initial state
  other than `draft`/`paper` (defense in depth under SP-2's handler
  400 — the store refuses to mint a live-at-birth row even if a
  future handler regresses).
- **SP-4a (attribution)** The new entry point persists the creating
  principal: additive guarded
  `ALTER TABLE strategies ADD COLUMN created_by TEXT NOT NULL
  DEFAULT ''` (legacy rows read ''), written with `actorID(pr)`
  (`multi-tenant-rbac.md` §Audit identity: the bare `token_id` for
  DB principals, `env-admin` for the env class — this is the first
  multi-principal creation surface, so a leaked admin token's
  creations must be attributable to its token). `created_by`
  is audit-only for now: read endpoints do NOT expose it (no read
  schema change).
- **SP-4b (quota)** Per-tenant bound: counting INSIDE the same
  transaction, a tenant at or above the cap ⇒ 409
  `STRATEGY_LIMIT_REACHED`. Default cap 100;
  `CONTROLPLANE_MAX_STRATEGIES_PER_TENANT` overrides (integer
  ≥ 1, anything else refuses to start — fail-closed config style).
  Rationale: the safety monitor and platform-kill
  `DriveSafetyEffects` iterate every strategies row, so unbounded
  tenant-driven growth is a tenant-credential attack on safety-tick
  latency. The cap counts ALL rows of the tenant regardless of
  lifecycle state.
- **SP-5 (response)** 200 (matching every other create in this API —
  tenants and token mint both return 200; the API has no 201
  precedent) with the full strategies row (`strategy_id`,
  `tenant_id`, `name`, `lifecycle_state`, `created_at`,
  `updated_at`); `created_at = updated_at = now`, RFC3339 UTC via
  the server clock. No token material is minted or returned — agent
  credentials remain the job of `POST /api/v1/tokens` (separation
  kept deliberately: a strategy without a token is reachable by its
  tenant owner, unlike a fresh tenant, so the tenant-creation
  bundling precedent does NOT apply).
- **SP-6 (not gated)** Creation is not trading intent: the DS-2
  restore gate does NOT block this route, and no standing kill
  (strategy/tenant/platform) blocks it. The global per-token POST
  rate limit applies as on every POST; the per-strategy proposal
  limiter is untouched.
- **SP-7 (audit)** The strategies row itself — including the SP-4a
  `created_by` actor — plus, for `paper`, the LC-16a bootstrap
  transition row is the audit; `created_at` pins when. This
  satisfies `multi-tenant-rbac.md` §Security rules ("every new
  mutating endpoint appends its audit row"): the created row IS the
  audit row, written before any side effect (there are none).
- **SP-8 (matrix doc)** The normative permission matrix in
  `multi-tenant-rbac.md` gains the row for this route (owner ✅,
  admin ✅, trader ✗, viewer ✗, agent ✗, env-admin ✅) — the
  matrix table and `Permissions()` must not drift.

## Test obligations

- Matrix: route present; agent/operator/read tokens 403/401 per the
  standard permission-matrix sweep (`TestRBACMatrix` picks the new
  row up automatically — verify it does).
- Tenant-bound owner and admin create in own tenant (200, row
  persisted, draft default); trader/viewer 403; foreign body
  tenant_id 403; env-admin with tenant_id 200, without 400,
  unknown tenant 404 `UNKNOWN_TENANT`.
- `lifecycle_state: paper` ⇒ 200 AND exactly one draft→paper
  bootstrap transition row (actor `bootstrap`, role `system`);
  `draft` ⇒ zero transition rows; `live_l2` ⇒ 400 and NO row.
- name: missing/empty/whitespace-only/129-byte ⇒ 400, no row;
  control chars / bidi overrides / invalid UTF-8 ⇒ 400, no row.
- Duplicate `(tenant_id, name)` ⇒ 409 `STRATEGY_NAME_TAKEN`, no
  second row; legacy stored `" alpha "` collides with new `"alpha"`
  (TRIM on the stored side); the SAME name in a DIFFERENT tenant ⇒
  200. Concurrent duplicate POSTs: exactly one 200, one 409.
- Store level: `CreateStrategyProvisioned` rejects `live_*` and
  every non-draft/paper state (no row); persists `created_by`;
  legacy rows read `created_by = ''`; read endpoints do not
  expose the column.
- Quota: tenant at the cap ⇒ 409 `STRATEGY_LIMIT_REACHED`, no row;
  cap env parse fail-closed (0, negative, junk ⇒ refuse to start).
- Unknown body field ⇒ 400 `SCHEMA_INVALID`.
- Restore-gate interplay: with the gate engaged, creation still 200
  (SP-6) while proposals stay 503.

## Drill (soak)

Create a tenant, create a `paper` strategy in it via this endpoint
(using the returned owner token — proves tenant self-service), mint
its agent token, POST one heartbeat with that token (proves the
minted credential binds to the new id end-to-end), and read the row
back via `GET /api/v1/strategies/{id}` with the env READ token
(env-admin has no read tier — pinned 403).

## RUNBOOK

Amend the EXISTING §7 "Tenant onboarding and token rotation" —
insert the strategy-creation step between tenant creation and agent
token mint, and finish the chain with the §10 scheduler instance
(`/etc/alphamintx/scheduler-<strategy_id>.env` + `systemctl enable
--now alphamintx-scheduler@<strategy_id>`) — zero manual DB edits.
No new section: one onboarding procedure, one place to drift.
