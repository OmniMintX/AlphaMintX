# Live OMS and Reconciler (Phase 3)

Normative. Defines the live Binance **spot** OMS behind the existing
Submitter seam, the write-ahead intent journal, idempotent placement, the
Reconciler (exchange-is-truth: startup reconcile, orphan handling, fill-gap
backfill), the user-data stream, live safety rails, API surface, DDL, and
test obligations. Companion to `docs/specs/risk-limits.md` (kill/breaker/
watchdog semantics the live OMS must implement), `docs/specs/
persistence-and-api.md` (store conventions), `docs/specs/market-data.md`
(symbol conventions, Binance client patterns), `docs/specs/
strategy-lifecycle.md` (state guards), `docs/specs/multi-tenant-rbac.md`
(permission matrix), and `docs/PLAN.md` Phase 3.

## Goals and non-goals

- Goal: make PLAN.md Phase 3 exit criterion 1 provable — "Reconciler proves
  exchange-is-truth: orphan adoption and gap detection tested against
  testnet outage/restart scenarios" — and define the live-OMS surfaces that
  criteria 2 and 3 (kill-switch drills, circuit-breaker/watchdog drills)
  execute against (§Safety-engine integration).
- The **paper OMS remains the default**. Live is an explicit opt-in
  (§Config). A paper deployment's behavior, schema reads, and API surface
  are unchanged: all DDL is additive, all new routes are unregistered
  unless live mode is wired.
- Non-goals (v1): margin/futures (spot only); multiple exchange accounts
  or venues; order modification (cancel + new order only); WebSocket order
  placement (REST placement only; WS is read-only user-data); OCO orders;
  cross-account netting; Postgres.

## Placement and seams

- `control-plane/internal/exchange` — venue adapter: typed Binance spot
  REST + user-data-stream client (patterns from
  `internal/marketdata/binance.go`), no store access, no business policy.
- `control-plane/internal/oms/live` — the live OMS: implements the same
  seam the paper `omsbridge.Bridge` fills today (`api.Submitter` plus the
  cancel/flatten operations the safety engines call), owns the intent
  journal, FSM, and Reconciler. It writes the SAME `orders`, `fills`,
  `positions`, `strategy_state` rows through the SAME accounting code path
  as `omsbridge` (invariant 10); `main.go` wires exactly one of
  paper-bridge or live OMS from config.
- The gate, approval flow, proposal contract, and risk limits are
  UNCHANGED: the live OMS receives only gate-approved actions.

### Exchange adapter interface (normative shape)

```go
type Exchange interface {
    ExchangeInfo(ctx, venueSymbols []string) (Filters, error)
    PlaceOrder(ctx, req PlaceRequest) (Ack, error)
    QueryOrder(ctx, venueSymbol, origClientOrderID string) (OrderState, error)
    CancelOrder(ctx, venueSymbol, origClientOrderID string) (OrderState, error)
    OpenOrders(ctx, venueSymbol string) ([]OrderState, error)
    MyTrades(ctx, venueSymbol string, fromID int64, startTime time.Time, limit int) ([]Trade, error)
    Balances(ctx) ([]Balance, error)   // free/locked per asset (flatten sizing, R6 sanity)
    NewListenKey(ctx) (string, error)
    KeepAliveListenKey(ctx, key string) error
    StreamUserData(ctx, key string) (<-chan UserEvent, error)
    ServerTime(ctx) (time.Time, error)
}
```

- Every adapter error is classified into exactly one of FOUR classes:
  **DefiniteReject** (venue processed and refused: HTTP 4xx with a
  Binance error code, e.g. -2010 insufficient balance, -1013 filter
  failure), **NotFound** (-2013 "Order does not exist"; for cancel
  operations -2011 "Unknown order sent" maps here too — the order is
  already gone), **Throttled** (HTTP 429/418 and 5xx maintenance
  responses carrying `Retry-After`: the request was NOT executed; NOT
  terminal, the attempt id is NOT poisoned — the sender resends the SAME
  attempt id after the `Retry-After` interval; a throttle response
  WITHOUT `Retry-After` degrades to Ambiguous and is query-resolved), or
  **Ambiguous** (timeout, connection reset, other HTTP 5xx, -1007).
  Callers MUST branch on this classification; an unclassifiable error is
  Ambiguous (fail toward the safe path).
- `MyTrades` paging handoff: when `fromID > 0`, page by
  `fromId=lastTradeID+1` (exact). When `fromID == 0` (cold start), the
  FIRST page is fetched by `startTime`; every subsequent page switches
  to `fromId = lastTradeID + 1`. startTime paging is bootstrap only —
  fromId paging is the correctness mechanism.
- Redaction (normative): adapter error values, log lines, and EVERY
  `oms_recon_events.details_json` payload MUST NOT contain request URLs,
  query strings, headers, or signatures — only `{operation, venue error
  code, venue error msg}`. The multi-tenant-rbac.md no-read-back
  invariant applies to venue credentials and signed material.
- Symbols: canonical `BASE/QUOTE` (market-data.md) everywhere in the
  store and API; the adapter alone maps to/from venue symbols
  (`BTC/USDT` ↔ `BTCUSDT`), persisted as `venue_symbol` where dedup
  requires it.
- Clock skew: requests carry `recvWindow` (default 5000 ms). On -1021 the
  adapter refreshes its offset from `ServerTime` and retries once; a
  second -1021 is DefiniteReject.
- Backoff for Ambiguous-class retries: exponential with jitter, base
  500 ms, cap 30 s. On Throttled, ALL requests stop for the `Retry-After`
  interval.

## Order identity and idempotent placement

### clientOrderId namespace

Every order the platform places carries
`newClientOrderId = "amx1-" + token + "-" + attempt`, where `token` is 22
chars of unpadded base64url over 16 CSPRNG bytes (the **intent token**)
and `attempt` is a single digit `0..9`. Total length 29 ≤ Binance's 36;
charset is within Binance's `^[\.A-Z\:/a-z0-9_-]{1,36}$`. The prefix
`amx1-` is the platform namespace: an order at the venue is **ours iff**
its `clientOrderId` matches `^amx1-[0-9A-Za-z_-]{22}-[0-9]$`. Operational
rule (normative): one exchange (sub)account per control-plane deployment;
nothing else may place `amx1-*` orders on that account.

### Write-ahead intent journal and send claims

`order_intents` (§Tables) holds one row per placement attempt. Attempt
rows are never deleted and their identity/parameter columns never
change; the ONLY mutable fields are the claim columns `claimed_at` and
`claim_revoked_at`, written exclusively through the named `Record*`
mutators of §Store-surface amendment. The submit path for every order
(proposal-driven and safety-engine-driven alike) is:

1. **Preflight** (in order): startup reconcile completed (else reject
   `RECONCILE_PENDING`, bookkeeping per §Safety-engine integration);
   kill-epoch re-check against the store — a stale epoch drops the order
   exactly as risk-limits.md requires; filters loaded and unexpired
   (else `FILTER_UNAVAILABLE`); normalize per §Filters.
2. **Journal.** In ONE store transaction: INSERT the `orders` row with
   `status='pending_new'` and `client_order_id` = the attempt-0 id, and
   INSERT the `order_intents` attempt-0 row. `orders.submitted_at` is
   the JOURNAL time — by definition pre-send. Commit BEFORE any HTTP.
3. **Claim, re-verify, send.** Immediately before the HTTP the sender
   CLAIMS the attempt (`RecordIntentClaim` sets `claimed_at`; it FAILS
   if the attempt is already claimed or revoked). After the claim
   commits, the sender re-reads the kill epoch and the claim:
   - Kill epoch newer than the order's ⇒ abandon in the SAME
     transaction: `status='rejected'`, append `intent_resolved_absent`
     with reason `kill_epoch_stale`. The id was never sent so it is not
     poisoned, but it is retired anyway (never reused) for simplicity.
   - Claim revoked (`claim_revoked_at` set, §Reconciler R2 /
     CancelOpenEntries) ⇒ MUST NOT transmit; the revoker owns the
     intent's resolution.
   - Otherwise call `PlaceOrder`.
4. **Ack (2xx):** record `exchange_order_id` (`RecordExchangeAck`),
   advance status per §FSM from the ack's `status` field.
5. **DefiniteReject:** set `status='rejected'`; append an
   `oms_recon_events` row `kind='intent_resolved_absent'` carrying the
   venue error code in `details_json`. The rejection surfaces to callers
   (approval path, preflight reasons) as `EXCHANGE_REJECTED`. The intent
   is terminal; no retry for -2010/-1013-class rejections (the condition
   is real).
6. **Throttled:** NOT terminal, NOT poisoned. Resend the SAME attempt id
   after the `Retry-After` interval (the claim stays held); no
   `Retry-After` ⇒ treat as Ambiguous (step 7).
7. **Ambiguous:** NEVER blindly resend. Enter the resolution loop:
   `QueryOrder(origClientOrderID)` with backoff (500 ms base, ≤ 5 tries).
   - Found ⇒ treat as ack (step 4), append `intent_resolved_present`.
   - NotFound while the venue is answering ⇒ the attempt id is
     **poisoned**: it MUST never be sent again (the venue may still
     materialize it later). Append `intent_resolved_absent`. If the order
     is still wanted (kill re-check repeats) and `attempt < 9`: INSERT an
     attempt+1 `order_intents` row, update `orders.client_order_id` to
     the new id (`RecordIntentAttempt`), and run steps 3+ with the NEW
     id. `attempt = 9` exhausted ⇒ `status='rejected'`, alert.
   - Venue unreachable ⇒ leave `pending_new`; the Reconciler owns it
     (startup step R2 / periodic audit).
8. **Poisoned late arrival.** If a poisoned id later appears at the venue
   (via sweep or stream), it is intent-attributed via its journal row:
   still open ⇒ cancel it REGARDLESS of ENTRY/PROTECTIVE shape (it
   duplicates a still-attributed order), append `orphan_canceled` with
   reason `poisoned_late`; already (partly) filled ⇒ the fills are real
   and ours — book them to the intent's strategy through the normal fill
   path and append `duplicate_exposure` (operator alert; risk reduction
   is an operator action, the OMS never un-books a venue fill).

`orders.client_order_id` always holds the LATEST attempt id; the full
attempt history lives in `order_intents`.

**In-flight exclusion is transactional, not clock-based.** The
Reconciler may resolve-absent ONLY attempts that are unclaimed, or
claimed attempts whose claim it has FIRST revoked
(`RecordIntentClaimRevoked`). Revocation and the sender's pre-transmit
re-check (step 3) are writes/reads on the same row under SQLite's
single-writer serialization, so exactly one of {transmit,
resolve-absent} wins — a backoff-delayed send can never race a
resolution into an untracked venue order. If a crash window still lets
a late send reach the venue after revocation (HTTP already on the
wire), the NEXT reconcile run detects the now-present venue order via
its journal row and cancels it, appending `orphan_canceled` with reason
`late_send_detected`. `CancelOpenEntries` (§Safety-engine integration)
hitting a claimed-but-unsent `pending_new` intent REVOKES the claim, so
the send cannot happen after the cancel.

## Order state machine (FSM)

Persisted statuses and ranks:

| rank | status | Binance mapping |
|---|---|---|
| 0 | `pending_new` | (local only: journaled, venue outcome unknown) |
| 1 | `open` | `NEW` |
| 2 | `partially_filled` | `PARTIALLY_FILLED` |
| 3 | `filled` | `FILLED` |
| 3 | `canceled` | `CANCELED` |
| 3 | `rejected` | `REJECTED` (or local definite-absent resolution) |
| 3 | `expired` | `EXPIRED`, `EXPIRED_IN_MATCH` (and §Venue epochs previously-acked-NotFound) |

- `PENDING_CANCEL` maps to the order's CURRENT non-terminal status (rank
  unchanged): the order is still open awaiting a terminal report; it is
  never a regression and never terminalizes by itself.
- Transitions are MONOTONE in rank: an update with rank lower than the
  current status is dropped (and appended as `kind='stale_update_dropped'`
  if the payload differs). Rank-3 statuses are terminal and immutable.
- Paper statuses (`open`,`filled`,`canceled`) embed unchanged at ranks
  1/3; paper rows never carry `pending_new` or a `client_order_id`.
- Fill bookkeeping on the row: `orders.fill_price` = the VWAP of the
  order's booked fills; `orders.filled_at` = the timestamp of its LAST
  booked fill. Pinned by the paper-parity scenario (§Test obligations).
- Executed quantity is DERIVED: `SUM(fills.qty_base)` over the order's
  fills (append-only, deduped). It is never stored mutably and never
  decreases. A venue report whose `cumulative executedQty` exceeds the
  local sum triggers a targeted `MyTrades` backfill for that order's
  symbol (§Gap detection); it never writes a synthetic fill.
- Cancel-then-fill race: a `canceled` order that gains a late backfilled
  fill KEEPS status `canceled`; the fill is booked normally (venue truth:
  partial execution before cancel). Append `kind='fill_after_terminal'`.

## Reconciler

The Reconciler is the exchange-is-truth engine. It runs as: (a) the
MANDATORY startup sequence — the live OMS rejects every submission with
`RECONCILE_PENDING` until R7 completes; (b) a periodic audit every
`reconcile_interval_seconds` (default 60); (c) on-demand after every
stream reconnect and via `POST /api/v1/oms/recon/run`. All runs execute
the same steps; one run at a time. Internal triggers (periodic, stream
reconnect) COALESCE into the in-progress run; only the HTTP POST
surfaces 409 `RECON_RUNNING`.

**Reconcile-before-trade blocks ALL sends** — including kill-switch,
circuit-breaker, and SL-contingency flattens: venue truth (positions,
balances, filters) is a sizing precondition for the safety orders
themselves. Pending re-drivable safety effects (kill_breaker_events
re-drive keys, unmet protective obligations) execute IMMEDIATELY once
reconciliation completes, in risk-limits.md order. If the startup
reconcile fails `recon_failure_alert_threshold` (default 3) consecutive
attempts while re-drivable safety effects are pending, the OMS appends
`recon_blocked_safety` + operator alert and KEEPS failing closed. This
is a deliberate, bounded exception to risk-limits.md's exit-exemption
principle (exits are never blocked by risk LIMITS, but they do wait for
venue truth), and it is stated here normatively.

**REST weight budget.** Periodic-run operations are budgeted: per run,
one `ExchangeInfo` (when refresh is due), one `OpenOrders` per
configured symbol, a bounded `QueryOrder` set (only unresolved
intents/absences), and `MyTrades` pages as needed. The default
`reconcile_interval_seconds` MUST keep the steady-state budget under
Binance's 1200 weight/min with ample placement headroom.

**R1 — Filters.** Load `ExchangeInfo` for all configured symbols iff the
loaded filters are due for refresh (`filter_refresh_seconds`). STARTUP
failure aborts the run (`run_failed` event) and the OMS stays closed
(`FILTER_UNAVAILABLE`) — fail closed, never trade unfiltered. A
PERIODIC-run transient failure does NOT close an open OMS while the
loaded filters are unexpired; only startup failure or filter expiry
fails closed.

**R2 — Intent resolution.** For every `orders` row in `pending_new`:
the latest attempt is either UNCLAIMED (crash before send, or journaled
by a sender that died) — resolve it directly — or CLAIMED, in which
case the Reconciler FIRST revokes the claim (`RecordIntentClaimRevoked`;
§In-flight exclusion) and then resolves; a claimed attempt whose sender
is provably mid-flight this instant simply loses the claim race next
run. Resolution: `QueryOrder` by the LATEST attempt `client_order_id`.
Found ⇒ adopt: set `exchange_order_id`, advance FSM, append
`intent_resolved_present`. NotFound ⇒ `rejected`, append
`intent_resolved_absent` (never-acked absence; id retired). Ambiguous ⇒
leave for the next run.

**R3 — Open-order sweep.** `OpenOrders` per symbol; classify each venue
open order:
- `client_order_id` matches a local order ⇒ sync (advance FSM, record
  `exchange_order_id` if missing).
- In-namespace with NO matching local `orders.client_order_id`: resolve
  against `order_intents` FIRST. Intent row found ⇒ poisoned-late /
  late-send handling (§Write-ahead intent journal step 8): cancel
  REGARDLESS of ENTRY/PROTECTIVE shape — it duplicates a
  still-attributed order — and book any fills per the
  duplicate-exposure rules; a revoked-claim late send is canceled with
  reason `late_send_detected`. Only INTENT-LESS ids (possible only if
  the DB regressed, e.g. restore from backup) are **unattributable
  orphans**: PROTECTIVE-shaped (type `STOP_LOSS`/`STOP_LOSS_LIMIT`/
  `TAKE_PROFIT`/`TAKE_PROFIT_LIMIT`) ⇒ LEAVE OPEN at the venue
  (invariant: never cancel a protective), append
  `orphan_protective_left` + operator alert; NOT adopted into local
  books (no strategy attribution exists). ENTRY-shaped (all other
  types) ⇒ cancel at the venue, append `orphan_canceled` with reason
  `unattributable`.
- Out-of-namespace ⇒ append `foreign_order_ignored` (first sighting per
  order id per run); NEVER cancel or adopt — the account may be shared
  with a human operator, and touching foreign orders is out of scope.

**R4 — Absence check.** For every local non-terminal order (rank 1–2,
plus `pending_new` handled in R2) NOT present in R3's sweep:
`QueryOrder`. Terminal at venue ⇒ terminalize locally (FSM), append
`order_terminalized`; if `executedQty > 0`, schedule R5 for its symbol.
NotFound splits on ack history (§Venue epochs):
- NEVER-ACKED (`exchange_order_id` IS NULL) ⇒ `rejected` +
  `intent_resolved_absent` (the venue never had it).
- PREVIOUSLY ACKED (`exchange_order_id` known) ⇒ venue-reset alarm:
  append `venue_reset`, mark the order `status='expired'` with the
  reason in the event details — NEVER a quiet reject — and transition
  the OMS to RECONCILE_PENDING per §Venue epochs.
Ambiguous ⇒ leave for the next run.

**R5 — Gap detection / fill backfill.** Per symbol: watermark
`W = MAX(fills.exchange_trade_id)` over fills of the CURRENT
`venue_epoch` (§Venue epochs) joined to orders of that canonical symbol
(NULL ⇒ cold start: first `MyTrades` page by `startTime` = the current
epoch's `venue_epochs.started_at` — for epoch 0 that is the
deployment's first live start, predating any placement — then switch
to fromId paging per the adapter handoff rule). Page
`MyTrades(fromID=W+1, limit=1000)` until exhausted. For each trade:
- Attribute via `orderId`/`clientOrderId` ⇒ local order ⇒ strategy.
  In-namespace but no local order (poisoned-late fill, R3 case) ⇒
  attribute via the `order_intents` row for that `client_order_id`
  (§Write-ahead intent journal step 8). Out-of-namespace ⇒ skip (not
  ours).
- INSERT the fill with `(venue_epoch, venue_symbol, exchange_trade_id)`
  and a locally minted UUIDv4 `fill_id` (stream and backfill alike); the
  partial unique index makes replays no-ops (`INSERT OR IGNORE`
  semantics). The fill INSERT and its accounting application are ONE
  transaction, conditional on the dedup INSERT affecting a row. A newly
  inserted fill flows through the IDENTICAL accounting path as a paper
  fill: fee-exclusive weighted-average entry, realized PnL net of ALL
  fees, `positions` + `strategy_state` updates (risk-limits.md
  Definitions). Append `fill_backfilled` per inserted fill.
- Fee conversion to `fee_quote`: `commissionAsset == quote` ⇒ verbatim
  decimal string; `== base` ⇒ `commission × that trade's price`
  (shopspring/decimal, no float); anything else (e.g. BNB) ⇒ convert at
  the current mark if fresh, append `commission_asset_anomaly` + alert.
  NO fresh mark ⇒ **deferred fee application**: the fill row INSERTS
  immediately (dedup intact) and position QUANTITY updates immediately
  (qty is fee-independent), but the fee-dependent accounting
  (`fees_quote`, realized PnL, `strategy_state`) is DEFERRED via a
  persisted `pending_fill_fees` row (§Tables) retried on every recon run
  until a fresh mark converts it (`RecordFeeConverted`), appending
  `fee_conversion_applied`. NO fee is ever silently zero. Operational
  MUST: disable BNB-fee discount on the account (see also §Config).
- Watermark advances implicitly (it is derived); it is monotone
  non-decreasing by construction within an epoch.

**R6 — Cross-checks.** The cumulative-quantity audit compares PER VENUE
ORDER: venue cumulative `executedQty` vs `SUM(fills.qty_base)` joined
through `order_intents` by attempt (each attempt id is its own venue
order; `orders.exchange_order_id` holds only the LATEST attempt's ack —
earlier attempts' venue ids live in `order_intents` and the event
trail). Mismatch after R5 ⇒ append `cum_qty_mismatch` + alert (venue is
truth; the gap will close on the next run or indicates a dedup defect);
repeated `cum_qty_mismatch` is SUPPRESSED for orders already flagged
`duplicate_exposure`. Balance sanity: free+locked base/quote vs the sum
of local positions; drift ⇒ `balance_drift` event (WARNING only — spot
balances are account-global and may legitimately include foreign
activity; positions remain fill-derived, invariant 1).

**R7 — Completion.** Append `run_completed` with counters
`{intents_resolved, orphans_adopted, orphans_canceled, fills_backfilled,
mismatches}` in `details_json`. Startup: the OMS now accepts
submissions, then immediately re-drives pending safety effects and
protective obligations (§Protective order lifecycle). Every run starts
by appending `run_started`. Journal-then-act (invariant 16) applies to
DESTRUCTIVE actions — cancels and flatten submissions append their event
row BEFORE the side effect executes; purely observational events
(`stale_update_dropped`, `balance_drift`, sightings) may follow their
observation. `oms_recon_events.run_id` is the recon run's own UUID —
NOT a foreign key to the `runs` table (which belongs to agent runs).

"Orphan adoption" (PLAN exit criterion) = R2 adoption of a journaled
order the crash left unacked, plus R3/R5 adoption of poisoned-late
orders via their intent rows. "Gap detection" = R5 watermark backfill.

## Venue epochs (testnet resets, restored accounts)

Binance testnet wipes orders/balances periodically; a prod account could
likewise be replaced. The OMS models this as a **venue epoch** — an
integer, default 0, stamped on every fill row.

- **Persistence**: the append-only `venue_epochs` table (§Tables) is
  the epoch's home. Inserting a row IS the epoch transition: epoch 0 is
  inserted at the live OMS's first start (`reason='initial'`); every
  later row is inserted only by operator acceptance
  (`reason='venue_reset_accepted'`). Current epoch =
  `MAX(venue_epochs.venue_epoch)`; the current epoch's `started_at` is
  the cold-start `startTime` bootstrap for R5. A freshly bumped epoch
  with zero fills therefore survives restarts without re-alarming.
- **Detection** (any of): a PREVIOUSLY ACKED order returns NotFound
  (R4); trade-id discontinuity — observed on a stream `executionReport`
  whose trade id is BELOW the current watermark yet absent locally, or
  on a cold-start `startTime` page returning such ids (R5's
  `fromId=W+1` paging by construction cannot observe them); gross
  balance discontinuity. On detection: append `venue_reset` (kind),
  transition to RECONCILE_PENDING, and REFUSE all sends — including
  safety flattens — until an operator acknowledges.
- **Operator acknowledgment**: `POST /api/v1/oms/recon/run` with
  `{"accept_venue_reset": true}` (env-admin only). This INSERTs the
  next `venue_epochs` row, after which the startup-grade reconcile runs
  against the new venue world.
- **On epoch bump**: fill dedup and the MAX watermark are per
  `(venue_epoch, venue_symbol)` — old trade ids cannot mask new ones,
  and the same `exchange_trade_id` re-issued by a reset venue cannot be
  dropped as a duplicate. Local positions are NOT auto-zeroed: the venue
  no longer backs them, but zeroing books is an accounting decision —
  the operator flattens/adjusts explicitly (the recon status surfaces
  the discrepancy until then).
- clientOrderId reuse across a reset (the venue forgot poisoned ids) is
  harmless: attribution goes through `order_intents`, which never
  forgets, and epoch-scoped fill identity keeps the books exact.

## Protective order lifecycle (SL/TP)

The live OMS implements risk-limits.md's protective-stop rules exactly;
this section defines the live machinery.

- **Placement on fill.** On EVERY entry fill event (including each
  partial fill), the OMS places or resizes the protective SL — and the
  TP when the proposal carries one (`orders.take_profit`) — through the
  SAME journal path (§Write-ahead intent journal), `class='PROTECTIVE'`,
  `reduce_only=1`. Protective orders get identical idempotency,
  claims, and reconciliation as any other order.
- **Quantity tracking.** The SL quantity MUST track the entry's
  cumulative filled quantity: when filled qty grows (further partials),
  cancel the resting protective and place a replacement sized to the
  new cumulative qty (cancel+replace; there is no modify in v1). Append
  `protective_resized`.
- **Persisted deadline.** Each entry order with an unmet protective
  obligation carries a restart-safe row in `protective_obligations`
  (§Tables; mirrors the `pending_approvals` timer pattern):
  `due_at = the triggering fill's time + sl_placement_deadline_seconds`
  (default 30; the JSON tuning key for risk-limits.md's
  `sl_placement_deadline`, same default). EVERY fill that grows the
  cumulative quantity creates a NEW obligation row with a fresh
  `due_at` (per risk-limits.md: the deadline runs from ANY entry fill);
  satisfied rows are never reopened. An obligation is satisfied
  (`RecordProtectiveSatisfied`) when the protective order is ACKED at
  the correct cumulative size.
- **Deadline breach (SL).** Retry placement with backoff while the
  deadline runs; on breach of an `'sl'` obligation: contingency-flatten
  the FILLED quantity via the flatten path with
  `origin='sl_contingency'`, append `sl_deadline_contingency` +
  strategy-tier alert (risk-limits.md contingency rule).
- **Deadline breach (TP).** A `'tp'` obligation breaching its deadline
  NEVER flattens (the contingency rule is SL-specific): the OMS keeps
  retrying with backoff and appends `tp_deadline_missed` + operator
  alert; the position remains protected by its SL.
- **Startup re-arm.** After the startup reconcile completes (and never
  before — §Reconciler reconcile-before-trade), the OMS re-derives unmet protective
  obligations from `orders` (`stop_price`, `take_profit`) joined to
  booked fills: every filled-but-unprotected quantity gets its
  protective placed immediately (fresh deadline row) or, if placement
  cannot succeed, contingency-flattened. A crash between an entry fill
  and its SL placement therefore converges to protected-or-flat.

## User-data stream (continuous truth feed)

- `NewListenKey` at OMS start; `KeepAliveListenKey` every 30 min
  (`listen_key_keepalive_seconds`, default 1800); reconnect + NEW key +
  full reconcile run on: WS error/close, `listenKeyExpired` event, or
  SILENCE — no received frame of ANY kind (data frames AND server pings
  both count as liveness) for `ws_silence_timeout_seconds` (default
  300 — Binance's server ping interval is 3 min, so a shorter timeout
  would false-positive on quiet markets). Append `stream_reconnect`
  with the reason.
- `executionReport` events drive the FSM (§FSM mapping) and, when
  `x=TRADE`, insert fills through the same deduped path as R5 (trade id
  `t`, `venue_symbol` from `s`) — stream and backfill converge on the
  same rows, so replays and overlaps are no-ops.
- Events for out-of-namespace orders are ignored. Events for in-namespace
  ids with no local row follow §Write-ahead intent journal step 8.
- The stream is an OPTIMIZATION of latency, not a source of correctness:
  every state it delivers is also reachable via R2–R6. Losing it silently
  degrades to the periodic audit (which is why silence forces reconcile).

## Filters and normalization

- From `exchangeInfo` per symbol: `PRICE_FILTER` (tickSize, min/max),
  `LOT_SIZE` (stepSize, minQty, maxQty), `NOTIONAL`/`MIN_NOTIONAL`.
  Refresh every `filter_refresh_seconds` (default 86400) and on any
  -1013 DefiniteReject (stale-filter suspicion). Transient refresh
  failure keeps the previous filters until expiry (§Reconciler R1).
- Normalization (before journaling, step 1): prices rounded to the
  nearest tick toward the passive side (buy limit DOWN, sell limit UP;
  stops toward trigger safety); quantities rounded DOWN to stepSize.
  The ROUNDED values are what is journaled and persisted on the order.
- Cap preservation: if passive-side price rounding would push
  `qty × price` ABOVE the effective cap (`clipped_size_quote` /
  notional caps from the verdict), shave ONE quantity step so the cap
  holds after rounding.
- Post-rounding `qty < minQty` or `qty × price < minNotional` ⇒ reject
  `BELOW_MIN_NOTIONAL` (the EXISTING registry code — no new code for
  this case; recorded on the order as `rejected`, evented). Other filter
  violations (e.g. maxQty, price bounds) reject as `FILTER_REJECTED`.
  EXCEPTION: reduce-only safety flattens round DOWN and send if any
  sendable quantity remains, else event `flatten_dust` + alert (dust
  below minQty cannot be flattened on spot; operator handles it).
- Quantities/prices are `shopspring/decimal` end to end; JSON/DB
  decimal-as-string (ADR-0003; persistence-and-api.md).

## Safety-engine integration (exit criteria 2 and 3 surfaces)

The live OMS exposes to the kill/breaker/watchdog engines the SAME
operations the paper bridge provides, with identical semantics
(risk-limits.md §Kill-switch, §Circuit breaker, §Watchdog):

- `CancelOpenEntries(strategyID | all)` — cancel every non-terminal
  ENTRY-class order via `CancelOrder`; NotFound is success (already
  gone); a claimed-but-unsent `pending_new` intent has its claim REVOKED
  (§In-flight exclusion) so the send cannot follow the cancel; Ambiguous
  retries on the next reconcile pass (the kill_breaker_events re-drive
  makes this resumable).
- `Flatten(strategyID)` — MARKET orders through the FULL journal path
  with `origin='kill'` (`'breaker'`/`'watchdog'` respectively).
  **`reduce_only=1` is a LOCAL intent marker** — Binance spot has NO
  reduceOnly flag — enforced by OMS-side sizing:
  `flatten qty = min(local fill-derived position, venue free base
  balance at query time)`, quantized DOWN to stepSize (§Filters dust
  rule). If the venue balance is SHORT of the local position, append
  `flatten_short_balance` + operator alert and flatten what is
  available; the OMS never sells beyond the min().
- **Stops-after-flatten** (risk-limits.md kill order): the flatten-fill
  BOOKING path is the trigger — once cumulative flatten fills cover a
  position, the OMS cancels that position's resting protectives. The
  step is journaled and re-drivable via the `kill_breaker_events`
  re-drive keys across restart (a crash between flatten fill and
  protective cancel re-drives on startup, AFTER reconcile).
- Kill re-check: immediately before EVERY `PlaceOrder` send the OMS
  re-reads the max kill epoch; a newer epoch than the order's drops it
  (risk-limits.md, unchanged; §Write-ahead intent journal step 3 defines
  the `kill_epoch_stale` bookkeeping). The tenant-scope predicate of
  multi-tenant-rbac.md §Tenant kill-switch applies verbatim.
- Breaker: halt = stop submitting ENTRY orders (protectives and
  reduce-only continue). The breaker-active predicate is DERIVED, not
  stored: a `kill_breaker_events` row with `kind='breaker'` whose
  `recorded_at` date equals the current UTC day — 00:00 UTC auto-reset
  falls out of the derivation, matching the paper OMS.
- Auto-approved submissions arriving while RECONCILE_PENDING are DROPPED
  (never queued) and recorded in `rejected_submissions` with reason
  `SUBMIT_FAILED`/`RECONCILE_PENDING`; the L1 approval path already
  surfaces the block as `approved_but_blocked` preflight reasons.

## Config (normative)

| Env var | Meaning |
|---|---|
| `CONTROLPLANE_OMS_MODE` | `paper` (DEFAULT) or `live`. Anything else: refuse to start. |
| `CONTROLPLANE_BINANCE_ENV` | `testnet` (DEFAULT) or `prod`. |
| `CONTROLPLANE_BINANCE_API_KEY` / `_API_SECRET` | REQUIRED iff mode=live. Control-plane env ONLY (invariant 14). |
| `CONTROLPLANE_LIVE_PROD_ACK` | Must equal the literal `I-UNDERSTAND-THIS-TRADES-REAL-FUNDS` for env=prod; else refuse to start. Testnet ignores it. |
| `CONTROLPLANE_LIVE_OMS_TUNING` | Optional JSON: `{reconcile_interval_seconds:60, ws_silence_timeout_seconds:300, listen_key_keepalive_seconds:1800, filter_refresh_seconds:86400, recv_window_ms:5000, sl_placement_deadline_seconds:30, recon_failure_alert_threshold:3}`. Unknown fields rejected (`DisallowUnknownFields`, config.go pattern). |

- Live prod therefore requires THREE explicit settings (mode, env, ack) —
  no single typo can reach real funds (invariant 15).
- Base URLs: testnet `https://testnet.binance.vision` + its WS endpoint;
  prod `https://api.binance.com` / `wss://stream.binance.com:9443`.
  Hardcoded per env; NOT configurable (no URL-swap foot-gun).
- **Venue pairing** (normative): `CONTROLPLANE_BINANCE_ENV=prod` REQUIRES
  prod market data; testnet MAY — and is RECOMMENDED to — use prod
  market data (testnet books are thin). These OMS variables are distinct
  from the existing market-data settings
  (`CONTROLPLANE_BINANCE_MARKET`/`_REST_URL`/`_WS_URL`), which continue
  to govern market data only.
- `scripts/check_plane_boundary.py` MUST flag `CONTROLPLANE_BINANCE_*` as
  control-plane-only tokens (extend its deny-list for the agent plane).
- Operational MUST: disable the BNB fee discount on the trading account.
  If it is not disabled, the BNB/quote symbol MUST be included in
  `CONTROLPLANE_SYMBOLS` so fee conversion has a mark (§Reconciler R5).
- The intent-token source (CSPRNG) is INJECTABLE for tests: the
  fake-adapter harness seeds it deterministically so scenario tests are
  reproducible.
- Testnet operational notes the drills must tolerate: testnet resets
  periodically (balances/orders wiped — §Venue epochs) — the drill
  scripts create fresh state per run; testnet fills can be sparse —
  drills use marketable limit orders on liquid symbols to guarantee
  executions.

## API surface

| Method + path | Roles | Classes | Requires |
|---|---|---|---|
| `GET /api/v1/oms/recon/status` | viewer/trader/admin/owner | read, env-admin | live OMS wired |
| `POST /api/v1/oms/recon/run` | — | env-admin ONLY | live OMS wired |

- Registered FROM the `api.Permissions()` table with a new
  `Requires: requiresLiveOMS` value; in paper deployments the routes do
  not exist (404), preserving the Phase 1/2 RBAC matrix exactly.
- **Wiring seam** (normative): the API depends on a
  `ReconStatusProvider` interface in `api.Config` —
  `Status(tenantScope) (...)` and
  `TriggerRun(ctx, acceptVenueReset bool) error` — implemented by the
  live OMS and wired in `main.go`. The RBAC test environment MUST wire a
  fake provider so `TestRBACMatrix` exercises the routes and the matrix
  pin holds.
- `GET .../status` is TENANT-FILTERED (multi-tenant-rbac.md isolation
  rule): tenant principals receive only `{mode, venue_env, reconciled,
  last_run:{status, completed_at}}` plus THEIR OWN strategies'
  pending-intent and orphan counts. Account-level detail — watermarks
  `[{symbol, venue_epoch, exchange_trade_id}]`, global `pending_intents`,
  full run counters, venue epoch — is env-class (read/env-admin) only.
- `POST .../run` runs R1–R7 synchronously; 200 with the `run_completed`
  counters; 409 `RECON_RUNNING` if a run is in progress (internal
  triggers coalesce instead — §Reconciler). Body (optional):
  `{"accept_venue_reset": true}` acknowledges a detected venue reset and
  bumps the venue epoch (§Venue epochs); without it, a run during
  RECONCILE_PENDING-due-to-reset re-detects and re-reports. Deployer
  act: env-admin only, like the billing POSTs.
- Error codes (error-code registry additions): `RECONCILE_PENDING`
  (submission preflight; surfaces in `approved_but_blocked`
  preflight_reasons exactly like other OMS blocks), `FILTER_UNAVAILABLE`
  (preflight), `FILTER_REJECTED` (order rejected by non-notional filter
  violations; min-notional/min-qty reuse the EXISTING
  `BELOW_MIN_NOTIONAL`), `EXCHANGE_REJECTED` (DefiniteReject; venue code
  in details), `RECON_RUNNING` (409 on the POST). Order-level codes are
  recorded in `oms_recon_events.details_json` and order status, not new
  HTTP shapes.

## Tables (normative DDL)

```sql
CREATE TABLE IF NOT EXISTS order_intents (              -- write-ahead journal (see mutation rule below)
  client_order_id TEXT PRIMARY KEY,                     -- amx1-<token22>-<attempt>
  intent_token TEXT NOT NULL, attempt INTEGER NOT NULL,
  order_id TEXT NOT NULL REFERENCES orders,
  strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, venue_symbol TEXT NOT NULL,
  side TEXT NOT NULL, type TEXT NOT NULL, qty_base TEXT NOT NULL,
  limit_price TEXT, stop_price TEXT,
  origin TEXT NOT NULL, proposal_id TEXT, kill_epoch INTEGER NOT NULL,
  journaled_at TEXT NOT NULL,
  claimed_at TEXT, claim_revoked_at TEXT,               -- send-claim state (Record* mutators ONLY)
  UNIQUE (intent_token, attempt));
CREATE TABLE IF NOT EXISTS oms_recon_events (event_id TEXT PRIMARY KEY,  -- append-only recon audit
  kind TEXT NOT NULL CHECK (kind IN ('run_started','run_completed','run_failed',
    'intent_resolved_present','intent_resolved_absent','orphan_canceled',
    'orphan_protective_left','foreign_order_ignored','order_terminalized',
    'fill_backfilled','fill_after_terminal','stale_update_dropped',
    'cum_qty_mismatch','balance_drift','commission_asset_anomaly',
    'duplicate_exposure','flatten_dust','flatten_short_balance',
    'stream_reconnect','venue_reset','recon_blocked_safety',
    'protective_resized','sl_deadline_contingency','tp_deadline_missed',
    'fee_conversion_applied')),
  run_id TEXT, strategy_id TEXT, symbol TEXT,           -- run_id = recon-run UUID (NOT the runs table)
  client_order_id TEXT, exchange_order_id TEXT, exchange_trade_id INTEGER,
  details_json TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS protective_obligations (     -- restart-safe SL/TP deadline timers
  obligation_id TEXT PRIMARY KEY, entry_order_id TEXT NOT NULL REFERENCES orders,
  strategy_id TEXT NOT NULL, kind TEXT NOT NULL CHECK (kind IN ('sl','tp')),
  due_at TEXT NOT NULL, created_at TEXT NOT NULL,
  satisfied_at TEXT);                                   -- RecordProtectiveSatisfied ONLY
CREATE TABLE IF NOT EXISTS pending_fill_fees (          -- deferred fee conversions (R5)
  fill_id TEXT PRIMARY KEY REFERENCES fills,
  commission TEXT NOT NULL, commission_asset TEXT NOT NULL,
  recorded_at TEXT NOT NULL,
  converted_at TEXT);                                   -- RecordFeeConverted ONLY
CREATE TABLE IF NOT EXISTS venue_epochs (               -- append-only; row insertion IS the epoch transition
  venue_epoch INTEGER PRIMARY KEY,                      -- current epoch = MAX(venue_epoch)
  started_at TEXT NOT NULL,                             -- R5 cold-start startTime bootstrap
  reason TEXT NOT NULL CHECK (reason IN ('initial','venue_reset_accepted')),
  details_json TEXT NOT NULL);
```

Guarded ALTERs (existing tables; §Migration): `orders` gains
`client_order_id TEXT` and `exchange_order_id TEXT`; `fills` gains
`venue_symbol TEXT`, `exchange_trade_id INTEGER` (Binance trade id,
int64), and `venue_epoch INTEGER NOT NULL DEFAULT 0` (§Venue epochs).
Partial unique indexes (paper rows, all-NULL, are unaffected):

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_client_order_id
  ON orders (client_order_id) WHERE client_order_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_fills_venue_trade
  ON fills (venue_epoch, venue_symbol, exchange_trade_id)
  WHERE exchange_trade_id IS NOT NULL;
```

Row rules: money/qty columns are decimal strings VERBATIM from the venue
where the venue supplied them; timestamps RFC 3339 UTC `Z`;
`oms_recon_events` and `venue_epochs` are append-only (no UPDATE, no
DELETE); `order_intents` rows are never deleted and only the claim
columns mutate; `protective_obligations`/`pending_fill_fees` rows are
never deleted and only their resolution timestamp mutates;
`order_intents.origin` uses the existing `orders.origin` vocabulary; the
name `oms_recon_events` is deliberate — `reconciliation_runs` already
belongs to billing.

## Store-surface amendment (normative)

Amendment to persistence-and-api.md's orders row rule: live columns
mutate ONLY through NAMED Record-style mutators (the `Record*` prefix
survives `TestStoreSurfaceIsAppendOnly`'s ban on `Update*`/`Delete*`
method names). The COMPLETE list of new store methods the
implementation may add:

- `RecordIntentAttempt` — bumps `orders.client_order_id` to the new
  attempt id (and INSERTs the attempt row).
- `RecordExchangeAck` — sets `orders.exchange_order_id` on ack.
- `RecordOrderStatus` — advances `orders.status` per the §FSM; the
  mutator itself enforces monotone rank (a regressive write is a no-op
  returning the current status) and terminal immutability.
- `RecordIntentClaim` / `RecordIntentClaimRevoked` — the send-claim
  columns on `order_intents`.
- `RecordProtectiveSatisfied` — sets
  `protective_obligations.satisfied_at`.
- `RecordFeeConverted` — sets `pending_fill_fees.converted_at` and
  applies the deferred accounting.
- Plus INSERT-only writers (`InsertOrderIntent`, `AppendOMSReconEvent`,
  `InsertProtectiveObligation`, `InsertPendingFillFee`,
  `InsertVenueEpoch`) and read methods.

`TestStoreSurfaceIsAppendOnly`'s allowlist MUST be extended in the SAME
commit that adds each method; any store method outside this enumeration
is a spec violation.

## Migration note (normative)

Follows the multi-tenant-rbac.md §Migration mechanism exactly:

1. Append the five `CREATE TABLE IF NOT EXISTS` statements to
   `schemaDDL`.
2. After applying `schemaDDL`, `store.Open` inspects
   `PRAGMA table_info(orders)` / `(fills)` and runs the five
   `ALTER TABLE ... ADD COLUMN` statements iff each column is absent —
   idempotent on the single WAL connection.
3. THEN apply the two `CREATE UNIQUE INDEX IF NOT EXISTS` statements
   (they reference the ALTERed columns, so they run post-ALTER, not in
   `schemaDDL`).
4. NO data backfill: existing paper rows keep NULL in every new column
   (`fills.venue_epoch` defaults to 0) and are excluded from the partial
   indexes; an existing soak `control.db` opens and serves unchanged.

## Test obligations

Scenario matrix — every scenario is a deterministic test against a fake
`Exchange` adapter (scripted responses/faults) in `oms/live`:

| # | Scenario | Test |
|---|---|---|
| S1 | Crash after send, before ack; restart; venue has the order | `TestReconcile_IntentResolvedPresent` |
| S2 | Crash after journal; venue never received; restart | `TestReconcile_IntentResolvedAbsent` |
| S3 | Ambiguous timeout; query finds order; NO duplicate sent | `TestSubmit_AmbiguousResolvesPresent` |
| S4 | Ambiguous timeout; absent; poisoned id; retry attempt+1; late poisoned order appears open ⇒ canceled / filled ⇒ fills booked + `duplicate_exposure` | `TestSubmit_PoisonedLateArrivalOpen`, `_Filled` |
| S5 | Fills executed during WS outage; watermark backfill books each exactly once | `TestReconcile_GapBackfill` |
| S6 | Duplicate/replayed executionReport and R5 overlap | `TestStream_DuplicateFillNoop` |
| S7 | Partial fill, restart, remainder fills; cum-qty converges | `TestReconcile_PartialFillRestart` |
| S8 | Cancel acked, late fill arrives in backfill | `TestReconcile_CancelFillRace` |
| S9 | Unattributable in-namespace orphan: ENTRY-shaped canceled; PROTECTIVE-shaped left open + alert | `TestReconcile_OrphanEntryCanceled`, `_OrphanProtectiveLeftOpen` |
| S10 | Out-of-namespace order in sweep and stream | `TestReconcile_ForeignOrderIgnored` |
| S11 | Reconciler resolution races a claimed/delayed send: claim revoked ⇒ sender never transmits; OR late send slips out ⇒ next run detects via journal and cancels with reason `late_send_detected` | `TestReconcile_ClaimRevokeBlocksSend`, `_LateSendDetected` |
| S12 | Listen-key silence ⇒ reconnect ⇒ full reconcile | `TestStream_SilenceForcesReconcile` |
| S13 | Stale/regressive status update dropped; terminal immutable; `PENDING_CANCEL` is not a regression | `TestFSM_Monotone` |
| S14 | Kill epoch bumps between journal and send ⇒ order rejected with reason `kill_epoch_stale`; id retired, never sent | `TestSubmit_KillEpochRecheck` |
| S15 | Submission before startup reconcile completes ⇒ `RECONCILE_PENDING`; auto-approved drop recorded in `rejected_submissions` | `TestSubmit_ReconcilePending` |
| S16 | Live fill accounting equals paper accounting for the same fill sequence (entry price, fees, realized PnL, strategy_state, VWAP `fill_price`, `filled_at`) | `TestAccounting_PaperParity` |
| S17 | Throttled (429 with `Retry-After`) ⇒ SAME attempt id resent after the interval; exactly one venue order; id not poisoned | `TestSubmit_ThrottledSameIdResend` |
| S18 | Venue reset: previously ACKED order returns NotFound ⇒ `venue_reset`, RECONCILE_PENDING, sends refused; `accept_venue_reset=true` bumps the epoch; watermark/dedup re-namespace; positions NOT auto-zeroed | `TestReconcile_VenueResetDetect`, `_VenueResetAccept` |
| S19 | SL placement deadline breached ⇒ retries, then contingency flatten of the filled qty (`origin='sl_contingency'`) + `sl_deadline_contingency` | `TestProtective_DeadlineContingencyFlatten` |
| S20 | Entry partial fill grows ⇒ protective cancel+replace at new cumulative qty (`protective_resized`) | `TestProtective_PartialFillResize` |
| S21 | Crash between entry fill and SL placement ⇒ startup re-arms the obligation (or contingency-flattens) after reconcile | `TestProtective_RestartRearm` |
| S22 | Flatten with venue free balance < local position ⇒ min() sizing, `flatten_short_balance` + alert; never oversells | `TestFlatten_ShortBalance` |
| S23 | Non-base/quote commission without fresh mark ⇒ fill booked, qty applied, fee application deferred and converted on a later run (`fee_conversion_applied`); fee never silently zero | `TestFees_DeferredConversion` |

**Non-vacuous evidence for exit criterion 1 (normative).** A testnet
drill test `TestTestnetDrill_OutageRestart`, skipped unless
`CONTROLPLANE_BINANCE_API_KEY`+`_SECRET` are set with env=testnet, that:
(1) places marketable limit orders on a liquid testnet symbol; (2) kills
the OMS process between journal-commit and ack for at least one order
(fault injection hook) and while at least one fill is un-consumed;
(3) restarts; (4) asserts the startup run appended `run_completed` AND
≥1 `intent_resolved_present` (orphan adoption) AND ≥1 `fill_backfilled`
whose `exchange_trade_id` matches a REAL venue trade id (gap detection)
AND final `SUM(fills.qty_base)` per order equals the venue's
`executedQty`; then (5) restarts a SECOND time and asserts zero
duplicate fills and a watermark that resumes > 0. A run with zero
adopted intents or zero backfilled fills FAILS — the criterion cannot
be satisfied vacuously. RBAC: the two new routes join `TestRBACMatrix`
via the permissions table automatically (fake `ReconStatusProvider`
wired in the test env).

## Invariants

1. **Exchange-is-truth.** Venue state wins every conflict; local
   `orders`/`fills`/`positions` are a durable cache derived from venue
   facts. Append-only audit rows are never rewritten to match.
2. **Reconcile-before-trade.** No order is sent — proposal OR safety
   flatten — before the startup reconcile run completes; submissions
   preflight-fail `RECONCILE_PENDING`; pending safety effects re-drive
   immediately after completion (bounded exception to exit-exemption,
   §Reconciler).
3. **Journal-before-send.** The `order_intents` row and its `pending_new`
   `orders` row commit in one transaction BEFORE any placement HTTP.
4. Every placement carries a namespaced `newClientOrderId`; attempt ids
   are globally unique, never reused, and poisoned ids are never resent
   (Throttled resends of an un-poisoned id are the sole same-id resend).
5. Ambiguous outcomes are resolved by query (`origClientOrderId`), never
   by blind resend.
6. **Claims beat clocks.** In-flight exclusion is transactional: the
   Reconciler resolves-absent only unclaimed or revoked-claim attempts;
   a sender never transmits a revoked claim; any late send that slips a
   crash window is detected via the journal and canceled
   (`late_send_detected`).
7. The order FSM is monotone in rank; terminal statuses are immutable;
   derived executed quantity never decreases.
8. Fill identity is `(venue_epoch, venue_symbol, exchange_trade_id)` —
   NEVER (orderId, price, qty) heuristics; fill booking is idempotent
   across stream, backfill, and replays; fill insert and accounting
   application are one transaction.
9. Watermarks are monotone non-decreasing WITHIN a venue epoch and
   derived from persisted fills (restart-safe by construction); venue
   resets require explicit operator epoch acknowledgment before any
   further send.
10. Live fills flow through the IDENTICAL accounting path as paper fills
    (fee-exclusive entry, PnL net of all fees, strategy_state); the
    gate's risk math is uniform across modes; no fee is ever silently
    zero (deferred conversion, §Reconciler R5).
11. The Reconciler NEVER cancels an unattributable protective-shaped
    order and NEVER touches out-of-namespace orders; intent-attributed
    duplicates (poisoned-late, late-send) are canceled regardless of
    shape.
12. **Protected-or-flat.** Every filled entry quantity converges to
    protective coverage or contingency flatten (persisted deadline,
    startup re-arm, §Protective order lifecycle).
13. The kill epoch is re-checked immediately before every send;
    safety-engine orders use the same journal path (`origin`
    kill/breaker/watchdog/sl_contingency) as proposal orders; flatten
    sizing never exceeds min(local position, venue free balance).
14. Exchange API keys exist ONLY in the control-plane environment —
    never in the agent plane (`check_plane_boundary.py`), the DB, logs,
    or API responses; adapter errors and event payloads carry no URLs,
    headers, or signatures.
15. Testnet is the default venue; production requires three explicit
    settings including the ack literal (§Config).
16. Destructive actions (cancels, flatten submissions) append their
    `oms_recon_events` row BEFORE the side effect executes;
    observational events may follow their observation; runs are
    bracketed by `run_started`/`run_completed|run_failed`.
17. Paper mode is the default and behaviorally unchanged: additive
    schema only, live routes unregistered, paper rows carry NULL in
    every new column; live-column mutations go ONLY through the
    enumerated `Record*` mutators (§Store-surface amendment).
