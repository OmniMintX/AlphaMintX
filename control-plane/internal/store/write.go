package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// CreateStrategy inserts a strategy row (the anchor for runs and lifecycle
// audit rows).
func (s *Store) CreateStrategy(st Strategy) error {
	_, err := s.db.Exec(`INSERT INTO strategies
		(strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		st.StrategyID, st.TenantID, st.Name, st.LifecycleState, st.CreatedAt, st.UpdatedAt)
	return err
}

// InsertProposal ingests a proposal submission. The UNIQUE proposal_id is
// the atomic insert backing at-least-once delivery: a duplicate with the
// same canonical payload hash is a no-op (false, nil); a different hash
// returns ErrIdempotencyConflict (riskgate step 0b, DB-backed). The run row
// is created if absent, keyed run_id = proposal.agent_trace_id and
// (strategy_id, tick_number) (Row rules); now is the run's created_at.
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

	var storedHash string
	err = tx.QueryRow(`SELECT payload_sha256 FROM proposals WHERE proposal_id = ?`, p.ProposalID).Scan(&storedHash)
	switch {
	case err == nil:
		if storedHash != hash {
			return false, ErrIdempotencyConflict
		}
		return false, nil
	case err != sql.ErrNoRows:
		return false, err
	}

	var runExists int
	err = tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE run_id = ?`, p.AgentTraceID).Scan(&runExists)
	if err != nil {
		return false, err
	}
	if runExists == 0 {
		if _, err := tx.Exec(`INSERT INTO runs (run_id, strategy_id, tick_number, created_at)
			VALUES (?, ?, ?, ?)`,
			p.AgentTraceID, p.StrategyID, sub.TickNumber, formatTime(now)); err != nil {
			return false, fmt.Errorf("create run %s: %w", p.AgentTraceID, err)
		}
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

// InsertOrder inserts an order row (FSM status mutations are out of scope
// for the Phase-1 store surface).
func (s *Store) InsertOrder(o Order) error {
	_, err := s.db.Exec(`INSERT INTO orders
		(order_id, proposal_id, origin, strategy_id, symbol, class, side, type, reduce_only,
		 qty_base, limit_price, stop_price, fill_price, kill_epoch, status, submitted_at, filled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderID, o.ProposalID, o.Origin, o.StrategyID, o.Symbol, o.Class, o.Side, o.Type,
		o.ReduceOnly, o.QtyBase, o.LimitPrice, o.StopPrice, o.FillPrice, o.KillEpoch,
		o.Status, o.SubmittedAt, o.FilledAt)
	return err
}

// InsertFill appends a fill (append-only).
func (s *Store) InsertFill(f Fill) error {
	_, err := s.db.Exec(`INSERT INTO fills (fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.FillID, f.OrderID, f.QtyBase, f.FillPrice, f.FeeQuote, f.FillTS)
	return err
}

// UpsertPosition writes the mutable position snapshot for (strategy, symbol).
func (s *Store) UpsertPosition(p Position) error {
	_, err := s.db.Exec(`INSERT INTO positions (strategy_id, symbol, qty_base, entry_price, fees_quote, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (strategy_id, symbol) DO UPDATE SET
		qty_base = excluded.qty_base, entry_price = excluded.entry_price,
		fees_quote = excluded.fees_quote, updated_at = excluded.updated_at`,
		p.StrategyID, p.Symbol, p.QtyBase, p.EntryPrice, p.FeesQuote, p.UpdatedAt)
	return err
}
