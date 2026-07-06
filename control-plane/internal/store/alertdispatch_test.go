package store

import (
	"strings"
	"testing"
)

// TestAlertDispatchPoolOfOne is the spec tripwire (alert-notifier.md
// Motivation): rowid monotonicity-as-commit-order REQUIRES the pool of
// exactly one connection; raising it above 1 invalidates the notifier's
// watermark design.
func TestAlertDispatchPoolOfOne(t *testing.T) {
	s := openStore(t)
	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want exactly 1 (alert-notifier.md: rowid watermarks are only safe under a single-connection pool)", got)
	}
}

// TestAlertDispatchWatermarks pins AN-7/AN-8/AN-9: the table exists
// unconditionally but empty (no config-gated seeding at Open), the
// one-statement seed lands at MAX(rowid) iff absent and never moves an
// existing row, the upsert updates in place, and unknown sources are
// refused by the whitelist.
func TestAlertDispatchWatermarks(t *testing.T) {
	s := openStore(t)
	if _, ok, err := s.AlertDispatchWatermark(AlertSourceKillBreaker); err != nil || ok {
		t.Fatalf("fresh watermark ok=%v err=%v, want absent (AN-7: empty table is the intended state)", ok, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.AppendPlatformKill(uid(10+i), "op-1", formatTime(testNow), false); err != nil {
			t.Fatalf("AppendPlatformKill: %v", err)
		}
	}
	if err := s.SeedAlertDispatchWatermark(AlertSourceKillBreaker, formatTime(testNow)); err != nil {
		t.Fatalf("SeedAlertDispatchWatermark: %v", err)
	}
	wm, ok, err := s.AlertDispatchWatermark(AlertSourceKillBreaker)
	if err != nil || !ok || wm != 2 {
		t.Fatalf("seeded watermark = %d ok=%v err=%v, want 2 (MAX rowid)", wm, ok, err)
	}
	// A second seed after another append never moves the existing row.
	if _, err := s.AppendPlatformKill(uid(12), "op-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}
	if err := s.SeedAlertDispatchWatermark(AlertSourceKillBreaker, formatTime(testNow)); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if wm, _, _ := s.AlertDispatchWatermark(AlertSourceKillBreaker); wm != 2 {
		t.Fatalf("watermark after re-seed = %d, want unchanged 2 (AN-8)", wm)
	}
	// An empty source seeds at 0.
	if err := s.SeedAlertDispatchWatermark(AlertSourceSafetyAlert, formatTime(testNow)); err != nil {
		t.Fatalf("seed empty source: %v", err)
	}
	if wm, ok, _ := s.AlertDispatchWatermark(AlertSourceSafetyAlert); !ok || wm != 0 {
		t.Fatalf("empty-source watermark = %d ok=%v, want 0", wm, ok)
	}
	// AN-9 in-place upsert.
	if err := s.UpsertAlertDispatchWatermark(AlertSourceKillBreaker, 7, formatTime(testNow)); err != nil {
		t.Fatalf("UpsertAlertDispatchWatermark: %v", err)
	}
	if wm, _, _ := s.AlertDispatchWatermark(AlertSourceKillBreaker); wm != 7 {
		t.Fatalf("upserted watermark = %d, want 7", wm)
	}
	if max, err := s.MaxAlertSourceRowid(AlertSourceKillBreaker); err != nil || max != 3 {
		t.Fatalf("MaxAlertSourceRowid = %d err=%v, want 3", max, err)
	}
	// The whitelist refuses non-source tables.
	if err := s.SeedAlertDispatchWatermark("oms_recon_events", formatTime(testNow)); err == nil {
		t.Error("seed of a non-source table accepted, want refusal")
	}
	if _, err := s.MaxAlertSourceRowid("strategies"); err == nil {
		t.Error("MaxAlertSourceRowid of a non-source table accepted, want refusal")
	}
}

// TestListAlertSourcesAfter pins the AN-2 reads: rowid > after, rowid ASC,
// LIMIT respected, and full row mapping including NULL columns and the
// tenant_id migration column.
func TestListAlertSourcesAfter(t *testing.T) {
	s := openStore(t)
	sid, tid, trigger := uid(1), "tenant-1", "breach-ref"
	epoch := int64(9)
	flatten := true
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(20), Kind: "kill", Scope: "strategy", StrategyID: &sid, TenantID: &tid,
		KillEpoch: &epoch, Flatten: &flatten, TriggerRef: &trigger,
		ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	killEpoch, err := s.AppendPlatformKill(uid(21), "op-1", formatTime(testNow), false)
	if err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}
	events, err := s.ListKillBreakerEventsAfter(0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("ListKillBreakerEventsAfter = %d rows err=%v, want 2", len(events), err)
	}
	if events[0].Rowid != 1 || events[1].Rowid != 2 {
		t.Errorf("rowids = %d, %d, want 1, 2 (rowid ASC)", events[0].Rowid, events[1].Rowid)
	}
	e0 := events[0]
	if e0.EventID != uid(20) || e0.StrategyID == nil || *e0.StrategyID != sid ||
		e0.TenantID == nil || *e0.TenantID != tid || e0.KillEpoch == nil || *e0.KillEpoch != 9 ||
		e0.Flatten == nil || !*e0.Flatten || e0.TriggerRef == nil || *e0.TriggerRef != trigger {
		t.Errorf("row 1 = %+v, want full column mapping", e0)
	}
	if e1 := events[1]; e1.StrategyID != nil || e1.TenantID != nil || e1.Flatten == nil || *e1.Flatten {
		t.Errorf("row 2 = %+v, want NULL scope ids and flatten=false", e1)
	}
	if got, err := s.ListKillBreakerEventsAfter(1, 10); err != nil || len(got) != 1 || got[0].Rowid != 2 {
		t.Errorf("after=1 rows = %+v err=%v, want only rowid 2", got, err)
	}
	if got, err := s.ListKillBreakerEventsAfter(0, 1); err != nil || len(got) != 1 {
		t.Errorf("limit=1 rows = %d err=%v, want 1", len(got), err)
	}

	if _, _, err := s.AppendKillClearPlatform(uid(30), "op-1", "resolved", killEpoch, formatTime(testNow)); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	clears, err := s.ListKillClearEventsAfter(0, 10)
	if err != nil || len(clears) != 1 {
		t.Fatalf("ListKillClearEventsAfter = %d rows err=%v, want 1", len(clears), err)
	}
	c := clears[0]
	if c.Rowid != 1 || c.ClearID != uid(30) || c.Scope != "platform" || c.StrategyID != nil ||
		c.TenantID != nil || c.ClearedEpoch != killEpoch || c.ActorID != "op-1" || c.Reason != "resolved" {
		t.Errorf("clear row = %+v, want full column mapping", c)
	}

	ref := uid(20)
	if err := s.AppendSafetyAlert(SafetyAlert{
		AlertID: uid(40), Kind: "venue_reset", StrategyID: &sid, RefID: &ref,
		DetailsJSON: `{"reason":"x"}`, RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendSafetyAlert: %v", err)
	}
	// The platform clear appended kill_effects_superseded alerts too; read
	// them all and check ordering plus the explicit row.
	alerts, err := s.ListSafetyAlertsAfter(0, 10)
	if err != nil || len(alerts) == 0 {
		t.Fatalf("ListSafetyAlertsAfter = %d rows err=%v, want >= 1", len(alerts), err)
	}
	for i := 1; i < len(alerts); i++ {
		if alerts[i].Rowid <= alerts[i-1].Rowid {
			t.Errorf("alert rowids not ascending: %d then %d", alerts[i-1].Rowid, alerts[i].Rowid)
		}
	}
	last := alerts[len(alerts)-1]
	if last.AlertID != uid(40) || last.Kind != "venue_reset" || last.RefID == nil || *last.RefID != ref {
		t.Errorf("last alert = %+v, want the explicit venue_reset row", last)
	}
}

// TestAppendOMSReconEventWithAlert pins the AN-1a combined mutator: the
// recon event and its companion alert commit in ONE transaction — a
// failing alert insert rolls the recon event back too (no crash window
// that keeps the event but loses the alert).
func TestAppendOMSReconEventWithAlert(t *testing.T) {
	s := openStore(t)
	eventID, ref := uid(50), uid(50)
	ev := OMSReconEvent{EventID: eventID, Kind: "venue_reset",
		DetailsJSON: `{"reason":"previously_acked_not_found"}`, RecordedAt: formatTime(testNow)}
	alert := SafetyAlert{AlertID: uid(51), Kind: "venue_reset", RefID: &ref,
		DetailsJSON: ev.DetailsJSON, RecordedAt: ev.RecordedAt}
	if err := s.AppendOMSReconEventWithAlert(ev, alert); err != nil {
		t.Fatalf("AppendOMSReconEventWithAlert: %v", err)
	}
	evs, err := s.ListOMSReconEvents(OMSReconEventFilter{Kind: "venue_reset"})
	if err != nil || len(evs) != 1 || evs[0].EventID != eventID {
		t.Fatalf("recon events = %+v err=%v, want the one appended row", evs, err)
	}
	alerts, err := s.ListSafetyAlerts(SafetyAlertFilter{Kind: "venue_reset", RefID: ref})
	if err != nil || len(alerts) != 1 || alerts[0].AlertID != uid(51) {
		t.Fatalf("companion alerts = %+v err=%v, want exactly one with ref_id = event_id", alerts, err)
	}

	// Atomicity: a duplicate alert_id fails the alert insert; the fresh
	// recon event must NOT survive on its own.
	ev2 := OMSReconEvent{EventID: uid(52), Kind: "sl_deadline_contingency",
		DetailsJSON: "{}", RecordedAt: formatTime(testNow)}
	dup := SafetyAlert{AlertID: uid(51), Kind: "sl_deadline_contingency",
		DetailsJSON: "{}", RecordedAt: formatTime(testNow)}
	err = s.AppendOMSReconEventWithAlert(ev2, dup)
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("duplicate alert_id err = %v, want a UNIQUE violation", err)
	}
	evs, err = s.ListOMSReconEvents(OMSReconEventFilter{Kind: "sl_deadline_contingency"})
	if err != nil || len(evs) != 0 {
		t.Fatalf("recon events after rollback = %+v err=%v, want none (atomic)", evs, err)
	}
}
