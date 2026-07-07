package paper

import (
	"fmt"
	"sort"
)

// Restart hydration seams (docs/specs/persistence-and-api.md Row rules): the
// OMS is in-memory, so a control-plane restart rebuilds it from the store —
// open orders re-armed (stop_price, take_profit), position books restored
// with their cumulative realized PnL. These methods only load state; they
// never book fills or place orders.

// RestoreOrder re-arms one persisted OPEN order after a restart: resting
// entry limits (with their SL/TP obligations), protective stops,
// take-profits, and queued exits all resume trigger processing on the next
// tick. Only open orders are restorable; terminal orders live in the store.
func (o *OMS) RestoreOrder(ord Order) error {
	if ord.ID == "" {
		return fmt.Errorf("restore order: empty order id")
	}
	if ord.Status != StatusOpen {
		return fmt.Errorf("restore order %s: status %q is not open", ord.ID, ord.Status)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, exists := o.orders[ord.ID]; exists {
		return fmt.Errorf("restore order %s: already present", ord.ID)
	}
	copied := ord
	o.orders[ord.ID] = &copied
	return nil
}

// RestorePosition restores the persisted (strategy, symbol) book snapshot,
// including flat books: RealizedPnLQuote must survive restarts so equity
// and the paper track record stay continuous.
func (o *OMS) RestorePosition(strategyID string, pos Position) {
	o.mu.Lock()
	defer o.mu.Unlock()
	copied := pos
	o.positions[positionKey{strategyID, pos.Symbol}] = &copied
}

// RestoreKillEpoch restores one strategy's persisted kill epoch WITHOUT the
// ENTRY cancel sweep of Kill (the cancels were persisted when the kill
// fired; hydration must not re-mutate restored state).
func (o *OMS) RestoreKillEpoch(strategyID string, epoch int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if epoch > o.killEpochs[strategyID] {
		o.killEpochs[strategyID] = epoch
	}
}

// Book pairs a strategy with one of its per-symbol position books.
type Book struct {
	StrategyID string
	Position
}

// Books returns a snapshot of every (strategy, symbol) book — INCLUDING
// flat ones, whose realized PnL must persist — sorted by (strategy_id,
// symbol) for deterministic persistence.
func (o *OMS) Books() []Book {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Book, 0, len(o.positions))
	for key, pos := range o.positions {
		out = append(out, Book{StrategyID: key.strategyID, Position: *pos})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StrategyID != out[j].StrategyID {
			return out[i].StrategyID < out[j].StrategyID
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}
