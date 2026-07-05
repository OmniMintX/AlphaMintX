package store

// schemaDDL is the normative DDL from docs/specs/persistence-and-api.md
// §Tables (safety_effects and safety_alerts: docs/specs/safety-wiring.md
// §Crash-resumable safety effects), made idempotent with IF NOT EXISTS so
// Open can always apply it.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS strategies (strategy_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL,
  name TEXT NOT NULL, lifecycle_state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS lifecycle_transitions (transition_id TEXT PRIMARY KEY,   -- append-only audit
  strategy_id TEXT NOT NULL REFERENCES strategies, from_state TEXT NOT NULL, to_state TEXT NOT NULL,
  actor_id TEXT NOT NULL, actor_role TEXT NOT NULL, reason TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (run_id TEXT PRIMARY KEY, strategy_id TEXT NOT NULL REFERENCES strategies,
  tick_number INTEGER NOT NULL, created_at TEXT NOT NULL, completed_at TEXT,
  UNIQUE (strategy_id, tick_number));
CREATE TABLE IF NOT EXISTS proposals (proposal_id TEXT PRIMARY KEY,   -- payload = contracts/proposal.schema.json
  run_id TEXT REFERENCES runs, strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, action TEXT NOT NULL,
  created_at TEXT NOT NULL, payload_json TEXT NOT NULL, payload_sha256 TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS verdicts (verdict_id TEXT PRIMARY KEY,     -- payload = contracts/riskverdict.schema.json
  proposal_id TEXT NOT NULL UNIQUE REFERENCES proposals, decision TEXT NOT NULL,
  evaluated_at TEXT NOT NULL, payload_json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS approvals (approval_id TEXT PRIMARY KEY,   -- append-only ApprovalDecision records
  verdict_id TEXT NOT NULL UNIQUE REFERENCES verdicts, proposal_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('approved','approved_but_blocked','rejected','timeout')),
  preflight_reasons TEXT,                               -- JSON array; non-null iff approved_but_blocked
  decided_by TEXT NOT NULL, decided_at TEXT NOT NULL, timeout_seconds INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS pending_approvals (          -- restart-safe L1 timer state
  verdict_id TEXT PRIMARY KEY REFERENCES verdicts, strategy_id TEXT NOT NULL,
  created_at TEXT NOT NULL, deadline_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS orders (order_id TEXT PRIMARY KEY, proposal_id TEXT REFERENCES proposals,
  origin TEXT NOT NULL CHECK (origin IN ('proposal','breaker','kill','watchdog','sl_contingency')),
  strategy_id TEXT NOT NULL, symbol TEXT NOT NULL, class TEXT NOT NULL CHECK (class IN ('ENTRY','PROTECTIVE')),
  side TEXT NOT NULL, type TEXT NOT NULL, reduce_only INTEGER NOT NULL, qty_base TEXT NOT NULL,
  limit_price TEXT, stop_price TEXT,
  take_profit TEXT,                                     -- TP obligation carried on a resting entry
  fill_price TEXT, kill_epoch INTEGER NOT NULL,
  status TEXT NOT NULL, submitted_at TEXT NOT NULL, filled_at TEXT);
CREATE TABLE IF NOT EXISTS fills (fill_id TEXT PRIMARY KEY,           -- append-only
  order_id TEXT NOT NULL REFERENCES orders, qty_base TEXT NOT NULL,
  fill_price TEXT NOT NULL, fee_quote TEXT NOT NULL, fill_ts TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS positions (strategy_id TEXT NOT NULL,      -- mutable snapshot
  symbol TEXT NOT NULL, qty_base TEXT NOT NULL,
  entry_price TEXT NOT NULL,                            -- fee-EXCLUSIVE (see Row rules)
  fees_quote TEXT NOT NULL,
  realized_pnl_quote TEXT NOT NULL,                     -- cumulative, net of ALL fees; survives a flat book
  updated_at TEXT NOT NULL, PRIMARY KEY (strategy_id, symbol));
CREATE TABLE IF NOT EXISTS agent_traces (trace_id TEXT PRIMARY KEY,   -- payload = contracts/agent_trace.schema.json
  run_id TEXT NOT NULL UNIQUE REFERENCES runs, strategy_id TEXT NOT NULL, proposal_id TEXT,
  started_at TEXT NOT NULL, completed_at TEXT NOT NULL,
  payload_json TEXT NOT NULL, payload_sha256 TEXT NOT NULL);
-- NO VACUUM: billing watermark_rowid windows reference model_costs' implicit rowids (billing spec §Billing).
CREATE TABLE IF NOT EXISTS model_costs (cost_id TEXT PRIMARY KEY,     -- append-only billing signal
  run_id TEXT NOT NULL REFERENCES runs, strategy_id TEXT NOT NULL, node TEXT NOT NULL,
  model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
  cost_usd TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS token_budget_ledger (        -- authoritative usage; daily_token_budget is
  strategy_id TEXT NOT NULL, utc_date TEXT NOT NULL,    -- Admin-set CONFIG, never ledger state
  tokens_used INTEGER NOT NULL, cost_usd_used TEXT NOT NULL, updated_at TEXT NOT NULL,
  PRIMARY KEY (strategy_id, utc_date));
CREATE TABLE IF NOT EXISTS rejected_submissions (       -- append-only; malformed, NO verdict
  rejection_id TEXT PRIMARY KEY, strategy_id TEXT, received_at TEXT NOT NULL,
  reason TEXT NOT NULL, payload_json TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kill_breaker_events (event_id TEXT PRIMARY KEY,  -- append-only safety audit
  kind TEXT NOT NULL CHECK (kind IN ('kill','breaker')), scope TEXT NOT NULL, strategy_id TEXT,
  -- tenant_id is NOT here: it arrives via migrateTenancy's guarded ALTER (store.go) so
  -- pre-existing databases gain it too.
  kill_epoch INTEGER, flatten INTEGER, trigger_ref TEXT, actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS risk_limit_changes (change_id TEXT PRIMARY KEY,  -- append-only limit audit
  strategy_id TEXT NOT NULL, field TEXT NOT NULL, old_value TEXT, new_value TEXT NOT NULL,
  actor_id TEXT NOT NULL, changed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS strategy_state (             -- mutable realized-equity snapshot
  strategy_id TEXT PRIMARY KEY REFERENCES strategies,
  equity_quote TEXT NOT NULL,                           -- allocated capital + cumulative realized PnL
  peak_equity_quote TEXT NOT NULL,                      -- monotone max of realized equity
  daily_realized_pnl_quote TEXT NOT NULL,               -- realized PnL of utc_date (fees included)
  utc_date TEXT NOT NULL,                               -- YYYY-MM-DD day the daily figure belongs to
  updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tenants (tenant_id TEXT PRIMARY KEY,        -- multi-tenant-rbac.md §Tables
  name TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS api_tokens (token_id TEXT PRIMARY KEY,      -- mutable snapshot; ONLY legal
  tenant_id TEXT NOT NULL REFERENCES tenants,                          -- mutation sets revoked_at once
  principal TEXT NOT NULL CHECK (principal IN ('user','agent')),
  role TEXT CHECK (role IN ('owner','admin','trader','viewer')),
  strategy_id TEXT REFERENCES strategies,
  token_hash TEXT NOT NULL UNIQUE,                                     -- hex(SHA-256(plaintext))
  label TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT,
  CHECK ((principal = 'user' AND role IS NOT NULL AND strategy_id IS NULL)
      OR (principal = 'agent' AND role IS NULL AND strategy_id IS NOT NULL)));
CREATE TABLE IF NOT EXISTS token_events (event_id TEXT PRIMARY KEY,    -- append-only token audit
  token_id TEXT NOT NULL REFERENCES api_tokens,
  event TEXT NOT NULL CHECK (event IN ('created','revoked')),
  actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS metering_records (record_id TEXT PRIMARY KEY,   -- append-only gateway import
  source TEXT NOT NULL,                                      -- export label, e.g. filename
  request_id TEXT NOT NULL UNIQUE, strategy_id TEXT NOT NULL REFERENCES strategies,
  model TEXT NOT NULL, input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL,
  cost_usd TEXT NOT NULL, metered_at TEXT NOT NULL, imported_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS billing_periods (period_id TEXT PRIMARY KEY,    -- append-only; absence = open
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL,      -- YYYY-MM
  period_start TEXT NOT NULL, period_end TEXT NOT NULL,      -- inclusive UTC dates
  status TEXT NOT NULL CHECK (status IN ('closed')),         -- row insertion IS the close
  closed_at TEXT NOT NULL,                                   -- informational ONLY (billing spec)
  watermark_rowid INTEGER NOT NULL,                          -- exactly-once window key
  UNIQUE (tenant_id, period));
CREATE TABLE IF NOT EXISTS invoices (invoice_id TEXT PRIMARY KEY,          -- append-only, immutable
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL,
  total_usd TEXT NOT NULL, line_count INTEGER NOT NULL, generated_at TEXT NOT NULL,
  UNIQUE (tenant_id, period));
CREATE TABLE IF NOT EXISTS invoice_lines (line_id TEXT PRIMARY KEY,        -- append-only
  invoice_id TEXT NOT NULL REFERENCES invoices, strategy_id TEXT NOT NULL,
  model TEXT NOT NULL, entry_type TEXT NOT NULL CHECK (entry_type IN ('usage','carry_over','credit_note')),
  original_period TEXT,                                      -- non-null iff carry_over
  input_tokens INTEGER NOT NULL, output_tokens INTEGER NOT NULL, amount_usd TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS reconciliation_runs (recon_id TEXT PRIMARY KEY, -- append-only
  tenant_id TEXT NOT NULL REFERENCES tenants, period TEXT NOT NULL, invoice_id TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pass','fail')),
  matched_count INTEGER NOT NULL, discrepancy_count INTEGER NOT NULL,
  matched_client_cost_usd TEXT NOT NULL, orphan_client_cost_usd TEXT NOT NULL,
  estimated_client_cost_usd TEXT NOT NULL, unattributed_client_cost_usd TEXT NOT NULL,
  invoice_total_usd TEXT NOT NULL, run_at TEXT NOT NULL);    -- == invoices.total_usd (identity)
CREATE TABLE IF NOT EXISTS discrepancies (discrepancy_id TEXT PRIMARY KEY, -- append-only
  recon_id TEXT NOT NULL REFERENCES reconciliation_runs,
  class TEXT NOT NULL CHECK (class IN ('ORPHAN_CLIENT','ORPHAN_GATEWAY','ESTIMATED_CLIENT',
    'MISMATCH_TOKENS','MISMATCH_COST','ATTRIBUTION_MISMATCH')),
  request_id TEXT, strategy_id TEXT, details_json TEXT NOT NULL);
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
CREATE TABLE IF NOT EXISTS safety_effects (             -- served markers: the insert IS completion
  event_id TEXT PRIMARY KEY REFERENCES kill_breaker_events,
  completed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS safety_alerts (              -- append-only monitor/driver alerts
  alert_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,                                   -- OPEN set (SS-25 pattern); registry in §Alerts
  strategy_id TEXT, ref_id TEXT,                        -- ref_id: nullable dedupe key (§Alerts)
  details_json TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kill_clear_events (clear_id TEXT PRIMARY KEY,  -- append-only SW-2 audit
  scope TEXT NOT NULL CHECK (scope IN ('strategy','tenant','platform')),
  strategy_id TEXT, tenant_id TEXT,
  cleared_epoch INTEGER NOT NULL, actor_id TEXT NOT NULL, reason TEXT NOT NULL,
  recorded_at TEXT NOT NULL,
  CHECK ((scope = 'strategy' AND strategy_id IS NOT NULL AND tenant_id IS NOT NULL)
      OR (scope = 'tenant' AND strategy_id IS NULL AND tenant_id IS NOT NULL)
      OR (scope = 'platform' AND strategy_id IS NULL AND tenant_id IS NULL)));
CREATE INDEX IF NOT EXISTS idx_kill_clear_scope
  ON kill_clear_events (scope, strategy_id, tenant_id, cleared_epoch);
`
