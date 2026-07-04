package omsbridge

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// persistBooksTx upserts every changed (strategy, symbol) book and advances
// the strategy_state snapshot of each strategy that realized PnL or booked
// a fill, inside the sweep transaction. Unchanged books write nothing;
// changed positions are staged into books (merged into b.books only after
// the transaction commits). Requires b.mu.
func (b *Bridge) persistBooksTx(tx *store.SweepTx, touched map[string]bool, books map[bookKey]paper.Position, now time.Time) error {
	deltas := make(map[string]decimal.Decimal)
	for _, book := range b.oms.Books() {
		key := bookKey{book.StrategyID, book.Symbol}
		prev, known := b.books[key]
		if known && samePosition(prev, book.Position) {
			continue
		}
		if err := tx.UpsertPosition(positionRow(book, now)); err != nil {
			return err
		}
		prevRealized := decimal.Zero
		if known {
			prevRealized = prev.RealizedPnLQuote
		}
		if d := book.RealizedPnLQuote.Sub(prevRealized); !d.IsZero() {
			deltas[book.StrategyID] = deltas[book.StrategyID].Add(d)
			touched[book.StrategyID] = true
		}
		books[key] = book.Position
	}
	ids := make([]string, 0, len(touched))
	for sid := range touched {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	for _, sid := range ids {
		if err := b.writeStrategyState(tx, sid, deltas[sid], now); err != nil {
			return err
		}
	}
	return nil
}

// writeStrategyState applies the realized delta to the strategy's snapshot
// with the normative rollover math: a row from an earlier UTC day restarts
// the daily figure at zero before the delta lands; peak is monotone; a
// missing row seeds equity/peak at the allocated capital.
func (b *Bridge) writeStrategyState(tx *store.SweepTx, strategyID string, delta decimal.Decimal, now time.Time) error {
	equity, peak, daily := b.allocated, b.allocated, decimal.Zero
	row, ok, err := tx.GetStrategyState(strategyID)
	if err != nil {
		return err
	}
	if ok {
		if equity, err = parseDec("strategy_state.equity_quote", row.EquityQuote); err != nil {
			return err
		}
		if peak, err = parseDec("strategy_state.peak_equity_quote", row.PeakEquityQuote); err != nil {
			return err
		}
		if row.UTCDate == utcDate(now) {
			if daily, err = parseDec("strategy_state.daily_realized_pnl_quote", row.DailyRealizedPnLQuote); err != nil {
				return err
			}
		}
	}
	equity = equity.Add(delta)
	daily = daily.Add(delta)
	peak = decimal.Max(peak, equity)
	return tx.UpsertStrategyState(store.StrategyState{
		StrategyID:            strategyID,
		EquityQuote:           equity.String(),
		PeakEquityQuote:       peak.String(),
		DailyRealizedPnLQuote: daily.String(),
		UTCDate:               utcDate(now),
		UpdatedAt:             formatTime(now),
	})
}

func insertFill(tx *store.SweepTx, ord paper.Order, now time.Time) error {
	return tx.InsertFill(store.Fill{
		FillID:    uuid.NewString(),
		OrderID:   ord.ID,
		QtyBase:   ord.QtyBase.String(),
		FillPrice: ord.FillPrice.String(),
		FeeQuote:  ord.FeeQuote.String(),
		FillTS:    formatTime(now),
	})
}

// orderRow maps a paper order to its store row. Every order the bridge
// creates is proposal-originated (safety paths are not wired in Phase 1),
// satisfying the proposal_id NOT NULL iff origin='proposal' rule.
func orderRow(ord paper.Order, proposalID string, now time.Time) store.Order {
	row := store.Order{
		OrderID:     ord.ID,
		ProposalID:  &proposalID,
		Origin:      "proposal",
		StrategyID:  ord.StrategyID,
		Symbol:      ord.Symbol,
		Class:       string(ord.Class),
		Side:        string(ord.Side),
		Type:        ord.Type,
		ReduceOnly:  ord.ReduceOnly,
		QtyBase:     ord.QtyBase.String(),
		KillEpoch:   ord.KillEpoch,
		Status:      string(ord.Status),
		SubmittedAt: formatTime(now),
	}
	row.LimitPrice = optDec(ord.LimitPrice)
	row.StopPrice = optDec(ord.StopPrice)
	row.TakeProfit = optDec(ord.TakeProfit)
	if ord.Status == paper.StatusFilled {
		fp, ts := ord.FillPrice.String(), formatTime(now)
		row.FillPrice, row.FilledAt = &fp, &ts
	}
	return row
}

func positionRow(book paper.Book, now time.Time) store.Position {
	return store.Position{
		StrategyID:       book.StrategyID,
		Symbol:           book.Symbol,
		QtyBase:          book.QtyBase.String(),
		EntryPrice:       book.EntryPrice.String(),
		FeesQuote:        book.FeesQuote.String(),
		RealizedPnLQuote: book.RealizedPnLQuote.String(),
		UpdatedAt:        formatTime(now),
	}
}

func samePosition(a, b paper.Position) bool {
	return a.QtyBase.Equal(b.QtyBase) && a.EntryPrice.Equal(b.EntryPrice) &&
		a.FeesQuote.Equal(b.FeesQuote) && a.RealizedPnLQuote.Equal(b.RealizedPnLQuote)
}

func parseDec(field, v string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("omsbridge: %s %q: %w", field, v, err)
	}
	return d, nil
}
