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

// GetPosition reads one (strategy, symbol) position snapshot inside the
// transaction (live fill accounting reads-then-upserts atomically);
// ok=false when no row exists yet.
func (t *SweepTx) GetPosition(strategyID, symbol string) (Position, bool, error) {
	return getPosition(t.tx, strategyID, symbol)
}

// InsertOrderIntent journals one placement attempt (Store semantics).
func (t *SweepTx) InsertOrderIntent(i OrderIntent) error { return insertOrderIntent(t.tx, i) }

// RecordOrderStatus advances the order FSM monotonically (Store semantics).
func (t *SweepTx) RecordOrderStatus(orderID, status string) (string, error) {
	return recordOrderStatus(t.tx, orderID, status)
}

// AppendOMSReconEvent appends one recon audit row (Store semantics): the
// step-3 kill-stale abandon writes the rejected status and its
// intent_resolved_absent event in the SAME transaction.
func (t *SweepTx) AppendOMSReconEvent(e OMSReconEvent) error {
	return appendOMSReconEvent(t.tx, e)
}

// InsertPendingFillFee persists a deferred fee conversion (Store semantics)
// inside the fill-booking transaction (Reconciler R5).
func (t *SweepTx) InsertPendingFillFee(f PendingFillFee) error {
	return insertPendingFillFee(t.tx, f)
}

// InsertProtectiveObligation persists a fresh SL/TP deadline timer (Store
// semantics) inside the fill-booking transaction: the timer and its
// triggering fill commit together (§Protective order lifecycle).
func (t *SweepTx) InsertProtectiveObligation(o ProtectiveObligation) error {
	return insertProtectiveObligation(t.tx, o)
}

// RecordFeeConverted resolves a deferred fee conversion (Store semantics)
// in the SAME transaction as its accounting application.
func (t *SweepTx) RecordFeeConverted(fillID, convertedAt string) error {
	return resolveOnce(t.tx,
		`UPDATE pending_fill_fees SET converted_at = ?
			WHERE fill_id = ? AND converted_at IS NULL`,
		`SELECT COUNT(*) FROM pending_fill_fees WHERE fill_id = ?`,
		"pending fill fee", fillID, convertedAt)
}
