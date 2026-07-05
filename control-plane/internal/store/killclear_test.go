package store

import (
	"errors"
	"strings"
	"testing"
)

func assertActive(t *testing.T, s *Store, name, strategyID string, want bool) {
	t.Helper()
	got, err := s.ActiveKill(strategyID)
	if err != nil || got != want {
		t.Fatalf("%s: ActiveKill(%s) = %v, %v, want %v, nil", name, strategyID, got, err, want)
	}
}

// clearRows counts the persisted kill_clear_events rows.
func clearRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM kill_clear_events`).Scan(&n); err != nil {
		t.Fatalf("count kill_clear_events: %v", err)
	}
	return n
}

// TestActiveKillScopeMatrix pins LC-28/LC-32: each clear flips exactly its
// own scope, a strategy under two tiers needs BOTH cleared, Phase-1 global
// rows classify as platform, and a re-kill after a clear is active again
// (higher epoch, LC-27's monotone counter).
func TestActiveKillScopeMatrix(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-b")
	at := formatTime(testNow)
	assertActive(t, s, "no kills", uid(1), false)

	// Strategy kill (epoch 1) + tenant kill (epoch 2): BOTH bind uid(1).
	if _, err := s.AppendStrategyKill(uid(60), uid(1), "trader-1", at, false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if _, err := s.AppendTenantKill(uid(61), "tenant-a", "admin-1", at, false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	assertActive(t, s, "two tiers bind", uid(1), true)
	assertActive(t, s, "foreign tenant untouched", uid(2), false)
	// The strategy clear flips ONLY its scope: the tenant kill stands.
	if epoch, sup, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "resolved", 1, at); err != nil ||
		epoch != 1 || len(sup) != 1 || sup[0] != uid(60) {
		t.Fatalf("strategy clear = (%d, %v, %v), want (1, [%s], nil)", epoch, sup, err, uid(60))
	}
	assertActive(t, s, "tenant kill still binds", uid(1), true)
	if epoch, _, err := s.AppendKillClearTenant(uid(71), "tenant-a", "admin-1", "resolved", 2, at); err != nil || epoch != 2 {
		t.Fatalf("tenant clear = (%d, %v), want (2, nil)", epoch, err)
	}
	assertActive(t, s, "both tiers cleared", uid(1), false)

	// Platform kill (epoch 3) binds everyone; only a platform clear lifts it.
	if _, err := s.AppendPlatformKill(uid(62), "env-admin", at, false); err != nil {
		t.Fatalf("AppendPlatformKill: %v", err)
	}
	assertActive(t, s, "platform binds uid(1)", uid(1), true)
	assertActive(t, s, "platform binds uid(2)", uid(2), true)
	if _, _, err := s.AppendKillClearPlatform(uid(72), "env-admin", "resolved", 3, at); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	assertActive(t, s, "platform cleared uid(1)", uid(1), false)
	assertActive(t, s, "platform cleared uid(2)", uid(2), false)

	// A Phase-1 global row (both ids NULL) classifies as platform (LC-26).
	four := int64(4)
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(63), Kind: "kill", Scope: "global", KillEpoch: &four,
		ActorID: "admin-1", RecordedAt: at,
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	assertActive(t, s, "global row binds", uid(2), true)
	if _, _, err := s.AppendKillClearStrategy(uid(73), uid(2), "admin-1", "r", 0, at); !errors.Is(err, ErrNoActiveKill) {
		t.Fatalf("strategy clear against global row err = %v, want ErrNoActiveKill (scope isolation)", err)
	}
	if _, _, err := s.AppendKillClearPlatform(uid(74), "env-admin", "resolved", 4, at); err != nil {
		t.Fatalf("platform clear of the global row: %v", err)
	}
	assertActive(t, s, "global row cleared", uid(2), false)

	// Re-kill after clear: the fresh epoch (5) exceeds every watermark.
	if _, err := s.AppendStrategyKill(uid(64), uid(1), "trader-1", at, false); err != nil {
		t.Fatalf("re-kill: %v", err)
	}
	assertActive(t, s, "re-kill is active again", uid(1), true)
}

// TestKillClearValidation pins LC-27/LC-31: NO_ACTIVE_KILL and
// CLEAR_CONFLICT write NOTHING — no clear row, no markers, no alerts —
// the active check answers before the epoch CAS, and unknown strategies
// are ErrNotFound before body semantics.
func TestKillClearValidation(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	at := formatTime(testNow)

	if _, _, err := s.AppendKillClearStrategy(uid(70), uid(99), "admin-1", "r", 0, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clear unknown strategy err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "r", 0, at); !errors.Is(err, ErrNoActiveKill) {
		t.Fatalf("clear with no kill err = %v, want ErrNoActiveKill", err)
	}
	if _, err := s.AppendStrategyKill(uid(60), uid(1), "trader-1", at, false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	// A stale observed_epoch is the LC-27 conflict; nothing is written.
	if _, _, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "r", 0, at); !errors.Is(err, ErrClearConflict) {
		t.Fatalf("stale observed_epoch err = %v, want ErrClearConflict", err)
	}
	if n := clearRows(t, s); n != 0 {
		t.Fatalf("kill_clear_events rows = %d, want 0 (rejections write nothing)", n)
	}
	if events, err := s.ListUnservedSafetyEvents(); err != nil || len(events) != 1 {
		t.Fatalf("unserved events = %d err=%v, want 1 (no supersede markers on rejection)", len(events), err)
	}
	if alerts, err := s.ListSafetyAlerts(SafetyAlertFilter{Kind: "kill_effects_superseded"}); err != nil || len(alerts) != 0 {
		t.Fatalf("superseded alerts = %d err=%v, want 0", len(alerts), err)
	}
	// Both checks failing answers NO_ACTIVE_KILL, never CLEAR_CONFLICT:
	// tenant scope has no kill AND the observed epoch is wrong.
	if _, _, err := s.AppendKillClearTenant(uid(71), "tenant-a", "admin-1", "r", 9, at); !errors.Is(err, ErrNoActiveKill) {
		t.Fatalf("no-active + wrong epoch err = %v, want ErrNoActiveKill first (LC-31 order)", err)
	}
	// The verified value clears; a second clear finds nothing active.
	if epoch, _, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "resolved", 1, at); err != nil || epoch != 1 {
		t.Fatalf("verified clear = (%d, %v), want (1, nil)", epoch, err)
	}
	if _, _, err := s.AppendKillClearStrategy(uid(72), uid(1), "admin-1", "r", 1, at); !errors.Is(err, ErrNoActiveKill) {
		t.Fatalf("duplicate clear err = %v, want ErrNoActiveKill", err)
	}
}

// TestKillClearSupersede pins LC-38: the clear transaction appends the
// safety_effects done-marker for every covered kill event lacking one,
// plus ONE kill_effects_superseded alert per superseded event, and returns
// their ids; already-served events and breaker rows are untouched, and the
// kill rows themselves are never mutated (LC-35).
func TestKillClearSupersede(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	at := formatTime(testNow)

	if _, err := s.AppendStrategyKill(uid(60), uid(1), "trader-1", at, true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if _, err := s.AppendStrategyKill(uid(61), uid(1), "trader-1", at, false); err != nil {
		t.Fatalf("second AppendStrategyKill: %v", err)
	}
	// uid(61) already served by the driver; uid(62) is a breaker row.
	if err := s.AppendSafetyEffectDone(uid(61), at); err != nil {
		t.Fatalf("AppendSafetyEffectDone: %v", err)
	}
	if err := s.AppendKillBreakerEvent(KillBreakerEvent{
		EventID: uid(62), Kind: "breaker", Scope: "strategy", StrategyID: strptr(uid(1)),
		ActorID: "breaker-monitor", RecordedAt: at,
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}

	clearedAt := "2026-07-04T13:00:00Z"
	epoch, superseded, err := s.AppendKillClearStrategy(uid(70), uid(1), "admin-1", "resolved", 2, clearedAt)
	if err != nil || epoch != 2 {
		t.Fatalf("clear = (%d, %v), want (2, nil)", epoch, err)
	}
	if len(superseded) != 1 || superseded[0] != uid(60) {
		t.Fatalf("superseded ids = %v, want only the unserved kill %s", superseded, uid(60))
	}
	if served, err := s.SafetyEffectServed(uid(60)); err != nil || !served {
		t.Fatalf("SafetyEffectServed(%s) = %v, %v, want true, nil", uid(60), served, err)
	}
	var completed string
	if err := s.db.QueryRow(`SELECT completed_at FROM safety_effects WHERE event_id = ?`,
		uid(60)).Scan(&completed); err != nil || completed != clearedAt {
		t.Fatalf("clear-written marker completed_at = %q err=%v, want the clear's recorded_at", completed, err)
	}
	alerts, err := s.ListSafetyAlerts(SafetyAlertFilter{Kind: "kill_effects_superseded"})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("superseded alerts = %d err=%v, want exactly 1", len(alerts), err)
	}
	a := alerts[0]
	if a.StrategyID == nil || *a.StrategyID != uid(1) || a.RefID == nil || *a.RefID != uid(60) ||
		!strings.Contains(a.DetailsJSON, uid(70)) {
		t.Fatalf("superseded alert = %+v, want strategy %s ref %s details referencing the clear", a, uid(1), uid(60))
	}
	// The breaker row is never covered: still unserved; the kill rows are
	// untouched (append-only history, LC-35).
	events, err := s.ListUnservedSafetyEvents()
	if err != nil || len(events) != 1 || events[0].EventID != uid(62) {
		t.Fatalf("unserved events = %+v err=%v, want only the breaker row", events, err)
	}
	var kills int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM kill_breaker_events WHERE kind = 'kill'`).Scan(&kills); err != nil || kills != 2 {
		t.Fatalf("kill rows = %d err=%v, want 2 (never mutated or deleted)", kills, err)
	}
	assertActive(t, s, "cleared", uid(1), false)
}
