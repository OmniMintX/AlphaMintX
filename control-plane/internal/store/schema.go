package store

// schemaDDL is the normative DDL from docs/specs/persistence-and-api.md
// §Tables, made idempotent with IF NOT EXISTS so Open can always apply it.
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
  limit_price TEXT, stop_price TEXT, fill_price TEXT, kill_epoch INTEGER NOT NULL,
  status TEXT NOT NULL, submitted_at TEXT NOT NULL, filled_at TEXT);
CREATE TABLE IF NOT EXISTS fills (fill_id TEXT PRIMARY KEY,           -- append-only
  order_id TEXT NOT NULL REFERENCES orders, qty_base TEXT NOT NULL,
  fill_price TEXT NOT NULL, fee_quote TEXT NOT NULL, fill_ts TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS positions (strategy_id TEXT NOT NULL,      -- mutable snapshot
  symbol TEXT NOT NULL, qty_base TEXT NOT NULL,
  entry_price TEXT NOT NULL,                            -- fee-EXCLUSIVE (see Row rules)
  fees_quote TEXT NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY (strategy_id, symbol));
CREATE TABLE IF NOT EXISTS agent_traces (trace_id TEXT PRIMARY KEY,   -- payload = contracts/agent_trace.schema.json
  run_id TEXT NOT NULL UNIQUE REFERENCES runs, strategy_id TEXT NOT NULL, proposal_id TEXT,
  started_at TEXT NOT NULL, completed_at TEXT NOT NULL,
  payload_json TEXT NOT NULL, payload_sha256 TEXT NOT NULL);
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
  kill_epoch INTEGER, flatten INTEGER, trigger_ref TEXT, actor_id TEXT NOT NULL, recorded_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS risk_limit_changes (change_id TEXT PRIMARY KEY,  -- append-only limit audit
  strategy_id TEXT NOT NULL, field TEXT NOT NULL, old_value TEXT, new_value TEXT NOT NULL,
  actor_id TEXT NOT NULL, changed_at TEXT NOT NULL);
`
