package store

// Lifecycle-API reads (docs/specs/lifecycle-api.md §Store surface): paused
// provenance (LC-7), the paper-gate window (LC-16), and the replay's fill
// join (LC-18). Every "newest lifecycle_transitions row" read orders
// ORDER BY recorded_at DESC, rowid DESC — the rowid breaks second-precision
// timestamp ties.

import "database/sql"

// PausedProvenance returns the from_state of the strategy's NEWEST
// to_state='paused' lifecycle_transitions row (LC-7: that row is
// necessarily the entry into the current paused period); ok=false when no
// such row exists (unknown provenance — the machine's paper-only exit).
func (s *Store) PausedProvenance(strategyID string) (string, bool, error) {
	return pausedProvenance(s.db, strategyID)
}

// pausedProvenance is the LC-7 read over dbtx: the lifecycle handler's
// resume path (via PausedProvenance) and the SafetyStatus snapshot
// transaction (operator-surface.md OS-7) share this one SQL text.
func pausedProvenance(q dbtx, strategyID string) (string, bool, error) {
	var from string
	err := q.QueryRow(`SELECT from_state FROM lifecycle_transitions
		WHERE strategy_id = ? AND to_state = 'paused'
		ORDER BY recorded_at DESC, rowid DESC LIMIT 1`, strategyID).Scan(&from)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return from, true, nil
}

// PaperWindowStart is the FULL LC-16 evaluation: S = recorded_at of the
// NEWEST QUALIFYING to_state='paper' row. A row QUALIFIES iff its
// from_state != 'paused' (first promotion, live demotion, killed unlock),
// OR from_state = 'paused' AND the newest earlier to_state='paused' row
// has from_state = 'killed' (the paused-after-kill exit), OR
// from_state = 'paused' AND a binding kill K exists AND no earlier
// qualifying paper row has recorded_at >= K (the audited re-entry after an
// in-place kill). ok=false fails the gate closed: no qualifying row, or a
// binding kill with no qualifying row at recorded_at >= K. K is the newest
// kind='kill' kill_breaker_events row binding the strategy under the LC-28
// 3-clause match, cleared or not — the counter reset holds by
// construction: no pre-kill fill can ever sit inside the window.
func (s *Store) PaperWindowStart(strategyID string) (string, bool, error) {
	var kill sql.NullString
	err := s.db.QueryRow(`SELECT recorded_at FROM kill_breaker_events e
		WHERE e.kind = 'kill' AND (e.strategy_id = ?1
			OR (e.strategy_id IS NULL AND e.tenant_id IS NOT NULL
				AND e.tenant_id = (SELECT tenant_id FROM strategies WHERE strategy_id = ?1))
			OR (e.strategy_id IS NULL AND e.tenant_id IS NULL))
		ORDER BY e.recorded_at DESC, e.rowid DESC LIMIT 1`, strategyID).Scan(&kill)
	if err != nil && err != sql.ErrNoRows {
		return "", false, err
	}
	rows, err := s.db.Query(`SELECT from_state, to_state, recorded_at
		FROM lifecycle_transitions
		WHERE strategy_id = ? AND to_state IN ('paper', 'paused')
		ORDER BY recorded_at, rowid`, strategyID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	var (
		start          string
		haveStart      bool
		anyQualSinceK  bool   // some qualifying row has recorded_at >= K
		lastPausedFrom string // newest earlier to_state='paused' row's from_state
	)
	for rows.Next() {
		var from, to, at string
		if err := rows.Scan(&from, &to, &at); err != nil {
			return "", false, err
		}
		if to == "paused" {
			lastPausedFrom = from
			continue
		}
		qualifies := from != "paused" ||
			lastPausedFrom == "killed" ||
			(kill.Valid && !anyQualSinceK)
		if !qualifies {
			continue
		}
		start, haveStart = at, true
		if kill.Valid && at >= kill.String {
			anyQualSinceK = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	if !haveStart || (kill.Valid && !anyQualSinceK) {
		return "", false, nil
	}
	return start, true, nil
}

// PaperGateFill is one LC-18 replay input row: a fill joined to its orders
// row's symbol/side/reduce_only, exactly the fields the closed-trade
// replay reconstructs books from.
type PaperGateFill struct {
	Symbol     string `json:"symbol"`
	Side       string `json:"side"`
	ReduceOnly bool   `json:"reduce_only"`
	QtyBase    string `json:"qty_base"`
	FillPrice  string `json:"fill_price"`
	FeeQuote   string `json:"fee_quote"`
	FillTS     string `json:"fill_ts"`
}

// ListPaperGateFills returns the strategy's fills with
// fill_ts >= sinceRFC3339 joined to their orders row, ordered
// (fill_ts, fills.rowid) — the LC-18 replay order. Read-only.
func (s *Store) ListPaperGateFills(strategyID, sinceRFC3339 string) ([]PaperGateFill, error) {
	rows, err := s.db.Query(`SELECT o.symbol, o.side, o.reduce_only,
		f.qty_base, f.fill_price, f.fee_quote, f.fill_ts
		FROM fills f JOIN orders o ON o.order_id = f.order_id
		WHERE o.strategy_id = ? AND f.fill_ts >= ?
		ORDER BY f.fill_ts, f.rowid`, strategyID, sinceRFC3339)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PaperGateFill
	for rows.Next() {
		var f PaperGateFill
		if err := rows.Scan(&f.Symbol, &f.Side, &f.ReduceOnly,
			&f.QtyBase, &f.FillPrice, &f.FeeQuote, &f.FillTS); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
