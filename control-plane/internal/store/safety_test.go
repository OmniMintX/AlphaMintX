package store

import (
	"errors"
	"strings"
	"testing"
)

// createLiveStrategy inserts a strategy already in a live_* lifecycle state.
func createLiveStrategy(t *testing.T, s *Store, strategyID, tenantID, state string) {
	t.Helper()
	err := s.CreateStrategy(Strategy{
		StrategyID: strategyID, TenantID: tenantID, Name: "strategy-" + strategyID,
		LifecycleState: state, CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	})
	if err != nil {
		t.Fatalf("CreateStrategy(%s): %v", strategyID, err)
	}
}

// TestKillEpochMonotonicAcrossTiers pins the ONE global epoch counter:
// interleaved strategy/tenant/platform kills strictly increase, whatever
// tier fired last (safety-wiring.md §Kill endpoints, invariant 2).
func TestKillEpochMonotonicAcrossTiers(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")

	epoch, err := s.AppendStrategyKill(uid(60), uid(1), "trader-1", formatTime(testNow), false)
	if err != nil || epoch != 1 {
		t.Fatalf("AppendStrategyKill: epoch=%d err=%v, want 1, nil", epoch, err)
	}
	if epoch, err = s.AppendTenantKill(uid(61), "tenant-a", "admin-1", formatTime(testNow), true); err != nil || epoch != 2 {
		t.Fatalf("AppendTenantKill: epoch=%d err=%v, want 2, nil", epoch, err)
	}
	if epoch, err = s.AppendPlatformKill(uid(62), "env-admin", formatTime(testNow), true); err != nil || epoch != 3 {
		t.Fatalf("AppendPlatformKill: epoch=%d err=%v, want 3, nil", epoch, err)
	}
	if epoch, err = s.AppendStrategyKill(uid(63), uid(1), "trader-1", formatTime(testNow), true); err != nil || epoch != 4 {
		t.Fatalf("second AppendStrategyKill: epoch=%d err=%v, want 4, nil", epoch, err)
	}
	// The predicate sees the highest epoch regardless of the firing tier.
	if got, err := s.GlobalMaxKillEpoch(uid(1)); err != nil || got != 4 {
		t.Fatalf("GlobalMaxKillEpoch: %d err=%v, want 4, nil", got, err)
	}
	// Unknown strategy: ErrNotFound, no row written.
	if _, err := s.AppendStrategyKill(uid(64), uid(99), "trader-1", formatTime(testNow), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AppendStrategyKill(unknown) err = %v, want ErrNotFound", err)
	}
	// The strategy row records the resolved tenant_id and flatten for audit.
	events, err := s.ListUnservedSafetyEvents()
	if err != nil || len(events) != 4 {
		t.Fatalf("ListUnservedSafetyEvents: %d events err=%v, want 4, nil", len(events), err)
	}
	last := events[len(events)-1]
	if last.EventID != uid(63) || last.Scope != "strategy" ||
		last.TenantID == nil || *last.TenantID != "tenant-a" ||
		last.Flatten == nil || !*last.Flatten {
		t.Fatalf("strategy kill row = %+v, want tenant-a audit tenant and flatten=true", last)
	}
}

// TestListUnservedSafetyEventsOrderAndMarking pins the driver's scan: kill
// AND breaker rows at ANY age until marked, tier precedence
// platform → tenant → strategy then rowid, marked rows excluded ONLY.
func TestListUnservedSafetyEventsOrderAndMarking(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")

	if _, err := s.AppendStrategyKill(uid(70), uid(1), "trader-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	// A breaker row (no epoch) is scanned exactly like a kill row.
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(71), Kind: "breaker", Scope: "strategy", StrategyID: strptr(uid(1)),
		ActorID: "system", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	if _, err := s.AppendTenantKill(uid(72), "tenant-a", "admin-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	if _, err := s.AppendPlatformKill(uid(73), "env-admin", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}

	got, err := s.ListUnservedSafetyEvents()
	if err != nil {
		t.Fatalf("ListUnservedSafetyEvents: %v", err)
	}
	want := []string{uid(73), uid(72), uid(70), uid(71)} // platform, tenant, then rowid
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.EventID != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, e.EventID, want[i])
		}
	}

	// Marking serves the row; duplicates are silent no-ops that never
	// overwrite the original completed_at (PK idempotence).
	if err := s.AppendSafetyEffectDone(uid(73), "2026-07-04T12:00:00Z"); err != nil {
		t.Fatalf("AppendSafetyEffectDone: %v", err)
	}
	if err := s.AppendSafetyEffectDone(uid(73), "2026-07-04T13:00:00Z"); err != nil {
		t.Fatalf("duplicate AppendSafetyEffectDone: %v", err)
	}
	var completed string
	if err := s.db.QueryRow(`SELECT completed_at FROM safety_effects WHERE event_id = ?`,
		uid(73)).Scan(&completed); err != nil || completed != "2026-07-04T12:00:00Z" {
		t.Fatalf("completed_at = %q err=%v, want the FIRST marker kept", completed, err)
	}
	if got, err = s.ListUnservedSafetyEvents(); err != nil || len(got) != 3 || got[0].EventID != uid(72) {
		t.Fatalf("after marking: %d events err=%v first=%v, want 3 starting at tenant row", len(got), err, got)
	}
	// The enforced FK rejects a marker for a nonexistent event row.
	if err := s.AppendSafetyEffectDone(uid(99), formatTime(testNow)); err == nil {
		t.Fatal("AppendSafetyEffectDone(unknown event) succeeded, want FK error")
	}
}

// TestAppendKillLifecycleLock pins driver step 3b: live_* states lock to
// killed with the system transition row; killed/non-live states no-op with
// locked=false; unknown strategies are ErrNotFound.
func TestAppendKillLifecycleLock(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a") // paper
	createLiveStrategy(t, s, uid(2), "tenant-a", "live_l2")

	if _, err := s.AppendKillLifecycleLock(uid(99), uid(80), "safety-engine", formatTime(testNow)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lock unknown strategy err = %v, want ErrNotFound", err)
	}
	// Non-live states keep their state (the standing kill blocks anyway).
	if locked, err := s.AppendKillLifecycleLock(uid(1), uid(80), "safety-engine", formatTime(testNow)); err != nil || locked {
		t.Fatalf("lock paper strategy = %v, %v, want false, nil", locked, err)
	}
	if st, err := s.GetStrategy(uid(1)); err != nil || st.LifecycleState != "paper" {
		t.Fatalf("paper strategy state = %q err=%v, want unchanged", st.LifecycleState, err)
	}
	// A live_* state locks: transition row to killed + snapshot update.
	locked, err := s.AppendKillLifecycleLock(uid(2), uid(81), "safety-engine", formatTime(testNow))
	if err != nil || !locked {
		t.Fatalf("lock live strategy = %v, %v, want true, nil", locked, err)
	}
	if st, err := s.GetStrategy(uid(2)); err != nil || st.LifecycleState != "killed" {
		t.Fatalf("locked strategy state = %q err=%v, want killed", st.LifecycleState, err)
	}
	var from, actor, role, reason string
	if err := s.db.QueryRow(`SELECT from_state, actor_id, actor_role, reason
		FROM lifecycle_transitions WHERE strategy_id = ? AND to_state = 'killed'`, uid(2)).
		Scan(&from, &actor, &role, &reason); err != nil {
		t.Fatalf("transition row: %v", err)
	}
	if from != "live_l2" || actor != "safety-engine" || role != "system" || !strings.Contains(reason, uid(81)) {
		t.Fatalf("transition row = (%s, %s, %s, %q), want live_l2/safety-engine/system/reason referencing %s",
			from, actor, role, reason, uid(81))
	}
	// Already killed: idempotent no-op, no second kill transition row
	// (the LC-16a bootstrap row is the only other row).
	if locked, err := s.AppendKillLifecycleLock(uid(2), uid(82), "safety-engine", formatTime(testNow)); err != nil || locked {
		t.Fatalf("re-lock killed strategy = %v, %v, want false, nil", locked, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_transitions
		WHERE strategy_id = ? AND to_state = 'killed'`,
		uid(2)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("kill transition rows = %d err=%v, want exactly 1", n, err)
	}
}

// TestLatestStrategyKillEvent pins the WD-16 back-fill read
// (docs/specs/watchdog.md §Wiring seams): the NEWEST kind='kill',
// scope='strategy' row for the strategy — served rows included, breaker
// rows and other strategies'/tiers' kills excluded; ok=false when none.
func TestLatestStrategyKillEvent(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-a")

	if _, _, ok, err := s.LatestStrategyKillEvent(uid(1)); err != nil || ok {
		t.Fatalf("empty table: ok=%v err=%v, want false, nil", ok, err)
	}
	// A breaker row and a tenant-scope kill never match (kill + strategy
	// scope only).
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(74), Kind: "breaker", Scope: "strategy", StrategyID: strptr(uid(1)),
		ActorID: "breaker-monitor", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	if _, err := s.AppendTenantKill(uid(75), "tenant-a", "admin-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	if _, _, ok, err := s.LatestStrategyKillEvent(uid(1)); err != nil || ok {
		t.Fatalf("breaker+tenant rows only: ok=%v err=%v, want false, nil", ok, err)
	}

	if _, err := s.AppendStrategyKill(uid(76), uid(1), "trader-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if _, err := s.AppendStrategyKill(uid(77), uid(2), "watchdog", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill(other strategy): %v", err)
	}
	if ev, actor, ok, err := s.LatestStrategyKillEvent(uid(1)); err != nil || !ok ||
		ev != uid(76) || actor != "trader-1" {
		t.Fatalf("after first kill: (%s, %s, %v, %v), want (%s, trader-1, true, nil)", ev, actor, ok, err, uid(76))
	}
	// A newer row wins; a served marker never hides it (served or not).
	if _, err := s.AppendStrategyKill(uid(78), uid(1), "watchdog", formatTime(testNow), false); err != nil {
		t.Fatalf("second AppendStrategyKill: %v", err)
	}
	if err := s.AppendSafetyEffectDone(uid(78), formatTime(testNow)); err != nil {
		t.Fatalf("AppendSafetyEffectDone: %v", err)
	}
	if ev, actor, ok, err := s.LatestStrategyKillEvent(uid(1)); err != nil || !ok ||
		ev != uid(78) || actor != "watchdog" {
		t.Fatalf("newest served kill: (%s, %s, %v, %v), want (%s, watchdog, true, nil)", ev, actor, ok, err, uid(78))
	}
}

// TestSafetyAlertDedupeReads pins the alert journal and its two dedupe
// reads: keyed (kind, strategy_id, ref_id[, utcDate]) with the
// empty-matches-NULL rule on BOTH nullable keys.
func TestSafetyAlertDedupeReads(t *testing.T) {
	s := openStore(t)
	at := formatTime(testNow) // 2026-07-04T12:00:00Z
	alerts := []SafetyAlert{
		{AlertID: uid(90), Kind: "breaker_mark_stale", StrategyID: strptr(uid(1)),
			RefID: strptr("stale_mark"), DetailsJSON: `{"cause":"stale_mark"}`, RecordedAt: at},
		{AlertID: uid(91), Kind: "monitor_tick_panic", DetailsJSON: `{}`, RecordedAt: at},
		{AlertID: uid(92), Kind: "safety_residue_abandoned", StrategyID: strptr(uid(1)),
			RefID: strptr(uid(70) + "/BTC-USDT"), DetailsJSON: `{"cause":"dust","qty_base":"0.00000100"}`,
			RecordedAt: "2026-07-03T23:59:59Z"},
	}
	for _, a := range alerts {
		if err := s.AppendSafetyAlert(a); err != nil {
			t.Fatalf("AppendSafetyAlert(%s): %v", a.AlertID, err)
		}
	}

	cases := []struct {
		name                    string
		kind, strategy, ref, at string
		want                    bool
	}{
		{"full key matches", "breaker_mark_stale", uid(1), "stale_mark", "2026-07-04", true},
		{"other cause same day is NOT deduped", "breaker_mark_stale", uid(1), "pnl_error", "2026-07-04", false},
		{"other day", "breaker_mark_stale", uid(1), "stale_mark", "2026-07-05", false},
		{"other strategy", "breaker_mark_stale", uid(2), "stale_mark", "2026-07-04", false},
		{"empty matches NULL on both keys", "monitor_tick_panic", "", "", "2026-07-04", true},
		{"empty does NOT match a non-NULL column", "breaker_mark_stale", "", "stale_mark", "2026-07-04", false},
		{"non-empty does NOT match a NULL column", "monitor_tick_panic", uid(1), "", "2026-07-04", false},
	}
	for _, c := range cases {
		got, err := s.HasSafetyAlertToday(c.kind, c.strategy, c.ref, c.at)
		if err != nil || got != c.want {
			t.Errorf("%s: HasSafetyAlertToday = %v, %v, want %v, nil", c.name, got, err, c.want)
		}
	}

	// Any-age dedupe: yesterday's residue row still suppresses today.
	if got, err := s.HasSafetyAlert("safety_residue_abandoned", uid(1), uid(70)+"/BTC-USDT"); err != nil || !got {
		t.Fatalf("HasSafetyAlert(residue) = %v, %v, want true, nil", got, err)
	}
	if got, err := s.HasSafetyAlert("safety_residue_abandoned", uid(1), uid(70)+"/ETH-USDT"); err != nil || got {
		t.Fatalf("HasSafetyAlert(other symbol) = %v, %v, want false, nil", got, err)
	}

	// ListSafetyAlerts: filters narrow, zero filter returns all in rowid order.
	all, err := s.ListSafetyAlerts(SafetyAlertFilter{})
	if err != nil || len(all) != 3 || all[0].AlertID != uid(90) {
		t.Fatalf("ListSafetyAlerts(all) = %d rows err=%v, want 3 in insertion order", len(all), err)
	}
	byKind, err := s.ListSafetyAlerts(SafetyAlertFilter{Kind: "safety_residue_abandoned", StrategyID: uid(1)})
	if err != nil || len(byKind) != 1 || byKind[0].AlertID != uid(92) {
		t.Fatalf("ListSafetyAlerts(kind+strategy) = %+v err=%v, want the residue row", byKind, err)
	}
	limited, err := s.ListSafetyAlerts(SafetyAlertFilter{Limit: 2})
	if err != nil || len(limited) != 2 {
		t.Fatalf("ListSafetyAlerts(limit 2) = %d rows err=%v, want 2", len(limited), err)
	}
}
