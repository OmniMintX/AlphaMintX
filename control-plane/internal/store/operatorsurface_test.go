package store

import (
	"errors"
	"testing"
)

const osToday = "2026-07-04"

func safetyStatus(t *testing.T, s *Store, strategyID string) SafetyStatusRow {
	t.Helper()
	row, err := s.SafetyStatus(strategyID, osToday)
	if err != nil {
		t.Fatalf("SafetyStatus(%s): %v", strategyID, err)
	}
	return row
}

// TestSafetyStatusScopeMatrix pins OS-8/OS-9/OS-10 from ONE call: the
// binding-kills join covers strategy, tenant, platform, and Phase-1 global
// rows in kill_epoch DESC order; a clear annotates ONLY its own scope's
// rows; active_kill is the LC-28 predicate in the same snapshot; a foreign
// tenant's strategy sees only the platform tiers.
func TestSafetyStatusScopeMatrix(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-b")
	at := formatTime(testNow)
	if _, err := s.AppendStrategyKill(uid(60), uid(1), "trader-1", at, true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if _, err := s.AppendTenantKill(uid(61), "tenant-a", "admin-1", at, false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	if _, err := s.AppendPlatformKill(uid(62), "env-admin", at, false); err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}
	four := int64(4)
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(63), Kind: "kill", Scope: "global", KillEpoch: &four,
		ActorID: "admin-1", RecordedAt: at,
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}

	row := safetyStatus(t, s, uid(1))
	if !row.ActiveKill || len(row.Kills) != 4 {
		t.Fatalf("uid(1) = active %v, %d kills, want true, 4", row.ActiveKill, len(row.Kills))
	}
	for i, want := range []string{uid(63), uid(62), uid(61), uid(60)} { // epoch DESC
		if row.Kills[i].EventID != want || row.Kills[i].Cleared != nil {
			t.Fatalf("kills[%d] = %s cleared=%v, want %s uncleared", i, row.Kills[i].EventID, row.Kills[i].Cleared, want)
		}
	}
	row2 := safetyStatus(t, s, uid(2))
	if len(row2.Kills) != 2 || row2.Kills[0].EventID != uid(63) || row2.Kills[1].EventID != uid(62) {
		t.Fatalf("uid(2) kills = %+v, want only the two both-NULL rows", row2.Kills)
	}

	// The strategy clear annotates ONLY the strategy row; the rest stand.
	if _, _, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "resolved", 1, at); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	row = safetyStatus(t, s, uid(1))
	if !row.ActiveKill {
		t.Fatal("tenant+platform kills still bind: active_kill must stay true")
	}
	for _, k := range row.Kills {
		if k.EventID == uid(60) {
			c := k.Cleared
			if c == nil || c.ClearID != uid(70) || c.ActorID != "admin-1" ||
				c.Reason != "resolved" || c.ClearedEpoch != 1 || c.RecordedAt != at {
				t.Fatalf("strategy kill cleared = %+v, want the uid(70) clear verbatim", c)
			}
		} else if k.Cleared != nil {
			t.Fatalf("kill %s cleared by a strategy-scope clear (scope key violated)", k.EventID)
		}
	}

	// Tenant + platform clears: the platform clear (epoch 4) covers BOTH
	// both-NULL rows (LC-27 watermark); everything is annotated and the
	// predicate flips false in the same snapshot.
	if _, _, err := s.AppendKillClearTenant(uid(71), "tenant-a", "admin-1", "resolved", 2, at); err != nil {
		t.Fatalf("AppendKillClearTenant: %v", err)
	}
	if _, _, err := s.AppendKillClearPlatform(uid(72), "env-admin", "resolved", 4, at); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	row = safetyStatus(t, s, uid(1))
	if row.ActiveKill {
		t.Fatal("all scopes cleared: active_kill must be false")
	}
	for _, k := range row.Kills {
		if k.Cleared == nil {
			t.Fatalf("kill %s uncleared after all-scope clears", k.EventID)
		}
		if (k.EventID == uid(62) || k.EventID == uid(63)) && k.Cleared.ClearedEpoch != 4 {
			t.Fatalf("platform kill %s cleared_epoch = %d, want 4 (newest covering clear)", k.EventID, k.Cleared.ClearedEpoch)
		}
	}
}

// TestSafetyStatusStrategyClearNeverCoversTenantKill pins the OS-9
// counterexample: a strategy-scope clear carries a non-NULL tenant_id by
// DDL, but the clear's SCOPE COLUMN is part of the covering key — it MUST
// NOT annotate the tenant kill, and active_kill stays true.
func TestSafetyStatusStrategyClearNeverCoversTenantKill(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	at := formatTime(testNow)
	if _, err := s.AppendTenantKill(uid(60), "tenant-a", "admin-1", at, false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	if _, err := s.AppendStrategyKill(uid(61), uid(1), "trader-1", at, false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	// The strategy clear (cleared_epoch 2 >= the tenant kill's epoch 1,
	// SAME tenant_id) covers ONLY the strategy row.
	if _, _, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "resolved", 2, at); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	row := safetyStatus(t, s, uid(1))
	if !row.ActiveKill || len(row.Kills) != 2 {
		t.Fatalf("status = active %v, %d kills, want true, 2", row.ActiveKill, len(row.Kills))
	}
	if row.Kills[0].EventID != uid(61) || row.Kills[0].Cleared == nil {
		t.Fatalf("strategy kill = %+v, want cleared", row.Kills[0])
	}
	if row.Kills[1].EventID != uid(60) || row.Kills[1].Cleared != nil {
		t.Fatalf("tenant kill = %+v, want cleared: nil (scope-column key)", row.Kills[1])
	}
}

// TestSafetyStatusNullEpochHidden pins OS-8b: a legacy NULL-epoch
// kind='kill' row is absent from the join and never flips the predicate.
func TestSafetyStatusNullEpochHidden(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(60), Kind: "kill", Scope: "strategy", StrategyID: strptr(uid(1)),
		ActorID: "legacy", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	row := safetyStatus(t, s, uid(1))
	if row.ActiveKill || len(row.Kills) != 0 {
		t.Fatalf("NULL-epoch row rendered: active %v, %d kills, want false, 0", row.ActiveKill, len(row.Kills))
	}
	// An unknown strategy is ErrNotFound (defense behind the 404).
	if _, err := s.SafetyStatus(uid(99), osToday); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown strategy err = %v, want ErrNotFound", err)
	}
}

// TestSafetyStatusBreaker pins OS-11 day edges and the 3-clause event
// match: yesterday's row never latches; strategy-, tenant-, and
// platform-scope rows each latch WITH a matching event; the newest row
// (recorded_at DESC, rowid DESC) is the event; foreign-tenant rows are
// invisible.
func TestSafetyStatusBreaker(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-b")
	breaker := func(eventID string, sid, tid *string, recordedAt string) {
		t.Helper()
		if err := s.AppendKillBreakerEvent(KillBreakerEvent{
			EventID: eventID, Kind: "breaker", Scope: "strategy",
			StrategyID: sid, TenantID: tid, ActorID: "breaker-monitor", RecordedAt: recordedAt,
		}); err != nil {
			t.Fatalf("AppendKillBreakerEvent(%s): %v", eventID, err)
		}
	}
	// Yesterday's row: latch false, event nil (the 00:00 UTC auto-reset).
	breaker(uid(60), strptr(uid(1)), strptr("tenant-a"), "2026-07-03T23:59:59Z")
	row := safetyStatus(t, s, uid(1))
	if row.BreakerActiveToday || row.BreakerEvent != nil {
		t.Fatalf("yesterday's row = latch %v event %+v, want false, nil", row.BreakerActiveToday, row.BreakerEvent)
	}
	// Strategy-scope today: latch true with the matching event.
	breaker(uid(61), strptr(uid(1)), strptr("tenant-a"), "2026-07-04T09:00:00Z")
	row = safetyStatus(t, s, uid(1))
	if !row.BreakerActiveToday || row.BreakerEvent == nil || row.BreakerEvent.EventID != uid(61) {
		t.Fatalf("strategy row = latch %v event %+v, want true, uid(61)", row.BreakerActiveToday, row.BreakerEvent)
	}
	// Tenant- then platform-scope rows: newest recorded_at wins; the
	// foreign tenant sees only the platform row.
	breaker(uid(62), nil, strptr("tenant-a"), "2026-07-04T10:00:00Z")
	breaker(uid(63), nil, nil, "2026-07-04T11:00:00Z")
	row = safetyStatus(t, s, uid(1))
	if row.BreakerEvent == nil || row.BreakerEvent.EventID != uid(63) {
		t.Fatalf("newest event = %+v, want the platform row uid(63)", row.BreakerEvent)
	}
	row2 := safetyStatus(t, s, uid(2))
	if !row2.BreakerActiveToday || row2.BreakerEvent == nil || row2.BreakerEvent.EventID != uid(63) {
		t.Fatalf("uid(2) = latch %v event %+v, want true, uid(63) only", row2.BreakerActiveToday, row2.BreakerEvent)
	}
	// Same-timestamp tiebreak: a later insert at 11:00 wins on rowid.
	breaker(uid(64), nil, nil, "2026-07-04T11:00:00Z")
	if row = safetyStatus(t, s, uid(1)); row.BreakerEvent == nil || row.BreakerEvent.EventID != uid(64) {
		t.Fatalf("rowid tiebreak = %+v, want uid(64)", row.BreakerEvent)
	}
}

// TestSafetyStatusPausedFrom pins the OS-7 paused_from derivation: LC-7
// provenance for a paused strategy, nil otherwise, nil when unknown.
func TestSafetyStatusPausedFrom(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	if row := safetyStatus(t, s, uid(1)); row.LifecycleState != "paper" || row.PausedFrom != nil {
		t.Fatalf("paper strategy = %s paused_from %v, want paper, nil", row.LifecycleState, row.PausedFrom)
	}
	if err := s.AppendLifecycleTransition(LifecycleTransition{
		TransitionID: uid(80), StrategyID: uid(1), FromState: "paper", ToState: "paused",
		ActorID: "trader-1", ActorRole: "trader", Reason: "pause", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}
	row := safetyStatus(t, s, uid(1))
	if row.LifecycleState != "paused" || row.PausedFrom == nil || *row.PausedFrom != "paper" {
		t.Fatalf("paused strategy = %s paused_from %v, want paused, paper", row.LifecycleState, row.PausedFrom)
	}
	// Paused with NO to_state='paused' audit row: provenance unknown.
	if err := s.CreateStrategy(Strategy{
		StrategyID: uid(2), TenantID: "tenant-a", Name: "orphan-paused",
		LifecycleState: "paused", CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("CreateStrategy: %v", err)
	}
	if row := safetyStatus(t, s, uid(2)); row.PausedFrom != nil {
		t.Fatalf("unknown provenance paused_from = %q, want nil", *row.PausedFrom)
	}
}

// TestListSafetyAlertsPages pins OS-16/OS-17/OS-21 at the store level:
// pinned ordering (recorded_at DESC, alert_id DESC — same-second ties
// break on alert_id), correct totals under LIMIT/OFFSET, exact strategy
// scoping (NULL rows excluded), the global feed including NULL rows, and
// the exact-match kind filter (unknown kind = empty page, not an error).
func TestListSafetyAlertsPages(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	alert := func(alertID, kind string, strategyID *string, recordedAt string) {
		t.Helper()
		if err := s.AppendSafetyAlert(SafetyAlert{
			AlertID: alertID, Kind: kind, StrategyID: strategyID,
			DetailsJSON: "{}", RecordedAt: recordedAt,
		}); err != nil {
			t.Fatalf("AppendSafetyAlert(%s): %v", alertID, err)
		}
	}
	// Insertion order deliberately scrambled against the pinned ordering.
	alert(uid(31), "watchdog_agent_silent", strptr(uid(1)), "2026-07-04T10:00:00Z")
	alert(uid(33), "watchdog_agent_silent", strptr(uid(1)), "2026-07-04T10:00:00Z") // same-second tie
	alert(uid(32), "kill_effects_superseded", strptr(uid(1)), "2026-07-04T09:00:00Z")
	alert(uid(34), "safety_effect_stalled", nil, "2026-07-04T11:00:00Z") // NULL-strategy row
	items, total, err := s.ListSafetyAlertsByStrategyPage(uid(1), 1, 2)
	if err != nil || total != 3 || len(items) != 2 {
		t.Fatalf("page 1 = %d items total %d err %v, want 2, 3, nil", len(items), total, err)
	}
	if items[0].AlertID != uid(33) || items[1].AlertID != uid(31) {
		t.Fatalf("page 1 order = [%s %s], want [uid(33) uid(31)] (alert_id DESC tiebreak)", items[0].AlertID, items[1].AlertID)
	}
	if items2, _, err := s.ListSafetyAlertsByStrategyPage(uid(1), 2, 2); err != nil ||
		len(items2) != 1 || items2[0].AlertID != uid(32) {
		t.Fatalf("page 2 = %+v err %v, want only uid(32)", items2, err)
	}
	global, total, err := s.ListSafetyAlertsGlobalPage("", 1, 10)
	if err != nil || total != 4 || len(global) != 4 || global[0].AlertID != uid(34) {
		t.Fatalf("global = %+v total %d err %v, want 4 rows newest-first with uid(34) first",
			global, total, err)
	}
	if kinds, total, err := s.ListSafetyAlertsGlobalPage("safety_effect_stalled", 1, 10); err != nil ||
		total != 1 || len(kinds) != 1 || kinds[0].StrategyID != nil {
		t.Fatalf("kind filter = %+v total %d err %v, want the one NULL-strategy row", kinds, total, err)
	}
	if none, total, err := s.ListSafetyAlertsGlobalPage("no_such_kind", 1, 10); err != nil || total != 0 || len(none) != 0 {
		t.Fatalf("unknown kind = %d items total %d err %v, want empty page, nil", len(none), total, err)
	}
}
