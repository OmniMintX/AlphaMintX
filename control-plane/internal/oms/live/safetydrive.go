package live

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// DriveSafetyEffects is the crash-resumable safety effects re-driver
// (safety-wiring.md §DriveSafetyEffects): one pass derives every unserved
// kill/breaker row's remaining effects from current store+venue state and
// re-executes them idempotently. It runs after every completed reconcile —
// BEFORE the protective drive — and on demand; passes serialize on the
// SAME driveMu as protective drives (on-demand invocations coalesce). No
// pass runs before Reconciled() — venue truth precedes safety sends
// (invariant 16) — and per-(event, strategy) errors never abort a pass:
// they are logged and counted as residual work (invariant 17). The error
// return satisfies the api.SafetyDriver and safety.Driver seams: only a
// failure to enumerate unserved rows surfaces (callers log, never wait).
func (o *OMS) DriveSafetyEffects(ctx context.Context) error {
	if !o.Reconciled() {
		return nil
	}
	o.driveMu.Lock()
	defer o.driveMu.Unlock()
	events, err := o.st.ListUnservedSafetyEvents()
	if err != nil {
		return fmt.Errorf("list unserved safety events: %w", err)
	}
	reconciledAt := o.lastReconcileCompletedAt()
	for _, ev := range events {
		o.driveSafetyEvent(ctx, ev, reconciledAt)
	}
	return nil
}

// lastReconcileCompletedAt returns the completion time of the last full
// reconcile run ("" before the first): the served-marker gate compares it
// to each event row's recorded_at (invariant 16).
func (o *OMS) lastReconcileCompletedAt() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastReconcileEnd.IsZero() {
		return ""
	}
	return formatTime(o.lastReconcileEnd)
}

// driveSafetyEvent serves ONE kill/breaker row: effects run for every
// affected strategy with per-(event, strategy) error isolation; the served
// marker is inserted only at ZERO residual work AND once the last
// completed reconcile finished STRICTLY after the row's recorded_at —
// otherwise the effects still ran and the marker defers to the next
// R7-hooked pass.
func (o *OMS) driveSafetyEvent(ctx context.Context, ev store.KillBreakerEvent, reconciledAt string) {
	strategies, err := o.affectedStrategies(ev)
	if err != nil {
		o.logf("live: safety drive: event %s: affected strategies: %v", ev.EventID, err)
		return // unserved: the next pass retries
	}
	residual := 0
	// Platform-scope rows (both ids NULL) run the GLOBAL cancel sweep ONCE
	// (spec §Driver step 2), not once per strategy; the per-strategy loop
	// below keeps the lifecycle locks and flattens, and its step-4 residual
	// accounting still counts in-scope non-terminal entries.
	platform := ev.StrategyID == nil && ev.TenantID == nil
	if platform {
		if err := o.CancelOpenEntries(ctx, ""); err != nil {
			o.logf("live: safety drive: event %s: global cancel sweep: %v (residual for the next pass)",
				ev.EventID, err)
			residual++
		}
	}
	for _, sid := range strategies {
		n, err := o.driveSafetyStrategy(ctx, ev, sid, !platform)
		if err != nil {
			// Error isolation (invariant 17): log, count, CONTINUE with
			// the next strategy — a pass never aborts early.
			o.logf("live: safety drive: event %s strategy %s: %v (residual for the next pass)",
				ev.EventID, sid, err)
		}
		residual += n
	}
	if residual > 0 {
		return
	}
	// Reconcile gate (invariant 16): a served marker asserts
	// venue-verified completion, so it requires a reconcile that finished
	// STRICTLY after the row was recorded — second-precision timestamps
	// from independent clocks make an equal-second tie ambiguous.
	if reconciledAt == "" || reconciledAt <= ev.RecordedAt {
		return
	}
	if err := o.st.AppendSafetyEffectDone(ev.EventID, formatTime(o.now())); err != nil {
		o.logf("live: safety drive: served marker for %s: %v", ev.EventID, err)
	}
}

// affectedStrategies resolves an event's affected strategy set (spec
// §Driver step 2): strategy scope => that strategy; tenant scope => every
// strategy of the row's tenant; platform (and Phase-1 global) scope =>
// every strategy. Per-strategy effects are idempotent no-ops for
// strategies with nothing in scope.
func (o *OMS) affectedStrategies(ev store.KillBreakerEvent) ([]string, error) {
	switch {
	case ev.StrategyID != nil:
		return []string{*ev.StrategyID}, nil
	case ev.TenantID != nil:
		return o.listStrategyIDs(func(page, limit int) ([]store.Strategy, int, error) {
			return o.st.ListStrategiesByTenant(*ev.TenantID, page, limit)
		})
	default:
		return o.listStrategyIDs(o.st.ListStrategies)
	}
}

// listStrategyIDs pages one strategy listing to completion.
func (o *OMS) listStrategyIDs(list func(page, limit int) ([]store.Strategy, int, error)) ([]string, error) {
	var out []string
	for page := 1; ; page++ {
		rows, total, err := list(page, store.MaxPageLimit)
		if err != nil {
			return nil, err
		}
		for _, s := range rows {
			out = append(out, s.StrategyID)
		}
		if page*store.MaxPageLimit >= total || len(rows) == 0 {
			return out, nil
		}
	}
}

// driveSafetyStrategy runs one (event, strategy)'s effects in THIS spec's
// order — cancel ENTRY orders, lock lifecycle (kill rows only, BEFORE
// flatten: the recorded parent override), flatten — and returns the
// residual work left for the next pass. An error return has already been
// counted in the returned residual. sweep=false for platform-scope rows:
// driveSafetyEvent already ran the global cancel sweep once.
func (o *OMS) driveSafetyStrategy(ctx context.Context, ev store.KillBreakerEvent, strategyID string, sweep bool) (int, error) {
	// 3a — cancel non-terminal ENTRY orders (claim-revoke included;
	// NotFound is success; an Ambiguous cancel stays residual below).
	if sweep {
		if err := o.CancelOpenEntries(ctx, strategyID); err != nil {
			return 1, err
		}
	}
	// 3b — lock lifecycle (kill rows ONLY): live_* -> killed, an
	// idempotent no-op otherwise; a breaker never touches lifecycle state.
	if ev.Kind == "kill" {
		if _, err := o.st.AppendKillLifecycleLock(strategyID, ev.EventID,
			"safety-engine", formatTime(o.now())); err != nil {
			return 1, err
		}
	}
	live, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		return 1, err
	}
	// Step 4 — every in-scope non-terminal ENTRY order (FSM ranks 0-2) is
	// residual work; an unconfigured-symbol order has no venue mapping and
	// is alerted once + EXCLUDED instead.
	residual := 0
	for i := range live {
		ord := &live[i]
		if ord.Class != "ENTRY" || ord.StrategyID != strategyID {
			continue
		}
		if _, ok := o.venueOf[ord.Symbol]; !ok {
			if err := o.appendResidueAbandoned(ev, strategyID, ord.Symbol,
				"unconfigured_symbol", ord.QtyBase); err != nil {
				return residual + 1, err
			}
			continue
		}
		residual++
	}
	// 3c — flatten: kill rows iff the operator chose it; breaker rows
	// ALWAYS flatten (spec §Fire step 6 — the loss bound wins).
	if ev.Kind != "breaker" && (ev.Flatten == nil || !*ev.Flatten) {
		return residual, nil
	}
	origin := "kill"
	if ev.Kind == "breaker" {
		origin = "breaker"
	}
	positions, err := o.st.ListPositions(strategyID)
	if err != nil {
		return residual + 1, err
	}
	for _, p := range positions {
		qty, err := parseDec("positions.qty_base", p.QtyBase)
		if err != nil {
			return residual + 1, err
		}
		if qty.IsZero() {
			continue
		}
		n, ferr := o.flattenForSafety(ctx, ev, strategyID, p.Symbol, p.QtyBase, origin, live)
		residual += n
		if ferr != nil {
			return residual, ferr
		}
	}
	return residual, nil
}

// flattenForSafety flattens ONE open position for a kill/breaker row and
// returns its residual-work contribution: 1 while a reduce-only market
// flatten rests or was just journaled (its completion is owned by the
// journal/Reconciler/fill machinery), 0 for the terminal-residue
// carve-outs — dust, alerted short balance, unconfigured symbol — each
// recorded once via safety_residue_abandoned. The carve-out cause comes
// from the flatten call's OWN outcome, never from diffing audit tables
// (any-age flatten_short_balance rows or concurrent dust appends would
// misattribute it).
func (o *OMS) flattenForSafety(ctx context.Context, ev store.KillBreakerEvent, strategyID, symbol, qtyBase, origin string, live []store.LiveOrder) (int, error) {
	// Double-flatten skip (invariant 6): ANY non-terminal reduce-only
	// market order for (strategy, symbol) — origin-independent, a
	// gate-approved close included — already closes the position; never
	// stack a second flatten.
	if findLiveReduceOnlyMarket(live, strategyID, symbol) != nil {
		return 1, nil
	}
	if _, ok := o.venueOf[symbol]; !ok {
		// Unconfigured symbol: no venue mapping, cannot flatten.
		return 0, o.appendResidueAbandoned(ev, strategyID, symbol, "unconfigured_symbol", qtyBase)
	}
	// Fresh re-check immediately before the flatten (belt and braces on
	// top of the driveMu serialization with SubmitApproved's close path):
	// the pass-level snapshot may predate a close journaled since.
	fresh, err := o.st.ListNonTerminalLiveOrders()
	if err != nil {
		return 1, err
	}
	if findLiveReduceOnlyMarket(fresh, strategyID, symbol) != nil {
		return 1, nil
	}
	outcome, err := o.flattenWithOutcome(ctx, strategyID, symbol, origin, nil)
	if err != nil {
		return 1, err
	}
	switch outcome {
	case flattenDust:
		return 0, o.appendResidueAbandoned(ev, strategyID, symbol, "dust", qtyBase)
	case flattenShortBalanceBounded:
		return 0, o.appendResidueAbandoned(ev, strategyID, symbol, "short_balance", qtyBase)
	default: // flattenJournaled: residual until its fills book the position flat
		return 1, nil
	}
}

// findLiveReduceOnlyMarket returns the first non-terminal reduce-only
// market order for (strategy, symbol) REGARDLESS of origin — the
// double-flatten skip's predicate (invariant 6), modeled on
// findLiveProtective's origin-agnostic match.
func findLiveReduceOnlyMarket(live []store.LiveOrder, strategyID, symbol string) *store.LiveOrder {
	for i := range live {
		ord := &live[i]
		if ord.ReduceOnly && ord.Type == "market" &&
			ord.StrategyID == strategyID && ord.Symbol == symbol {
			return ord
		}
	}
	return nil
}

// appendResidueAbandoned records one terminal-residue carve-out for
// OPERATOR action: a ONE-TIME safety_residue_abandoned alert per
// (event, strategy, symbol), deduped any-age on
// (kind, strategy_id, ref_id = event_id/symbol) (spec §Alerts).
func (o *OMS) appendResidueAbandoned(ev store.KillBreakerEvent, strategyID, symbol, cause, qty string) error {
	refID := ev.EventID + "/" + symbol
	dup, err := o.st.HasSafetyAlert("safety_residue_abandoned", strategyID, refID)
	if err != nil || dup {
		return err
	}
	details, err := json.Marshal(map[string]any{"cause": cause, "abandoned_qty": qty})
	if err != nil {
		return err
	}
	o.logf("live: ALERT safety residue abandoned for %s %s (event %s, cause %s, qty %s)",
		strategyID, symbol, ev.EventID, cause, qty)
	return o.st.AppendSafetyAlert(store.SafetyAlert{
		AlertID: newUUID(), Kind: "safety_residue_abandoned",
		StrategyID: &strategyID, RefID: &refID,
		DetailsJSON: string(details), RecordedAt: formatTime(o.now()),
	})
}
