package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// withStops sets the protective obligations on the harness proposal.
func withStops(t *testing.T, sl, tp string) func(*contract.Proposal) {
	t.Helper()
	return func(p *contract.Proposal) {
		if sl != "" {
			d := mustDec(t, sl)
			p.StopLoss = &d
		}
		if tp != "" {
			d := mustDec(t, tp)
			p.TakeProfit = &d
		}
	}
}

func definiteReject() *exchange.VenueError {
	return &exchange.VenueError{Op: "PlaceOrder", Class: exchange.ClassDefiniteReject,
		VenueCode: -2010, VenueMsg: "Account has insufficient balance."}
}

// openObligations returns the unmet protective timer rows.
func (e *env) openObligations() int {
	e.t.Helper()
	obls, err := e.st.ListOpenProtectiveObligations()
	if err != nil {
		e.t.Fatalf("ListOpenProtectiveObligations: %v", err)
	}
	return len(obls)
}

// S20: a growing entry partial fill cancel+replaces the resting protective
// at the new cumulative quantity (protective_resized).
func TestProtective_PartialFillResize(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile() // books the partial; the drive places the SL at 0.005

	sl := e.order(idN(2, 0))
	if sl.Class != "PROTECTIVE" || sl.Type != "stop" || !sl.ReduceOnly ||
		sl.QtyBase != "0.005" || sl.Status != "open" {
		t.Fatalf("SL order = %+v, want open reduce-only stop for 0.005", sl.Order)
	}
	if sl.StopPrice == nil || *sl.StopPrice != "60000" {
		t.Errorf("SL trigger = %v, want 60000", sl.StopPrice)
	}
	if got := e.openObligations(); got != 0 {
		t.Fatalf("open obligations after placement = %d, want 0 (satisfied)", got)
	}

	if err := e.venue.Fill(idN(1, 0), "0.005", "64100"); err != nil {
		t.Fatalf("Fill remainder: %v", err)
	}
	e.reconcile() // cumulative 0.01: cancel+replace at the new size

	if evs := e.events("protective_resized"); len(evs) != 1 {
		t.Fatalf("protective_resized events = %d, want 1", len(evs))
	}
	if old := e.order(idN(2, 0)); old.Status != "canceled" {
		t.Errorf("old SL status = %s, want canceled", old.Status)
	}
	repl := e.order(idN(3, 0))
	if repl.QtyBase != "0.01" || repl.Status != "open" || repl.Type != "stop" {
		t.Errorf("replacement SL = qty %s status %s type %s, want 0.01 open stop",
			repl.QtyBase, repl.Status, repl.Type)
	}
	if got := e.openObligations(); got != 0 {
		t.Errorf("open obligations after resize = %d, want 0", got)
	}
}

// S19: the SL placement deadline breaches after failed retries — the OMS
// contingency-flattens the filled quantity (origin sl_contingency) and
// appends sl_deadline_contingency; the position converges to flat.
func TestProtective_DeadlineContingencyFlatten(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01", "0")

	// Placement fails while the deadline runs: retried, never flattened.
	e.venue.FailNext("PlaceOrder", definiteReject())
	e.reconcile()
	if evs := e.events("sl_deadline_contingency"); len(evs) != 0 {
		t.Fatalf("contingency before the deadline = %d events, want 0 (still retrying)", len(evs))
	}
	if got := e.openObligations(); got != 1 {
		t.Fatalf("open obligations = %d, want 1 (unmet)", got)
	}

	// The 30 s deadline breaches; the next attempt still fails => flatten.
	e.now = testNow.Add(31 * time.Second)
	e.venue.FailNext("PlaceOrder", definiteReject())
	e.reconcile()

	evs := e.events("sl_deadline_contingency")
	if len(evs) != 1 {
		t.Fatalf("sl_deadline_contingency events = %d, want 1", len(evs))
	}
	// AN-1a companion alert: same kind, ref_id = the recon event_id,
	// committed with the event (alert-notifier.md).
	alerts, err := e.st.ListSafetyAlerts(store.SafetyAlertFilter{Kind: "sl_deadline_contingency"})
	if err != nil {
		t.Fatalf("ListSafetyAlerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].RefID == nil || *alerts[0].RefID != evs[0].EventID ||
		alerts[0].StrategyID == nil || *alerts[0].StrategyID != uid(1) {
		t.Fatalf("companion alerts = %+v, want one sl_deadline_contingency alert with ref_id %s",
			alerts, evs[0].EventID)
	}
	fl := e.order(idN(4, 0))
	if fl.Origin != "sl_contingency" || fl.Class != "PROTECTIVE" || fl.Type != "market" ||
		!fl.ReduceOnly || fl.QtyBase != "0.01" || fl.Side != "sell" {
		t.Errorf("contingency flatten order = %+v, want reduce-only market sell 0.01", fl.Order)
	}

	// The flatten fills; the position converges to flat and the timers
	// resolve (protected-or-flat, invariant 12); no repeated contingency.
	if err := e.venue.Fill(idN(4, 0), "0.01", "63000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}
	e.reconcile()
	if pos, ok := e.position(); !ok || pos.QtyBase != "0" {
		t.Errorf("position = %+v ok=%v, want flat", pos, ok)
	}
	if got := e.openObligations(); got != 0 {
		t.Errorf("open obligations after flatten = %d, want 0", got)
	}
	if evs := e.events("sl_deadline_contingency"); len(evs) != 1 {
		t.Errorf("contingency repeated = %d events, want still 1", len(evs))
	}
}

// S21: a crash between the entry fill and the SL placement leaves a booked
// fill with an unmet obligation and no protective — the RESTART re-arms it:
// the startup reconcile completes first, then the drive places the SL.
func TestProtective_RestartRearm(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.FailNext("PlaceOrder", definiteReject())
	e.reconcile() // books the fill; the SL placement dies (crash analog)
	if got := e.openObligations(); got != 1 {
		t.Fatalf("open obligations pre-restart = %d, want 1 (unprotected)", got)
	}

	e.oms = e.newOMS() // restart: fresh process over the durable state
	e.reconcile()      // startup reconcile, then the immediate re-drive

	sl := e.order(idN(3, 0))
	if sl.Class != "PROTECTIVE" || sl.Type != "stop" || sl.QtyBase != "0.005" ||
		sl.Status != "open" || !sl.ReduceOnly {
		t.Fatalf("re-armed SL = %+v, want open reduce-only stop for 0.005", sl.Order)
	}
	if got := e.openObligations(); got != 0 {
		t.Errorf("open obligations after re-arm = %d, want 0 (satisfied)", got)
	}
	// The venue holds exactly the resting entry remainder and its SL.
	if open := e.venueOpen(); len(open) != 2 {
		t.Errorf("venue open orders = %d, want 2 (entry + protective)", len(open))
	}
}

// M2: a crash separates the closing fill's booking from stops-after-
// flatten, leaving the SL resting over an already-flat book. The restart
// reconcile backfills the flatten fill; its immediate cancel fails
// transiently, and the post-run drive retries and cancels the orphaned
// protective — every attempt journals orphan_canceled
// (stops_after_flatten) BEFORE the cancel.
func TestProtective_StopsAfterFlattenRedrive(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill entry: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01562", "0")
	e.reconcile() // books the entry fill; the drive places the SL
	if sl := e.order(idN(2, 0)); sl.Status != "open" || sl.Type != "stop" {
		t.Fatalf("SL = status %s type %s, want open stop", sl.Status, sl.Type)
	}
	if err := e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "kill", nil); err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if err := e.venue.Fill(idN(3, 0), "0.01562", "63000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}

	// Crash analog: restart BEFORE the flatten fill was booked locally;
	// the booking path's immediate cancel fails transiently on top.
	e.venue.FailNext("CancelOrder", ambiguousErr("CancelOrder"))
	e.oms = e.newOMS()
	e.reconcile() // R5 books the flatten fill flat; the drive retries the cancel

	if pos, ok := e.position(); !ok || pos.QtyBase != "0" {
		t.Fatalf("position = %+v ok=%v, want flat", pos, ok)
	}
	if sl := e.order(idN(2, 0)); sl.Status != "canceled" {
		t.Errorf("SL status = %s, want canceled (orphaned protective removed)", sl.Status)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0", len(got))
	}
	evs := e.events("orphan_canceled")
	if len(evs) != 2 {
		t.Fatalf("orphan_canceled events = %d, want 2 (booking attempt + drive retry)", len(evs))
	}
	for _, ev := range evs {
		if !strings.Contains(ev.DetailsJSON, `"reason":"stops_after_flatten"`) {
			t.Errorf("orphan_canceled details = %s, want reason stops_after_flatten", ev.DetailsJSON)
		}
	}
	if got := e.openObligations(); got != 0 {
		t.Errorf("open obligations = %d, want 0 (flat book resolves the timers)", got)
	}
}
