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
- **`proposals.jsonl` line shape (normative).** Each line carries an explicit
  `tick_number` (the alignment key for every comparison in §Lookahead — no
  line-index alignment) plus the per-tick snapshot string the pipeline
  actually consumed (or its sha256): M2's pass condition compares against
  this recorded value, so stage 1 MUST persist it.
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
- **Sub-ticks are mark writes within a tick, NOT separate `tick_number`s**:
  tick-number alignment is one per candle by definition.
- **Staleness.** Mark age at decision time is ~1 s by construction, so
  `max_age_seconds` is satisfied identically to a healthy live feed. A test
  case with a deliberately gapped dataset is REQUIRED, proving the
  fail-closed `MARK_PRICE_UNAVAILABLE` rejects still occur.
- **UTC daily rollover.** Daily-loss / circuit-breaker day boundaries derive
  from virtual `T` crossing 00:00 UTC (the same `utcDate` convention as
  runstate); replay never reads the wall clock.

## Lookahead-bias detection (mask levels)

| Level | Meaning | Pass condition |
|---|---|---|
| M0 | Control: full dataset run. | Baseline for comparison. |
| M1 | Forming-candle mask: the snapshot builder consumes ONLY closed candles. | Per-tick comparison keys identical to M0. |
| M2 | Dataset-slice masking with snapshot-string equality. | For each decision tick t, the snapshot string built from the dataset sliced at t is byte-identical to the snapshot string the emitter actually used at t in the M0 run. |

- Detection target: the deterministic snapshot/dataset-slicing code. M2
  detects any deterministic-tier code path reading past the mask at O(n),
  WITHOUT re-running the pipeline (a naive per-tick full-pipeline re-run is
  O(n²) and vacuous under StubLLM — explicitly rejected).
- **Comparison keys (per tick):** `(tick_number, decision, ordered reason
  codes, clipped_size_quote, fill price/qty as decimal strings)`. Pass =
  identical across M0/M1; M2 pass = snapshot-string equality at every tick.
- **NORMATIVE LIMITATION.** The LLM tier is out of scope for automated
  lookahead detection: StubLLM ignores prompts (canned per role/symbol) and
  recorded transcripts replay recorded outputs, so masking cannot alter LLM
  behavior. Lookahead through a real LLM's reasoning is not mechanically
  detectable here; the exit criterion is scoped to the deterministic tier.
  This qualifies README invariant 7 ("backtests free of lookahead bias"):
  the guarantee covers everything the platform feeds the pipeline — the
  deterministic snapshot/dataset path — not the model's own priors.
- **Migration note (normative, all three parts):**
  1. Today's live provider (`scheduler/snapshot.py`) includes the FORMING
     candle — `closes[-1]` is effectively a mid-candle last price. Phase 2
     mandates a closed-candle basis for BOTH backtest snapshots and the live
     provider, so M1 parity is achievable.
  2. `tests/test_scheduler_snapshot.py` pins the current semantics and MUST
     be updated with that change.
  3. This changes only the PIPELINE INPUT basis — the gate/OMS mark basis
     (per-trade live marks) is unchanged, so this mandate does not by itself
     close the intra-candle fidelity gap (§Goals).

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

- All backtest ids are uuid5 in a NEW `NAMESPACE_BACKTEST`, derived the same
  way as the e2e `NAMESPACE_E2E` (uuid5 of `uuid.NAMESPACE_URL` + a pinned
  URL).
- The `seed` is recorded in `backtest_runs`; same dataset (`dataset_sha256`)
  + same seed ⇒ byte-identical output.
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
  clock: with 1 h candles it can never bind (one decision per hour). The
  check is KEPT, noted as inert at coarse intervals.
- Closed-loop mode, if strategies ever condition on portfolio state
  (§Two-stage recorded-proposal architecture).
