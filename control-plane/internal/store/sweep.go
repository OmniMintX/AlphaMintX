package store

import "database/sql"

// ApplySweep runs fn inside ONE transaction over the runtime tables
// (orders, fills, positions, strategy_state): either every row an OMS
// action produced commits, or none do. Readers (StrategySnapshot) can
// never observe a torn sweep, and a mid-batch failure rolls the whole
// batch back.
func (s *Store) ApplySweep(fn func(*SweepTx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := fn(&SweepTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// SweepTx is the transactional writer ApplySweep hands its callback; its
// methods run the same statements as the matching Store methods.
type SweepTx struct{ tx *sql.Tx }

// InsertOrder inserts an order row.
func (t *SweepTx) InsertOrder(o Order) error { return insertOrder(t.tx, o) }

// RecordOrderFill advances an order's FSM to filled (Store semantics).
func (t *SweepTx) RecordOrderFill(orderID, fillPrice, filledAt string) error {
	return recordOrderFill(t.tx, orderID, fillPrice, filledAt)
}

// RecordOrderCancel advances an order's FSM to canceled (Store semantics).
func (t *SweepTx) RecordOrderCancel(orderID string) error {
	return recordOrderCancel(t.tx, orderID)
}

// InsertFill appends a fill (append-only).
func (t *SweepTx) InsertFill(f Fill) error { return insertFill(t.tx, f) }

// UpsertPosition writes the mutable position snapshot for (strategy, symbol).
func (t *SweepTx) UpsertPosition(p Position) error { return upsertPosition(t.tx, p) }

// UpsertStrategyState writes the mutable realized-equity snapshot.
func (t *SweepTx) UpsertStrategyState(st StrategyState) error {
	return upsertStrategyState(t.tx, st)
}

// GetStrategyState reads the realized-equity snapshot inside the sweep
// transaction (the rollover math needs the current row).
func (t *SweepTx) GetStrategyState(strategyID string) (StrategyState, bool, error) {
	return getStrategyState(t.tx, strategyID)
}
