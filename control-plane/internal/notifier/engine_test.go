package notifier

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestDispatchMatrix pins the AN-13 wire: rows in all three tables are
// delivered in rowid order per source with correct envelopes — schema
// pin, seq = rowid, explicit wire columns including nulls, flatten as a
// JSON bool, tenant_id present on kill_breaker_events — and webhook mode
// emits NO SAFETY-EVENT marker line.
func TestDispatchMatrix(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})

	sid, tid, trigger := uid(1), "tenant-1", "breach-ref"
	epoch, flatten := int64(3), true
	if err := h.st.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(20), Kind: "kill", Scope: "strategy", StrategyID: &sid, TenantID: &tid,
		KillEpoch: &epoch, Flatten: &flatten, TriggerRef: &trigger,
		ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	killEpoch := h.platformKill(21)
	if _, _, err := h.st.AppendKillClearPlatform(uid(30), "op-1", "resolved", killEpoch, formatTime(testNow)); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	h.alert(40, "watchdog_escalated", `{"stage":2}`)

	e.runPass(context.Background())

	bySource := map[string][]capture{}
	for _, c := range r.accepted() {
		bySource[c.Source] = append(bySource[c.Source], c)
	}
	kills := bySource[store.AlertSourceKillBreaker]
	if len(kills) != 2 || kills[0].Seq != 1 || kills[1].Seq != 2 {
		t.Fatalf("kill envelopes = %+v, want seq 1 then 2 (rowid order)", kills)
	}
	k := kills[0]
	if k.Schema != Schema || k.ID != uid(20) || k.DeliveredAt == "" {
		t.Errorf("kill envelope header = %+v, want schema pin + id + delivered_at", k)
	}
	if k.Event["event_id"] != uid(20) || k.Event["strategy_id"] != sid ||
		k.Event["tenant_id"] != tid || k.Event["kill_epoch"] != float64(3) ||
		k.Event["flatten"] != true || k.Event["trigger_ref"] != trigger ||
		k.Event["actor_id"] != "admin-1" {
		t.Errorf("kill event = %+v, want explicit AN-13 wire columns", k.Event)
	}
	if v, present := kills[1].Event["strategy_id"]; !present || v != nil {
		t.Errorf("platform kill strategy_id = %v (present %v), want explicit null", v, present)
	}
	if v, present := kills[1].Event["tenant_id"]; !present || v != nil {
		t.Errorf("platform kill tenant_id = %v (present %v), want explicit null", v, present)
	}

	clears := bySource[store.AlertSourceKillClear]
	if len(clears) != 1 || clears[0].ID != uid(30) || clears[0].Seq != 1 {
		t.Fatalf("clear envelopes = %+v, want one with seq 1", clears)
	}
	ce := clears[0].Event
	if ce["clear_id"] != uid(30) || ce["scope"] != "platform" || ce["cleared_epoch"] != float64(killEpoch) ||
		ce["reason"] != "resolved" || ce["actor_id"] != "op-1" {
		t.Errorf("clear event = %+v, want full kill_clear_events row", ce)
	}
	if _, hasFlatten := ce["flatten"]; hasFlatten {
		t.Error("clear event carries flatten, want absent (no such column)")
	}

	alerts := bySource[store.AlertSourceSafetyAlert]
	// The platform clear appended a kill_effects_superseded companion; the
	// explicit alert arrives last, in rowid order.
	if len(alerts) < 1 {
		t.Fatal("no safety_alert envelopes delivered")
	}
	lastAlert := alerts[len(alerts)-1]
	if lastAlert.ID != uid(40) || lastAlert.Event["kind"] != "watchdog_escalated" ||
		lastAlert.Event["details_json"] != `{"stage":2}` {
		t.Errorf("alert envelope = %+v, want the OS-18 DTO shape with raw details_json", lastAlert)
	}
	for i := 1; i < len(alerts); i++ {
		if alerts[i].Seq <= alerts[i-1].Seq {
			t.Errorf("alert seq not ascending: %d then %d", alerts[i-1].Seq, alerts[i].Seq)
		}
	}
	// Watermarks advanced to each source's max rowid.
	for _, src := range sources {
		max, err := h.st.MaxAlertSourceRowid(src)
		if err != nil {
			t.Fatalf("MaxAlertSourceRowid(%s): %v", src, err)
		}
		if got := h.watermark(src); got != max {
			t.Errorf("watermark %s = %d, want %d", src, got, max)
		}
	}
	// Webhook mode: no SAFETY-EVENT line (AN-14), one summary line per
	// delivery.
	if h.events.String() != "" {
		t.Errorf("webhook mode wrote SAFETY-EVENT lines: %q", h.events.String())
	}
	if got, want := h.logs.count("alert dispatched source="), len(r.accepted()); got != want {
		t.Errorf("success summary lines = %d, want %d", got, want)
	}
}

// TestSourceOrderWithinPass pins AN-6a: kill_breaker_events strictly
// before kill_clear_events, strictly before safety_alerts.
func TestSourceOrderWithinPass(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})

	// Insert in REVERSE urgency order: arrival order must still be fixed.
	h.alert(40, "test_alert", "{}")
	killEpoch := h.platformKill(20)
	if _, _, err := h.st.AppendKillClearPlatform(uid(30), "op-1", "resolved", killEpoch, formatTime(testNow)); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	e.runPass(context.Background())

	rank := map[string]int{
		store.AlertSourceKillBreaker: 0,
		store.AlertSourceKillClear:   1,
		store.AlertSourceSafetyAlert: 2,
	}
	got := r.accepted()
	if len(got) < 3 {
		t.Fatalf("deliveries = %d, want >= 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if rank[got[i].Source] < rank[got[i-1].Source] {
			t.Fatalf("source order violated: %s after %s", got[i].Source, got[i-1].Source)
		}
	}
	if got[0].Source != store.AlertSourceKillBreaker {
		t.Errorf("first delivery source = %s, want kill_breaker_events", got[0].Source)
	}
}

// TestSeedAtEnable pins AN-8: pre-existing rows in all tables, first
// start ⇒ zero deliveries and watermarks at max rowid; a row appended
// after the seed point IS delivered.
func TestSeedAtEnable(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	killEpoch := h.platformKill(20)
	if _, _, err := h.st.AppendKillClearPlatform(uid(30), "op-1", "resolved", killEpoch, formatTime(testNow)); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	h.alert(40, "pre_enable", "{}")

	e, backlog := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	for _, src := range sources {
		if backlog[src] != 0 {
			t.Errorf("backlog[%s] = %d, want 0 at first enable", src, backlog[src])
		}
		max, _ := h.st.MaxAlertSourceRowid(src)
		if got := h.watermark(src); got != max {
			t.Errorf("seeded watermark %s = %d, want MAX rowid %d", src, got, max)
		}
	}
	e.runPass(context.Background())
	if got := r.captures(); len(got) != 0 {
		t.Fatalf("deliveries after seed = %d, want 0 (no history flood)", len(got))
	}
	h.alert(41, "post_enable", "{}")
	e.runPass(context.Background())
	got := r.accepted()
	if len(got) != 1 || got[0].ID != uid(41) {
		t.Fatalf("post-seed deliveries = %+v, want exactly the new row", got)
	}
}

// TestSeedRace pins the AN-8 seed-race case: a row appended CONCURRENTLY
// with seeding is never lost — the one-statement seed leaves it either at
// or below the watermark (committed before the seed point) or above it,
// in which case it IS delivered. A row appended after Seed returns is
// always delivered.
func TestSeedRace(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e := h.engine(Config{URL: r.srv.URL, Heartbeat: 0})
	appended := make(chan error, 1)
	go func() {
		appended <- h.st.AppendSafetyAlert(store.SafetyAlert{
			AlertID: uid(40), Kind: "racing", DetailsJSON: "{}",
			RecordedAt: formatTime(testNow),
		})
	}()
	if _, err := e.Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if err := <-appended; err != nil {
		t.Fatalf("AppendSafetyAlert: %v", err)
	}
	seedWM := h.watermark(store.AlertSourceSafetyAlert)
	e.runPass(context.Background())
	delivered := len(r.accepted())
	if seedWM >= 1 && delivered != 0 {
		t.Fatalf("row below the seed point delivered anyway: seed wm=%d delivered=%d", seedWM, delivered)
	}
	if seedWM == 0 && delivered != 1 {
		t.Fatalf("row above the seed point lost: seed wm=%d delivered=%d", seedWM, delivered)
	}
	// After the pass, either way, the watermark covers the row.
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 1 {
		t.Fatalf("final watermark = %d, want 1 (the row is accounted for)", got)
	}
}

// TestWatermarkClampAtStart pins AN-8a(a): last_rowid > MAX(rowid) is
// clamped down and logged loudly, and a subsequently appended row IS
// delivered.
func TestWatermarkClampAtStart(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	h.platformKill(20)
	h.platformKill(21)
	if err := h.st.UpsertAlertDispatchWatermark(store.AlertSourceKillBreaker, 99, formatTime(testNow)); err != nil {
		t.Fatalf("UpsertAlertDispatchWatermark: %v", err)
	}
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	if !strings.Contains(h.logs.String(), "ALERT DISPATCH watermark clamped source=kill_breaker_events") {
		t.Fatalf("no clamp log line; logs:\n%s", h.logs.String())
	}
	if got := h.watermark(store.AlertSourceKillBreaker); got != 2 {
		t.Fatalf("clamped watermark = %d, want 2 (MAX rowid)", got)
	}
	h.platformKill(22) // rowid 3: would be below the stale watermark 99
	e.runPass(context.Background())
	got := r.accepted()
	if len(got) != 1 || got[0].ID != uid(22) || got[0].Seq != 3 {
		t.Fatalf("post-clamp deliveries = %+v, want the new kill only", got)
	}
}

// TestReEnableBacklog pins AN-8a(b): an existing watermark with a gap
// logs the backlog size AND fully dispatches it — silent skipping is
// forbidden.
func TestReEnableBacklog(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	for i := 0; i < 3; i++ {
		h.alert(40+i, "backlogged", "{}")
	}
	if err := h.st.UpsertAlertDispatchWatermark(store.AlertSourceSafetyAlert, 1, formatTime(testNow)); err != nil {
		t.Fatalf("UpsertAlertDispatchWatermark: %v", err)
	}
	e, backlog := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	if backlog[store.AlertSourceSafetyAlert] != 2 {
		t.Fatalf("backlog = %d, want 2", backlog[store.AlertSourceSafetyAlert])
	}
	if !strings.Contains(h.logs.String(), "alert dispatch backlog source=safety_alerts events=2") {
		t.Fatalf("no backlog log line; logs:\n%s", h.logs.String())
	}
	e.runPass(context.Background())
	got := r.accepted()
	if len(got) != 2 || got[0].ID != uid(41) || got[1].ID != uid(42) {
		t.Fatalf("backlog deliveries = %+v, want rows 2 and 3 in order", got)
	}
}

// TestStopOnFailureResume pins AN-4/AN-16: a 5xx mid-stream holds the
// watermark with no skip; recovery resumes at the failed row — duplicates
// possible, gaps never.
func TestStopOnFailureResume(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	for i := 0; i < 3; i++ {
		h.alert(40+i, "streamed", "{}")
	}
	r.setStatus(func(c capture) int {
		if c.ID == uid(41) {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	e.runPass(context.Background())
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 1 {
		t.Fatalf("watermark after mid-stream 5xx = %d, want 1 (held at last success)", got)
	}
	r.setStatus(nil) // receiver recovers
	e.runPass(context.Background())
	var ids []string
	for _, c := range r.accepted() {
		ids = append(ids, c.ID)
	}
	want := []string{uid(40), uid(41), uid(42)}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("accepted sequence = %v, want exact resume %v", ids, want)
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 3 {
		t.Errorf("final watermark = %d, want 3", got)
	}
}

// TestPerSourceIndependence pins the independent-streams rule: a failing
// source never stalls another source's dispatch.
func TestPerSourceIndependence(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	h.platformKill(20)
	h.alert(40, "independent", "{}")
	r.setStatus(func(c capture) int {
		if c.Source == store.AlertSourceKillBreaker {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	e.runPass(context.Background())
	if got := h.watermark(store.AlertSourceKillBreaker); got != 0 {
		t.Errorf("failing source watermark = %d, want 0", got)
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 1 {
		t.Errorf("healthy source watermark = %d, want 1 (not stalled)", got)
	}
	acc := r.accepted()
	if len(acc) != 1 || acc[0].ID != uid(40) {
		t.Fatalf("accepted = %+v, want the safety alert only", acc)
	}
}

// TestMaxPerTickCap pins AN-6: a backlog of N > cap drains across
// ⌈N/cap⌉ ticks, one event per POST.
func TestMaxPerTickCap(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0, MaxPerTick: 2})
	for i := 0; i < 5; i++ {
		h.alert(40+i, "bulk", "{}")
	}
	for pass, want := range []int{2, 4, 5} {
		e.runPass(context.Background())
		if got := len(r.accepted()); got != want {
			t.Fatalf("after pass %d: deliveries = %d, want %d", pass+1, got, want)
		}
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 5 {
		t.Errorf("final watermark = %d, want 5", got)
	}
}
