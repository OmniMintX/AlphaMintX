package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Read helpers backing serve-mode proposal ingestion, the Risk Gate
// hydrator (internal/runstate), and the OMS bridge (internal/omsbridge).
// None of them mutate anything.

// GetVerdictByProposalID returns the stored RiskVerdict payload for a
// proposal VERBATIM (Payload rule: payload_json is the source of truth — a
// duplicate submission answers with these exact bytes), or ErrNotFound.
func (s *Store) GetVerdictByProposalID(proposalID string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM verdicts WHERE proposal_id = ?`, proposalID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("verdict for proposal %s: %w", proposalID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

// GetProposalPayload returns the stored TradeProposal payload verbatim, or
// ErrNotFound. The OMS bridge parses it to build the entry submission.
func (s *Store) GetProposalPayload(proposalID string) (json.RawMessage, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM proposals WHERE proposal_id = ?`, proposalID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("proposal %s: %w", proposalID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

// ListOpenOrders returns a strategy's status='open' orders in deterministic
// (submitted_at, order_id) order: the pending-ENTRY count for the gate and
// the re-arm set for OMS restart hydration.
func (s *Store) ListOpenOrders(strategyID string) ([]Order, error) {
	return listOpenOrders(s.db, strategyID)
}

func listOpenOrders(q dbtx, strategyID string) ([]Order, error) {
	rows, err := q.Query(orderSelect+` WHERE strategy_id = ? AND status = 'open'
		ORDER BY submitted_at, order_id`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListPositions returns a strategy's position snapshots (including flat
// books, whose realized_pnl_quote must survive restarts), symbol ascending.
func (s *Store) ListPositions(strategyID string) ([]Position, error) {
	return listPositions(s.db, strategyID)
}

func listPositions(q dbtx, strategyID string) ([]Position, error) {
	rows, err := q.Query(`SELECT strategy_id, symbol, qty_base, entry_price, fees_quote,
		realized_pnl_quote, updated_at
		FROM positions WHERE strategy_id = ? ORDER BY symbol`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.StrategyID, &p.Symbol, &p.QtyBase, &p.EntryPrice,
			&p.FeesQuote, &p.RealizedPnLQuote, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetStrategyState returns the realized-equity snapshot; ok=false when the
// strategy has no row yet (nothing realized so far).
func (s *Store) GetStrategyState(strategyID string) (StrategyState, bool, error) {
	return getStrategyState(s.db, strategyID)
}

func getStrategyState(q dbtx, strategyID string) (StrategyState, bool, error) {
	var st StrategyState
	err := q.QueryRow(`SELECT strategy_id, equity_quote, peak_equity_quote,
		daily_realized_pnl_quote, utc_date, updated_at
		FROM strategy_state WHERE strategy_id = ?`, strategyID).
		Scan(&st.StrategyID, &st.EquityQuote, &st.PeakEquityQuote,
			&st.DailyRealizedPnLQuote, &st.UTCDate, &st.UpdatedAt)
	if err == sql.ErrNoRows {
		return StrategyState{}, false, nil
	}
	if err != nil {
		return StrategyState{}, false, err
	}
	return st, true, nil
}

// Snapshot is one strategy's runtime view read in a single transaction:
// positions, open orders, and the strategy_state row are mutually
// consistent — a concurrent sweep can never tear them.
type Snapshot struct {
	Positions  []Position
	OpenOrders []Order
	State      StrategyState
	HasState   bool // false: no strategy_state row yet
}

// StrategySnapshot reads a strategy's positions, open orders, and
// realized-equity snapshot in ONE transaction (the read twin of ApplySweep).
func (s *Store) StrategySnapshot(strategyID string) (Snapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Snapshot{}, err
	}
	defer rollback(tx)
	var snap Snapshot
	if snap.Positions, err = listPositions(tx, strategyID); err != nil {
		return Snapshot{}, err
	}
	if snap.OpenOrders, err = listOpenOrders(tx, strategyID); err != nil {
		return Snapshot{}, err
	}
	if snap.State, snap.HasState, err = getStrategyState(tx, strategyID); err != nil {
		return Snapshot{}, err
	}
	return snap, tx.Commit()
}

// CountRateVerdictsSince counts the strategy's rate-window consumers for
// gate step 8: approve/clip verdicts on non-hold proposals evaluated
// strictly after the RFC 3339 UTC cutoff (an approved verdict reserves a
// rate token; safety-path submissions never reach the gate).
func (s *Store) CountRateVerdictsSince(strategyID, afterRFC3339 string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM verdicts v
		JOIN proposals p ON p.proposal_id = v.proposal_id
		WHERE p.strategy_id = ? AND p.action != 'hold'
		AND v.decision IN ('approve', 'clip') AND v.evaluated_at > ?`,
		strategyID, afterRFC3339).Scan(&n)
	return n, err
}

// GlobalMaxKillEpoch returns the highest persisted kill epoch affecting a
// strategy (global scope or the strategy's own) over ALL time; 0 when none.
// It is the epoch the OMS bridge stamps on submissions (kill re-check) and
// the hydrator's standing-kill signal.
func (s *Store) GlobalMaxKillEpoch(strategyID string) (int64, error) {
	var epoch int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(kill_epoch), 0) FROM kill_breaker_events
		WHERE kind = 'kill' AND (strategy_id IS NULL OR strategy_id = ?)`,
		strategyID).Scan(&epoch)
	return epoch, err
}

const orderSelect = `SELECT order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
	reduce_only, qty_base, limit_price, stop_price, take_profit, fill_price, kill_epoch,
	status, submitted_at, filled_at FROM orders`

// scanOrder scans one orders row (orderSelect column order).
func scanOrder(row rowScanner) (Order, error) {
	var o Order
	var pid, limitPrice, stopPrice, takeProfit, fillPrice, filledAt sql.NullString
	if err := row.Scan(&o.OrderID, &pid, &o.Origin, &o.StrategyID, &o.Symbol, &o.Class,
		&o.Side, &o.Type, &o.ReduceOnly, &o.QtyBase, &limitPrice, &stopPrice, &takeProfit,
		&fillPrice, &o.KillEpoch, &o.Status, &o.SubmittedAt, &filledAt); err != nil {
		return Order{}, err
	}
	o.ProposalID = nullable(pid)
	o.LimitPrice = nullable(limitPrice)
	o.StopPrice = nullable(stopPrice)
	o.TakeProfit = nullable(takeProfit)
	o.FillPrice = nullable(fillPrice)
	o.FilledAt = nullable(filledAt)
	return o, nil
}
