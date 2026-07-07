# Platform secrets

Encrypted at-rest storage for the two platform credentials — the Binance
exchange keys and the LLM gateway key — plus the admin-console listings.
Normative; implemented in `internal/vault`, `internal/store/secrets.go`,
and `internal/api/secrets.go`.

## Threat model

- **Secrets at rest are encrypted.** `platform_secrets` stores only
  AES-256-GCM ciphertext (`payload_ciphertext`) and non-secret display
  metadata (`meta_json`). A copied database file yields no credentials.
- **The key lives OUTSIDE the DB.** The 32-byte master key sits in its own
  file (§Key file), never in SQLite — a DB backup or exfiltration alone is
  insufficient to decrypt.
- **Plane boundary.** The binance payload NEVER leaves the process: no API
  route returns it — `LoadBinanceSecret` (api package) decrypts it
  in-process for startup wiring only. The llm payload crosses the API
  boundary through EXACTLY ONE route, `GET /api/v1/agent/llm-config`,
  restricted to agent tokens. Every other response carries metadata only
  (`api_key_last4`, never the key or secret).
- **No secret logging.** Key material and plaintext never appear in logs
  or error messages (the vault errors are static strings).

## Vault (AES-256-GCM)

`internal/vault`: `Seal(plaintext) -> base64(nonce || ciphertext)` with a
fresh random 12-byte nonce per call; `Open` reverses it and fails on any
tamper (GCM authentication). Key = 32 random bytes.

### Key file

- Path: `CONTROLPLANE_SECRETS_KEY_FILE` if set, else `<db path>.secrets.key`
  next to the SQLite file.
- Auto-generated (hex-encoded, `O_EXCL`) with 0600 permissions on first use.
- An existing file with ANY group/other permission bits REFUSES to start.
- Malformed or wrong-length contents refuse to start.

## Schema (additive)

```sql
platform_secrets (kind TEXT PRIMARY KEY CHECK (kind IN ('binance','llm')),
  payload_ciphertext TEXT NOT NULL, meta_json TEXT NOT NULL,
  updated_at TEXT NOT NULL, updated_by TEXT NOT NULL);   -- mutable snapshot
secret_events (event_id TEXT PRIMARY KEY, kind TEXT NOT NULL,
  action TEXT NOT NULL CHECK (action IN ('set','rotated')),
  actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);    -- append-only audit
```

`platform_secrets` is a mutable snapshot (the `strategy_state` exemption);
every upsert appends a `secret_events` row in the SAME transaction —
`'set'` the first time a kind appears, `'rotated'` thereafter (invariant 7).

## API

The three admin routes are **env-admin ONLY** (the env admin token and
`platform_admin` web sessions — NOT tenant owners in v1). All four routes
answer `503 VAULT_UNAVAILABLE` when no vault is wired.

1. `GET /api/v1/platform/secrets` — 200 `{"items":[secretMetaView...]}`
   sorted by kind; empty vault ⇒ `{"items":[]}`.
   `secretMetaView = {"kind","meta":{...decoded meta_json},"updated_at",
   "updated_by"}`; for `binance` meta = `{"env","api_key_last4"}`, for
   `llm` meta = `{"base_url","api_key_last4","timeout_seconds",
   "trader_model","default_model","role_models"?}`.
2. `POST /api/v1/platform/secrets/binance` — body
   `{"env":"testnet"|"prod","api_key","api_secret"}` (strict decode; env
   pinned; key/secret ≤ 256 chars). Leaving BOTH `api_key` and
   `api_secret` empty keeps the stored pair (edit env without re-entering
   credentials; 400 when nothing is stored, or when exactly one of the
   two is empty). Seals payload `{"api_key","api_secret"}`; meta
   `{"env","api_key_last4"}`. 200 `{"secret":secretMetaView}` — the
   plaintext is NEVER echoed.
3. `POST /api/v1/platform/secrets/llm` — body
   `{"base_url","api_key","timeout_seconds"?,"trader_model"?,
   "default_model"?,"role_models"?}` (`base_url` http(s) URL; `api_key`
   ≤ 256; `timeout_seconds` optional integer 1..600, default 30; models
   1..128 chars, defaults `gpt-4o` / `gpt-4o-mini`; `role_models` an
   optional role→model map — keys pinned to the seven pipeline roles of
   `llm-routing-and-budget.md` §2, values 1..128 chars, a subset of
   roles is valid). An empty `api_key` keeps the stored key (edit
   base_url / timeout / models without re-entering it; 400 when nothing
   is stored). Seals payload `{"base_url","api_key","timeout_seconds",
   "trader_model","default_model","role_models"?}`; meta the same minus
   the key (last4 only). 200 `{"secret":secretMetaView}`.
4. `GET /api/v1/agent/llm-config` — **agent tokens ONLY** (any agent: the
   route has no `{id}` segment, so the strategy-scope guard does not
   apply). 200 `{"base_url","api_key","timeout_seconds","trader_model",
   "default_model","role_models"}`; vault empty ⇒ 404 `NOT_CONFIGURED`.
   `role_models` is the fully resolved 7-role map: the
   `trader_model`/`default_model` defaults, overlaid by the platform
   secret's `role_models`, overlaid by the requesting token's strategy
   `role_models` (`strategy-provisioning.md` SP-2; a token scoped to a
   strategy with no row gets the platform view). This is the ONE
   endpoint that returns a secret value, and it returns ONLY the llm
   payload.

Validation failures are 400 `SCHEMA_INVALID`. All routes register through
the `api.Permissions()` matrix (multi-tenant-rbac.md §Permission matrix),
`Requires: ""` — always registered, 503 when `api.Config.Vault` is nil.

### Startup wiring

`api.Config` gains `Vault SecretVault` (`Seal`/`Open`; `*vault.Vault`
satisfies it); `cmd/controlplane serve` constructs it from the key file
before building the server. `api.LoadBinanceSecret(st, v) (env, key,
secret, ok, err)` is the exported startup helper: `ok=false` when no vault
or no stored secret; main.go consumption is a later task.

## Admin listings

Both **env-admin ONLY**, registered through the matrix:

- `GET /api/v1/tenants` — 200
  `{"items":[{"tenant_id","name","created_at"}...]}` ordered `created_at`
  then `tenant_id` (`Store.ListTenants`).
- `GET /api/v1/users` — 200 `{"items":[{"user_id","email","tenant_id"
  (nullable),"role","created_at","disabled":bool}...]}` ordered
  `created_at` then `user_id` (`Store.ListUsers`). `password_hash` NEVER
  crosses the store read boundary (no-read-back invariant).

## Store surface

`UpsertPlatformSecret` (one tx: snapshot upsert + audit insert),
`GetPlatformSecret` (ciphertext + metadata, `ErrNotFound`),
`ListPlatformSecretMeta` (metadata ONLY — the ciphertext column is never
selected), `ListTenants`, `ListUsers` — all named in the
`TestStoreSurfaceIsAppendOnly` allowlist; `secret_events` has no mutators.

## Test requirements

- Vault: seal/open round-trip, tamper/foreign-key rejection, key-file
  generation perms (0600), loose-permission and malformed-key refusal.
- Store: upsert/rotate audit trail (`set` then `rotated`, same-tx),
  metadata listing without ciphertext, listing order.
- API: admin set+list both kinds (meta shapes, correct `api_key_last4`);
  non-admin principals 403 (matrix-driven); agent llm-config 404 before /
  200 after set; a shape test pinning that NO response carries the
  binance `api_secret` or full `api_key`; 503 on every route with a nil
  vault; tenants/users listing shapes + 403 for non-admin.
