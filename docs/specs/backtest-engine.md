# Spec: Backtest engine and lookahead detection (Phase 2)

Defines the Phase 2 backtest engine: `control-plane/internal/backtest`
(historical kline replay through the identical Risk Gate + paper OMS path)
and the agent-plane offline backtest emitter. Companion to
`docs/specs/market-data.md` (fill model v2, staleness, trigger sweeps),
`docs/specs/persistence-and-api.md` (store), and `docs/PLAN.md` Phase 2.

## Goals, non-goals, and the parity invariant

- Goal: the two PLAN.md Phase 2 exit criteria this spec serves — "backtest vs
  paper parity" and "lookahead check passes on all shipped strategy
  templates".
- **Parity invariant (normative).** Backtest ≡ **candle-driven replay-paper**.
  The backtest replays the identical code path — `riskgate.Evaluate`, the
  paper OMS fill model v2, the agent pipeline — over a deterministic
  candle-derived tick stream; same dataset + same seeds ⇒ byte-identical
  decision sequence: proposals, verdicts (including ordered reason codes),
  orders, and fills, with decimal strings compared verbatim.
- **NORMATIVE LIMITATION.** Live paper runs sweep triggers on every observed
  mark (per-trade ticks, `docs/specs/market-data.md`); a backtest sees only
  the candle-derived sub-tick path (§Clock model), so intra-candle fidelity
  is approximated, not exact. The parity exit criterion is therefore defined
  against a candle-driven replay-paper run, NOT against a live paper soak.
- Non-goals: live data; multi-tenant and billing (separate Phase 2 tracks);
  latency/queue modeling (fill model v1 models none — inherited); partial
  fills; funding rates (§Open questions).

## Two-stage recorded-proposal architecture

| Stage | Plane | Component | Input → output |
|---|---|---|---|
| 1 — emit | agent-plane | `alphamintx_agent_plane.backtest.emit` (offline CLI) | dataset file → `proposals.jsonl` |
| 2 — replay | control-plane | `internal/backtest` `BacktestService` | dataset file + `proposals.jsonl` → verdicts/orders/fills in `backtest.db` |

- **Stage 1** runs the REAL pipeline (StubLLM or recorded transcripts,
  §Determinism) once per candle close over the dataset window, emitting one
  proposal line per decision tick.
- **`proposals.jsonl` shape (normative, as implemented).** Line 1 is a meta
  line — `{"kind":"backtest_meta","strategy_id","symbol","interval",
  "dataset_sha256","seed","mask_level","window","scenario"}` — so the
  artifact alone identifies and reproduces its stage-1 run; the replay
  fails closed on any mismatch with the runspec or the dataset actually
  read. Every following line is `{"tick_number","snapshot_sha256",
  "proposal"}`: an explicit `tick_number` (the alignment key for every
  comparison in §Lookahead — no line-index alignment) plus the sha256 of
  the exact snapshot string the pipeline consumed — M2's pass condition
  compares against this recorded value, so stage 1 MUST persist it.
- **Tick range.** Grid tick `t` of a candle = `(open_time −
  first_open_time) / d`; gapped indices own ticks with no candle. The
  emitter writes exactly one line per grid tick `t ∈ [0, N−1]`, ascending,
  gaps included; the snapshot at tick t covers the trailing
  `min(window, closed-candles-at-t)` rows. The replay enforces the exact
  sequence and `created_at = T(t) + 1s` per line.
- **Open-loop assumption (normative).** The pipeline input is exactly the
  snapshot fields (`market_data`/`news`/`fundamentals`) derived from the
  dataset. The emitter MUST NOT read control-plane state — positions, equity,
  verdicts, budget. Tripwire: the backtest emitter makes NO HTTP calls at
  all (dataset file in, `proposals.jsonl` out). If a future strategy
  conditions on portfolio state, the two-stage design must be revisited
  (closed-loop mode, §Open questions) — recorded as an explicit constraint.
- **Stage 2** replays `proposals.jsonl` through a **HistoricalFeed** — it
  emits `marketdata.Tick` sub-ticks per §Clock model, reusing `Tick` and
  `Store` — into riskgate + paper OMS, exactly like the e2e harness
  (`internal/e2e` is the pattern).
- **Data.** A `KlineStore` interface + Binance REST kline fetcher (respecting
  the existing endpoint-override env vars, market-data.md §Endpoint
  overrides) live in the CONTROL-plane; a `backtestctl fetch` step
  materializes a canonical dataset file (JSONL, decimal strings, sha256
  recorded) consumed by BOTH stages — identical bytes on both planes without
  granting agent-plane DB access. The plane boundary is preserved: read-only
  market-data REST is already permitted by the boundary gate
  (`scripts/check_plane_boundary.py`).
- **Canonical dataset form (normative, as implemented).** One kline per
  line, compact JSON, pinned key order `symbol, interval, open_time, open,
  high, low, close, volume`; canonical symbol (`BTC/USDT` — the venue form
  exists only inside the HTTP query); OHLCV decimal strings VERBATIM from
  the venue; single symbol+interval per file; strictly ascending,
  grid-aligned `open_time` (gaps legal). `close_time` is NEVER stored —
  both planes derive `close_time = open_time + d` (not Binance's
  `open+d−1ms`), one rule, two implementations. Legal intervals are a
  closed 1m…1d whitelist duplicated verbatim on both planes (Go
  `time.ParseDuration` cannot parse `1d`; an open-ended parser would
  diverge cross-plane). `dataset_sha256` = sha256 of the entire file bytes,
  recomputed independently by the emitter and the replay.
- **Persistence isolation (normative).** The klines cache, `backtest_runs`,
  and `backtest_records` live in a SEPARATE SQLite DB file (`backtest.db`),
  NOT in `control.db`: backtests never pollute live/paper state, paper-gate
  counters, or the live WAL. This is a carve-out from
  `docs/specs/persistence-and-api.md`'s one-DB-file rule (amended there).

## Clock model and sub-tick pumping (normative)

- **One decision tick per candle close.** `tick_number` increments once per
  candle; the virtual decision time is `T = close_time`. The gate evaluates
  at `T + 1s` (mirroring the e2e loop's evaluation clock, `loop.go`) with
  proposal `created_at = T + 1s` — pinned so `PROPOSAL_STALE` derives from
  the virtual clock, never the wall clock.
- **Sub-tick pumping.** Each candle pumps FOUR mark writes into the Store
  before the next decision, in a pinned order:

| Candle | Sub-tick order |
|---|---|
| bullish (`close ≥ open`) | open → low → high → close |
| bearish (`close < open`) | open → high → low → close |

- Every sub-tick write runs the full trigger sweep, identical to the live
  per-write sweeps (`ProcessTick` after EVERY Store write, market-data.md
  §Fill model v2). `Tick.TS` advances monotonically within the candle,
  pinned: `open_time`, `open_time + ¼d`, `open_time + ½d`, `close_time`
  (d = candle duration). The existing per-sweep pessimism rule — the stop
  wins over the TP when both trigger in the same sub-tick — is inherited
  unchanged.
- **Candle-grouped pumping (normative).** `close_time(t) == open_time(t+1)`
  — the boundary timestamps COLLIDE, and the Store keeps only the latest
  tick per symbol. A time-threshold drain (`advance(T+1s)`, the e2e
  tickPump shape) would therefore overwrite candle t's close with candle
  t+1's open BEFORE the tick-t decision — literal lookahead. The replay
  MUST pump candle t's four sub-ticks, take decision t, and only then
  touch candle t+1 (regression-tested: the decision-t mark equals
  close(t), never open(t+1)).
- **`max_age_seconds` bound (normative).** The runspec MUST satisfy
  `1 ≤ max_age_seconds ≤ interval`: the healthy mark age at decision time
  is 1s by construction and a gapped candle leaves the last mark
  `interval + 1s` old, so any larger bound would silently disable gap
  staleness detection.
- **Sub-ticks are mark writes within a tick, NOT separate `tick_number`s**:
  tick-number alignment is one per candle by definition.
- **Staleness.** Mark age at decision time is ~1 s by construction, so
  `max_age_seconds` is satisfied identically to a healthy live feed. A test
  case with a deliberately gapped dataset is REQUIRED, proving the
  fail-closed `MARK_PRICE_UNAVAILABLE` rejects still occur. Scope note
  (same gate code as live): the zero-mark guard binds MARKET entries;
  a LIMIT entry at a gapped tick is approved and rests unfilled in the
  OMS — placing a limit order needs no current mark, and no sub-ticks
  arrive to fill it until the next present candle.
- **UTC daily rollover.** Daily-loss / circuit-breaker day boundaries derive
  from virtual `T` crossing 00:00 UTC (the same `utcDate` convention as
  runstate); replay never reads the wall clock.

## Lookahead-bias detection (mask levels)

| Level | Meaning | Pass condition |
|---|---|---|
| M0 | Control: index-masked slicing (rows with grid index ≤ t, trailing window). | Baseline for comparison. |
| M1 | PHYSICAL truncation: the emitter windows a literally truncated row list (`rows[:last_present_index_at_t + 1]`) — a structurally different mechanism from M0's masking, so a masking bug in M0's path cannot hide in both. | Emitted files identical to M0 (proposal lines byte-equal; metas differ only in `mask_level`). `check --mode m1`. |
| M2 | Independent recheck: for each proposal line, rebuild the snapshot string from the dataset sliced at that tick using a SEPARATE slicing implementation (deliberately duplicated inline in the checker — it MUST NOT share the emitter's slicing code, or a common lookahead bug would recompute the same wrong hash and pass blindly). | Recomputed sha256 equals the recorded `snapshot_sha256` at every tick. `check --mode m2`. |

- Detection target: the deterministic snapshot/dataset-slicing code, at O(n)
  WITHOUT re-running the pipeline (a naive per-tick full-pipeline re-run is
  O(n²) and vacuous under StubLLM — explicitly rejected).
- `mask_level ∈ {M0, M1}` is the EMITTER mode recorded in the meta line and
  `backtest_runs`; M2 is a checker mode over an existing emit, never an
  emitter mode — an `M2` run row cannot exist.
- **NORMATIVE LIMITATION.** The LLM tier is out of scope for automated
  lookahead detection: StubLLM ignores prompts (canned per role/symbol) and
  recorded transcripts replay recorded outputs, so masking cannot alter LLM
  behavior. Lookahead through a real LLM's reasoning is not mechanically
  detectable here; the exit criterion is scoped to the deterministic tier.
  This qualifies README invariant 7 ("backtests free of lookahead bias"):
  the guarantee covers everything the platform feeds the pipeline — the
  deterministic snapshot/dataset path — not the model's own priors.
- **Migration note (normative — DONE 2026-07-04, all three parts):**
  1. The live provider (`scheduler/snapshot.py`) previously included the
     FORMING candle — `closes[-1]` was a mid-candle last price. It now
     fetches one extra kline and DROPS the forming row, so both live and
     backtest snapshots share the closed-candle basis M1 needs.
  2. `tests/test_scheduler_snapshot.py` was updated to pin the new
     closed-candle semantics.
  3. This changed only the PIPELINE INPUT basis — the gate/OMS mark basis
     (per-trade live marks) is unchanged, so it does not by itself close
     the intra-candle fidelity gap (§Goals).

## Persistence and lifecycle

Schema sketch (normative SHAPE, not final DDL; all prices/volumes are TEXT
decimal strings, ADR-0003):

```sql
CREATE TABLE klines (symbol TEXT NOT NULL, interval TEXT NOT NULL,  -- append-only fetch cache
  open_time INTEGER NOT NULL, open TEXT NOT NULL, high TEXT NOT NULL, low TEXT NOT NULL,
  close TEXT NOT NULL, volume TEXT NOT NULL, PRIMARY KEY (symbol, interval, open_time));
CREATE TABLE backtest_runs (backtest_id TEXT PRIMARY KEY, strategy_id TEXT NOT NULL,
  config_hash TEXT NOT NULL, dataset_sha256 TEXT NOT NULL, code_version TEXT NOT NULL,
  seed INTEGER NOT NULL, mask_level TEXT NOT NULL, status TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE backtest_records (backtest_id TEXT NOT NULL REFERENCES backtest_runs,
  seq INTEGER NOT NULL, kind TEXT NOT NULL, payload_json TEXT NOT NULL,  -- shaped like e2e records.jsonl rows
  PRIMARY KEY (backtest_id, seq));                                       -- append-only
```

- **NO backtest lifecycle state.** A backtest is an operation on a strategy
  config snapshot, not a strategy mode: `docs/specs/strategy-lifecycle.md`
  is untouched, and paper-gate counters are unaffected by construction
  (separate DB).
- **`escalate` verdicts.** Recorded as-is with NO approval resolution: an
  escalate is a non-execution in backtest v1 (no operator in the loop).
  Pinned so parity comparisons are well-defined.

## Determinism (normative)

- All backtest ids are uuid5 in a NEW `NAMESPACE_BACKTEST` (uuid5 of
  `uuid.NAMESPACE_URL` + the pinned URL `https://alphamintx.dev/backtest`),
  derived the same way as the e2e `NAMESPACE_E2E`. `backtest_id =
  uuid5(NAMESPACE_BACKTEST, "backtest/<dataset_sha256>/<config_hash>/<seed>/
  <mask_level>")` with `config_hash` = sha256 of the runspec file bytes.
- The `seed` is recorded in `backtest_runs`; same dataset (`dataset_sha256`)
  + same seed ⇒ byte-identical output. The seed is an id-salt only: StubLLM
  is canned and nothing in the pipeline is stochastic.
- `code_version` is the VCS revision from Go build info, or `"unknown"`
  (test binaries and `go run` builds do not stamp VCS metadata).
- Decimal-as-string end to end (ADR-0003; `docs/specs/proposal-contract.md`
  §Decimal-as-string regex); never float64.
- LLM tier: StubLLM or recorded transcripts ONLY — a backtest makes no live
  LLM calls. The recorded-transcript replay format is an open question,
  bounded by the 256 KiB trace transcript cap
  (`docs/specs/persistence-and-api.md` §Trace ingestion).
- Golden-style regression: a committed backtest golden + `make
  backtest-check` (double-run + diff, patterned on `make e2e-check`).
  Regenerate only via the sanctioned make target; never hand-edit goldens.

## Open questions (recorded, not silent)

- Funding rates (perps) — spot-basis first; futures backtests deferred.
- Partial fills — excluded to preserve parity with fill model v1
  (all-or-nothing fills).
- Fee tiers — fixed from the limits/`fill_model` config; no volume-tier fee
  schedule.
- Kline gaps / exchange-downtime semantics beyond the fail-closed
  gapped-dataset test case (§Clock model).
- Recorded-LLM-transcript replay format: keying, storage, cap (≤ 256 KiB per
  trace envelope).
- `max_orders_per_minute`'s sliding 60 s window under a virtual candle
  clock: the replay leaves `EntryOrdersInLastMinute` (and the pending-entry
  counters) at 0 — exactly like the e2e harness — so the check is inert in
  backtest v1 at ANY interval; wiring it from OMS order timestamps is
  deferred.
- Closed-loop mode, if strategies ever condition on portfolio state
  (§Two-stage recorded-proposal architecture).
