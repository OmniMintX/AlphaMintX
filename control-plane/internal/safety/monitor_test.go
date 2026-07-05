package safety

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

const sid = "00000000-0000-4000-8000-000000000001"

// A tick with DailyPnL <= -daily_loss_limit_quote appends the breaker row
// (persist-then-execute: kind='breaker', scope='strategy', flatten=1,
// kill_epoch NULL, actor 'breaker-monitor', trigger_ref = the monitor
// sample) and THEN invokes DriveSafetyEffects.
func TestMonitorFiresBreakerAndDrives(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.set(sid, "-500") // boundary: <= -limit fires

	h.tick()

	events := h.st.breakerEvents()
	if len(events) != 1 {
		t.Fatalf("breaker events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Scope != "strategy" || ev.StrategyID == nil || *ev.StrategyID != sid {
		t.Errorf("scope/strategy = %s/%v, want strategy/%s", ev.Scope, ev.StrategyID, sid)
	}
	if ev.TenantID == nil || *ev.TenantID != "tenant-1" {
		t.Errorf("tenant_id = %v, want tenant-1 (audit)", ev.TenantID)
	}
	if ev.KillEpoch != nil {
		t.Errorf("kill_epoch = %v, want NULL (a breaker never bumps the epoch)", *ev.KillEpoch)
	}
	if ev.Flatten == nil || !*ev.Flatten {
		t.Error("flatten unset, want 1 (the breaker ALWAYS flattens)")
	}
	if ev.ActorID != "breaker-monitor" {
		t.Errorf("actor_id = %s, want breaker-monitor", ev.ActorID)
	}
	if ev.TriggerRef == nil {
		t.Fatal("trigger_ref = NULL, want the monitor sample JSON")
	}
	var sample map[string]string
	if err := json.Unmarshal([]byte(*ev.TriggerRef), &sample); err != nil {
		t.Fatalf("trigger_ref %q: %v", *ev.TriggerRef, err)
	}
	if sample["daily_pnl"] != "-500" || sample["limit"] != "500" ||
		sample["evaluated_at"] != "2026-07-04T12:00:00Z" {
		t.Errorf("trigger_ref sample = %v", sample)
	}
	h.driver.waitDrive(t)
}

// A second tick the same UTC day appends NO new row: BreakerActiveToday is
// the dedupe AND the latch (invariant 7).
func TestMonitorFiresOncePerUTCDay(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.set(sid, "-750")

	h.tick()
	h.driver.waitDrive(t)
	h.tick()

	if events := h.st.breakerEvents(); len(events) != 1 {
		t.Fatalf("breaker events after second tick = %d, want 1", len(events))
	}
	if n := h.driver.count(); n != 1 {
		t.Errorf("drive invocations = %d, want 1 (no re-fire)", n)
	}
}

// An unset/zero/negative daily_loss_limit_quote NEVER fires: the monitor
// alerts breaker_limit_unset once per strategy per UTC day instead.
func TestMonitorLimitUnsetNeverFires(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "0", "0.5")
	h.pnl.set(sid, "-10000")

	h.tick()
	h.tick()

	if events := h.st.breakerEvents(); len(events) != 0 {
		t.Fatalf("breaker events = %d, want 0 (limit guard)", len(events))
	}
	alerts := h.st.alertsOf("breaker_limit_unset", "")
	if len(alerts) != 1 {
		t.Fatalf("breaker_limit_unset alerts = %d, want 1 (daily dedupe)", len(alerts))
	}
	if alerts[0].StrategyID == nil || *alerts[0].StrategyID != sid {
		t.Errorf("alert strategy = %v, want %s", alerts[0].StrategyID, sid)
	}
}

// Before the first completed startup reconcile the tick is skipped and
// breaker_mark_stale cause=not_reconciled is appended (daily dedupe per
// (kind, cause-as-ref_id, strategy)); a later same-day stale_mark alert is
// NOT suppressed by it.
func TestMonitorNotReconciledSkips(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.set(sid, "-10000")
	h.recon.set(false)

	h.tick()
	h.tick()

	if events := h.st.breakerEvents(); len(events) != 0 {
		t.Fatalf("breaker events = %d, want 0 (reconcile gate)", len(events))
	}
	if alerts := h.st.alertsOf("breaker_mark_stale", "not_reconciled"); len(alerts) != 1 {
		t.Fatalf("not_reconciled alerts = %d, want 1", len(alerts))
	}

	// Reconciled, but now the mark is stale: the same-day stale_mark alert
	// lands despite the earlier not_reconciled row (distinct ref_id).
	h.recon.set(true)
	h.marks.mu.Lock()
	h.marks.stale["BTC/USDT"] = true
	h.marks.mu.Unlock()
	h.tick()
	if alerts := h.st.alertsOf("breaker_mark_stale", "stale_mark"); len(alerts) != 1 {
		t.Fatalf("stale_mark alerts = %d, want 1 (not suppressed by not_reconciled)", len(alerts))
	}
}

// A stale mark on an open position never fires and never silently passes:
// breaker_mark_stale with details_json.cause=stale_mark, once per day.
func TestMonitorStaleMarkNoFire(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.set(sid, "-10000")
	h.marks.stale["BTC/USDT"] = true

	h.tick()
	h.tick()

	if events := h.st.breakerEvents(); len(events) != 0 {
		t.Fatalf("breaker events = %d, want 0 (fail-open, loud)", len(events))
	}
	alerts := h.st.alertsOf("breaker_mark_stale", "stale_mark")
	if len(alerts) != 1 {
		t.Fatalf("stale_mark alerts = %d, want 1 (daily dedupe)", len(alerts))
	}
	if !strings.Contains(alerts[0].DetailsJSON, `"cause":"stale_mark"`) {
		t.Errorf("details_json = %s, want cause stale_mark", alerts[0].DetailsJSON)
	}
}

// A DailyPnL error never fires: breaker_mark_stale with cause=pnl_error.
func TestMonitorPnLErrorNoFire(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.err = errors.New("snapshot torn")

	h.tick()

	if events := h.st.breakerEvents(); len(events) != 0 {
		t.Fatalf("breaker events = %d, want 0", len(events))
	}
	alerts := h.st.alertsOf("breaker_mark_stale", "pnl_error")
	if len(alerts) != 1 {
		t.Fatalf("pnl_error alerts = %d, want 1", len(alerts))
	}
	if !strings.Contains(alerts[0].DetailsJSON, `"cause":"pnl_error"`) {
		t.Errorf("details_json = %s, want cause pnl_error", alerts[0].DetailsJSON)
	}
}

// The TIME-based stall scan alerts an unserved event past the threshold
// even when no drive ever ran (pre-reconcile), deduped per event per UTC
// day; a younger row stays silent.
func TestMonitorStallAlertWithoutDrive(t *testing.T) {
	h := newHarness(t)
	h.recon.set(false) // drives can never run
	old, fresh := "event-old", "event-fresh"
	h.st.events = append(h.st.events,
		store.KillBreakerEvent{EventID: old, Kind: "kill", Scope: "platform",
			ActorID: "op-1", RecordedAt: formatTime(testNow.Add(-11 * time.Minute))},
		store.KillBreakerEvent{EventID: fresh, Kind: "kill", Scope: "platform",
			ActorID: "op-1", RecordedAt: formatTime(testNow.Add(-time.Minute))})

	h.tick()
	h.tick()

	alerts := h.st.alertsOf("safety_effect_stalled", old)
	if len(alerts) != 1 {
		t.Fatalf("stalled alerts for the old event = %d, want 1 (daily dedupe)", len(alerts))
	}
	if alerts[0].StrategyID != nil {
		t.Errorf("alert strategy_id = %v, want NULL for this kind", *alerts[0].StrategyID)
	}
	if got := h.st.alertsOf("safety_effect_stalled", fresh); len(got) != 0 {
		t.Errorf("stalled alerts for the fresh event = %d, want 0", len(got))
	}
}

// A tick panic is recovered, logged, and alerted monitor_tick_panic; the
// next tick still works (invariant 14).
func TestMonitorTickPanicRecovered(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.set(sid, "-750")
	h.limits.panics = true

	if next := h.tick(); next != h.m.active {
		t.Errorf("post-panic interval = %s, want ACTIVE %s", next, h.m.active)
	}
	if alerts := h.st.alertsOf("monitor_tick_panic", ""); len(alerts) != 1 {
		t.Fatalf("monitor_tick_panic alerts = %d, want 1", len(alerts))
	}
	if events := h.st.breakerEvents(); len(events) != 0 {
		t.Fatalf("breaker events = %d, want 0 (the panicked tick never fired)", len(events))
	}

	h.limits.mu.Lock()
	h.limits.panics = false
	h.limits.mu.Unlock()
	h.tick()
	if events := h.st.breakerEvents(); len(events) != 1 {
		t.Fatalf("breaker events after recovery = %d, want 1 (monitor continued)", len(events))
	}
	h.driver.waitDrive(t)
}

// Cadence selection: IDLE when every monitored strategy is flat and quiet;
// ACTIVE on a nonzero position or a non-terminal live order of a monitored
// strategy (a foreign strategy's order stays IDLE).
func TestMonitorCadenceSelection(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "")

	if got := h.tick(); got != h.m.idle {
		t.Errorf("flat-and-quiet interval = %s, want IDLE %s", got, h.m.idle)
	}

	h.st.mu.Lock()
	h.st.orders = []store.LiveOrder{{Order: store.Order{
		OrderID: "o-1", StrategyID: "someone-else", Symbol: "BTC/USDT", Status: "open"}}}
	h.st.mu.Unlock()
	if got := h.tick(); got != h.m.idle {
		t.Errorf("foreign-order interval = %s, want IDLE %s", got, h.m.idle)
	}

	h.st.mu.Lock()
	h.st.orders[0].StrategyID = sid
	h.st.mu.Unlock()
	if got := h.tick(); got != h.m.active {
		t.Errorf("open-order interval = %s, want ACTIVE %s", got, h.m.active)
	}

	h.st.mu.Lock()
	h.st.orders = nil
	h.st.positions[sid] = []store.Position{{StrategyID: sid, Symbol: "BTC/USDT", QtyBase: "0.5"}}
	h.st.mu.Unlock()
	if got := h.tick(); got != h.m.active {
		t.Errorf("open-position interval = %s, want ACTIVE %s", got, h.m.active)
	}
}

// A killed strategy with residual exposure stays monitored: the breaker
// still fires for it (spec §Monitored set).
func TestMonitorKilledWithPositionStillMonitored(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "killed", "500", "0.5")
	h.pnl.set(sid, "-600")

	h.tick()

	if events := h.st.breakerEvents(); len(events) != 1 {
		t.Fatalf("breaker events = %d, want 1 (killed book with exposure is monitored)", len(events))
	}
	h.driver.waitDrive(t)
}

// Poke triggers an immediate evaluation without waiting out the timer: with
// hour-long intervals, the post-poke tick fires the breaker.
func TestMonitorPokeTriggersImmediateEval(t *testing.T) {
	h := newHarness(t)
	h.addStrategy(sid, "live_l1", "500", "0.5")
	h.pnl.calls = make(chan struct{}, 16)
	h.m.active, h.m.idle = time.Hour, time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.m.Run(ctx)
	}()

	select { // the startup tick evaluated (PnL 0: no fire)
	case <-h.pnl.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("startup tick never evaluated")
	}
	h.pnl.set(sid, "-750")
	h.m.Poke(sid)
	h.driver.waitDrive(t) // only a poked tick can fire before the 1 h timer

	cancel()
	<-done
	if events := h.st.breakerEvents(); len(events) != 1 {
		t.Fatalf("breaker events = %d, want 1 (poked evaluation fired)", len(events))
	}
}
