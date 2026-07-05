package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// openPosition opens a full 0.01562 BTC long for strategy 1 (entry, venue
// fill, reconcile) and seeds the venue base balance so a sell flatten is
// never balance-bounded. Returns the entry's attempt id.
func (e *env) openPosition(base int) string {
	e.t.Helper()
	if err := e.submitEntry(base); err != nil {
		e.t.Fatalf("SubmitApproved: %v", err)
	}
	clientID := idN(e.tokens.n, 0)
	if err := e.venue.Fill(clientID, "0.01562", "64000"); err != nil {
		e.t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0.01562", "0")
	return clientID
}

// createStrategy adds one live_l1 strategy under the given tenant.
func (e *env) createStrategy(id int, tenantID string) {
	e.t.Helper()
	if err := e.st.CreateStrategy(store.Strategy{
		StrategyID: uid(id), TenantID: tenantID, Name: fmt.Sprintf("s%d", id),
		LifecycleState: "live_l1", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	}); err != nil {
		e.t.Fatalf("CreateStrategy: %v", err)
	}
}

// unserved lists the kill/breaker rows with no served marker.
func (e *env) unserved() []store.KillBreakerEvent {
	e.t.Helper()
	out, err := e.st.ListUnservedSafetyEvents()
	if err != nil {
		e.t.Fatalf("ListUnservedSafetyEvents: %v", err)
	}
	return out
}

// alerts lists the safety_alerts rows of one kind.
func (e *env) alerts(kind string) []store.SafetyAlert {
	e.t.Helper()
	out, err := e.st.ListSafetyAlerts(store.SafetyAlertFilter{Kind: kind})
	if err != nil {
		e.t.Fatalf("ListSafetyAlerts(%s): %v", kind, err)
	}
	return out
}

// lifecycle reads one strategy's current lifecycle state.
func (e *env) lifecycle(strategyID string) string {
	e.t.Helper()
	s, err := e.st.GetStrategy(strategyID)
	if err != nil {
		e.t.Fatalf("GetStrategy(%s): %v", strategyID, err)
	}
	return s.LifecycleState
}

// KD8: after a kill, SubmitApproved of an ENTRY returns KILL_SWITCH_ACTIVE
// via the standing-kill check (invariant 15 — the fresh submission would
// stamp the post-kill epoch, so the transmit staleness re-check alone
// would pass it), while the kill's OWN flatten still executes at the
// post-bump epoch (never self-deadlocked); the strategy locks to killed
// and no pass ever transitions it back out.
func TestKillDrill_SubmitRejectedNoAutoRestart(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10) // token 1

	epoch, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true)
	if err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if err := e.submitEntry(20); !errors.Is(err, ErrKillSwitchActive) {
		t.Fatalf("SubmitApproved err = %v, want ErrKillSwitchActive", err)
	}

	e.oms.DriveSafetyEffects(context.Background())
	fl := e.order(idN(2, 0))
	if fl.Origin != "kill" || !fl.ReduceOnly || fl.Type != "market" || fl.KillEpoch != epoch {
		t.Errorf("flatten order = %+v, want reduce-only market origin kill at post-bump epoch %d",
			fl.Order, epoch)
	}
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle = %s, want killed", got)
	}
	e.oms.DriveSafetyEffects(context.Background())
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle after re-drive = %s, want still killed (no auto-restart)", got)
	}
}

// KD4 core drive sequence: a kill with flatten over an open position plus
// a resting ENTRY cancels the entry at the venue, locks live_l1 -> killed,
// journals the flatten, PRESERVES the protective stop (stops-after-flatten
// owns its cancel), and serves the row only after the flatten fill books
// flat; a second drive pass over the completed world is a pure no-op.
func TestKillDrill_FlattenStopsAfterFill(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01562", "0")
	e.reconcile() // books the fill; the protective drive places the SL (token 2)
	if sl := e.order(idN(2, 0)); sl.Type != "stop" || sl.Status != "open" {
		t.Fatalf("SL = type %s status %s, want open stop", sl.Type, sl.Status)
	}
	if err := e.submitEntry(20); err != nil { // resting entry (token 3)
		t.Fatalf("SubmitApproved: %v", err)
	}

	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	e.reconcile() // R7 hook: DriveSafetyEffects BEFORE the protective drive

	if ord := e.order(idN(3, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	types := map[string]int{}
	for _, vo := range e.venueOpen() {
		types[vo.Type]++
	}
	if types["STOP_LOSS"] != 1 || types["MARKET"] != 1 || len(types) != 2 {
		t.Fatalf("venue open types = %+v, want the preserved SL plus the resting flatten", types)
	}
	if fl := e.order(idN(4, 0)); fl.Origin != "kill" || !fl.ReduceOnly || fl.Type != "market" {
		t.Errorf("flatten order = %+v, want reduce-only market origin kill", fl.Order)
	}
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle = %s, want killed (live_l1 locked)", got)
	}
	if got := e.unserved(); len(got) != 1 {
		t.Fatalf("unserved events = %d, want 1 (flatten still resting)", len(got))
	}

	if err := e.venue.Fill(idN(4, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()
	if pos, ok := e.position(); ok && pos.QtyBase != "0" {
		t.Errorf("position = %s, want flat", pos.QtyBase)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (SL canceled AFTER the covering fill)", len(got))
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (served marker appended)", len(got))
	}

	before := e.tokens.n
	e.oms.DriveSafetyEffects(context.Background())
	if e.tokens.n != before {
		t.Errorf("re-drive minted %d new intents, want 0 (idempotent)", e.tokens.n-before)
	}
}

// Invariant 16: a kill appended AFTER the last completed reconcile has its
// effects EXECUTED by an on-demand pass, but the served marker defers to
// the next R7-hooked pass, which follows a post-event reconcile.
func TestKillDrill_ReconcileGateDefersMarker(t *testing.T) {
	e := newEnv(t)
	e.reconcile() // last completed reconcile: t0

	e.now = e.now.Add(time.Minute)
	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	e.oms.DriveSafetyEffects(context.Background())
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle = %s, want killed (effects execute pre-marker)", got)
	}
	if got := e.unserved(); len(got) != 1 {
		t.Fatalf("unserved events = %d, want 1 (marker deferred: only a pre-event reconcile completed)", len(got))
	}

	e.now = e.now.Add(time.Second) // the serving reconcile must finish STRICTLY after recorded_at
	e.reconcile()
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 after the post-event reconcile", len(got))
	}
}

// KD9: a resting unfilled reduce-only market flatten of ANY origin (here a
// gate-approved close journaled through the FULL SubmitApproved seam —
// the ActionClose path serializes on driveMu) makes a drive pass submit
// ZERO new orders for that (strategy, symbol) — invariant 6.
func TestKillDrill_NoDoubleFlatten(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10) // token 1
	if err := e.submitEntryWith(20, func(p *contract.Proposal) {
		p.Action = contract.ActionClose
		p.SizeQuote = mustDec(t, "0")
		p.Entry = contract.Entry{Type: "market"}
	}); err != nil {
		t.Fatalf("SubmitApproved close: %v", err) // resting close, token 2
	}
	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}

	openBefore, tokensBefore := len(e.venueOpen()), e.tokens.n
	e.oms.DriveSafetyEffects(context.Background())
	if got := len(e.venueOpen()); got != openBefore {
		t.Errorf("venue open orders = %d, want still %d (no stacked flatten)", got, openBefore)
	}
	if e.tokens.n != tokensBefore {
		t.Errorf("drive minted %d new intents, want 0 (double-flatten skip)", e.tokens.n-tokensBefore)
	}
	if got := e.unserved(); len(got) != 1 {
		t.Errorf("unserved events = %d, want 1 (the resting flatten is residual)", len(got))
	}
}

// KD10: a kill with flatten over a below-minNotional remainder journals
// flatten_dust, appends ONE safety_residue_abandoned (cause dust, deduped
// per (event, strategy, symbol)), and the row is SERVED — the carve-out
// counts as zero residual work.
func TestKillDrill_DustResidueServed(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	// 0.00005 BTC at the 64000 mark = 3.2 quote < minNotional 5: dust.
	if err := e.venue.Fill(idN(1, 0), "0.00005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0.00005", "0")

	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()

	if evs := e.events("flatten_dust"); len(evs) != 1 {
		t.Errorf("flatten_dust events = %d, want 1", len(evs))
	}
	alerts := e.alerts("safety_residue_abandoned")
	if len(alerts) != 1 {
		t.Fatalf("safety_residue_abandoned alerts = %d, want 1", len(alerts))
	}
	a := alerts[0]
	if a.StrategyID == nil || *a.StrategyID != uid(1) ||
		a.RefID == nil || *a.RefID != uid(90)+"/BTC/USDT" ||
		!strings.Contains(a.DetailsJSON, `"cause":"dust"`) {
		t.Errorf("alert = %+v, want cause dust keyed (event, strategy, symbol)", a)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (dust residue counts as zero residual)", len(got))
	}

	e.reconcile()
	if got := e.alerts("safety_residue_abandoned"); len(got) != 1 {
		t.Errorf("alerts after re-drive = %d, want still 1 (one-time)", len(got))
	}
}

// Invariant 17: under a tenant kill, strategy 1's unconfigured-symbol
// residue never blocks strategy 2 — its entry cancels and it locks; the
// residue is alerted once (cause unconfigured_symbol) and EXCLUDED from
// residual work, so the row still serves.
func TestKillDrill_ErrorIsolationUnconfiguredSymbol(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.createStrategy(2, "tenant-1")
	if err := e.submitEntryWith(20, func(p *contract.Proposal) { p.StrategyID = uid(2) }); err != nil {
		t.Fatalf("SubmitApproved: %v", err) // strategy 2's resting entry (token 1)
	}
	// Strategy 1 holds a journaled non-terminal ENTRY on an UNCONFIGURED
	// symbol: no venue mapping, the sweep cannot cancel it.
	limit := "3200"
	orderID := newUUID()
	if err := e.st.InsertJournaledOrder(store.Order{
		OrderID: orderID, Origin: "proposal", StrategyID: uid(1), Symbol: "ETH/USDT",
		Class: "ENTRY", Side: "buy", Type: "limit", QtyBase: "0.5",
		LimitPrice: &limit, Status: "pending_new", SubmittedAt: formatTime(e.now),
	}, store.OrderIntent{
		ClientOrderID: attemptID(tokenN(9), 0), IntentToken: tokenN(9), Attempt: 0,
		OrderID: orderID, StrategyID: uid(1), Symbol: "ETH/USDT", VenueSymbol: "ETHUSDT",
		Side: "buy", Type: "limit", QtyBase: "0.5", LimitPrice: &limit,
		Origin: "proposal", JournaledAt: formatTime(e.now),
	}); err != nil {
		t.Fatalf("InsertJournaledOrder: %v", err)
	}

	if _, err := e.st.AppendTenantKill(uid(90), "tenant-1", "op-1", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	e.oms.DriveSafetyEffects(context.Background())
	// The on-demand pass EXECUTED the effects; the marker defers to a
	// reconcile that finishes STRICTLY after recorded_at (invariant 16).
	e.now = e.now.Add(time.Second)
	e.reconcile()

	if got := len(e.venueOpen()); got != 0 {
		t.Errorf("venue open orders = %d, want 0 (strategy 2's entry canceled)", got)
	}
	for _, sid := range []string{uid(1), uid(2)} {
		if got := e.lifecycle(sid); got != "killed" {
			t.Errorf("lifecycle(%s) = %s, want killed", sid, got)
		}
	}
	alerts := e.alerts("safety_residue_abandoned")
	if len(alerts) != 1 || alerts[0].RefID == nil || *alerts[0].RefID != uid(90)+"/ETH/USDT" ||
		!strings.Contains(alerts[0].DetailsJSON, `"cause":"unconfigured_symbol"`) {
		t.Fatalf("alerts = %+v, want ONE unconfigured_symbol residue row for strategy 1", alerts)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (excluded residue leaves zero residual)", len(got))
	}
}

// KD6: a tenant kill never bleeds — the other tenant's resting entry stays
// open, its lifecycle is untouched, and it remains fully submittable while
// the killed tenant is standing-rejected.
func TestKillDrill_TenantScopeNoBleed(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.createStrategy(3, "tenant-2")
	if err := e.submitEntry(10); err != nil { // tenant-1 entry (token 1)
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.submitEntryWith(20, func(p *contract.Proposal) { p.StrategyID = uid(3) }); err != nil {
		t.Fatalf("SubmitApproved: %v", err) // tenant-2 entry (token 2)
	}
	if _, err := e.st.AppendTenantKill(uid(90), "tenant-1", "op-1", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()

	if ord := e.order(idN(1, 0)); ord.Status != "canceled" {
		t.Errorf("tenant-1 entry status = %s, want canceled", ord.Status)
	}
	if ord := e.order(idN(2, 0)); ord.Status != "open" {
		t.Errorf("tenant-2 entry status = %s, want open (untouched)", ord.Status)
	}
	if got := e.lifecycle(uid(3)); got != "live_l1" {
		t.Errorf("tenant-2 lifecycle = %s, want live_l1", got)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0", len(got))
	}
	if err := e.submitEntryWith(30, func(p *contract.Proposal) { p.StrategyID = uid(3) }); err != nil {
		t.Errorf("tenant-2 submit err = %v, want nil (still submittable)", err)
	}
	if err := e.submitEntry(40); !errors.Is(err, ErrKillSwitchActive) {
		t.Errorf("tenant-1 submit err = %v, want ErrKillSwitchActive", err)
	}
}

// KD7: a platform kill covers every strategy of every tenant — all entries
// canceled, all live_* lifecycles locked, all submissions standing-rejected.
func TestKillDrill_PlatformCoversAllTenants(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.createStrategy(3, "tenant-2")
	if err := e.submitEntry(10); err != nil { // tenant-1 entry (token 1)
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.submitEntryWith(20, func(p *contract.Proposal) { p.StrategyID = uid(3) }); err != nil {
		t.Fatalf("SubmitApproved: %v", err) // tenant-2 entry (token 2)
	}
	if _, err := e.st.AppendPlatformKill(uid(90), "env-admin", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()

	if got := len(e.venueOpen()); got != 0 {
		t.Errorf("venue open orders = %d, want 0 (both tenants swept)", got)
	}
	for _, sid := range []string{uid(1), uid(3)} {
		if got := e.lifecycle(sid); got != "killed" {
			t.Errorf("lifecycle(%s) = %s, want killed", sid, got)
		}
	}
	if err := e.submitEntry(30); !errors.Is(err, ErrKillSwitchActive) {
		t.Errorf("tenant-1 submit err = %v, want ErrKillSwitchActive", err)
	}
	if err := e.submitEntryWith(40, func(p *contract.Proposal) { p.StrategyID = uid(3) }); !errors.Is(err, ErrKillSwitchActive) {
		t.Errorf("tenant-2 submit err = %v, want ErrKillSwitchActive", err)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0", len(got))
	}
}

// KD1: the kill sweep cancels a RESTING venue entry AND claim-revokes a
// claimed-but-unsent journaled entry BEFORE its venue cancel
// (§In-flight exclusion), so a resumed sender can never transmit the
// attempt — no send ever reaches the venue.
func TestKillDrill_EntryCancelAndClaimRevoke(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil { // resting venue entry (token 1)
		t.Fatalf("SubmitApproved: %v", err)
	}
	// The crash state "journaled, claimed, never sent": the claim is held
	// exactly as sendIntent holds it before the placement HTTP.
	_, intent := e.journalOrder(tokenN(9))
	if claimed, err := e.st.RecordIntentClaim(intent.ClientOrderID, formatTime(e.now)); err != nil || !claimed {
		t.Fatalf("RecordIntentClaim = %v, %v, want true, nil", claimed, err)
	}

	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	e.oms.DriveSafetyEffects(context.Background())

	if got := len(e.venueOpen()); got != 0 {
		t.Errorf("venue open orders = %d, want 0 (resting entry canceled)", got)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	if ord := e.order(intent.ClientOrderID); ord.Status != "canceled" {
		t.Errorf("claimed-unsent entry status = %s, want canceled", ord.Status)
	}
	row, err := e.st.GetOrderIntent(intent.ClientOrderID)
	if err != nil {
		t.Fatalf("GetOrderIntent: %v", err)
	}
	if row.ClaimRevokedAt == nil {
		t.Fatal("claim_revoked_at = NULL, want the sweep's revocation BEFORE the cancel")
	}
	// The crashed sender resumes: the revoked claim resolves to a no-op —
	// the attempt is NEVER sent.
	if err := e.oms.sendIntent(context.Background(), intent); err != nil {
		t.Fatalf("resumed sendIntent: %v", err)
	}
	if _, err := e.venue.QueryOrder(context.Background(), "BTCUSDT", intent.ClientOrderID); err == nil {
		t.Error("the claimed-unsent attempt reached the venue, want no send ever")
	}
	// A reconcile STRICTLY after recorded_at serves the row (invariant 16).
	e.now = e.now.Add(time.Second)
	e.reconcile()
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0", len(got))
	}
}

// KD3: a kill WITHOUT flatten cancels entries and locks the lifecycle but
// leaves the position and its protective stop at the venue — and the
// Reconciler's next passes (R3 included) never orphan-cancel the preserved
// protective.
func TestKillDrill_ProtectivesPreservedNoFlatten(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01562", "0")
	e.reconcile()                             // books the fill; the drive places the SL (token 2)
	if err := e.submitEntry(20); err != nil { // resting entry (token 3)
		t.Fatalf("SubmitApproved: %v", err)
	}

	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()                  // R7 drive: cancel the entry, lock lifecycle, NO flatten

	if ord := e.order(idN(3, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	open := e.venueOpen()
	if len(open) != 1 || open[0].Type != "STOP_LOSS" || open[0].ClientOrderID != idN(2, 0) {
		t.Fatalf("venue open orders = %+v, want ONLY the preserved protective stop", open)
	}
	if sl := e.order(idN(2, 0)); sl.Status != "open" {
		t.Errorf("SL status = %s, want open (never canceled by the kill)", sl.Status)
	}
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle = %s, want killed", got)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (no flatten leaves zero residual)", len(got))
	}

	// Another full pass: the locally-known protective of the still-open
	// position is never treated as an orphan.
	e.reconcile()
	if got := e.events("orphan_canceled"); len(got) != 0 {
		t.Errorf("orphan_canceled events = %d, want 0 (R3 preserves the protective)", len(got))
	}
	if sl := e.order(idN(2, 0)); sl.Status != "open" {
		t.Errorf("SL status after another pass = %s, want still open", sl.Status)
	}
}

// KD5: a crash between the kill append and its effects (row appended,
// effects never ran) is healed by RESTART: the startup reconcile + R7
// drive cancel the entry, lock the lifecycle, and journal the flatten;
// once the flatten fill books flat the row serves; a SECOND restart over
// the completed world is a pure no-op — no duplicate cancels, flattens, or
// markers.
func TestKillDrill_CrashResume(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10)                        // token 1
	if err := e.submitEntry(20); err != nil { // resting entry (token 2)
		t.Fatalf("SubmitApproved: %v", err)
	}
	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	// CRASH: no drive ever ran; a fresh process opens over the same store.
	e.oms = e.newOMS()
	e.reconcile()

	if ord := e.order(idN(2, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle = %s, want killed", got)
	}
	if fl := e.order(idN(3, 0)); fl.Origin != "kill" || !fl.ReduceOnly || fl.Type != "market" {
		t.Errorf("flatten order = %+v, want reduce-only market origin kill", fl.Order)
	}
	if got := e.unserved(); len(got) != 1 {
		t.Fatalf("unserved events = %d, want 1 (flatten still resting)", len(got))
	}

	if err := e.venue.Fill(idN(3, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()
	if pos, ok := e.position(); ok && pos.QtyBase != "0" {
		t.Errorf("position = %s, want flat", pos.QtyBase)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (served)", len(got))
	}

	// SECOND restart: zero new intents, zero venue orders, still served.
	e.oms = e.newOMS()
	before := e.tokens.n
	e.reconcile()
	if e.tokens.n != before {
		t.Errorf("second restart minted %d new intents, want 0", e.tokens.n-before)
	}
	if got := len(e.venueOpen()); got != 0 {
		t.Errorf("venue open orders = %d, want 0", got)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 after the second restart", len(got))
	}
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle after the second restart = %s, want still killed", got)
	}
}

// BD5: a breaker row appended but never served (effects crashed) is
// re-driven after RESTART via the shared served/unserved mechanism; an
// unserved PRIOR-DAY row still re-drives its effects — the flatten intent
// survives the UTC-day boundary with origin 'breaker' and no lifecycle
// touch — while its ENTRY halt expired at 00:00 UTC, so entries submit
// again once the row is served.
func TestBreakerDrill_CrashResumeEffects(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10) // token 1
	sid := uid(1)
	if err := e.st.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(90), Kind: "breaker", Scope: "strategy", StrategyID: &sid,
		ActorID: "breaker-monitor", RecordedAt: formatTime(testNow.AddDate(0, 0, -1)),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	e.oms = e.newOMS() // effects crashed: a fresh process over the same store
	e.reconcile()

	fl := e.order(idN(2, 0)) // the breaker ALWAYS flattens (no flatten flag)
	if fl.Origin != "breaker" || !fl.ReduceOnly || fl.Type != "market" {
		t.Errorf("flatten order = %+v, want reduce-only market origin breaker", fl.Order)
	}
	if got := e.lifecycle(uid(1)); got != "live_l1" {
		t.Errorf("lifecycle = %s, want live_l1 (a breaker never locks lifecycle)", got)
	}
	if got := e.unserved(); len(got) != 1 {
		t.Fatalf("unserved events = %d, want 1 (flatten still resting)", len(got))
	}

	if err := e.venue.Fill(idN(2, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}
	e.reconcile()
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (prior-day breaker served)", len(got))
	}
	if err := e.submitEntry(20); err != nil {
		t.Errorf("entry after the halt's UTC day err = %v, want nil (halt expired)", err)
	}
}

// BD2: the breaker latch derives from the persisted row — after a restart
// on the SAME UTC day BreakerActiveToday still binds and fresh entries
// stay halted (no mutable latch state to lose).
func TestBreakerDrill_LatchAcrossRestart(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10) // token 1
	sid := uid(1)
	if err := e.st.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(90), Kind: "breaker", Scope: "strategy", StrategyID: &sid,
		ActorID: "breaker-monitor", RecordedAt: formatTime(e.now),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	e.reconcile() // drive: the flatten journals (token 2) and rests
	if err := e.submitEntry(20); !errors.Is(err, ErrBreakerActive) {
		t.Fatalf("entry err = %v, want ErrBreakerActive", err)
	}

	e.oms = e.newOMS() // restart, same UTC day
	before := e.tokens.n
	e.reconcile()
	if e.tokens.n != before {
		t.Errorf("restart minted %d new intents, want 0 (no stacked flatten)", e.tokens.n-before)
	}
	if active, err := e.st.BreakerActiveToday(sid, utcDate(e.now)); err != nil || !active {
		t.Fatalf("BreakerActiveToday = %v, %v, want true, nil (latched)", active, err)
	}
	if err := e.submitEntry(30); !errors.Is(err, ErrBreakerActive) {
		t.Errorf("entry after restart err = %v, want ErrBreakerActive", err)
	}
}

// BD3: the halt auto-re-arms at 00:00 UTC — advancing the injected clock
// past the boundary permits entries again with NO reset job: the predicate
// is derived per-UTC-day from the same immutable row.
func TestBreakerDrill_RearmNextUTCDay(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	sid := uid(1)
	if err := e.st.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(90), Kind: "breaker", Scope: "strategy", StrategyID: &sid,
		ActorID: "breaker-monitor", RecordedAt: formatTime(e.now),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	e.reconcile() // drive executes (nothing in scope); the marker defers to a post-event reconcile
	if err := e.submitEntry(10); !errors.Is(err, ErrBreakerActive) {
		t.Fatalf("same-day entry err = %v, want ErrBreakerActive", err)
	}

	e.now = e.now.Add(13 * time.Hour) // 2026-07-05 01:00 UTC: past 00:00
	e.reconcile()                     // routine pass (R1 refreshes filters); no reset job exists
	if active, err := e.st.BreakerActiveToday(sid, utcDate(e.now)); err != nil || active {
		t.Fatalf("BreakerActiveToday = %v, %v, want false, nil (re-armed)", active, err)
	}
	if err := e.submitEntry(20); err != nil {
		t.Fatalf("next-day entry err = %v, want nil (halt expired)", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" {
		t.Errorf("next-day entry status = %s, want open", ord.Status)
	}
}

// LC-34 split, OMS half (lifecycle-api.md): a clear at the kill's own
// scope re-enables ENTRY submissions — the standing check is the
// ActiveKill predicate — while the post-clear submission stamps the RAW
// epoch (LC-34a) and an intent stamped PRE-kill still abandons stale
// (invariant 3: staleness survives clearing).
func TestKillClearDrill_ClearReenablesSubmitStaleStillAbandons(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	epoch, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), false)
	if err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if err := e.submitEntry(10); !errors.Is(err, ErrKillSwitchActive) {
		t.Fatalf("pre-clear submit err = %v, want ErrKillSwitchActive", err)
	}
	cleared, superseded, err := e.st.AppendKillClearStrategy(uid(91), uid(1), "admin-1",
		"resolved", epoch, formatTime(e.now))
	if err != nil || cleared != epoch || len(superseded) != 1 || superseded[0] != uid(90) {
		t.Fatalf("clear = (%d, %v, %v), want (%d, [%s], nil)", cleared, superseded, err, epoch, uid(90))
	}
	// The standing check passes; the fresh entry stamps the RAW epoch.
	if err := e.submitEntry(20); err != nil { // token 1
		t.Fatalf("post-clear submit err = %v, want nil (re-enabled)", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" || ord.KillEpoch != epoch {
		t.Errorf("post-clear order = status %s epoch %d, want open at RAW stamp %d",
			ord.Status, ord.KillEpoch, epoch)
	}
	// An intent stamped PRE-kill (epoch 0) is still stale: the transmit
	// loop compares against the RAW max, which a clear never lowers.
	_, intent := e.journalOrder(tokenN(9))
	if err := e.oms.sendIntent(context.Background(), intent); !errors.Is(err, ErrKillEpochStale) {
		t.Fatalf("stale-stamp send err = %v, want ErrKillEpochStale", err)
	}
	if ord := e.order(intent.ClientOrderID); ord.Status != "rejected" {
		t.Errorf("stale intent status = %s, want rejected", ord.Status)
	}
	if open := e.venueOpen(); len(open) != 1 {
		t.Errorf("venue open orders = %d, want 1 (only the post-clear entry)", len(open))
	}
}

// LC-34a stamp-then-check: a kill landing AFTER the stamp read (between
// journal and send) is caught by the transmit-loop staleness comparison
// EVEN IF it is already cleared by then — no interleaving lets an intent
// transmit under a kill it never observed, and a clear never un-stales it.
func TestKillClearDrill_ClearedPostStampKillStillStale(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.oms.afterJournal = func() {
		epoch, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(testNow), false)
		if err != nil {
			t.Fatalf("AppendStrategyKill: %v", err)
		}
		if _, _, err := e.st.AppendKillClearStrategy(uid(91), uid(1), "admin-1",
			"cleared immediately", epoch, formatTime(testNow)); err != nil {
			t.Fatalf("AppendKillClearStrategy: %v", err)
		}
	}
	if err := e.submitEntry(10); !errors.Is(err, ErrKillEpochStale) {
		t.Fatalf("SubmitApproved err = %v, want ErrKillEpochStale (RAW epoch survives the clear)", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "rejected" {
		t.Errorf("order status = %s, want rejected", ord.Status)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (never sent)", len(got))
	}
}

// LC-38 driver half: an in-flight drive holding a pre-clear event snapshot
// re-checks the served marker immediately before executing — a concurrent
// clear's supersede marker makes it a no-op, so superseded effects (cancel,
// lifecycle lock, flatten) NEVER execute, before or after.
func TestKillClearDrill_SupersededEffectsNeverExecute(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10)                        // open long, token 1
	if err := e.submitEntry(20); err != nil { // resting entry, token 2
		t.Fatalf("SubmitApproved: %v", err)
	}
	epoch, err := e.st.AppendStrategyKill(uid(90), uid(1), "op-1", formatTime(e.now), true)
	if err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	// The in-flight pass listed the row BEFORE the clear landed.
	var ev store.KillBreakerEvent
	for _, u := range e.unserved() {
		if u.EventID == uid(90) {
			ev = u
		}
	}
	if ev.EventID == "" {
		t.Fatal("kill row not listed unserved")
	}
	if _, superseded, err := e.st.AppendKillClearStrategy(uid(91), uid(1), "admin-1",
		"resolved", epoch, formatTime(e.now)); err != nil || len(superseded) != 1 {
		t.Fatalf("clear = (%v, %v), want the kill superseded", superseded, err)
	}
	e.oms.driveSafetyEvent(context.Background(), ev, "")

	assertUntouched := func(when string) {
		t.Helper()
		if got := e.lifecycle(uid(1)); got != "live_l1" {
			t.Errorf("%s: lifecycle = %s, want live_l1 (no lock)", when, got)
		}
		if ord := e.order(idN(2, 0)); ord.Status != "open" {
			t.Errorf("%s: resting entry = %s, want open (no cancel sweep)", when, ord.Status)
		}
		live, err := e.st.ListNonTerminalLiveOrders()
		if err != nil {
			t.Fatalf("%s: ListNonTerminalLiveOrders: %v", when, err)
		}
		if findLiveReduceOnlyMarket(live, uid(1), "BTC/USDT") != nil {
			t.Errorf("%s: a flatten was journaled, want none", when)
		}
	}
	assertUntouched("stale-snapshot drive")
	// A fresh full pass sees zero unserved rows: equally a no-op.
	if err := e.oms.DriveSafetyEffects(context.Background()); err != nil {
		t.Fatalf("DriveSafetyEffects: %v", err)
	}
	assertUntouched("fresh drive")
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (superseded)", len(got))
	}
	if alerts := e.alerts("kill_effects_superseded"); len(alerts) != 1 ||
		alerts[0].RefID == nil || *alerts[0].RefID != uid(90) {
		t.Errorf("superseded alerts = %+v, want one row ref %s", alerts, uid(90))
	}
}

// LC-38 per-strategy re-check: a clear landing MID-PASS — after the first
// strategy of a tenant kill journals its flatten (the afterJournal seam)
// but before the second strategy's flatten half — stops every remaining
// flatten: exactly ONE flatten exists across both strategies and the
// clear's marker leaves the row served.
func TestKillClearDrill_MidPassClearStopsRemainingFlattens(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.createStrategy(2, "tenant-1")
	// Open a full position for BOTH strategies; the shared BTC balance
	// covers both, so no flatten is ever balance-bounded.
	if err := e.submitEntry(10); err != nil { // strategy 1, token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.submitEntryWith(20, func(p *contract.Proposal) { p.StrategyID = uid(2) }); err != nil {
		t.Fatalf("SubmitApproved: %v", err) // strategy 2, token 2
	}
	for _, tok := range []byte{1, 2} {
		if err := e.venue.Fill(idN(tok, 0), "0.01562", "64000"); err != nil {
			t.Fatalf("Fill token %d: %v", tok, err)
		}
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0.03124", "0")

	epoch, err := e.st.AppendTenantKill(uid(90), "tenant-1", "op-1", formatTime(e.now), true)
	if err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	// The FIRST flatten's journal fires the clear: the pass is mid-event,
	// between the two strategies' flatten halves.
	cleared := false
	e.oms.afterJournal = func() {
		if cleared {
			return
		}
		cleared = true
		if _, _, err := e.st.AppendKillClearTenant(uid(91), "tenant-1", "admin-1",
			"cleared mid-pass", epoch, formatTime(e.now)); err != nil {
			t.Errorf("AppendKillClearTenant: %v", err)
		}
	}
	if err := e.oms.DriveSafetyEffects(context.Background()); err != nil {
		t.Fatalf("DriveSafetyEffects: %v", err)
	}

	if !cleared {
		t.Fatal("no flatten ever journaled: the fixture never exercised the mid-pass window")
	}
	live, err := e.st.ListNonTerminalLiveOrders()
	if err != nil {
		t.Fatalf("ListNonTerminalLiveOrders: %v", err)
	}
	flattens := 0
	for _, sid := range []string{uid(1), uid(2)} {
		if findLiveReduceOnlyMarket(live, sid, "BTC/USDT") != nil {
			flattens++
		}
	}
	if flattens != 1 {
		t.Errorf("flattens journaled = %d, want 1 (the clear stops the remaining strategy's flatten)", flattens)
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (the clear's supersede marker serves the row)", len(got))
	}
	// A fresh pass over the superseded row journals nothing new.
	before := e.tokens.n
	if err := e.oms.DriveSafetyEffects(context.Background()); err != nil {
		t.Fatalf("re-drive: %v", err)
	}
	if e.tokens.n != before {
		t.Errorf("re-drive minted %d new intents, want 0", e.tokens.n-before)
	}
}
