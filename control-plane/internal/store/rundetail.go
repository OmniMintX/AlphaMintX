package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// GetRunDetail returns the run-detail embed: the run row plus proposal,
// verdict, and trace contract payloads verbatim from their payload_json
// columns, and the run's orders, fills, and approvals. Missing pieces (no
// verdict yet, no trace, ...) are nil/empty; an unknown (strategy, run)
// pair returns ErrNotFound.
func (s *Store) GetRunDetail(strategyID, runID string) (RunDetail, error) {
	var d RunDetail
	run, err := scanRun(s.db.QueryRow(`SELECT run_id, strategy_id, tick_number, created_at, completed_at
		FROM runs WHERE run_id = ? AND strategy_id = ?`, runID, strategyID))
	if err == sql.ErrNoRows {
		return RunDetail{}, fmt.Errorf("run %s: %w", runID, ErrNotFound)
	}
	if err != nil {
		return RunDetail{}, err
	}
	d.Run = run

	var proposalID, proposalJSON string
	err = s.db.QueryRow(`SELECT proposal_id, payload_json FROM proposals
		WHERE run_id = ? ORDER BY created_at, proposal_id LIMIT 1`, runID).
		Scan(&proposalID, &proposalJSON)
	if err != nil && err != sql.ErrNoRows {
		return RunDetail{}, err
	}
	if err == nil {
		d.Proposal = json.RawMessage(proposalJSON)

		var verdictID, verdictJSON string
		err = s.db.QueryRow(`SELECT verdict_id, payload_json FROM verdicts WHERE proposal_id = ?`, proposalID).
			Scan(&verdictID, &verdictJSON)
		if err != nil && err != sql.ErrNoRows {
			return RunDetail{}, err
		}
		if err == nil {
			d.Verdict = json.RawMessage(verdictJSON)
			if a, ok, err := scanApproval(s.db.QueryRow(
				approvalSelect+` WHERE verdict_id = ?`, verdictID)); err != nil {
				return RunDetail{}, err
			} else if ok {
				d.Approvals = []Approval{a}
			}
		}

		if d.Orders, err = s.ordersByProposal(proposalID); err != nil {
			return RunDetail{}, err
		}
		if d.Fills, err = s.fillsByProposal(proposalID); err != nil {
			return RunDetail{}, err
		}
	}

	var traceJSON string
	err = s.db.QueryRow(`SELECT payload_json FROM agent_traces WHERE run_id = ?`, runID).Scan(&traceJSON)
	if err != nil && err != sql.ErrNoRows {
		return RunDetail{}, err
	}
	if err == nil {
		d.Trace = json.RawMessage(traceJSON)
	}
	return d, nil
}

func (s *Store) ordersByProposal(proposalID string) ([]Order, error) {
	rows, err := s.db.Query(`SELECT order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
		reduce_only, qty_base, limit_price, stop_price, fill_price, kill_epoch, status, submitted_at, filled_at
		FROM orders WHERE proposal_id = ? ORDER BY submitted_at, order_id`, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		var o Order
		var pid, limitPrice, stopPrice, fillPrice, filledAt sql.NullString
		if err := rows.Scan(&o.OrderID, &pid, &o.Origin, &o.StrategyID, &o.Symbol, &o.Class,
			&o.Side, &o.Type, &o.ReduceOnly, &o.QtyBase, &limitPrice, &stopPrice, &fillPrice,
			&o.KillEpoch, &o.Status, &o.SubmittedAt, &filledAt); err != nil {
			return nil, err
		}
		o.ProposalID = nullable(pid)
		o.LimitPrice = nullable(limitPrice)
		o.StopPrice = nullable(stopPrice)
		o.FillPrice = nullable(fillPrice)
		o.FilledAt = nullable(filledAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) fillsByProposal(proposalID string) ([]Fill, error) {
	rows, err := s.db.Query(`SELECT f.fill_id, f.order_id, f.qty_base, f.fill_price, f.fee_quote, f.fill_ts
		FROM fills f JOIN orders o ON o.order_id = f.order_id
		WHERE o.proposal_id = ? ORDER BY f.fill_ts, f.fill_id`, proposalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fill
	for rows.Next() {
		var f Fill
		if err := rows.Scan(&f.FillID, &f.OrderID, &f.QtyBase, &f.FillPrice, &f.FeeQuote, &f.FillTS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func nullable(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
