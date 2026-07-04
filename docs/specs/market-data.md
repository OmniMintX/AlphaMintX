# Spec: Market data feed and fill model v2 (Phase 1)

Defines `control-plane/internal/marketdata` (live + replay mark-price feeds)
and the paper-OMS fill model v2 (fees + slippage). The gate stays fail-closed:
no fresh mark ⇒ reject, never guess. Companion to `docs/specs/risk-limits.md`
(staleness, watchdog, reduce-only) and `docs/specs/proposal-contract.md`.

## Package contract (`control-plane/internal/marketdata`)

```go
type Tick struct {
    Symbol string          // canonical BASE/QUOTE, e.g. "BTC/USDT"
    Mark   decimal.Decimal // mark price (futures) or last trade (spot)
    Last   decimal.Decimal
    TS     time.Time       // exchange event time (live) / tick time (replay)
}

type Feed interface {
    // Subscribe streams ticks for the given canonical symbols until ctx is
    // done. The channel is closed on ctx cancellation or fatal error.
    Subscribe(ctx context.Context, symbols []string) (<-chan Tick, error)
}
```

- **Store** is the last-tick cache consumed at proposal evaluation:
  `Mark(symbol) (price decimal.Decimal, ts time.Time, ok bool)`. It retains
  only the latest tick per symbol; the writer goroutine drains the Feed
  channel into it.
- **Staleness rule (normative).** `max_age_seconds` is a REQUIRED config
  value (no default). A mark is usable iff `ok` AND
  `−5 ≤ now − ts ≤ max_age_seconds`: the −5 s lower bound is the future-skew
  tolerance — a tick timestamped more than 5 s in the future is NOT usable.
  The control-plane clock is authoritative. Under ReplayFeed, "now" is the
  current tick time (`clock_start + index × tick_seconds`) and
  `max_age_seconds` comes from the runspec (§Determinism); replay never
  reads the wall clock.
- **Gate wiring (fail-closed).** The proposal-evaluation call site resolves
  the mark from the Store before `Evaluate`. Stale or missing ⇒ it MUST pass
  a zero `MarkPrice` in `RuntimeState`; the existing zero-price guard in
  `riskgate/evaluate.go` then rejects market-entry opens
  `MARK_PRICE_UNAVAILABLE`. The gate MUST NOT be given the last known stale
  price, and the Store MUST NOT fabricate or extrapolate prices. When
  staleness caused the zero mark, the verdict `limits_snapshot` MUST also
  record the stale mark's `ts`, its age, and `max_age_seconds` (OPTIONAL
  snapshot fields, added with a contract minor-version bump) so
  `MARK_PRICE_UNAVAILABLE` verdicts are reproducible from the verdict alone.
- **Exits fail-closed (normative, OMS-side guard).** Exits — `close`,
  flatten (breaker/kill/human), and protective-stop fills — REQUIRE a
  usable mark. With a stale or missing mark the exit is **QUEUED, never
  filled**: the position stays open, the stop stays armed, and the flatten
  retries on the next fresh tick. The paper OMS MUST refuse to book ANY
  fill at a price ≤ 0 or at a stale price; this guard lives on the OMS fill
  path itself, not only in the gate. "Exits always possible"
  (`docs/specs/risk-limits.md` step 3) resumes the instant data returns,
  and PnL, `daily_loss`, and the circuit breaker are never corrupted by a
  zero-price fill.

Symbol mapping lives in `marketdata/symbol.go` as stateless functions:
AlphaMintX `BTC/USDT` ⇔ Binance `BTCUSDT` (WS stream names lowercase). All
package boundaries speak canonical `BASE/QUOTE`; venue forms never leak out.

## Feed implementations

### BinanceFeed (live paper/live runs)

| Concern | Rule |
|---|---|
| Streams / price basis | Spot: `wss://stream.binance.com:9443/stream?streams=<sym>@trade` — **spot mark = last trade price**, the same basis as the REST bootstrap (`ticker/price` is also last-trade), so reconnects never mix quote-derived and trade-derived marks. USDT-M futures: `wss://fstream.binance.com/stream?streams=<sym>@markPrice@1s` (venue mark price). One combined-stream connection per venue. |
| REST bootstrap | On start and on every reconnect, snapshot via spot `GET /api/v3/ticker/price` / futures `GET /fapi/v1/premiumIndex` and write the snapshot into the Store before trusting WS ticks. |
| Keepalive | Server pings every 20 s; the client MUST answer pong within 60 s or the server drops the connection. |
| Silent-connection watchdog | Track the last-message timestamp; no message for > 60 s ⇒ treat the connection as dead and reconnect. A connected-but-silent socket MUST NOT keep stale marks alive (staleness rule still applies). |
| Reconnect | Exponential backoff 100 ms → 30 s cap, with jitter; reset on a successful (re-)subscribe. Respect Binance connection limits (~300 attempts / 5 min / IP; ≤ 5 inbound msg/s). |
| Re-snapshot | Every reconnect MUST re-run the REST bootstrap before resuming WS consumption (ticks missed while disconnected are otherwise silently lost). |

### ReplayFeed (e2e / CI)

- Sources ticks from the runspec `marks` series; one tick per symbol per
  index, `TS = clock_start + index × tick_seconds` (index-based clock).
- MUST NOT read the wall clock, sleep, or open any network connection: the
  e2e run stays byte-deterministic and offline.
- Exhausted series repeat their last element (matches `markAt` fallback);
  unknown symbols yield no tick ⇒ zero mark ⇒ `MARK_PRICE_UNAVAILABLE`.

## Fill model v2 (paper OMS, normative)

Phase 0 filled at mark with no fee or slippage. Phase 1 replaces that with:

| Order | Trigger | Fill price | Fee |
|---|---|---|---|
| Market entry | immediate at submission mark | buy: `mark × (1 + market_slippage_bps/10000)`; sell: `mark × (1 − market_slippage_bps/10000)` | taker |
| Limit entry, **marketable at placement** (buy: `mark ≤ limit`; sell: `mark ≥ limit` at submission) | fills IMMEDIATELY on submission | `limit_price` exactly | **taker** — a crossing limit executes as a taker on a real venue; maker pricing here would let strategies pocket the taker−maker spread on the paper track record |
| Limit entry, resting | order rested ≥ 1 tick; mark crosses limit (buy: `mark ≤ limit`; sell: `mark ≥ limit`), checked on every tick | `limit_price` exactly — no slippage | maker |
| Protective stop (SL) | FIRST mark at/through `stop_price` (long stop: `mark ≤ stop`; short stop: `mark ≥ stop`), checked on every tick | stop-market: the **observed triggering mark** ± directional slippage on the closing side — on a gap through the stop this is the gapped mark, never `stop_price` itself | taker |
| Take-profit | limit semantics: mark crosses TP | `take_profit` exactly — no slippage | maker |
| Flatten / breaker / kill | immediate at current usable mark (stale/missing ⇒ queued, §Exits fail-closed) | market semantics: mark ± directional slippage | taker |

- **Trigger checks are tick-granular and deterministically ordered.** Every
  Store write — per-tick marks (`e2e/execute.go`, the live Store writer) AND
  the REST bootstrap snapshot on start/reconnect — MUST run stop/limit/TP
  trigger checks for every open order. Symbols are processed in
  lexicographic order; within a symbol, protective stops fire before
  take-profits before entry limits; ties within a class break by `order_id`
  lexicographic. When one tick crosses both a position's SL and TP, the
  stop fills (pessimistic = safe) and the sibling TP is canceled once the
  position is closed. No intra-tick price path is modeled. In serve mode this
  component is the **feed writer**: it runs the OMS trigger sweep
  (`ProcessTick`) after EVERY mark-store write, including the REST bootstrap
  snapshot on start/reconnect (the behavior above is already normative; this
  names the component).
- Protective stops simulate exchange-resident orders (invariant 2): they
  trigger from the feed inside the OMS, never from an LLM loop, and remain
  reduce-only per `docs/specs/risk-limits.md`.

### Fill arithmetic (normative order of operations)

1. **Effective size** = `clipped_size_quote` when the verdict is `clip`,
   else the proposal `size_quote`. The notional cap binds the NOTIONAL at
   fill price (step 4), not a pre-slippage figure.
2. **Fill price** per the table above (directional slippage for market
   semantics; shorts are symmetric — sells and short entries receive
   `mark × (1 − slip)`).
3. **`qty_base`** = effective size ÷ fill price, rounded per §Rounding. If
   the rounded quantity would push notional above the effective size / cap,
   round DOWN one unit at the 8th decimal instead: rounding MUST NOT
   increase notional above the cap / `clipped_size_quote`
   (`docs/specs/risk-limits.md` §OMS execution rules).
4. **`notional`** = fill price × `qty_base`, in quote currency — ≤ effective
   size ≤ cap by construction, slippage included.
5. **`fee`** = `qty_base × fill price × fee_bps/10000` (taker/maker per the
   table), rounded per §Rounding, recorded **separately** on the fill
   (`fills.fee_quote`). Fees are NEVER baked into the fill price or
   `entry_price` (fee-EXCLUSIVE; `docs/specs/persistence-and-api.md`):
   realized PnL = exit proceeds − entry cost − Σ fees (entry + exit), so no
   fee is counted twice. Positions accumulate fees paid
   (`positions.fees_quote`). This feeds the `daily_loss` definition
   ("including fees") in `docs/specs/risk-limits.md` and closes MS-15.
6. **Protective stop quantity** = the filled `qty_base` — the stop covers
   exactly the clipped/filled quantity, never the pre-clip request.

### `fill_model` configuration (normative)

| Field | Unit / type |
|---|---|
| `market_slippage_bps` | decimal string, basis points |
| `taker_fee_bps` | decimal string, basis points |
| `maker_fee_bps` | decimal string, basis points |

- All three fields are REQUIRED decimal strings (`docs/specs/proposal-contract.md`
  §Decimal-as-string); parse with `shopspring/decimal`, never float64.
- Source: the runspec `fill_model` object for e2e; strategy/platform config
  (same struct) for live paper runs. There are NO hidden defaults: a missing
  `fill_model` or missing field is a startup/parse error, not a silent zero.

### Rounding (normative)

Fill prices, quantities, fees, and realized-PnL amounts are rounded **half
away from zero to 8 decimal places** — stated once, applied everywhere
including signed PnL: round the absolute value, then reapply the sign. The
Go implementation is shopspring `Decimal.Round(8)`, which is
half-away-from-zero and equals half-up ONLY for non-negative values;
banker's rounding MUST NOT be used. Intermediate arithmetic is
unrounded; rounding happens once per persisted value. Venue tick/step
normalization from `docs/specs/risk-limits.md` §OMS execution rules applies
after this rule where venue filters are modeled.

## Determinism (normative)

- The fill model changes fills and PnL, so committed e2e goldens change.
  `make e2e-golden` is the ONLY sanctioned regeneration path; review the
  resulting diff before committing (never hand-edit goldens).
- `fill_model` AND `max_age_seconds` MUST be explicit in `e2e/runspec.json`
  — reproducibility requires the parameters in the run's single source of
  truth.
- ReplayFeed MUST NOT read the wall clock; all replay time derives from
  `clock_start` + tick index. `make e2e-check` continues to assert
  byte-identical output across two emits and two replays, offline.

## Out of scope (Phase 1, explicit)

- **Funding rates** — futures PnL is slightly optimistic; Phase 2.
- **Depth/impact slippage** — slippage is flat-bps; no order-book model.
- **Partial fills** — every fill is all-or-nothing at one price.
- **Spot-vs-futures venue selection** — Phase 1 simulates one venue; venue
  routing and per-venue fee schedules are deferred.
- **Intra-outage price paths** — ticks missed while disconnected are not
  reconstructed; triggers evaluate only observed marks. A stop gapped
  through during an outage fills at the first observed post-outage mark
  ± slippage (per the fill table), not at `stop_price`.
