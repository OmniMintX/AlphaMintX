package omsbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/paper"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// SubmitApproved submits the gate-approved proposal behind a verdict to the
// paper OMS (api.Submitter seam: called at most once per verdict, on the
// single winning approved decision or the direct paper/L2/L3 path). The
// submission carries the CURRENT persisted kill epoch (OMS kill re-check);
// market entries with no fresh mark fail, while a close with no fresh mark
// is QUEUED, never filled (market-data.md §Exits fail-closed). All OMS
// effects are persisted even when the submission itself errors.
func (b *Bridge) SubmitApproved(meta store.VerdictMeta) error {
	rawP, err := b.st.GetProposalPayload(meta.ProposalID)
	if err != nil {
		return err
	}
	var p contract.Proposal
	if err := json.Unmarshal(rawP, &p); err != nil {
		return fmt.Errorf("omsbridge: proposal %s payload: %w", meta.ProposalID, err)
	}
	rawV, err := b.st.GetVerdictByProposalID(meta.ProposalID)
	if err != nil {
		return err
	}
	var v contract.Verdict
	if err := json.Unmarshal(rawV, &v); err != nil {
		return fmt.Errorf("omsbridge: verdict for %s payload: %w", meta.ProposalID, err)
	}
	epoch, err := b.st.GlobalMaxKillEpoch(meta.StrategyID)
	if err != nil {
		return err
	}
	now := b.now()
	mark := decimal.Zero
	if m, _, ok := b.marks.Mark(p.Symbol, now); ok {
		mark = m
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	var submitErr error
	switch p.Action {
	case contract.ActionOpenLong, contract.ActionOpenShort:
		size := p.SizeQuote.Decimal()
		if v.ClippedSizeQuote != nil {
			// The clipped size is the effective NOTIONAL cap at fill price.
			size = v.ClippedSizeQuote.Decimal()
		}
		side := paper.SideBuy
		if p.Action == contract.ActionOpenShort {
			side = paper.SideSell
		}
		req := paper.EntryRequest{
			StrategyID: p.StrategyID,
			Symbol:     p.Symbol,
			Side:       side,
			Type:       p.Entry.Type,
			SizeQuote:  size,
			MarkPrice:  mark,
			KillEpoch:  epoch,
		}
		if p.Entry.LimitPrice != nil {
			req.LimitPrice = p.Entry.LimitPrice.Decimal()
		}
		if p.StopLoss != nil {
			req.StopPrice = p.StopLoss.Decimal()
		}
		if p.TakeProfit != nil {
			req.TakeProfit = p.TakeProfit.Decimal()
		}
		_, submitErr = b.oms.SubmitEntry(req)
	case contract.ActionClose:
		// Reduce-only flatten; a zero mark queues the exit (order rests
		// open and retries on the next fresh tick via the sweep).
		_, submitErr = b.oms.Flatten(meta.StrategyID, p.Symbol, mark)
	default:
		return fmt.Errorf("omsbridge: action %q never submits an order", p.Action)
	}
	if err := b.persistLocked(&meta.ProposalID, now); err != nil {
		return err
	}
	return submitErr
}

// CancelOpenEntries cancels the paper book's resting un-filled ENTRY
// orders for the strategy and persists the status flips — reduce-only
// protectives untouched (lifecycle-api.md LC-12a: the paper half of the
// api.Config.EntryCanceler seam; a paused paper strategy stops filling its
// resting limit entries). The context parameter exists only to satisfy the
// shared seam signature the live OMS defines.
func (b *Bridge) CancelOpenEntries(_ context.Context, strategyID string) error {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.oms.CancelOpenEntries(strategyID)
	return b.persistLocked(nil, now)
}

// Sweep runs the deterministic per-tick trigger sweep over fresh marks and
// persists whatever it booked; a sweep that fills nothing writes zero rows.
// Every marketdata Store write MUST be followed by a Sweep call
// (market-data.md §Fill model v2).
func (b *Bridge) Sweep(marks map[string]decimal.Decimal) error {
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	_, tickErr := b.oms.ProcessTick(marks)
	if err := b.persistLocked(nil, now); err != nil {
		return err
	}
	return tickErr
}

// persistLocked mirrors the OMS deltas into the store in ONE ApplySweep
// transaction: new orders inserted (all proposal-originated in Phase 1 —
// either the submission in flight or, for protectives placed when a resting
// entry fills on a tick, the entry's own proposal), FSM changes applied
// (filled orders also append their fill row), changed books upserted, and
// each filled strategy's realized-equity snapshot advanced. The bridge's
// in-memory mirrors are staged and merged only after the transaction
// commits, so a rollback leaves memory and DB aligned. Requires b.mu.
func (b *Bridge) persistLocked(proposalID *string, now time.Time) error {
	orders := b.oms.Orders()
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	statuses := make(map[string]paper.Status)
	proposals := make(map[string]string)
	books := make(map[bookKey]paper.Position)
	err := b.st.ApplySweep(func(tx *store.SweepTx) error {
		touched := make(map[string]bool)
		// entryFills maps (strategy, symbol) to the proposal of the ENTRY
		// order that filled in this diff: sweep-created protectives inherit it.
		entryFills := make(map[bookKey]string)
		var created []paper.Order
		for _, ord := range orders {
			prev, known := b.statuses[ord.ID]
			if !known {
				created = append(created, ord)
				continue
			}
			if prev == ord.Status {
				continue
			}
			switch ord.Status {
			case paper.StatusFilled:
				if err := tx.RecordOrderFill(ord.ID, ord.FillPrice.String(), formatTime(now)); err != nil {
					return err
				}
				if err := insertFill(tx, ord, now); err != nil {
					return err
				}
				touched[ord.StrategyID] = true
				if ord.Class == paper.ClassEntry {
					entryFills[bookKey{ord.StrategyID, ord.Symbol}] = b.proposals[ord.ID]
				}
			case paper.StatusCanceled:
				if err := tx.RecordOrderCancel(ord.ID); err != nil {
					return err
				}
			}
			statuses[ord.ID] = ord.Status
		}
		for _, ord := range created {
			pid := entryFills[bookKey{ord.StrategyID, ord.Symbol}]
			if proposalID != nil {
				pid = *proposalID
			}
			if pid == "" {
				return fmt.Errorf("omsbridge: order %s has no originating proposal", ord.ID)
			}
			if err := tx.InsertOrder(orderRow(ord, pid, now)); err != nil {
				return err
			}
			if ord.Status == paper.StatusFilled {
				if err := insertFill(tx, ord, now); err != nil {
					return err
				}
				touched[ord.StrategyID] = true
			}
			statuses[ord.ID] = ord.Status
			proposals[ord.ID] = pid
		}
		return b.persistBooksTx(tx, touched, books, now)
	})
	if err != nil {
		return err
	}
	for id, st := range statuses {
		b.statuses[id] = st
	}
	for id, pid := range proposals {
		b.proposals[id] = pid
	}
	for key, pos := range books {
		b.books[key] = pos
	}
	return nil
}
