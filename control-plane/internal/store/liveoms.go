package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Live-OMS persistence surface (docs/specs/live-oms-and-reconciler.md
// §Tables, §Store-surface amendment): the write-ahead intent journal, the
// recon audit trail, protective-obligation timers, deferred fee
// conversions, and venue epochs. Live columns mutate ONLY through the named
// Record* mutators below; the append-only tables have INSERT methods and no
// mutators.

// OrderIntent mirrors the order_intents write-ahead journal: one row per
// placement attempt. Identity/parameter columns never change; only the
// claim columns mutate (RecordIntentClaim / RecordIntentClaimRevoked).
type OrderIntent struct {
	ClientOrderID  string  `json:"client_order_id"` // amx1-<token22>-<attempt>
	IntentToken    string  `json:"intent_token"`
	Attempt        int     `json:"attempt"`
	OrderID        string  `json:"order_id"`
	StrategyID     string  `json:"strategy_id"`
	Symbol         string  `json:"symbol"`
	VenueSymbol    string  `json:"venue_symbol"`
	Side           string  `json:"side"`
	Type           string  `json:"type"`
	QtyBase        string  `json:"qty_base"`
	LimitPrice     *string `json:"limit_price"`
	StopPrice      *string `json:"stop_price"`
	Origin         string  `json:"origin"` // orders.origin vocabulary
	ProposalID     *string `json:"proposal_id"`
	KillEpoch      int64   `json:"kill_epoch"`
	JournaledAt    string  `json:"journaled_at"`
	ClaimedAt      *string `json:"claimed_at"`
	ClaimRevokedAt *string `json:"claim_revoked_at"`
}

// OMSReconEvent mirrors the append-only oms_recon_events audit table.
// RunID is the recon run's own UUID — NOT a runs-table foreign key.
type OMSReconEvent struct {
	EventID         string  `json:"event_id"`
	Kind            string  `json:"kind"`
	RunID           *string `json:"run_id"`
	StrategyID      *string `json:"strategy_id"`
	Symbol          *string `json:"symbol"`
	ClientOrderID   *string `json:"client_order_id"`
	ExchangeOrderID *string `json:"exchange_order_id"`
	ExchangeTradeID *int64  `json:"exchange_trade_id"`
	DetailsJSON     string  `json:"details_json"`
	RecordedAt      string  `json:"recorded_at"`
}

// ProtectiveObligation mirrors protective_obligations (restart-safe SL/TP
// deadline timers); only satisfied_at mutates (RecordProtectiveSatisfied).
type ProtectiveObligation struct {
	ObligationID string  `json:"obligation_id"`
	EntryOrderID string  `json:"entry_order_id"`
	StrategyID   string  `json:"strategy_id"`
	Kind         string  `json:"kind"` // "sl" or "tp"
	DueAt        string  `json:"due_at"`
	CreatedAt    string  `json:"created_at"`
	SatisfiedAt  *string `json:"satisfied_at"`
}

// PendingFillFee mirrors pending_fill_fees (deferred fee conversions,
// Reconciler R5); only converted_at mutates (RecordFeeConverted).
type PendingFillFee struct {
	FillID          string  `json:"fill_id"`
	Commission      string  `json:"commission"`
	CommissionAsset string  `json:"commission_asset"`
	RecordedAt      string  `json:"recorded_at"`
	ConvertedAt     *string `json:"converted_at"`
}

// VenueEpoch mirrors the append-only venue_epochs table: inserting a row IS
// the epoch transition; the current epoch is MAX(venue_epoch).
type VenueEpoch struct {
	VenueEpoch  int64  `json:"venue_epoch"`
	StartedAt   string `json:"started_at"`
	Reason      string `json:"reason"` // "initial" or "venue_reset_accepted"
	DetailsJSON string `json:"details_json"`
}

// LiveOrder is an orders row plus its live columns (client_order_id holds
// the LATEST attempt id; exchange_order_id the latest attempt's ack).
type LiveOrder struct {
	Order
	ClientOrderID   *string `json:"client_order_id"`
	ExchangeOrderID *string `json:"exchange_order_id"`
}

// VenueFill is a fills row plus its venue identity columns; fill identity
// is (venue_epoch, venue_symbol, exchange_trade_id) (invariant 8).
type VenueFill struct {
	Fill
	VenueSymbol     string `json:"venue_symbol"`
	ExchangeTradeID int64  `json:"exchange_trade_id"`
	VenueEpoch      int64  `json:"venue_epoch"`
}

// InsertOrderIntent journals one placement attempt (journal-before-send).
func (s *Store) InsertOrderIntent(i OrderIntent) error { return insertOrderIntent(s.db, i) }

func insertOrderIntent(q dbtx, i OrderIntent) error {
	_, err := q.Exec(`INSERT INTO order_intents
		(client_order_id, intent_token, attempt, order_id, strategy_id, symbol, venue_symbol,
		 side, type, qty_base, limit_price, stop_price, origin, proposal_id, kill_epoch,
		 journaled_at, claimed_at, claim_revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ClientOrderID, i.IntentToken, i.Attempt, i.OrderID, i.StrategyID, i.Symbol,
		i.VenueSymbol, i.Side, i.Type, i.QtyBase, i.LimitPrice, i.StopPrice, i.Origin,
		i.ProposalID, i.KillEpoch, i.JournaledAt, i.ClaimedAt, i.ClaimRevokedAt)
	return err
}

// InsertJournaledOrder journals one live placement in ONE transaction
// (journal-before-send, invariant 3): the pending_new orders row carrying
// the attempt-0 client_order_id plus its attempt-0 order_intents row commit
// together BEFORE any placement HTTP.
func (s *Store) InsertJournaledOrder(o Order, i OrderIntent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := insertOrder(tx, o); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE orders SET client_order_id = ? WHERE order_id = ?`,
		i.ClientOrderID, o.OrderID); err != nil {
		return err
	}
	if err := insertOrderIntent(tx, i); err != nil {
		return err
	}
	return tx.Commit()
}

// AppendOMSReconEvent appends one recon audit row (append-only; destructive
// actions journal their event BEFORE the side effect executes).
func (s *Store) AppendOMSReconEvent(e OMSReconEvent) error {
	return appendOMSReconEvent(s.db, e)
}

func appendOMSReconEvent(q dbtx, e OMSReconEvent) error {
	_, err := q.Exec(`INSERT INTO oms_recon_events
		(event_id, kind, run_id, strategy_id, symbol, client_order_id, exchange_order_id,
		 exchange_trade_id, details_json, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.EventID, e.Kind, e.RunID, e.StrategyID, e.Symbol, e.ClientOrderID,
		e.ExchangeOrderID, e.ExchangeTradeID, e.DetailsJSON, e.RecordedAt)
	return err
}

// InsertProtectiveObligation persists a fresh SL/TP deadline timer (every
// cumulative-quantity-growing fill creates a NEW row; rows never reopen).
func (s *Store) InsertProtectiveObligation(o ProtectiveObligation) error {
	return insertProtectiveObligation(s.db, o)
}

func insertProtectiveObligation(q dbtx, o ProtectiveObligation) error {
	_, err := q.Exec(`INSERT INTO protective_obligations
		(obligation_id, entry_order_id, strategy_id, kind, due_at, created_at, satisfied_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		o.ObligationID, o.EntryOrderID, o.StrategyID, o.Kind, o.DueAt, o.CreatedAt, o.SatisfiedAt)
	return err
}

// InsertPendingFillFee persists a deferred fee conversion (R5: the fill
// books immediately, the fee-dependent accounting waits for a fresh mark).
func (s *Store) InsertPendingFillFee(f PendingFillFee) error {
	return insertPendingFillFee(s.db, f)
}

func insertPendingFillFee(q dbtx, f PendingFillFee) error {
	_, err := q.Exec(`INSERT INTO pending_fill_fees
		(fill_id, commission, commission_asset, recorded_at, converted_at)
		VALUES (?, ?, ?, ?, ?)`,
		f.FillID, f.Commission, f.CommissionAsset, f.RecordedAt, f.ConvertedAt)
	return err
}

// InsertVenueEpoch appends the next venue epoch; the insert IS the epoch
// transition (epoch 0 at first live start, later rows by operator
// acceptance only).
func (s *Store) InsertVenueEpoch(e VenueEpoch) error {
	_, err := s.db.Exec(`INSERT INTO venue_epochs
		(venue_epoch, started_at, reason, details_json)
		VALUES (?, ?, ?, ?)`,
		e.VenueEpoch, e.StartedAt, e.Reason, e.DetailsJSON)
	return err
}

// RecordIntentClaim claims the attempt for sending: claimed_at is set iff
// the attempt is currently unclaimed AND unrevoked (in-flight exclusion is
// transactional, not clock-based). false: the claim was lost (already
// claimed or revoked); ErrNotFound: no such attempt.
func (s *Store) RecordIntentClaim(clientOrderID, claimedAt string) (bool, error) {
	res, err := s.db.Exec(`UPDATE order_intents SET claimed_at = ?
		WHERE client_order_id = ? AND claimed_at IS NULL AND claim_revoked_at IS NULL`,
		claimedAt, clientOrderID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		return true, nil
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM order_intents
		WHERE client_order_id = ?`, clientOrderID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, fmt.Errorf("order intent %s: %w", clientOrderID, ErrNotFound)
	}
	return false, nil
}

// RecordIntentClaimRevoked sets claim_revoked_at (Reconciler R2 /
// CancelOpenEntries: the revoker owns the intent's resolution; the sender's
// pre-transmit re-check MUST NOT transmit a revoked claim). A second
// revocation is a no-op; ErrNotFound: no such attempt.
func (s *Store) RecordIntentClaimRevoked(clientOrderID, revokedAt string) error {
	return resolveOnce(s.db,
		`UPDATE order_intents SET claim_revoked_at = ?
			WHERE client_order_id = ? AND claim_revoked_at IS NULL`,
		`SELECT COUNT(*) FROM order_intents WHERE client_order_id = ?`,
		"order intent", clientOrderID, revokedAt)
}

// RecordIntentAttempt journals the attempt+1 order_intents row and bumps
// orders.client_order_id to the new attempt id in ONE transaction (the
// poisoned-id retry path); ErrNotFound when the order does not exist.
func (s *Store) RecordIntentAttempt(i OrderIntent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)
	res, err := tx.Exec(`UPDATE orders SET client_order_id = ? WHERE order_id = ?`,
		i.ClientOrderID, i.OrderID)
	if err != nil {
		return err
	}
	if err := oneRow(res, "order", i.OrderID); err != nil {
		return err // ErrNotFound BEFORE the FK-checked journal insert
	}
	if err := insertOrderIntent(tx, i); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordExchangeAck sets orders.exchange_order_id from the venue ack;
// ErrNotFound when the order does not exist.
func (s *Store) RecordExchangeAck(orderID, exchangeOrderID string) error {
	res, err := s.db.Exec(`UPDATE orders SET exchange_order_id = ? WHERE order_id = ?`,
		exchangeOrderID, orderID)
	if err != nil {
		return err
	}
	return oneRow(res, "order", orderID)
}

// statusRank is the order FSM rank table (live-oms-and-reconciler.md §FSM):
// transitions are MONOTONE in rank and rank-3 statuses are terminal.
var statusRank = map[string]int{
	"pending_new": 0, "open": 1, "partially_filled": 2,
	"filled": 3, "canceled": 3, "rejected": 3, "expired": 3,
}

// RecordOrderStatus advances orders.status per the FSM: a write with rank
// lower than or equal to the current status is a no-op returning the
// current status (regressions drop; terminal statuses are immutable).
// Returns the status now on the row; ErrNotFound for an unknown order.
func (s *Store) RecordOrderStatus(orderID, status string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer rollback(tx)
	now, err := recordOrderStatus(tx, orderID, status)
	if err != nil {
		return "", err
	}
	return now, tx.Commit()
}

func recordOrderStatus(q dbtx, orderID, status string) (string, error) {
	newRank, ok := statusRank[status]
	if !ok {
		return "", fmt.Errorf("unknown order status %q", status)
	}
	var current string
	err := q.QueryRow(`SELECT status FROM orders WHERE order_id = ?`, orderID).Scan(&current)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("order %s: %w", orderID, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if newRank <= statusRank[current] {
		return current, nil // regressive or terminal: no-op
	}
	if _, err := q.Exec(`UPDATE orders SET status = ? WHERE order_id = ?`, status, orderID); err != nil {
		return "", err
	}
	return status, nil
}

// RecordProtectiveSatisfied sets protective_obligations.satisfied_at (the
// row's ONLY legal mutation: the protective was acked at the correct
// cumulative size). A second call is a no-op; ErrNotFound: no such row.
func (s *Store) RecordProtectiveSatisfied(obligationID, satisfiedAt string) error {
	return resolveOnce(s.db,
		`UPDATE protective_obligations SET satisfied_at = ?
			WHERE obligation_id = ? AND satisfied_at IS NULL`,
		`SELECT COUNT(*) FROM protective_obligations WHERE obligation_id = ?`,
		"protective obligation", obligationID, satisfiedAt)
}

// RecordFeeConverted sets pending_fill_fees.converted_at (the row's ONLY
// legal mutation: a fresh mark converted the deferred fee). A second call
// is a no-op; ErrNotFound: no such row.
func (s *Store) RecordFeeConverted(fillID, convertedAt string) error {
	return resolveOnce(s.db,
		`UPDATE pending_fill_fees SET converted_at = ?
			WHERE fill_id = ? AND converted_at IS NULL`,
		`SELECT COUNT(*) FROM pending_fill_fees WHERE fill_id = ?`,
		"pending fill fee", fillID, convertedAt)
}

// resolveOnce runs the guarded one-shot resolution UPDATE (ts, id args):
// one row resolved is success, an already-resolved row is an idempotent
// no-op, and a missing row is ErrNotFound (existsSQL takes id).
func resolveOnce(q dbtx, updateSQL, existsSQL, kind, id, ts string) error {
	res, err := q.Exec(updateSQL, ts, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var exists int
	if err := q.QueryRow(existsSQL, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%s %s: %w", kind, id, ErrNotFound)
	}
	return nil
}

const orderIntentSelect = `SELECT client_order_id, intent_token, attempt, order_id, strategy_id,
	symbol, venue_symbol, side, type, qty_base, limit_price, stop_price, origin, proposal_id,
	kill_epoch, journaled_at, claimed_at, claim_revoked_at FROM order_intents`

// scanOrderIntent scans one order_intents row (orderIntentSelect order).
func scanOrderIntent(row rowScanner) (OrderIntent, error) {
	var i OrderIntent
	var limitPrice, stopPrice, proposalID, claimedAt, claimRevokedAt sql.NullString
	if err := row.Scan(&i.ClientOrderID, &i.IntentToken, &i.Attempt, &i.OrderID, &i.StrategyID,
		&i.Symbol, &i.VenueSymbol, &i.Side, &i.Type, &i.QtyBase, &limitPrice, &stopPrice,
		&i.Origin, &proposalID, &i.KillEpoch, &i.JournaledAt, &claimedAt, &claimRevokedAt); err != nil {
		return OrderIntent{}, err
	}
	i.LimitPrice = nullable(limitPrice)
	i.StopPrice = nullable(stopPrice)
	i.ProposalID = nullable(proposalID)
	i.ClaimedAt = nullable(claimedAt)
	i.ClaimRevokedAt = nullable(claimRevokedAt)
	return i, nil
}

// ListPendingNewIntents returns the LATEST attempt (orders.client_order_id)
// of every pending_new order, claim state included — the Reconciler R2
// resolution set — in deterministic (journaled_at, client_order_id) order.
func (s *Store) ListPendingNewIntents() ([]OrderIntent, error) {
	rows, err := s.db.Query(orderIntentSelect + ` WHERE client_order_id IN
		(SELECT client_order_id FROM orders
		 WHERE status = 'pending_new' AND client_order_id IS NOT NULL)
		ORDER BY journaled_at, client_order_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrderIntent
	for rows.Next() {
		i, err := scanOrderIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ListOpenProtectiveObligations returns every unsatisfied obligation,
// due_at ascending: the startup re-arm set; callers compare due_at to now
// for deadline breaches.
func (s *Store) ListOpenProtectiveObligations() ([]ProtectiveObligation, error) {
	rows, err := s.db.Query(`SELECT obligation_id, entry_order_id, strategy_id, kind,
		due_at, created_at, satisfied_at
		FROM protective_obligations WHERE satisfied_at IS NULL
		ORDER BY due_at, obligation_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProtectiveObligation
	for rows.Next() {
		var o ProtectiveObligation
		var satisfiedAt sql.NullString
		if err := rows.Scan(&o.ObligationID, &o.EntryOrderID, &o.StrategyID, &o.Kind,
			&o.DueAt, &o.CreatedAt, &satisfiedAt); err != nil {
			return nil, err
		}
		o.SatisfiedAt = nullable(satisfiedAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListUnconvertedPendingFillFees returns every deferred fee conversion not
// yet applied, oldest first (retried on every recon run).
func (s *Store) ListUnconvertedPendingFillFees() ([]PendingFillFee, error) {
	rows, err := s.db.Query(`SELECT fill_id, commission, commission_asset, recorded_at, converted_at
		FROM pending_fill_fees WHERE converted_at IS NULL
		ORDER BY recorded_at, fill_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingFillFee
	for rows.Next() {
		var f PendingFillFee
		var convertedAt sql.NullString
		if err := rows.Scan(&f.FillID, &f.Commission, &f.CommissionAsset,
			&f.RecordedAt, &convertedAt); err != nil {
			return nil, err
		}
		f.ConvertedAt = nullable(convertedAt)
		out = append(out, f)
	}
	return out, rows.Err()
}

// CurrentVenueEpoch returns the row with MAX(venue_epoch); ok=false when no
// epoch exists yet (the live OMS has never started).
func (s *Store) CurrentVenueEpoch() (VenueEpoch, bool, error) {
	var e VenueEpoch
	err := s.db.QueryRow(`SELECT venue_epoch, started_at, reason, details_json
		FROM venue_epochs ORDER BY venue_epoch DESC LIMIT 1`).
		Scan(&e.VenueEpoch, &e.StartedAt, &e.Reason, &e.DetailsJSON)
	if err == sql.ErrNoRows {
		return VenueEpoch{}, false, nil
	}
	if err != nil {
		return VenueEpoch{}, false, err
	}
	return e, true, nil
}

// FillWatermark returns MAX(fills.exchange_trade_id) for one
// (venue_epoch, venue_symbol) — the R5 gap-detection watermark, derived and
// monotone within an epoch; ok=false means cold start (no venue fills yet).
func (s *Store) FillWatermark(venueEpoch int64, venueSymbol string) (int64, bool, error) {
	var w sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(exchange_trade_id) FROM fills
		WHERE venue_epoch = ? AND venue_symbol = ? AND exchange_trade_id IS NOT NULL`,
		venueEpoch, venueSymbol).Scan(&w)
	if err != nil {
		return 0, false, err
	}
	return w.Int64, w.Valid, nil
}

// OMSReconEventFilter narrows ListOMSReconEvents; zero values mean no
// constraint (Limit <= 0: unbounded).
type OMSReconEventFilter struct {
	RunID      string
	Kind       string
	StrategyID string
	Limit      int
}

// ListOMSReconEvents returns recon audit rows matching the filter in
// insertion (rowid) order.
func (s *Store) ListOMSReconEvents(f OMSReconEventFilter) ([]OMSReconEvent, error) {
	q := `SELECT event_id, kind, run_id, strategy_id, symbol, client_order_id,
		exchange_order_id, exchange_trade_id, details_json, recorded_at
		FROM oms_recon_events`
	var conds []string
	var args []any
	if f.RunID != "" {
		conds, args = append(conds, `run_id = ?`), append(args, f.RunID)
	}
	if f.Kind != "" {
		conds, args = append(conds, `kind = ?`), append(args, f.Kind)
	}
	if f.StrategyID != "" {
		conds, args = append(conds, `strategy_id = ?`), append(args, f.StrategyID)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY rowid`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OMSReconEvent
	for rows.Next() {
		var e OMSReconEvent
		var runID, strategyID, symbol, clientOrderID, exchangeOrderID sql.NullString
		var tradeID sql.NullInt64
		if err := rows.Scan(&e.EventID, &e.Kind, &runID, &strategyID, &symbol, &clientOrderID,
			&exchangeOrderID, &tradeID, &e.DetailsJSON, &e.RecordedAt); err != nil {
			return nil, err
		}
		e.RunID = nullable(runID)
		e.StrategyID = nullable(strategyID)
		e.Symbol = nullable(symbol)
		e.ClientOrderID = nullable(clientOrderID)
		e.ExchangeOrderID = nullable(exchangeOrderID)
		if tradeID.Valid {
			e.ExchangeTradeID = &tradeID.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordVenueFill books one venue fill atomically (invariant 8): the
// explicit (venue_epoch, venue_symbol, exchange_trade_id) dedup check and
// the fill INSERT, the order's fill bookkeeping (fill_price = the VWAP of
// its booked fills, filled_at = this fill's fill_ts), the monotone FSM
// advance to status, and the caller's accounting application, all in ONE
// transaction conditional on the dedup check finding no prior row. Returns
// false when the fill is a replay: the whole call is a no-op (stream,
// backfill, and replays converge on the same rows). The dedup is an
// explicit SELECT rather than INSERT OR IGNORE so only a genuine
// trade-key replay is silent — any other constraint violation (fill_id
// collision, FK failure) surfaces as an error instead of masquerading as
// a replay.
func (s *Store) RecordVenueFill(f VenueFill, status string, book func(*SweepTx) error) (bool, error) {
	if _, ok := statusRank[status]; !ok {
		return false, fmt.Errorf("unknown order status %q", status)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	var dup int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM fills
		WHERE venue_epoch = ? AND venue_symbol = ? AND exchange_trade_id = ?`,
		f.VenueEpoch, f.VenueSymbol, f.ExchangeTradeID).Scan(&dup); err != nil {
		return false, err
	}
	if dup > 0 {
		return false, tx.Commit() // replay: dedup no-op
	}
	if _, err := tx.Exec(`INSERT INTO fills
		(fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts,
		 venue_symbol, exchange_trade_id, venue_epoch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.FillID, f.OrderID, f.QtyBase, f.FillPrice, f.FeeQuote, f.FillTS,
		f.VenueSymbol, f.ExchangeTradeID, f.VenueEpoch); err != nil {
		return false, err
	}
	vwap, err := fillVWAP(tx, f.OrderID)
	if err != nil {
		return false, err
	}
	upd, err := tx.Exec(`UPDATE orders SET fill_price = ?, filled_at = ? WHERE order_id = ?`,
		vwap, f.FillTS, f.OrderID)
	if err != nil {
		return false, err
	}
	if err := oneRow(upd, "order", f.OrderID); err != nil {
		return false, err
	}
	if _, err := recordOrderStatus(tx, f.OrderID, status); err != nil {
		return false, err
	}
	if book != nil {
		if err := book(&SweepTx{tx: tx}); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// fillVWAP computes SUM(qty*price)/SUM(qty) over the order's booked fills,
// rounded half away from zero to 8 decimals (market-data.md §Rounding).
func fillVWAP(q dbtx, orderID string) (string, error) {
	rows, err := q.Query(`SELECT qty_base, fill_price FROM fills WHERE order_id = ?`, orderID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	sumQty, sumNotional := decimal.Zero, decimal.Zero
	for rows.Next() {
		var qty, price string
		if err := rows.Scan(&qty, &price); err != nil {
			return "", err
		}
		qd, err := decimal.NewFromString(qty)
		if err != nil {
			return "", fmt.Errorf("fills.qty_base %q: %w", qty, err)
		}
		pd, err := decimal.NewFromString(price)
		if err != nil {
			return "", fmt.Errorf("fills.fill_price %q: %w", price, err)
		}
		sumQty = sumQty.Add(qd)
		sumNotional = sumNotional.Add(qd.Mul(pd))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if sumQty.Sign() <= 0 {
		return "", fmt.Errorf("order %s: VWAP over non-positive filled quantity", orderID)
	}
	return sumNotional.Div(sumQty).Round(8).String(), nil
}

const liveOrderSelect = `SELECT order_id, proposal_id, origin, strategy_id, symbol, class, side, type,
	reduce_only, qty_base, limit_price, stop_price, take_profit, fill_price, kill_epoch,
	status, submitted_at, filled_at, client_order_id, exchange_order_id FROM orders`

// scanLiveOrder scans one orders row plus live columns (liveOrderSelect).
func scanLiveOrder(row rowScanner) (LiveOrder, error) {
	var o LiveOrder
	var pid, limitPrice, stopPrice, takeProfit, fillPrice, filledAt sql.NullString
	var clientOrderID, exchangeOrderID sql.NullString
	if err := row.Scan(&o.OrderID, &pid, &o.Origin, &o.StrategyID, &o.Symbol, &o.Class,
		&o.Side, &o.Type, &o.ReduceOnly, &o.QtyBase, &limitPrice, &stopPrice, &takeProfit,
		&fillPrice, &o.KillEpoch, &o.Status, &o.SubmittedAt, &filledAt,
		&clientOrderID, &exchangeOrderID); err != nil {
		return LiveOrder{}, err
	}
	o.ProposalID = nullable(pid)
	o.LimitPrice = nullable(limitPrice)
	o.StopPrice = nullable(stopPrice)
	o.TakeProfit = nullable(takeProfit)
	o.FillPrice = nullable(fillPrice)
	o.FilledAt = nullable(filledAt)
	o.ClientOrderID = nullable(clientOrderID)
	o.ExchangeOrderID = nullable(exchangeOrderID)
	return o, nil
}

// GetLiveOrderByClientOrderID resolves the order whose LATEST attempt id is
// clientOrderID (stream/R3/R5 attribution), or ErrNotFound.
func (s *Store) GetLiveOrderByClientOrderID(clientOrderID string) (LiveOrder, error) {
	o, err := scanLiveOrder(s.db.QueryRow(liveOrderSelect+` WHERE client_order_id = ?`, clientOrderID))
	if err == sql.ErrNoRows {
		return LiveOrder{}, fmt.Errorf("order with client_order_id %s: %w", clientOrderID, ErrNotFound)
	}
	return o, err
}

// GetLiveOrderByExchangeOrderID resolves the order acked with
// exchangeOrderID on the canonical symbol (myTrades attributes by orderId;
// venue order ids are unique per symbol), or ErrNotFound.
func (s *Store) GetLiveOrderByExchangeOrderID(symbol, exchangeOrderID string) (LiveOrder, error) {
	o, err := scanLiveOrder(s.db.QueryRow(liveOrderSelect+
		` WHERE symbol = ? AND exchange_order_id = ?`, symbol, exchangeOrderID))
	if err == sql.ErrNoRows {
		return LiveOrder{}, fmt.Errorf("order with exchange_order_id %s %s: %w",
			symbol, exchangeOrderID, ErrNotFound)
	}
	return o, err
}

// ListNonTerminalLiveOrders returns every live (client_order_id NOT NULL)
// order at rank 0-2 in deterministic (submitted_at, order_id) order — the
// Reconciler R2/R4 working set.
func (s *Store) ListNonTerminalLiveOrders() ([]LiveOrder, error) {
	rows, err := s.db.Query(liveOrderSelect + ` WHERE client_order_id IS NOT NULL
		AND status IN ('pending_new', 'open', 'partially_filled')
		ORDER BY submitted_at, order_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveOrder
	for rows.Next() {
		o, err := scanLiveOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetOrderIntent returns one write-ahead journal row by attempt id
// (step-8 poisoned-late attribution), or ErrNotFound.
func (s *Store) GetOrderIntent(clientOrderID string) (OrderIntent, error) {
	i, err := scanOrderIntent(s.db.QueryRow(orderIntentSelect+
		` WHERE client_order_id = ?`, clientOrderID))
	if err == sql.ErrNoRows {
		return OrderIntent{}, fmt.Errorf("order intent %s: %w", clientOrderID, ErrNotFound)
	}
	return i, err
}

// ListFilledProtectiveEntries returns every live ENTRY order that carries a
// protective obligation (stop_price or take_profit) AND has at least one
// booked fill — the §Protective order lifecycle working set. Startup re-arm
// derives coverage from orders joined to fills (not from timer rows), so a
// crash between a fill booking and its obligation insert still converges to
// protected-or-flat.
func (s *Store) ListFilledProtectiveEntries() ([]LiveOrder, error) {
	rows, err := s.db.Query(liveOrderSelect + ` WHERE class = 'ENTRY'
		AND client_order_id IS NOT NULL
		AND (stop_price IS NOT NULL OR take_profit IS NOT NULL)
		AND EXISTS (SELECT 1 FROM fills WHERE fills.order_id = orders.order_id)
		ORDER BY submitted_at, order_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveOrder
	for rows.Next() {
		o, err := scanLiveOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetLiveOrderForFill resolves the order a booked fill belongs to (deferred
// fee retry re-derives strategy/symbol from the fill), or ErrNotFound.
func (s *Store) GetLiveOrderForFill(fillID string) (LiveOrder, error) {
	var orderID string
	err := s.db.QueryRow(`SELECT order_id FROM fills WHERE fill_id = ?`, fillID).Scan(&orderID)
	if err == sql.ErrNoRows {
		return LiveOrder{}, fmt.Errorf("fill %s: %w", fillID, ErrNotFound)
	}
	if err != nil {
		return LiveOrder{}, err
	}
	o, err := scanLiveOrder(s.db.QueryRow(liveOrderSelect+` WHERE order_id = ?`, orderID))
	if err == sql.ErrNoRows {
		return LiveOrder{}, fmt.Errorf("order %s: %w", orderID, ErrNotFound)
	}
	return o, err
}

// BreakerActiveToday derives the circuit-breaker halt predicate: a
// kill_breaker_events row with kind='breaker' binding the strategy (its
// own, its tenant's, or Phase-1 global scope) whose recorded_at date equals
// the given UTC day. The 00:00 UTC auto-reset falls out of the derivation
// (live-oms-and-reconciler.md §Safety-engine integration).
func (s *Store) BreakerActiveToday(strategyID, utcDate string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM kill_breaker_events
		WHERE kind = 'breaker' AND substr(recorded_at, 1, 10) = ?
		AND ((strategy_id IS NULL AND tenant_id IS NULL)
			OR strategy_id = ?
			OR (tenant_id = (SELECT tenant_id FROM strategies WHERE strategy_id = ?) AND strategy_id IS NULL))`,
		utcDate, strategyID, strategyID).Scan(&n)
	return n > 0, err
}

// ListFillsByOrder returns an order's booked fills (venue columns included;
// paper rows read zero venue values) in insertion (rowid) order — the
// derived executed quantity is SUM(qty_base) over them.
func (s *Store) ListFillsByOrder(orderID string) ([]VenueFill, error) {
	rows, err := s.db.Query(`SELECT fill_id, order_id, qty_base, fill_price, fee_quote, fill_ts,
		venue_symbol, exchange_trade_id, venue_epoch
		FROM fills WHERE order_id = ? ORDER BY rowid`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VenueFill
	for rows.Next() {
		var f VenueFill
		var venueSymbol sql.NullString
		var tradeID sql.NullInt64
		if err := rows.Scan(&f.FillID, &f.OrderID, &f.QtyBase, &f.FillPrice, &f.FeeQuote,
			&f.FillTS, &venueSymbol, &tradeID, &f.VenueEpoch); err != nil {
			return nil, err
		}
		f.VenueSymbol = venueSymbol.String
		f.ExchangeTradeID = tradeID.Int64
		out = append(out, f)
	}
	return out, rows.Err()
}
