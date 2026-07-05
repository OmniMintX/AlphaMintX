package live

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/safety"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// The WD drills (docs/specs/watchdog.md §Test obligations): deterministic
// fake-venue scenarios over the env harness with an injected, shiftable
// monitor clock and a fake heartbeat feed via Monitor.Beat.

// scanCountStore wraps the real store as the monitor's safety.Store: the
// deferred stall scan's ListUnservedSafetyEvents is the LAST wrapper read
// of every tick, so its count marks tick COMPLETION deterministically
// (the OMS drives hold the raw store and never touch this wrapper).
type scanCountStore struct {
	*store.Store
	scans atomic.Int64
}

func (s *scanCountStore) ListUnservedSafetyEvents() ([]store.KillBreakerEvent, error) {
	out, err := s.Store.ListUnservedSafetyEvents()
	s.scans.Add(1)
	return out, err
}

// watchdogHarness runs one safety.Monitor with the watchdog ENABLED over
// an env: hour-long intervals (ticks fire ONLY via step), the monitor
// clock at e.now + an atomic offset — the test advances ONLY the offset
// while the monitor runs, so e.now stays race-free — a dedicated
// long-max-age mark store (marks stay fresh across advances), and stub
// PnL/limits that never trip the breaker.
type watchdogHarness struct {
	t      *testing.T
	e      *env
	cst    *scanCountStore
	marks  *marketdata.Store
	off    atomic.Int64 // monitor-clock offset from e.now, nanoseconds
	m      *safety.Monitor
	cancel context.CancelFunc
	done   chan struct{}
}

// newWatchdog builds the harness; startOffset seeds the monitor clock —
// safety.New stamps the WD-9 restart baseline from it (the WD8 drill's
// "process start").
func newWatchdog(e *env, startOffset time.Duration) *watchdogHarness {
	e.t.Helper()
	marks, err := marketdata.NewStore(48 * time.Hour)
	if err != nil {
		e.t.Fatalf("marketdata.NewStore: %v", err)
	}
	marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("64000"), TS: testNow})
	h := &watchdogHarness{t: e.t, e: e, cst: &scanCountStore{Store: e.st}, marks: marks}
	h.off.Store(int64(startOffset))
	m, err := safety.New(safety.Config{
		Store: h.cst, PnL: stubPnL{decimal.Zero},
		Limits: stubLimits{decimal.NewFromInt(1000000)},
		Marks:  marks, Driver: e.oms, Recon: e.oms,
		Entries: e.oms, Filters: e.oms,
		ActiveInterval: time.Hour, IdleInterval: time.Hour,
		Now:  h.now,
		Logf: func(string, ...any) {}, // never log after the test ends
	})
	if err != nil {
		e.t.Fatalf("safety.New: %v", err)
	}
	h.m = m
	return h
}

// now is the monitor's clock: e.now shifted by the atomic offset.
func (h *watchdogHarness) now() time.Time {
	return h.e.now.Add(time.Duration(h.off.Load()))
}

// advance moves the monitor clock forward (silence accrues; nothing else
// in the env observes this clock).
func (h *watchdogHarness) advance(d time.Duration) { h.off.Add(int64(d)) }

// start launches Run and waits out the startup tick. The Cleanup catches
// Fatal exits (stop is idempotent: a second call re-cancels and reads the
// closed done channel).
func (h *watchdogHarness) start() {
	h.t.Helper()
	before := h.cst.scans.Load()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() { defer close(h.done); h.m.Run(ctx) }()
	h.t.Cleanup(h.stop)
	waitFor(h.t, "the startup tick", func() bool { return h.cst.scans.Load() > before })
}

// stop cancels Run and waits for it (required before mutating e.now).
func (h *watchdogHarness) stop() {
	h.cancel()
	<-h.done
}

// step forces one full tick and waits for its completion.
func (h *watchdogHarness) step() {
	h.t.Helper()
	before := h.cst.scans.Load()
	h.m.Poke(uid(1))
	waitFor(h.t, "a poked tick", func() bool { return h.cst.scans.Load() > before })
}

// seedPosition books a bare position snapshot (the flatten-dust /
// crash-residue shapes the drills need without a venue round trip).
func seedPosition(e *env, strategyID, qty string) {
	e.t.Helper()
	err := e.st.ApplySweep(func(tx *store.SweepTx) error {
		return tx.UpsertPosition(store.Position{
			StrategyID: strategyID, Symbol: "BTC/USDT", QtyBase: qty,
			EntryPrice: "64000", FeesQuote: "0", RealizedPnLQuote: "0",
			UpdatedAt: formatTime(testNow),
		})
	})
	if err != nil {
		e.t.Fatalf("seed position: %v", err)
	}
}

// killEpoch is the strategy's standing-kill epoch (0 = no kill row).
func killEpoch(e *env, strategyID string) int64 {
	e.t.Helper()
	epoch, err := e.st.GlobalMaxKillEpoch(strategyID)
	if err != nil {
		e.t.Fatalf("GlobalMaxKillEpoch(%s): %v", strategyID, err)
	}
	return epoch
}

// alertsFor lists one kind's alerts for one strategy.
func alertsFor(e *env, kind, strategyID string) []store.SafetyAlert {
	e.t.Helper()
	out, err := e.st.ListSafetyAlerts(store.SafetyAlertFilter{Kind: kind, StrategyID: strategyID})
	if err != nil {
		e.t.Fatalf("ListSafetyAlerts(%s, %s): %v", kind, strategyID, err)
	}
	return out
}

// WD1: beats every 30 s keep the ladder quiet across many ticks — no
// alert, no cancel, no kill (WD-15: no "recovered" bookkeeping either).
func TestWatchdogDrill_HeartbeatKeepsQuiet(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil { // resting entry (token 1)
		t.Fatalf("SubmitApproved: %v", err)
	}
	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	for i := 0; i < 6; i++ { // 180 s of wall time, never > 30 s silent
		h.advance(30 * time.Second)
		h.m.Beat(uid(1), h.now())
		h.step()
	}
	if got := e.alerts("watchdog_silence"); len(got) != 0 {
		t.Errorf("watchdog_silence alerts = %d, want 0", len(got))
	}
	if got := e.alerts("watchdog_kill_escalation"); len(got) != 0 {
		t.Errorf("watchdog_kill_escalation alerts = %d, want 0", len(got))
	}
	if got := killEpoch(e, uid(1)); got != 0 {
		t.Errorf("kill epoch = %d, want 0", got)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" {
		t.Errorf("resting entry status = %s, want open (never swept)", ord.Status)
	}
	if got := e.lifecycle(uid(1)); got != "live_l1" {
		t.Errorf("lifecycle = %s, want live_l1", got)
	}
}

// WD2: 90 s silence engages rung 1 ONLY — the resting ENTRY is canceled
// and the claimed-unsent pending_new intent claim-revoked, the PROTECTIVE
// stays at the venue, ONE watchdog_silence alert lands, and neither a
// kill row nor a lifecycle change exists (invariant 2).
func TestWatchdogDrill_SilenceCancelsEntriesOnly(t *testing.T) {
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
	// A claimed-but-unsent pending_new intent (crash between claim and
	// send): the sweep must revoke the claim so the send cannot follow.
	_, intent := e.journalOrder(tokenN(9))
	if ok, err := e.st.RecordIntentClaim(intent.ClientOrderID, formatTime(testNow)); err != nil || !ok {
		t.Fatalf("RecordIntentClaim: ok=%v err=%v", ok, err)
	}

	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	h.advance(91 * time.Second)
	h.step()

	if ord := e.order(idN(3, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	got, err := e.st.GetOrderIntent(intent.ClientOrderID)
	if err != nil || got.ClaimRevokedAt == nil {
		t.Errorf("claimed-unsent intent = %+v (err %v), want claim REVOKED", got, err)
	}
	if ord := e.order(intent.ClientOrderID); ord.Status != "canceled" {
		t.Errorf("journaled entry status = %s, want canceled", ord.Status)
	}
	// The protective is untouched at the venue (invariant 2).
	open := e.venueOpen()
	if len(open) != 1 || open[0].Type != "STOP_LOSS" {
		t.Errorf("venue open orders = %+v, want exactly the resting SL", open)
	}
	silence := e.alerts("watchdog_silence")
	if len(silence) != 1 {
		t.Fatalf("watchdog_silence alerts = %d, want 1", len(silence))
	}
	a := silence[0]
	if a.StrategyID == nil || *a.StrategyID != uid(1) || a.RefID == nil || *a.RefID != "silence" {
		t.Errorf("alert row = %+v, want strategy %s ref 'silence'", a, uid(1))
	}
	if !strings.Contains(a.DetailsJSON, `"cause":"silence"`) ||
		!strings.Contains(a.DetailsJSON, `"last_seen":""`) {
		t.Errorf("details_json = %s, want cause silence with the baseline's empty last_seen", a.DetailsJSON)
	}
	if got := killEpoch(e, uid(1)); got != 0 {
		t.Errorf("kill epoch = %d, want 0 (rung 1 never kills)", got)
	}
	if got := e.lifecycle(uid(1)); got != "live_l1" {
		t.Errorf("lifecycle = %s, want live_l1 (rung 1 never touches lifecycle)", got)
	}
}

// WD3: persisting silence re-runs the sweep every tick — a fresh ENTRY
// appearing mid-silence is swept — with ZERO duplicate watchdog_silence
// rows the same UTC day; the next UTC day alerts once more.
func TestWatchdogDrill_RepeatCancelNoAlertSpam(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	h.advance(91 * time.Second)
	h.step()
	if ord := e.order(idN(1, 0)); ord.Status != "canceled" {
		t.Fatalf("first entry status = %s, want canceled", ord.Status)
	}
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Fatalf("watchdog_silence alerts = %d, want 1", len(got))
	}

	// A fresh ENTRY mid-silence is swept by the next tick's re-run; the
	// same-day alert never duplicates (dedupe alerts, never actions).
	if err := e.submitEntry(20); err != nil { // token 2
		t.Fatalf("SubmitApproved: %v", err)
	}
	h.advance(10 * time.Second)
	h.step()
	if ord := e.order(idN(2, 0)); ord.Status != "canceled" {
		t.Errorf("mid-silence entry status = %s, want canceled (sweep re-ran)", ord.Status)
	}
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("same-day watchdog_silence alerts = %d, want 1", len(got))
	}

	// Next UTC day, held inside the rung-1 band by a 23:59:00 beat: the
	// daily dedupe re-arms and exactly one more alert lands.
	h.off.Store(int64(11*time.Hour + 59*time.Minute))
	h.m.Beat(uid(1), h.now())
	h.advance(91 * time.Second) // 2026-07-05T00:00:31Z, 91 s after the beat
	h.step()
	silence := e.alerts("watchdog_silence")
	if len(silence) != 2 {
		t.Fatalf("watchdog_silence alerts after midnight = %d, want 2", len(silence))
	}
	if !strings.HasPrefix(silence[1].RecordedAt, "2026-07-05") ||
		!strings.Contains(silence[1].DetailsJSON, `"last_seen":"2026-07-04T23:59:00Z"`) {
		t.Errorf("second alert = %+v, want day-2 row carrying the beat's last_seen", silence[1])
	}
	if got := killEpoch(e, uid(1)); got != 0 {
		t.Errorf("kill epoch = %d, want 0 (still rung 1)", got)
	}
}

// WD4: 10 minutes of silence escalates to rung 2 — ONE kill row (actor
// watchdog, flatten=0), the watchdog_kill_escalation alert (ref_id = the
// kill event_id, cause silence_10m), entries swept, lifecycle killed,
// protectives REMAIN, and the row serves; subsequent ticks skip on the
// standing kill (never a second row).
func TestWatchdogDrill_TenMinuteKillEscalation(t *testing.T) {
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

	h := newWatchdog(e, 0)
	h.start()
	h.advance(601 * time.Second)
	h.step()

	var kill store.KillBreakerEvent
	for _, ev := range e.unserved() {
		if ev.Kind == "kill" {
			kill = ev
		}
	}
	if kill.EventID == "" {
		t.Fatal("no kill row after 601 s of silence")
	}
	if kill.Scope != "strategy" || kill.ActorID != "watchdog" ||
		kill.Flatten == nil || *kill.Flatten ||
		kill.KillEpoch == nil || *kill.KillEpoch != 1 {
		t.Errorf("kill row = %+v, want strategy scope, actor watchdog, flatten=0, epoch 1", kill)
	}
	esc := e.alerts("watchdog_kill_escalation")
	if len(esc) != 1 || esc[0].RefID == nil || *esc[0].RefID != kill.EventID ||
		!strings.Contains(esc[0].DetailsJSON, `"cause":"silence_10m"`) {
		t.Errorf("escalation alerts = %+v, want one row ref %s cause silence_10m", esc, kill.EventID)
	}
	// The fire's async drive sweeps entries and locks the lifecycle.
	waitFor(t, "the lifecycle lock", func() bool { return e.lifecycle(uid(1)) == "killed" })
	waitFor(t, "the entry sweep", func() bool { return e.order(idN(3, 0)).Status == "canceled" })

	// The standing kill skips every later evaluation: no second row, no
	// second alert (invariant 4).
	h.advance(10 * time.Second)
	h.step()
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Errorf("kill epoch after the skip tick = %d, want still 1", got)
	}
	if got := e.alerts("watchdog_kill_escalation"); len(got) != 1 {
		t.Errorf("escalation alerts after the skip tick = %d, want still 1", len(got))
	}
	h.stop()

	// A reconcile STRICTLY after recorded_at serves the row; the
	// protective SL still rests (flatten=0: stops remain — WD-19).
	e.now = e.now.Add(602 * time.Second)
	e.reconcile()
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (served)", len(got))
	}
	open := e.venueOpen()
	if len(open) != 1 || open[0].Type != "STOP_LOSS" {
		t.Errorf("venue open orders = %+v, want exactly the preserved SL", open)
	}
}

// WD5: a position with NO protective at >90 s of silence takes the
// unprotected-exposure FAST PATH — the rung-2 kill fires immediately, ten
// minutes early.
func TestWatchdogDrill_UnprotectedExposureFastPath(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.openPosition(10) // filled and booked, NO stop: naked exposure

	h := newWatchdog(e, 0)
	h.start()
	h.advance(91 * time.Second)
	h.step()

	if got := killEpoch(e, uid(1)); got != 1 {
		t.Fatalf("kill epoch at 91 s = %d, want 1 (the fast path fires ten minutes early)", got)
	}
	esc := e.alerts("watchdog_kill_escalation")
	if len(esc) != 1 || !strings.Contains(esc[0].DetailsJSON, `"cause":"unprotected_exposure"`) {
		t.Errorf("escalation alerts = %+v, want one row cause unprotected_exposure", esc)
	}
	waitFor(t, "the lifecycle lock", func() bool { return e.lifecycle(uid(1)) == "killed" })
	h.stop()
}

// WD6: the same silence with the position PROTECTED stays on rung 1 at
// 91 s (protection defers the fast path) and still escalates at the
// unconditional 10-minute rung.
func TestWatchdogDrill_ProtectedExposureNoEscalation(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01562", "0")
	e.reconcile() // books the fill; the drive places the SL (token 2)

	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	h.advance(91 * time.Second)
	h.step()
	if got := killEpoch(e, uid(1)); got != 0 {
		t.Fatalf("kill epoch at 91 s = %d, want 0 (protected exposure never fast-paths)", got)
	}
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("watchdog_silence alerts = %d, want 1 (rung 1 ran)", len(got))
	}

	h.advance(510 * time.Second) // 601 s total
	h.step()
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Fatalf("kill epoch at 601 s = %d, want 1 (the 10-minute rung is unconditional)", got)
	}
	esc := e.alerts("watchdog_kill_escalation")
	if len(esc) != 1 || !strings.Contains(esc[0].DetailsJSON, `"cause":"silence_10m"`) {
		t.Errorf("escalation alerts = %+v, want one row cause silence_10m", esc)
	}
	waitFor(t, "the lifecycle lock", func() bool { return e.lifecycle(uid(1)) == "killed" })
}

// WD7: a standing kill (any actor) makes the watchdog SKIP the strategy
// entirely — 700 s of silence appends no second kill row and no watchdog
// alerts (invariant 4: at most one kill row per silence episode).
func TestWatchdogDrill_AlreadyKilledSkip(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "drill-operator",
		formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	h.advance(700 * time.Second)
	h.step()
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Errorf("kill epoch = %d, want still 1 (no watchdog row)", got)
	}
	if got := e.alerts("watchdog_silence"); len(got) != 0 {
		t.Errorf("watchdog_silence alerts = %d, want 0 (the skip precedes rung 1)", len(got))
	}
	if got := e.alerts("watchdog_kill_escalation"); len(got) != 0 {
		t.Errorf("watchdog_kill_escalation alerts = %d, want 0 (the operator kill is not the watchdog's)", len(got))
	}
}

// The WD-16 back-fill: a watchdog-actor kill row with NO escalation alert
// (the crash between WD-19 steps 2 and 3) gets the missing alert appended
// by the skip path — details EXACTLY {"cause":"backfill"} — and never
// twice.
func TestWatchdogDrill_EscalationAlertBackfill(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if _, err := e.st.AppendStrategyKill(uid(90), uid(1), "watchdog",
		formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	h := newWatchdog(e, 0)
	h.start() // the startup tick's skip path back-fills
	defer h.stop()
	esc := e.alerts("watchdog_kill_escalation")
	if len(esc) != 1 {
		t.Fatalf("escalation alerts after the startup tick = %d, want the back-fill", len(esc))
	}
	if esc[0].RefID == nil || *esc[0].RefID != uid(90) || esc[0].DetailsJSON != `{"cause":"backfill"}` {
		t.Errorf("back-fill alert = %+v, want ref %s details {\"cause\":\"backfill\"}", esc[0], uid(90))
	}
	h.step() // idempotent: the dedupe holds
	if got := e.alerts("watchdog_kill_escalation"); len(got) != 1 {
		t.Errorf("escalation alerts after a second tick = %d, want still 1", len(got))
	}
}

// WD8: a monitor restart mid-silence resets the baseline to PROCESS START
// (WD-9: lastSeen is in-memory only): no fire at +89 s from the restart
// even though total silence is far past 90 s, rung 1 at >90 s, and a
// never-heartbeating strategy is still caught by rung 2 from watch-set
// entry.
func TestWatchdogDrill_RestartBaselineGrace(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	h1 := newWatchdog(e, 0)
	h1.start()
	h1.advance(60 * time.Second) // silence accrues below the threshold
	h1.step()
	h1.stop() // the "restart": all heartbeat state is lost

	if err := e.submitEntry(10); err != nil { // resting entry (token 1)
		t.Fatalf("SubmitApproved: %v", err)
	}
	h2 := newWatchdog(e, 60*time.Second) // process start = t0 + 60 s
	h2.start()
	h2.advance(89 * time.Second) // 149 s of TOTAL silence, 89 s from restart
	h2.step()
	if got := e.alerts("watchdog_silence"); len(got) != 0 {
		t.Fatalf("watchdog_silence alerts at +89 s = %d, want 0 (baseline = process start)", len(got))
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" {
		t.Fatalf("entry status at +89 s = %s, want open (no sweep inside the grace window)", ord.Status)
	}

	h2.advance(2 * time.Second) // 91 s from the restart baseline
	h2.step()
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("watchdog_silence alerts at +91 s = %d, want 1", len(got))
	}
	if ord := e.order(idN(1, 0)); ord.Status != "canceled" {
		t.Errorf("entry status at +91 s = %s, want canceled", ord.Status)
	}

	// Never a single heartbeat: rung 2 still catches it from the baseline.
	h2.off.Store(int64(60*time.Second + 601*time.Second))
	h2.step()
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Errorf("kill epoch at baseline+601 s = %d, want 1", got)
	}
	waitFor(t, "the lifecycle lock", func() bool { return e.lifecycle(uid(1)) == "killed" })
	h2.stop()
}

// WD9: while Reconciled() is false the rung-1 alert still appends but the
// sweep and the fast path are gated (WD-14); the PURE 10-minute rung
// appends the kill row anyway, and the effects run only after the first
// completed reconcile (the R7-hooked drive).
func TestWatchdogDrill_ReconGateDeferral(t *testing.T) {
	e := newEnv(t)
	// NO startup reconcile. The crash shape: a journaled pending_new
	// ENTRY resting at the venue, plus an unprotected position on the
	// books (it would arm the fast path were it not recon-gated).
	_, intent := e.journalOrder(tokenN(9))
	e.venue.AddOpenOrder(exchange.OrderState{
		VenueSymbol: "BTCUSDT", ClientOrderID: intent.ClientOrderID, Status: "NEW",
		Side: "BUY", Type: "LIMIT", Price: "64000", OrigQty: "0.01562",
		ExecutedQty: "0", CumQuoteQty: "0",
	})
	seedPosition(e, uid(1), "0.01562")
	e.venue.SetBalance("BTC", "0.01562", "0")

	h := newWatchdog(e, 0)
	h.start()
	h.advance(91 * time.Second)
	h.step()
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("watchdog_silence alerts = %d, want 1 (the alert is not recon-gated)", len(got))
	}
	if ord := e.order(intent.ClientOrderID); ord.Status != "pending_new" {
		t.Errorf("journaled entry status = %s, want pending_new (sweep gated)", ord.Status)
	}
	if got := killEpoch(e, uid(1)); got != 0 {
		t.Errorf("kill epoch at 91 s = %d, want 0 (fast path gated)", got)
	}

	h.advance(510 * time.Second) // 601 s: the pure rung is NOT gated
	h.step()
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Fatalf("kill epoch at 601 s = %d, want 1 (the row appends while un-reconciled)", got)
	}
	if got := e.lifecycle(uid(1)); got != "live_l1" {
		t.Errorf("lifecycle = %s, want live_l1 (effects deferred to the reconcile)", got)
	}
	if got := e.venueOpen(); len(got) != 1 {
		t.Errorf("venue open orders = %d, want 1 (nothing canceled yet)", len(got))
	}
	h.stop()

	// The first completed reconcile adopts the intent and its R7-hooked
	// drive runs the deferred effects to a served row.
	e.now = e.now.Add(602 * time.Second)
	e.reconcile()
	if got := e.lifecycle(uid(1)); got != "killed" {
		t.Errorf("lifecycle after the reconcile = %s, want killed", got)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders after the reconcile = %d, want 0", len(got))
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0 (served)", len(got))
	}
}

// WD10: the watch set is STRICTLY live_* — paper/paused strategies and a
// killed book with residual exposure are never evaluated; a strategy
// promoted to live_* mid-run gets a FRESH firstWatched baseline.
func TestWatchdogDrill_WatchSetScope(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	for id, state := range map[int]string{2: "paper", 3: "paused", 4: "killed"} {
		if err := e.st.CreateStrategy(store.Strategy{
			StrategyID: uid(id), TenantID: "tenant-1", Name: uid(id), LifecycleState: state,
			CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
		}); err != nil {
			t.Fatalf("CreateStrategy(%d): %v", id, err)
		}
	}
	seedPosition(e, uid(4), "0.5") // killed residual exposure: a breaker candidate, never a watchdog target

	h := newWatchdog(e, 0)
	h.start()
	h.advance(700 * time.Second)
	h.step()
	// Only the live strategy escalated.
	if got := killEpoch(e, uid(1)); got != 1 {
		t.Errorf("live strategy kill epoch = %d, want 1", got)
	}
	for _, id := range []int{2, 3, 4} {
		if got := killEpoch(e, uid(id)); got != 0 {
			t.Errorf("%s (%s) kill epoch = %d, want 0", uid(id), "non-live", got)
		}
		for _, kind := range []string{"watchdog_silence", "watchdog_kill_escalation"} {
			if got := alertsFor(e, kind, uid(id)); len(got) != 0 {
				t.Errorf("%s alerts for %s = %d, want 0 (never watched)", kind, uid(id), len(got))
			}
		}
	}

	// Promotion mid-run: paper -> live_l1 enters the watch set with a
	// fresh baseline — quiet at +89 s, rung 1 at +91 s, never rung 2.
	if err := e.st.AppendLifecycleTransition(store.LifecycleTransition{
		TransitionID: newUUID(), StrategyID: uid(2), FromState: "paper", ToState: "live_l1",
		ActorID: "admin-1", ActorRole: "admin", Reason: "drill promotion",
		RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}
	h.step() // watch-set entry stamps firstWatched at the CURRENT tick
	h.advance(89 * time.Second)
	h.step()
	if got := alertsFor(e, "watchdog_silence", uid(2)); len(got) != 0 {
		t.Fatalf("promoted strategy alerts at +89 s = %d, want 0 (fresh baseline)", len(got))
	}
	h.advance(2 * time.Second)
	h.step()
	if got := alertsFor(e, "watchdog_silence", uid(2)); len(got) != 1 {
		t.Errorf("promoted strategy alerts at +91 s = %d, want 1", len(got))
	}
	if got := killEpoch(e, uid(2)); got != 0 {
		t.Errorf("promoted strategy kill epoch = %d, want 0 (rung 1 only)", got)
	}
	h.stop()
}

// WD11: a venue error during the rung-1 cancel is logged and re-attempted
// by the NEXT tick's sweep — no crash, no duplicate alert.
func TestWatchdogDrill_CancelRetryAfterError(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	h := newWatchdog(e, 0)
	h.start()
	defer h.stop()
	e.venue.FailNext("CancelOrder", errors.New("venue exploded"))
	h.advance(91 * time.Second)
	h.step()
	// The failed cancel left the order untouched everywhere.
	if ord := e.order(idN(1, 0)); ord.Status != "open" {
		t.Errorf("entry status after the failed sweep = %s, want open", ord.Status)
	}
	if got := e.venueOpen(); len(got) != 1 {
		t.Errorf("venue open orders = %d, want 1", len(got))
	}
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("watchdog_silence alerts = %d, want 1", len(got))
	}

	h.advance(10 * time.Second)
	h.step() // the next sweep succeeds
	if ord := e.order(idN(1, 0)); ord.Status != "canceled" {
		t.Errorf("entry status after the retry = %s, want canceled", ord.Status)
	}
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders after the retry = %d, want 0", len(got))
	}
	if got := e.alerts("watchdog_silence"); len(got) != 1 {
		t.Errorf("watchdog_silence alerts after the retry = %d, want still 1 (deduped)", len(got))
	}
}

// WD12: dust residues the OMS itself could never protect — |qty| below
// the venue minQty, or notional below minNotional at a fresh mark — are
// EXCLUDED from the unprotected-exposure predicate (no fast-path kill at
// 91 s); the unconditional 10-minute rung still escalates.
func TestWatchdogDrill_DustResidueNotUnprotected(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	e.createStrategy(2, "tenant-1")
	seedPosition(e, uid(1), "0.000005") // below minQty 0.00001
	seedPosition(e, uid(2), "0.00006")  // notional 3.84 below minNotional 5 at mark 64000

	h := newWatchdog(e, 0)
	h.start()
	h.advance(91 * time.Second)
	h.step()
	for _, id := range []int{1, 2} {
		if got := killEpoch(e, uid(id)); got != 0 {
			t.Errorf("%s kill epoch at 91 s = %d, want 0 (dust never arms the fast path)", uid(id), got)
		}
		if got := alertsFor(e, "watchdog_silence", uid(id)); len(got) != 1 {
			t.Errorf("%s watchdog_silence alerts = %d, want 1 (rung 1 still runs)", uid(id), len(got))
		}
	}

	h.advance(510 * time.Second) // 601 s: the backstop is unconditional
	h.step()
	for _, id := range []int{1, 2} {
		if got := killEpoch(e, uid(id)); got == 0 {
			t.Errorf("%s kill epoch at 601 s = 0, want a rung-2 kill", uid(id))
		}
		esc := alertsFor(e, "watchdog_kill_escalation", uid(id))
		if len(esc) != 1 || !strings.Contains(esc[0].DetailsJSON, `"cause":"silence_10m"`) {
			t.Errorf("%s escalation alerts = %+v, want one row cause silence_10m", uid(id), esc)
		}
	}
	waitFor(t, "the lifecycle locks", func() bool {
		return e.lifecycle(uid(1)) == "killed" && e.lifecycle(uid(2)) == "killed"
	})
	h.stop()
}

// waitTestnet polls cond against a real-time deadline; the timeout FAILS
// the drill (never satisfied vacuously).
func waitTestnet(t *testing.T, ctx context.Context, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTestnetDrill_Watchdog is the real-venue watchdog drill (watchdog.md
// §Test obligations): REAL entries plus a REAL protective on testnet, NO
// heartbeats ever, the monitor on REAL time (the thresholds are
// constants, so the drill runs past 10 real minutes). Past 90 s the
// rung-1 sweep cancels the resting ENTRY at the venue while the
// protective rests; past 10 min rung 2 appends the watchdog kill row,
// locks the lifecycle, and the row serves — flatten=0, so the position
// and its protective REMAIN on the testnet book. Non-vacuity by
// construction: >= 1 REAL venue cancel observed — zero fails.
func TestTestnetDrill_Watchdog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	d := newTestnetDrill(t, ctx)
	oms := d.newOMS()
	if err := oms.TriggerRun(ctx, false); err != nil {
		t.Fatalf("startup TriggerRun: %v", err)
	}
	// Entry #1 (marketable, WITH a stop): fills; the post-run protective
	// drive places the REAL SL.
	marketable := floorToStep(d.last.Mul(decimal.RequireFromString("1.02")), d.sf.tick)
	stop := floorToStep(d.last.Mul(decimal.RequireFromString("0.9")), d.sf.tick)
	entry1 := d.submitDrillEntry(t, oms, 10, marketable, &stop)
	waitVenueFilled(t, ctx, d.bn, entry1)
	slBefore := d.tokens.count()
	if err := oms.TriggerRun(ctx, false); err != nil {
		t.Fatalf("post-fill TriggerRun: %v", err)
	}
	slID := latestAttemptID(t, d.st, d.tokens.at(t, slBefore))
	// Entry #2 (2% below the book): the REAL resting entry for the sweep.
	resting := floorToStep(d.last.Mul(decimal.RequireFromString("0.98")), d.sf.tick)
	entry2 := d.submitDrillEntry(t, oms, 20, resting, nil)

	m, err := safety.New(safety.Config{
		Store: d.st, PnL: stubPnL{decimal.Zero},
		Limits: stubLimits{decimal.NewFromInt(1000000)},
		Marks:  d.marks, Driver: oms, Recon: oms,
		Entries: oms, Filters: oms,
		ActiveInterval: 5 * time.Second, IdleInterval: 5 * time.Second,
		Logf: func(string, ...any) {}, // never log after the test ends
	})
	if err != nil {
		t.Fatalf("safety.New: %v", err)
	}
	mctx, mcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); m.Run(mctx) }()

	// Past 90 s of real silence: the rung-1 sweep cancels the REAL entry
	// while the protective rests and NO kill exists yet.
	waitTestnet(t, ctx, 3*time.Minute, "the rung-1 entry cancel", func() bool {
		state, err := d.bn.QueryOrder(ctx, "BTCUSDT", entry2)
		return err == nil && state.Status == "CANCELED"
	})
	if state, err := d.bn.QueryOrder(ctx, "BTCUSDT", slID); err != nil || state.Status != "NEW" {
		t.Fatalf("protective %s = %+v (err %v), want still resting NEW after rung 1", slID, state, err)
	}
	if epoch, err := d.st.GlobalMaxKillEpoch(uid(1)); err != nil || epoch != 0 {
		t.Fatalf("kill epoch after rung 1 = %d (err %v), want 0", epoch, err)
	}
	if rows, err := d.st.ListSafetyAlerts(store.SafetyAlertFilter{
		Kind: "watchdog_silence", StrategyID: uid(1)}); err != nil || len(rows) != 1 {
		t.Errorf("watchdog_silence alerts = %d (err %v), want exactly 1 (daily dedupe)", len(rows), err)
	}

	// Past 10 real minutes: rung 2 appends the kill row.
	waitTestnet(t, ctx, 12*time.Minute, "the rung-2 kill row", func() bool {
		epoch, err := d.st.GlobalMaxKillEpoch(uid(1))
		return err == nil && epoch > 0
	})
	mcancel()
	<-done
	eventID, actor, ok, err := d.st.LatestStrategyKillEvent(uid(1))
	if err != nil || !ok || actor != "watchdog" {
		t.Fatalf("latest kill = (%s, %s, %v, %v), want a watchdog-actor row", eventID, actor, ok, err)
	}
	if rows, err := d.st.ListSafetyAlerts(store.SafetyAlertFilter{
		Kind: "watchdog_kill_escalation", StrategyID: uid(1)}); err != nil ||
		len(rows) != 1 || rows[0].RefID == nil || *rows[0].RefID != eventID {
		t.Errorf("escalation alerts = %+v (err %v), want one row ref %s", rows, err, eventID)
	}

	// The effects drive to a served row; flatten=0 leaves the position
	// and its protective resting on the testnet book.
	waitServed(t, ctx, d.st, oms)
	if s, err := d.st.GetStrategy(uid(1)); err != nil || s.LifecycleState != "killed" {
		t.Errorf("lifecycle = %q (err %v), want killed", s.LifecycleState, err)
	}
	if state, err := d.bn.QueryOrder(ctx, "BTCUSDT", slID); err != nil || state.Status != "NEW" {
		t.Errorf("protective %s = %+v (err %v), want STILL resting (flatten=0: stops remain)", slID, state, err)
	}
}
