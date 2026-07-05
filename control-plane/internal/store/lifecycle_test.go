package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// ts renders an RFC 3339 UTC timestamp on the test day at the given hour.
func ts(hour int) string {
	return fmt.Sprintf("2026-07-04T%02d:00:00Z", hour)
}

// transitionRows counts a strategy's lifecycle_transitions rows, optionally
// narrowed to one to_state ("" = all).
func transitionRows(t *testing.T, s *Store, strategyID, toState string) int {
	t.Helper()
	q, args := `SELECT COUNT(*) FROM lifecycle_transitions WHERE strategy_id = ?`, []any{strategyID}
	if toState != "" {
		q, args = q+` AND to_state = ?`, append(args, toState)
	}
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return n
}

// mustTransition appends one lifecycle transition row via the audit
// appender (advancing the snapshot), failing the test on error.
func mustTransition(t *testing.T, s *Store, idx int, strategyID, from, to, at string) {
	t.Helper()
	if err := s.AppendLifecycleTransition(LifecycleTransition{
		TransitionID: uid(idx), StrategyID: strategyID, FromState: from, ToState: to,
		ActorID: "admin-1", ActorRole: "admin", Reason: "test", RecordedAt: at,
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition(%s -> %s): %v", from, to, err)
	}
}

// TestCreateStrategyBootstrapRow pins LC-16a: an initial `paper` or
// `live_*` state writes the strategies row AND ONE draft→<initial>
// bootstrap transition row atomically (actor 'bootstrap', role 'system',
// reason 'bootstrap', recorded_at = created_at); other initial states
// write no transition row.
func TestCreateStrategyBootstrapRow(t *testing.T) {
	s := openStore(t)
	for i, c := range []struct{ state, wantTo string }{
		{"paper", "paper"}, {"live_l2", "live_l2"}, {"draft", ""},
	} {
		sid := uid(i + 1)
		if err := s.CreateStrategy(Strategy{
			StrategyID: sid, TenantID: "tenant-a", Name: "s", LifecycleState: c.state,
			CreatedAt: formatTime(testNow), UpdatedAt: formatTime(testNow),
		}); err != nil {
			t.Fatalf("CreateStrategy(%s): %v", c.state, err)
		}
		if c.wantTo == "" {
			if n := transitionRows(t, s, sid, ""); n != 0 {
				t.Errorf("%s: transition rows = %d, want 0 (no bootstrap row)", c.state, n)
			}
			continue
		}
		var from, to, actor, role, reason, at string
		if err := s.db.QueryRow(`SELECT from_state, to_state, actor_id, actor_role, reason, recorded_at
			FROM lifecycle_transitions WHERE strategy_id = ?`, sid).
			Scan(&from, &to, &actor, &role, &reason, &at); err != nil {
			t.Fatalf("%s: bootstrap row: %v", c.state, err)
		}
		if from != "draft" || to != c.wantTo || actor != "bootstrap" ||
			role != "system" || reason != "bootstrap" || at != formatTime(testNow) {
			t.Errorf("%s: bootstrap row = (%s, %s, %s, %s, %s, %s), want draft/%s/bootstrap/system/bootstrap at created_at",
				c.state, from, to, actor, role, reason, at, c.wantTo)
		}
		if n := transitionRows(t, s, sid, ""); n != 1 {
			t.Errorf("%s: transition rows = %d, want exactly 1", c.state, n)
		}
	}
}

// TestOpenMigratesLifecycleBootstrap pins the LC-16a migration: legacy
// `paper` and `paused` strategies with no to_state='paper' row get ONE
// synthetic draft→paper row (recorded_at = created_at) exactly once across
// reopens; a migrated paused strategy's PausedProvenance is unchanged, and
// draft strategies are untouched.
func TestOpenMigratesLifecycleBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Legacy shape: strategies rows inserted WITHOUT CreateStrategy's
	// bootstrap (pre-lifecycle-api databases).
	for i, state := range []string{"paper", "paused", "draft"} {
		if _, err := s.db.Exec(`INSERT INTO strategies
			(strategy_id, tenant_id, name, lifecycle_state, created_at, updated_at)
			VALUES (?, 'tenant-a', 'legacy', ?, ?, ?)`,
			uid(i+1), state, ts(9), ts(9)); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
	}
	// The paused strategy's real paused-entry row (from live_l1).
	mustTransition(t, s, 50, uid(2), "live_l1", "paused", ts(10))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := 0; i < 2; i++ { // reopen twice: the migration is idempotent
		if s, err = Open(path); err != nil {
			t.Fatalf("reopen #%d: %v", i+1, err)
		}
		if i == 0 {
			s.Close()
		}
	}
	defer s.Close()
	for _, sid := range []string{uid(1), uid(2)} {
		if n := transitionRows(t, s, sid, "paper"); n != 1 {
			t.Errorf("%s: synthetic paper rows = %d, want exactly 1", sid, n)
		}
		var from, actor, role, at string
		if err := s.db.QueryRow(`SELECT from_state, actor_id, actor_role, recorded_at
			FROM lifecycle_transitions WHERE strategy_id = ? AND to_state = 'paper'`, sid).
			Scan(&from, &actor, &role, &at); err != nil {
			t.Fatalf("%s: synthetic row: %v", sid, err)
		}
		if from != "draft" || actor != "bootstrap" || role != "system" || at != ts(9) {
			t.Errorf("%s: synthetic row = (%s, %s, %s, %s), want draft/bootstrap/system at created_at", sid, from, actor, role, at)
		}
	}
	// The synthetic row is a WINDOW-START record only: provenance still
	// reads the real to_state='paused' row.
	if from, ok, err := s.PausedProvenance(uid(2)); err != nil || !ok || from != "live_l1" {
		t.Errorf("PausedProvenance = (%s, %v, %v), want (live_l1, true, nil)", from, ok, err)
	}
	if n := transitionRows(t, s, uid(3), ""); n != 0 {
		t.Errorf("draft strategy transition rows = %d, want 0", n)
	}
}

// TestAppendLifecycleTransitionCAS pins LC-9: the CAS mutator writes the
// audit row and advances the snapshot only when the observed from_state
// still holds; a conflict writes NOTHING; concurrent kill-lock vs unlock
// serialize with exactly one audit row per winner.
func TestAppendLifecycleTransitionCAS(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a") // paper (+ bootstrap row)

	if _, err := s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(50), StrategyID: uid(99), FromState: "paper", ToState: "paused",
		ActorID: "a", ActorRole: "admin", Reason: "r", RecordedAt: ts(10),
	}, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CAS unknown strategy err = %v, want ErrNotFound", err)
	}
	// Stale from_state: conflict, nothing written.
	ok, err := s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(51), StrategyID: uid(1), FromState: "draft", ToState: "paper",
		ActorID: "a", ActorRole: "admin", Reason: "r", RecordedAt: ts(10),
	}, false)
	if err != nil || ok {
		t.Fatalf("stale CAS = %v, %v, want false, nil", ok, err)
	}
	if st, _ := s.GetStrategy(uid(1)); st.LifecycleState != "paper" {
		t.Fatalf("state after conflict = %s, want paper", st.LifecycleState)
	}
	if n := transitionRows(t, s, uid(1), ""); n != 1 {
		t.Fatalf("transition rows after conflict = %d, want 1 (bootstrap only)", n)
	}
	// Matching from_state: audit row + snapshot advance (a live target
	// with NO active kill passes the in-transaction re-check).
	ok, err = s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(52), StrategyID: uid(1), FromState: "paper", ToState: "live_l1",
		ActorID: "trader-1", ActorRole: "trader", Reason: "promotion", RecordedAt: ts(11),
	}, true)
	if err != nil || !ok {
		t.Fatalf("CAS promotion = %v, %v, want true, nil", ok, err)
	}
	if st, _ := s.GetStrategy(uid(1)); st.LifecycleState != "live_l1" || st.UpdatedAt != ts(11) {
		t.Fatalf("snapshot = %+v, want live_l1 at %s", st, ts(11))
	}
	// Concurrent kill-lock vs unlock: the lock lands first, the CAS keyed
	// on the pre-lock state loses without writing.
	if locked, err := s.AppendKillLifecycleLock(uid(1), uid(60), "safety-engine", ts(12)); err != nil || !locked {
		t.Fatalf("kill lock = %v, %v, want true, nil", locked, err)
	}
	ok, err = s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(53), StrategyID: uid(1), FromState: "live_l1", ToState: "paused",
		ActorID: "trader-1", ActorRole: "trader", Reason: "pause", RecordedAt: ts(12),
	}, false)
	if err != nil || ok {
		t.Fatalf("CAS after kill lock = %v, %v, want false, nil (loser writes nothing)", ok, err)
	}
	if st, _ := s.GetStrategy(uid(1)); st.LifecycleState != "killed" {
		t.Fatalf("state = %s, want killed (lock won)", st.LifecycleState)
	}
	// The unlock CAS wins from killed; a late kill lock then no-ops.
	ok, err = s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(54), StrategyID: uid(1), FromState: "killed", ToState: "paper",
		ActorID: "admin-1", ActorRole: "admin", Reason: "unlock", RecordedAt: ts(13),
	}, false)
	if err != nil || !ok {
		t.Fatalf("unlock CAS = %v, %v, want true, nil", ok, err)
	}
	if locked, err := s.AppendKillLifecycleLock(uid(1), uid(61), "safety-engine", ts(13)); err != nil || locked {
		t.Fatalf("late kill lock = %v, %v, want false, nil (paper is not live_*)", locked, err)
	}
	// One audit row per winner: bootstrap, promotion, lock, unlock.
	if n := transitionRows(t, s, uid(1), ""); n != 4 {
		t.Fatalf("transition rows = %d, want 4 (no loser rows, no skipped states)", n)
	}
}

// TestAppendLifecycleTransitionCASKillActive pins the LC-9 in-transaction
// live-target re-check: an active kill fails a live-target CAS with
// ErrKillActive writing NOTHING — even though from_state still matches —
// while non-live targets pass under the kill and the live target passes
// once the kill is cleared.
func TestAppendLifecycleTransitionCASKillActive(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a") // paper (+ bootstrap row)
	epoch, err := s.AppendStrategyKill(uid(60), uid(1), "op-1", ts(10), false)
	if err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}

	if _, err := s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(50), StrategyID: uid(1), FromState: "paper", ToState: "live_l1",
		ActorID: "admin-1", ActorRole: "admin", Reason: "promote", RecordedAt: ts(11),
	}, true); !errors.Is(err, ErrKillActive) {
		t.Fatalf("live-target CAS under active kill err = %v, want ErrKillActive", err)
	}
	if st, _ := s.GetStrategy(uid(1)); st.LifecycleState != "paper" {
		t.Fatalf("state = %s, want paper untouched", st.LifecycleState)
	}
	if n := transitionRows(t, s, uid(1), "live_l1"); n != 0 {
		t.Fatalf("live_l1 rows = %d, want 0 (nothing written)", n)
	}

	// A non-live target is untouched by the kill re-check.
	ok, err := s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(51), StrategyID: uid(1), FromState: "paper", ToState: "paused",
		ActorID: "admin-1", ActorRole: "admin", Reason: "pause", RecordedAt: ts(11),
	}, false)
	if err != nil || !ok {
		t.Fatalf("pause CAS under active kill = %v, %v, want true, nil", ok, err)
	}

	// Cleared: the live-target CAS commits.
	if _, _, err := s.AppendKillClearStrategy(uid(61), uid(1), "admin-1",
		"resolved", epoch, ts(12)); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	ok, err = s.AppendLifecycleTransitionCAS(LifecycleTransition{
		TransitionID: uid(52), StrategyID: uid(1), FromState: "paused", ToState: "live_l1",
		ActorID: "admin-1", ActorRole: "admin", Reason: "resume", RecordedAt: ts(13),
	}, true)
	if err != nil || !ok {
		t.Fatalf("live-target CAS after clear = %v, %v, want true, nil", ok, err)
	}
	if st, _ := s.GetStrategy(uid(1)); st.LifecycleState != "live_l1" {
		t.Fatalf("state = %s, want live_l1", st.LifecycleState)
	}
}

// TestPausedProvenance pins the LC-7 read: the NEWEST to_state='paused'
// row's from_state, rowid breaking recorded_at ties; ok=false when none.
func TestPausedProvenance(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")

	if _, ok, err := s.PausedProvenance(uid(1)); err != nil || ok {
		t.Fatalf("no paused rows: ok=%v err=%v, want false, nil", ok, err)
	}
	mustTransition(t, s, 50, uid(1), "paper", "paused", ts(10))
	if from, ok, err := s.PausedProvenance(uid(1)); err != nil || !ok || from != "paper" {
		t.Fatalf("first pause = (%s, %v, %v), want (paper, true, nil)", from, ok, err)
	}
	mustTransition(t, s, 51, uid(1), "paused", "paper", ts(11))
	// Two paused entries in the SAME second: rowid picks the later insert.
	mustTransition(t, s, 52, uid(1), "paper", "paused", ts(12))
	mustTransition(t, s, 53, uid(1), "paused", "live_l2", ts(12))
	mustTransition(t, s, 54, uid(1), "live_l2", "paused", ts(12))
	if from, ok, err := s.PausedProvenance(uid(1)); err != nil || !ok || from != "live_l2" {
		t.Fatalf("tied pauses = (%s, %v, %v), want (live_l2, true, nil)", from, ok, err)
	}
}

func assertWindow(t *testing.T, s *Store, name, strategyID, wantStart string, wantOK bool) {
	t.Helper()
	start, ok, err := s.PaperWindowStart(strategyID)
	if err != nil || ok != wantOK || start != wantStart {
		t.Fatalf("%s: PaperWindowStart = (%q, %v, %v), want (%q, %v, nil)",
			name, start, ok, err, wantStart, wantOK)
	}
}

// TestPaperWindowStartOrdinaryResume pins LC-16's base cases: the
// bootstrap entry is the qualifying window start, an ORDINARY
// paused→paper resume never restarts it (pre-pause fills still count),
// and a strategy with no qualifying row fails closed.
func TestPaperWindowStartOrdinaryResume(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a") // paper, bootstrap at 12:00
	assertWindow(t, s, "fresh paper strategy", uid(1), ts(12), true)

	mustTransition(t, s, 50, uid(1), "paper", "paused", ts(13))
	mustTransition(t, s, 51, uid(1), "paused", "paper", ts(14))
	assertWindow(t, s, "ordinary resume keeps S", uid(1), ts(12), true)

	if err := s.CreateStrategy(Strategy{
		StrategyID: uid(2), TenantID: "tenant-a", Name: "d", LifecycleState: "draft",
		CreatedAt: ts(12), UpdatedAt: ts(12),
	}); err != nil {
		t.Fatalf("CreateStrategy(draft): %v", err)
	}
	assertWindow(t, s, "no qualifying row fails closed", uid(2), "", false)
}

// TestPaperWindowStartInPlaceKill pins the LC-16 kill reset: an in-place
// tenant kill on a `paper` strategy fails the gate closed — even after the
// kill is CLEARED — until a pause→resume re-entry restarts the window.
func TestPaperWindowStartInPlaceKill(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-b")

	epoch, err := s.AppendTenantKill(uid(60), "tenant-b", "admin-1", ts(15), false)
	if err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	assertWindow(t, s, "in-place kill closes the gate", uid(1), "", false)
	if _, _, err := s.AppendKillClearTenant(uid(61), "tenant-b", "admin-1", "resolved", epoch, ts(15)); err != nil {
		t.Fatalf("AppendKillClearTenant: %v", err)
	}
	// K keys off the kill EVENT, cleared or not: still closed.
	assertWindow(t, s, "cleared kill still closes the gate", uid(1), "", false)

	mustTransition(t, s, 50, uid(1), "paper", "paused", ts(16))
	mustTransition(t, s, 51, uid(1), "paused", "paper", ts(17))
	assertWindow(t, s, "pause->resume re-entry restarts", uid(1), ts(17), true)
}

// TestPaperWindowStartKilledPausedExit pins the paused-after-kill exit:
// killed → paused → paper QUALIFIES (LC-7's lock reproduced from the
// audit trail) and restarts the window.
func TestPaperWindowStartKilledPausedExit(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	mustTransition(t, s, 50, uid(1), "paper", "live_l1", ts(13))
	if _, err := s.AppendStrategyKill(uid(60), uid(1), "op-1", ts(14), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if locked, err := s.AppendKillLifecycleLock(uid(1), uid(60), "safety-engine", ts(14)); err != nil || !locked {
		t.Fatalf("AppendKillLifecycleLock = %v, %v, want true, nil", locked, err)
	}
	assertWindow(t, s, "killed strategy fails closed", uid(1), "", false)

	mustTransition(t, s, 51, uid(1), "killed", "paused", ts(15))
	mustTransition(t, s, 52, uid(1), "paused", "paper", ts(16))
	assertWindow(t, s, "killed->paused->paper restarts", uid(1), ts(16), true)
}

// TestListPaperGateFills pins the LC-18 join: the strategy's fills with
// fill_ts >= since, joined to symbol/side/reduce_only, ordered
// (fill_ts, fills.rowid); other strategies' and pre-window fills excluded.
func TestListPaperGateFills(t *testing.T) {
	s := openStore(t)
	createTenantStrategy(t, s, uid(1), "tenant-a")
	createTenantStrategy(t, s, uid(2), "tenant-a")
	orders := []Order{
		{OrderID: uid(70), Origin: "kill", StrategyID: uid(1), Symbol: "BTC/USDT", Class: "ENTRY",
			Side: "buy", Type: "market", QtyBase: "0.5", Status: "filled", SubmittedAt: ts(12)},
		{OrderID: uid(71), Origin: "kill", StrategyID: uid(1), Symbol: "ETH/USDT", Class: "PROTECTIVE",
			Side: "sell", Type: "market", ReduceOnly: true, QtyBase: "0.5", Status: "filled", SubmittedAt: ts(12)},
		{OrderID: uid(72), Origin: "kill", StrategyID: uid(2), Symbol: "BTC/USDT", Class: "ENTRY",
			Side: "buy", Type: "market", QtyBase: "0.5", Status: "filled", SubmittedAt: ts(12)},
	}
	for _, o := range orders {
		if err := s.InsertOrder(o); err != nil {
			t.Fatalf("InsertOrder(%s): %v", o.OrderID, err)
		}
	}
	fills := []Fill{
		{FillID: uid(80), OrderID: uid(70), QtyBase: "0.5", FillPrice: "100", FeeQuote: "0.1", FillTS: ts(11)},
		{FillID: uid(81), OrderID: uid(70), QtyBase: "0.5", FillPrice: "101", FeeQuote: "0.1", FillTS: ts(12)},
		{FillID: uid(82), OrderID: uid(71), QtyBase: "0.5", FillPrice: "102", FeeQuote: "0.1", FillTS: ts(12)},
		{FillID: uid(83), OrderID: uid(72), QtyBase: "0.5", FillPrice: "103", FeeQuote: "0.1", FillTS: ts(13)},
		{FillID: uid(84), OrderID: uid(71), QtyBase: "0.5", FillPrice: "104", FeeQuote: "0.1", FillTS: ts(13)},
	}
	for _, f := range fills {
		if err := s.InsertFill(f); err != nil {
			t.Fatalf("InsertFill(%s): %v", f.FillID, err)
		}
	}
	got, err := s.ListPaperGateFills(uid(1), ts(12))
	if err != nil {
		t.Fatalf("ListPaperGateFills: %v", err)
	}
	want := []PaperGateFill{
		{Symbol: "BTC/USDT", Side: "buy", QtyBase: "0.5", FillPrice: "101", FeeQuote: "0.1", FillTS: ts(12)},
		{Symbol: "ETH/USDT", Side: "sell", ReduceOnly: true, QtyBase: "0.5", FillPrice: "102", FeeQuote: "0.1", FillTS: ts(12)},
		{Symbol: "ETH/USDT", Side: "sell", ReduceOnly: true, QtyBase: "0.5", FillPrice: "104", FeeQuote: "0.1", FillTS: ts(13)},
	}
	if len(got) != len(want) {
		t.Fatalf("fills = %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fill[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
