package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// CreateStrategy inserts a strategy row (the anchor for runs and lifecycle
// audit rows) and, when the initial lifecycle state is `paper` or a
// `live_*` tier, the LC-16a bootstrap transition row — from_state 'draft',
// actor 'bootstrap', actor_role 'system' (the LC-10 carve-out), reason
// 'bootstrap', recorded_at = created_at — in the SAME transaction, so the
// LC-16 paper window has its qualifying entry from birth.
func (s *Store) CreateStrategy(st Strategy) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(`INSERT INTO strategies
		(strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		st.StrategyID, st.TenantID, st.Name, st.LifecycleState, st.CreatedAt, st.UpdatedAt); err != nil {
		return err
	}
	if st.LifecycleState == "paper" || strings.HasPrefix(st.LifecycleState, "live_") {
		if _, err := tx.Exec(`INSERT INTO lifecycle_transitions
			(transition_id, strategy_id, from_state, to_state, actor_id, actor_role, reason, recorded_at)
			VALUES (?, ?, 'draft', ?, 'bootstrap', 'system', 'bootstrap', ?)`,
			uuid.NewString(), st.StrategyID, st.LifecycleState, st.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// InsertProposal ingests a proposal submission. The UNIQUE proposal_id is
// the atomic insert backing at-least-once delivery: a verbatim duplicate
// (same canonical payload hash AND same tick) is a no-op (false, nil); a
// different hash or tick returns ErrIdempotencyConflict (riskgate step 0b,
// DB-backed). The run row is created if absent, keyed
// run_id = proposal.agent_trace_id and (strategy_id, tick_number) (Row
// rules); a run/tick contradiction returns ErrRunTickConflict; now is the
// run's created_at.
func (s *Store) InsertProposal(sub ProposalSubmission, now time.Time) (bool, error) {
	p := sub.Proposal
	payload, hash, err := canonicalJSON(p)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)

	if dup, err := classifySubmission(tx, sub, hash); err != nil || dup {
		return false, err
	}

	var runTick int
	err = tx.QueryRow(`SELECT tick_number FROM runs WHERE run_id = ?`, p.AgentTraceID).Scan(&runTick)
	switch {
	case err == nil:
		if runTick != sub.TickNumber {
			return false, ErrRunTickConflict
		}
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`INSERT INTO runs (run_id, strategy_id, tick_number, created_at)
			VALUES (?, ?, ?, ?)`,
			p.AgentTraceID, p.StrategyID, sub.TickNumber, formatTime(now)); err != nil {
			if isUniqueConstraint(err) {
				// UNIQUE (strategy_id, tick_number): another run owns the tick.
				return false, ErrRunTickConflict
			}
			return false, fmt.Errorf("create run %s: %w", p.AgentTraceID, err)
		}
	default:
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO proposals
		(proposal_id, run_id, strategy_id, symbol, action, created_at, payload_json, payload_sha256)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProposalID, p.AgentTraceID, p.StrategyID, p.Symbol, string(p.Action),
		p.CreatedAt.String(), string(payload), hash); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// IsDuplicateProposal reports whether sub is a verbatim duplicate of an
// already-ingested submission (same proposal_id, canonical payload hash,
// and tick): the API skips the proposal rate limiter for verbatim
// at-least-once retries. ErrIdempotencyConflict when the proposal_id is
// taken by a different payload or tick. Read-only.
func (s *Store) IsDuplicateProposal(sub ProposalSubmission) (bool, error) {
	_, hash, err := canonicalJSON(sub.Proposal)
	if err != nil {
		return false, err
	}
	return classifySubmission(s.db, sub, hash)
}

// classifySubmission checks a submission against its stored proposal_id
// row: (false, nil) fresh; (true, nil) verbatim duplicate; a hash or tick
// mismatch is ErrIdempotencyConflict (the envelope, tick included, is the
// idempotency unit).
func classifySubmission(q dbtx, sub ProposalSubmission, hash string) (bool, error) {
	var storedHash string
	var storedTick sql.NullInt64
	err := q.QueryRow(`SELECT p.payload_sha256, r.tick_number FROM proposals p
		LEFT JOIN runs r ON r.run_id = p.run_id
		WHERE p.proposal_id = ?`, sub.Proposal.ProposalID).Scan(&storedHash, &storedTick)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	if storedHash != hash || !storedTick.Valid || storedTick.Int64 != int64(sub.TickNumber) {
		return false, ErrIdempotencyConflict
	}
	return true, nil
}

// isUniqueConstraint reports a uniqueness failure from the
// modernc.org/sqlite driver: SQLITE_CONSTRAINT_UNIQUE for UNIQUE columns,
// SQLITE_CONSTRAINT_PRIMARYKEY for PRIMARY KEY collisions (e.g. tenants).
func isUniqueConstraint(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) &&
		(se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

// InsertVerdict persists the single RiskVerdict for a proposal (UNIQUE
// proposal_id: exactly one verdict per proposal, ever). A duplicate with an
// identical canonical payload is a no-op (false, nil); any other verdict for
// the same proposal returns ErrIdempotencyConflict. The proposal must exist.
func (s *Store) InsertVerdict(v *contract.Verdict) (bool, error) {
	payload, _, err := canonicalJSON(v)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)

	var stored string
	err = tx.QueryRow(`SELECT payload_json FROM verdicts WHERE proposal_id = ?`, v.ProposalID).Scan(&stored)
	switch {
	case err == nil:
		if stored != string(payload) {
			return false, ErrIdempotencyConflict
		}
		return false, nil
	case err != sql.ErrNoRows:
		return false, err
	}
	var proposalExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM proposals WHERE proposal_id = ?`,
		v.ProposalID).Scan(&proposalExists); err != nil {
		return false, err
	}
	if proposalExists == 0 {
		return false, fmt.Errorf("proposal %s: %w", v.ProposalID, ErrNotFound)
	}
	if _, err := tx.Exec(`INSERT INTO verdicts
		(verdict_id, proposal_id, decision, evaluated_at, payload_json)
		VALUES (?, ?, ?, ?, ?)`,
		v.VerdictID, v.ProposalID, string(v.Decision),
		v.EvaluatedAt.String(), string(payload)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// InsertOrder inserts an order row.
func (s *Store) InsertOrder(o Order) error { return insertOrder(s.db, o) }

func insertOrder(q dbtx, o Order) error {
	_, err := q.Exec(`INSERT INTO orders
		(order_id, proposal_id, origin, strategy_id, symbol, class, side, type, reduce_only,
		 qty_base, limit_price, stop_price, take_profit, fill_price, kill_epoch, status, submitted_at, filled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderID, o.ProposalID, o.Origin, o.StrategyID, o.Symbol, o.Class, o.Side, o.Type,
		o.ReduceOnly, o.QtyBase, o.LimitPrice, o.StopPrice, o.TakeProfit, o.FillPrice, o.KillEpoch,
		o.Status, o.SubmittedAt, o.FilledAt)
	return err
}

// RecordOrderFill advances an order's FSM to filled with its fill price and
// timestamp. Orders mutate ONLY their status/fill columns (Store rules);
// ErrNotFound when the order does not exist.
func (s *Store) RecordOrderFill(orderID, fillPrice, filledAt string) error {
	return recordOrderFill(s.db, orderID, fillPrice, filledAt)
}

func recordOrderFill(q dbtx, orderID, fillPrice, filledAt string) error {
	res, err := q.Exec(`UPDATE orders SET status = 'filled', fill_price = ?, filled_at = ?
		WHERE order_id = ?`, fillPrice, filledAt, orderID)
	if err != nil {
		return err
	}
	return oneRow(res, "order", orderID)
}

// RecordOrderCancel advances an order's FSM to canceled; ErrNotFound when
// the order does not exist.
func (s *Store) RecordOrderCancel(orderID string) error {
	return recordOrderCancel(s.db, orderID)
}

func recordOrderCancel(q dbtx, orderID string) error {
	res, err := q.Exec(`UPDATE orders SET status = 'canceled' WHERE order_id = ?`, orderID)
	if err != nil {
		return err
	}
	return oneRow(res, "order", orderID)
}

func oneRow(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %s: %w", kind, id, ErrNotFound)
	}
	return nil
}

// InsertFill appends a fill (append-only).
func (s *Store) InsertFill(f Fill) error { return insertFill(s.db, f) }

func insertFill(q dbtx, f Fill) error {
	_, err := q.Exec(`INSERT INTO fills (fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.FillID, f.OrderID, f.QtyBase, f.FillPrice, f.FeeQuote, f.FillTS)
	return err
}

// UpsertPosition writes the mutable position snapshot for (strategy, symbol).
func (s *Store) UpsertPosition(p Position) error { return upsertPosition(s.db, p) }

func upsertPosition(q dbtx, p Position) error {
	_, err := q.Exec(`INSERT INTO positions
		(strategy_id, symbol, qty_base, entry_price, fees_quote, realized_pnl_quote, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (strategy_id, symbol) DO UPDATE SET
		qty_base = excluded.qty_base, entry_price = excluded.entry_price,
		fees_quote = excluded.fees_quote, realized_pnl_quote = excluded.realized_pnl_quote,
		updated_at = excluded.updated_at`,
		p.StrategyID, p.Symbol, p.QtyBase, p.EntryPrice, p.FeesQuote, p.RealizedPnLQuote, p.UpdatedAt)
	return err
}

// UpsertStrategyState writes the mutable realized-equity snapshot for a
// strategy (the daily rollover math is the writer's responsibility).
func (s *Store) UpsertStrategyState(st StrategyState) error {
	return upsertStrategyState(s.db, st)
}

func upsertStrategyState(q dbtx, st StrategyState) error {
	_, err := q.Exec(`INSERT INTO strategy_state
		(strategy_id, equity_quote, peak_equity_quote, daily_realized_pnl_quote, utc_date, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (strategy_id) DO UPDATE SET
		equity_quote = excluded.equity_quote, peak_equity_quote = excluded.peak_equity_quote,
		daily_realized_pnl_quote = excluded.daily_realized_pnl_quote,
		utc_date = excluded.utc_date, updated_at = excluded.updated_at`,
		st.StrategyID, st.EquityQuote, st.PeakEquityQuote, st.DailyRealizedPnLQuote, st.UTCDate, st.UpdatedAt)
	return err
}
