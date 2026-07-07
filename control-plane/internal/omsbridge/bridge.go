// Package omsbridge connects the in-memory paper OMS to the control-plane
// store: it is the serve-mode Submitter (api.Submitter seam), the per-tick
// trigger-sweep driver, and the persistence writer that keeps orders,
// fills, positions, and the strategy_state realized-equity snapshot in sync
// with every OMS action. On startup it re-hydrates the OMS from the store
// (open orders re-armed, books restored with realized PnL, per-strategy
// kill epochs restored) so restarts are lossless.
package omsbridge

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// MarkSource is the freshness-checked last-tick cache;
// *marketdata.Store satisfies it.
type MarkSource interface {
	Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool)
}

// Config wires a Bridge. All fields are required except Now (defaults to
// time.Now).
type Config struct {
	Store *store.Store
	Marks MarkSource
	// FillModel is the REQUIRED fill model v2 (market-data.md): no hidden
	// defaults, validated by paper.New.
	FillModel paper.FillModel
	// AllocatedCapitalQuote seeds a strategy's realized-equity snapshot on
	// its first fill (risk-limits.md Definitions).
	AllocatedCapitalQuote decimal.Decimal
	Now                   func() time.Time
}

type bookKey struct{ strategyID, symbol string }

// Bridge owns the paper OMS and mirrors its state into the store. One
// mutex serializes OMS actions with their persistence so rows never
// interleave across concurrent submissions and sweeps.
type Bridge struct {
	st        *store.Store
	marks     MarkSource
	now       func() time.Time
	allocated decimal.Decimal

	mu  sync.Mutex
	oms *paper.OMS
	// statuses is the persisted order FSM state; books the persisted book
	// snapshots. Diffing the OMS against them yields exactly the rows each
	// action changed. proposals maps every known order to its originating
	// proposal so sweep-time protectives inherit the right audit link.
	statuses  map[string]paper.Status
	books     map[bookKey]paper.Position
	proposals map[string]string
}

// New builds the bridge and re-hydrates the paper OMS from the store.
func New(cfg Config) (*Bridge, error) {
	if cfg.Store == nil || cfg.Marks == nil {
		return nil, errors.New("omsbridge: Store and Marks are required")
	}
	if cfg.AllocatedCapitalQuote.Sign() <= 0 {
		return nil, errors.New("omsbridge: AllocatedCapitalQuote must be > 0 (equity seed, risk-limits.md)")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	oms, err := paper.New(cfg.FillModel)
	if err != nil {
		return nil, err
	}
	b := &Bridge{
		st:        cfg.Store,
		marks:     cfg.Marks,
		now:       cfg.Now,
		allocated: cfg.AllocatedCapitalQuote,
		oms:       oms,
		statuses:  make(map[string]paper.Status),
		books:     make(map[bookKey]paper.Position),
		proposals: make(map[string]string),
	}
	if err := b.hydrate(); err != nil {
		return nil, err
	}
	return b, nil
}

// hydrate restores every strategy's books and open orders plus its own
// highest persisted kill epoch into the fresh OMS.
// GlobalMaxKillEpoch(strategyID) is the same per-strategy binding max
// SubmitApproved stamps (the strategy's own, its tenant's, and platform
// kills — multi-tenant-rbac.md normative predicate, platform-scope rows
// included), so a restart never couples one strategy's epoch to another's.
func (b *Bridge) hydrate() error {
	for page := 1; ; page++ {
		strategies, total, err := b.st.ListStrategies(page, store.MaxPageLimit)
		if err != nil {
			return err
		}
		for _, s := range strategies {
			if err := b.hydrateStrategy(s.StrategyID); err != nil {
				return err
			}
			epoch, err := b.st.GlobalMaxKillEpoch(s.StrategyID)
			if err != nil {
				return err
			}
			b.oms.RestoreKillEpoch(s.StrategyID, epoch)
		}
		if page*store.MaxPageLimit >= total || len(strategies) == 0 {
			break
		}
	}
	return nil
}

func (b *Bridge) hydrateStrategy(strategyID string) error {
	positions, err := b.st.ListPositions(strategyID)
	if err != nil {
		return err
	}
	for _, row := range positions {
		pos, err := paperPosition(row)
		if err != nil {
			return err
		}
		b.oms.RestorePosition(strategyID, pos)
		b.books[bookKey{strategyID, row.Symbol}] = pos
	}
	orders, err := b.st.ListOpenOrders(strategyID)
	if err != nil {
		return err
	}
	for _, row := range orders {
		ord, err := paperOrder(row)
		if err != nil {
			return err
		}
		if err := b.oms.RestoreOrder(ord); err != nil {
			return fmt.Errorf("omsbridge: %w", err)
		}
		b.statuses[row.OrderID] = paper.StatusOpen
		if row.ProposalID != nil {
			b.proposals[row.OrderID] = *row.ProposalID
		}
	}
	return nil
}

// paperPosition maps a positions row back into a paper OMS book, realized
// PnL included (it survives flat books and restarts).
func paperPosition(row store.Position) (paper.Position, error) {
	pos := paper.Position{Symbol: row.Symbol}
	var err error
	if pos.QtyBase, err = parseDec("positions.qty_base", row.QtyBase); err != nil {
		return paper.Position{}, err
	}
	if pos.EntryPrice, err = parseDec("positions.entry_price", row.EntryPrice); err != nil {
		return paper.Position{}, err
	}
	if pos.FeesQuote, err = parseDec("positions.fees_quote", row.FeesQuote); err != nil {
		return paper.Position{}, err
	}
	if pos.RealizedPnLQuote, err = parseDec("positions.realized_pnl_quote", row.RealizedPnLQuote); err != nil {
		return paper.Position{}, err
	}
	return pos, nil
}

// paperOrder maps an open orders row back into a re-armed paper order:
// resting limits keep their SL/TP obligations (stop_price, take_profit) so
// a post-restart fill still places its protectives.
func paperOrder(row store.Order) (paper.Order, error) {
	ord := paper.Order{
		ID:         row.OrderID,
		StrategyID: row.StrategyID,
		Symbol:     row.Symbol,
		Class:      paper.Class(row.Class),
		Side:       paper.Side(row.Side),
		Type:       row.Type,
		ReduceOnly: row.ReduceOnly,
		KillEpoch:  row.KillEpoch,
		Status:     paper.Status(row.Status),
	}
	var err error
	if ord.QtyBase, err = parseDec("orders.qty_base", row.QtyBase); err != nil {
		return paper.Order{}, err
	}
	if ord.LimitPrice, err = parseOptDec("orders.limit_price", row.LimitPrice); err != nil {
		return paper.Order{}, err
	}
	if ord.StopPrice, err = parseOptDec("orders.stop_price", row.StopPrice); err != nil {
		return paper.Order{}, err
	}
	if ord.TakeProfit, err = parseOptDec("orders.take_profit", row.TakeProfit); err != nil {
		return paper.Order{}, err
	}
	return ord, nil
}

func parseOptDec(field string, v *string) (decimal.Decimal, error) {
	if v == nil {
		return decimal.Zero, nil
	}
	return parseDec(field, *v)
}

// optDec renders a strictly positive decimal as a nullable column value.
func optDec(d decimal.Decimal) *string {
	if d.Sign() <= 0 {
		return nil
	}
	s := d.String()
	return &s
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// utcDate is the YYYY-MM-DD UTC day of t (00:00 UTC boundary).
func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}
