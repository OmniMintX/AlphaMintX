package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// stubWatchdog satisfies WatchdogLiveness; tests mutate the fields.
type stubWatchdog struct {
	at time.Time
	ok bool
}

func (w *stubWatchdog) LastBeat(string) (time.Time, bool) { return w.at, w.ok }

func getSafety(t *testing.T, e *testEnv, token, strategyID string) safetyStatusResponse {
	t.Helper()
	rec := e.do(t, "GET", "/api/v1/strategies/"+strategyID+"/safety", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET safety(%s) = %d (body %q)", strategyID, rec.Code, rec.Body.String())
	}
	var resp safetyStatusResponse
	decodeJSON(t, rec, &resp)
	return resp
}

// TestTenantIsolation_SafetyReads pins OS2: cross-tenant safety/alerts
// GETs are 404 identical to absence (no existence oracle), and the global
// feed is 403 FORBIDDEN for EVERY DB principal (OS-20) while the env read
// and env-admin classes pass.
func TestTenantIsolation_SafetyReads(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	createTenant(t, e.store, "tenant-b")
	createTenantStrategy(t, e.store, strat1, "tenant-a", "paper")
	foreign := seedUserToken(t, e.store, "tenant-b", RoleViewer, "tok-b-viewer")

	for _, path := range []string{
		"/api/v1/strategies/" + strat1 + "/safety",
		"/api/v1/strategies/" + strat1 + "/alerts",
	} {
		got := e.do(t, "GET", path, foreign, nil)
		wantError(t, got, 404, codeUnknownStrategy)
		absent := e.do(t, "GET", "/api/v1/strategies/"+uid(99)+"/safety", foreign, nil)
		if got.Body.String() != absent.Body.String() {
			t.Errorf("%s: foreign body %q != absent body %q (existence oracle)", path, got.Body.String(), absent.Body.String())
		}
	}
	for _, role := range []string{RoleViewer, RoleTrader, RoleAdmin, RoleOwner} {
		tok := seedUserToken(t, e.store, "tenant-a", role, "tok-a-"+role)
		wantError(t, e.do(t, "GET", "/api/v1/alerts", tok, nil), 403, codeForbidden)
	}
	for _, tok := range []string{readTok, adminTok} {
		if rec := e.do(t, "GET", "/api/v1/alerts", tok, nil); rec.Code != http.StatusOK {
			t.Errorf("global feed with env class = %d (body %q), want 200", rec.Code, rec.Body.String())
		}
	}
}

// TestSafetyStatus_KillClearedJoin pins OS3 plus the OS-8a wire shape: an
// uncleared strategy kill renders with cleared null and active_kill true;
// after the clear the SAME row carries the clear info and active_kill is
// false; kill items expose EXACTLY the OS-7 keys (no tenant_id, no
// trigger_ref, no kind).
func TestSafetyStatus_KillClearedJoin(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	if _, err := e.store.AppendStrategyKill(uid(60), strat1, "trader-1", formatTime(testNow), true); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if !resp.ActiveKill || len(resp.Kills) != 1 {
		t.Fatalf("status = active %v, %d kills, want true, 1", resp.ActiveKill, len(resp.Kills))
	}
	k := resp.Kills[0]
	if k.EventID != uid(60) || k.Scope != "strategy" || k.KillEpoch != 1 ||
		!k.Flatten || k.ActorID != "trader-1" || k.Cleared != nil {
		t.Fatalf("kill = %+v, want the uncleared uid(60) row", k)
	}
	// Wire-shape pin: exactly the OS-7 kill keys, nothing from the store
	// struct's json tags.
	var raw struct {
		Kills []map[string]any `json:"kills"`
	}
	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/safety", readTok, nil)
	decodeJSON(t, rec, &raw)
	wantKeys := map[string]bool{"event_id": true, "scope": true, "kill_epoch": true,
		"flatten": true, "actor_id": true, "recorded_at": true, "cleared": true}
	if len(raw.Kills[0]) != len(wantKeys) {
		t.Fatalf("kill wire keys = %v, want exactly %v", raw.Kills[0], wantKeys)
	}
	for key := range raw.Kills[0] {
		if !wantKeys[key] {
			t.Fatalf("unexpected kill wire key %q (store json tags must not leak)", key)
		}
	}

	clearedAt := "2026-07-04T13:00:00Z"
	if _, _, err := e.store.AppendKillClearStrategy(uid(70), strat1, "admin-1", "resolved", 1, clearedAt); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	resp = getSafety(t, e, readTok, strat1)
	if resp.ActiveKill || len(resp.Kills) != 1 || resp.Kills[0].Cleared == nil {
		t.Fatalf("after clear = active %v kills %+v, want false with cleared populated", resp.ActiveKill, resp.Kills)
	}
	c := resp.Kills[0].Cleared
	if c.ClearID != uid(70) || c.ActorID != "admin-1" || c.Reason != "resolved" ||
		c.RecordedAt != clearedAt || c.ClearedEpoch != 1 {
		t.Fatalf("cleared = %+v, want the uid(70) clear verbatim", c)
	}
}

// TestSafetyStatus_ScopeCover pins OS4: a tenant kill covers a member
// strategy with scope derived from id NULL-ness ("tenant"); a Phase-1
// global row reports "platform"; a foreign tenant's strategy sees only
// the both-NULL tiers.
func TestSafetyStatus_ScopeCover(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	createTenant(t, e.store, "tenant-b")
	createTenantStrategy(t, e.store, strat1, "tenant-a", "paper")
	createTenantStrategy(t, e.store, strat2, "tenant-b", "paper")
	if _, err := e.store.AppendTenantKill(uid(60), "tenant-a", "admin-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	two := int64(2)
	if err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(61), Kind: "kill", Scope: "global", KillEpoch: &two,
		ActorID: "admin-1", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if len(resp.Kills) != 2 || resp.Kills[0].Scope != "platform" || resp.Kills[1].Scope != "tenant" {
		t.Fatalf("strat1 kills = %+v, want [platform(global row), tenant]", resp.Kills)
	}
	resp2 := getSafety(t, e, readTok, strat2)
	if len(resp2.Kills) != 1 || resp2.Kills[0].EventID != uid(61) || !resp2.ActiveKill {
		t.Fatalf("strat2 kills = %+v active %v, want only the global row, true", resp2.Kills, resp2.ActiveKill)
	}
}

// TestSafetyStatus_BreakerToday pins OS5: a breaker row fired today
// latches active_today true with the event (trigger_ref verbatim); the
// injected clock crossing the UTC boundary flips it to false with a null
// event (the 00:00 UTC auto-reset).
func TestSafetyStatus_BreakerToday(t *testing.T) {
	cur := testNow
	e := newEnv(t, func(c *Config) {
		c.Now = func() time.Time { return cur }
	})
	createStrategy(t, e.store, strat1, "paper")
	trigger := `{"daily_pnl":"-600","limit":"500","evaluated_at":"2026-07-04T12:00:00Z"}`
	if err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(60), Kind: "breaker", Scope: "strategy", StrategyID: &strat1,
		TriggerRef: &trigger, ActorID: "breaker-monitor", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if !resp.Breaker.ActiveToday || resp.Breaker.Event == nil {
		t.Fatalf("breaker today = %+v, want active with event", resp.Breaker)
	}
	ev := resp.Breaker.Event
	if ev.EventID != uid(60) || ev.RecordedAt != formatTime(testNow) ||
		ev.TriggerRef == nil || *ev.TriggerRef != trigger {
		t.Fatalf("breaker event = %+v, want uid(60) with trigger_ref verbatim", ev)
	}
	cur = time.Date(2026, 7, 5, 0, 0, 1, 0, time.UTC) // past the UTC boundary
	resp = getSafety(t, e, readTok, strat1)
	if resp.Breaker.ActiveToday || resp.Breaker.Event != nil {
		t.Fatalf("breaker after midnight = %+v, want false with null event", resp.Breaker)
	}
}

// TestSafetyStatus_WatchdogLiveness pins OS6: nil seam ⇒ enabled false
// with nulls; wired seam with no beat ⇒ enabled true with nulls (never a
// baseline-derived timestamp); a beat ⇒ its instant plus seconds_since
// from the injected clock, clamped to 0 on clock skew.
func TestSafetyStatus_WatchdogLiveness(t *testing.T) {
	e := newEnv(t, nil) // Watchdog nil (paper mode / disabled)
	createStrategy(t, e.store, strat1, "paper")
	resp := getSafety(t, e, readTok, strat1)
	if resp.Watchdog.Enabled || resp.Watchdog.LastHeartbeatAt != nil || resp.Watchdog.SecondsSince != nil {
		t.Fatalf("nil seam watchdog = %+v, want enabled false with nulls", resp.Watchdog)
	}

	wd := &stubWatchdog{}
	e2 := newEnv(t, func(c *Config) { c.Watchdog = wd })
	createStrategy(t, e2.store, strat1, "live_l1")
	resp = getSafety(t, e2, readTok, strat1)
	if !resp.Watchdog.Enabled || resp.Watchdog.LastHeartbeatAt != nil || resp.Watchdog.SecondsSince != nil {
		t.Fatalf("no-beat watchdog = %+v, want enabled true with nulls", resp.Watchdog)
	}
	wd.at, wd.ok = testNow.Add(-12*time.Second), true
	resp = getSafety(t, e2, readTok, strat1)
	w := resp.Watchdog
	if !w.Enabled || w.LastHeartbeatAt == nil || *w.LastHeartbeatAt != "2026-07-04T12:29:48Z" ||
		w.SecondsSince == nil || *w.SecondsSince != 12 {
		t.Fatalf("beat watchdog = %+v, want 2026-07-04T12:29:48Z / 12 s", w)
	}
	wd.at = testNow.Add(5 * time.Second) // clock skew: clamp, never negative
	resp = getSafety(t, e2, readTok, strat1)
	if resp.Watchdog.SecondsSince == nil || *resp.Watchdog.SecondsSince != 0 {
		t.Fatalf("skewed beat seconds_since = %v, want 0", resp.Watchdog.SecondsSince)
	}
}

// TestAlertsFeed_PaginationAndScope pins OS7: pinned ordering
// (recorded_at DESC, alert_id DESC on same-second rows), correct envelope
// totals, identical results across repeated reads of a fixed dataset,
// NULL-strategy rows absent from the strategy feed but present in the
// global feed, and the exact-match ?kind= filter.
func TestAlertsFeed_PaginationAndScope(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	alert := func(alertID, kind string, strategyID *string, recordedAt string) {
		t.Helper()
		if err := e.store.AppendSafetyAlert(store.SafetyAlert{
			AlertID: alertID, Kind: kind, StrategyID: strategyID,
			DetailsJSON: "{}", RecordedAt: recordedAt,
		}); err != nil {
			t.Fatalf("AppendSafetyAlert(%s): %v", alertID, err)
		}
	}
	alert(uid(31), "watchdog_agent_silent", &strat1, "2026-07-04T10:00:00Z")
	alert(uid(33), "watchdog_agent_silent", &strat1, "2026-07-04T10:00:00Z") // same-second tie
	alert(uid(32), "kill_effects_superseded", &strat1, "2026-07-04T09:00:00Z")
	alert(uid(34), "safety_effect_stalled", nil, "2026-07-04T11:00:00Z") // NULL-strategy row

	feed := "/api/v1/strategies/" + strat1 + "/alerts"
	rec := e.do(t, "GET", feed+"?page=1&limit=2", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET alerts = %d (body %q)", rec.Code, rec.Body.String())
	}
	var p page[store.SafetyAlert]
	decodeJSON(t, rec, &p)
	if p.Total != 3 || p.Page != 1 || p.Limit != 2 || len(p.Items) != 2 ||
		p.Items[0].AlertID != uid(33) || p.Items[1].AlertID != uid(31) {
		t.Fatalf("page 1 = %+v, want total 3, [uid(33) uid(31)] (alert_id DESC tiebreak)", p)
	}
	// A fixed dataset reads identically on every poll (OS-16).
	if again := e.do(t, "GET", feed+"?page=1&limit=2", readTok, nil); again.Body.String() != rec.Body.String() {
		t.Fatalf("repeated read differs: %q vs %q", again.Body.String(), rec.Body.String())
	}
	var p2 page[store.SafetyAlert]
	decodeJSON(t, e.do(t, "GET", feed+"?page=2&limit=2", readTok, nil), &p2)
	if len(p2.Items) != 1 || p2.Items[0].AlertID != uid(32) {
		t.Fatalf("page 2 = %+v, want only uid(32) (the NULL-strategy row never leaks in)", p2)
	}

	var global page[store.SafetyAlert]
	decodeJSON(t, e.do(t, "GET", "/api/v1/alerts", readTok, nil), &global)
	if global.Total != 4 || len(global.Items) != 4 || global.Items[0].AlertID != uid(34) ||
		global.Items[0].StrategyID != nil {
		t.Fatalf("global feed = %+v, want 4 rows, NULL-strategy row first", global)
	}
	var byKind page[store.SafetyAlert]
	decodeJSON(t, e.do(t, "GET", "/api/v1/alerts?kind=safety_effect_stalled", readTok, nil), &byKind)
	if byKind.Total != 1 || len(byKind.Items) != 1 || byKind.Items[0].AlertID != uid(34) {
		t.Fatalf("kind filter = %+v, want only uid(34)", byKind)
	}
	var unknown page[store.SafetyAlert]
	decodeJSON(t, e.do(t, "GET", "/api/v1/alerts?kind=no_such_kind", readTok, nil), &unknown)
	if unknown.Total != 0 || unknown.Items == nil || len(unknown.Items) != 0 {
		t.Fatalf("unknown kind = %+v, want an empty non-null items array", unknown)
	}
}

// TestSafetyStatus_ReadOnly pins OS8 (invariant 2): the safety and alert
// GETs write nothing to any safety table, and GETs charge no rate bucket
// — more requests than the 60/min budget all answer 200.
func TestSafetyStatus_ReadOnly(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	if _, err := e.store.AppendStrategyKill(uid(60), strat1, "trader-1", formatTime(testNow), false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if err := e.store.AppendSafetyAlert(store.SafetyAlert{
		AlertID: uid(31), Kind: "watchdog_agent_silent", StrategyID: &strat1,
		DetailsJSON: "{}", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendSafetyAlert: %v", err)
	}
	alertsBefore, err := e.store.ListSafetyAlerts(store.SafetyAlertFilter{})
	if err != nil {
		t.Fatalf("ListSafetyAlerts: %v", err)
	}
	unservedBefore, err := e.store.ListUnservedSafetyEvents()
	if err != nil {
		t.Fatalf("ListUnservedSafetyEvents: %v", err)
	}
	for i := 0; i < 65; i++ { // past the 60/min bucket: GETs never charge
		if rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/safety", readTok, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET #%d = %d (body %q), want 200 (no rate charge on GET)", i+1, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/alerts", readTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET alerts = %d, want 200", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/v1/alerts", readTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET global alerts = %d, want 200", rec.Code)
	}
	alertsAfter, err := e.store.ListSafetyAlerts(store.SafetyAlertFilter{})
	if err != nil {
		t.Fatalf("ListSafetyAlerts after: %v", err)
	}
	unservedAfter, err := e.store.ListUnservedSafetyEvents()
	if err != nil {
		t.Fatalf("ListUnservedSafetyEvents after: %v", err)
	}
	if len(alertsAfter) != len(alertsBefore) || len(unservedAfter) != len(unservedBefore) {
		t.Fatalf("safety rows changed under GETs: alerts %d->%d, unserved %d->%d",
			len(alertsBefore), len(alertsAfter), len(unservedBefore), len(unservedAfter))
	}
}

// TestSafetyStatus_StrategyClearNeverCoversTenantKill pins OS9 end to
// end: a strategy-scope clear whose row carries the SAME tenant_id never
// annotates the tenant kill — it stays cleared: null and active_kill true.
func TestSafetyStatus_StrategyClearNeverCoversTenantKill(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	createTenantStrategy(t, e.store, strat1, "tenant-a", "paper")
	at := formatTime(testNow)
	if _, err := e.store.AppendTenantKill(uid(60), "tenant-a", "admin-1", at, false); err != nil {
		t.Fatalf("AppendTenantKill: %v", err)
	}
	if _, err := e.store.AppendStrategyKill(uid(61), strat1, "trader-1", at, false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	if _, _, err := e.store.AppendKillClearStrategy(uid(70), strat1, "admin-1", "resolved", 2, at); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if !resp.ActiveKill || len(resp.Kills) != 2 {
		t.Fatalf("status = active %v, %d kills, want true, 2", resp.ActiveKill, len(resp.Kills))
	}
	if resp.Kills[0].EventID != uid(61) || resp.Kills[0].Cleared == nil {
		t.Fatalf("strategy kill = %+v, want cleared", resp.Kills[0])
	}
	if resp.Kills[1].EventID != uid(60) || resp.Kills[1].Scope != "tenant" || resp.Kills[1].Cleared != nil {
		t.Fatalf("tenant kill = %+v, want scope tenant with cleared: null", resp.Kills[1])
	}
}

// TestSafetyStatus_SingleSnapshot pins OS10: with kill/clear appends
// interleaved between requests, every response is internally consistent —
// active_kill agrees with the join, the breaker latch agrees with its
// event, paused_from agrees with lifecycle_state.
func TestSafetyStatus_SingleSnapshot(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	at := formatTime(testNow)
	assertConsistent := func(step string) safetyStatusResponse {
		t.Helper()
		resp := getSafety(t, e, readTok, strat1)
		uncleared := false
		for _, k := range resp.Kills {
			if k.Cleared == nil {
				uncleared = true
			}
		}
		if resp.ActiveKill != uncleared {
			t.Fatalf("%s: torn response — active_kill %v vs uncleared-join %v", step, resp.ActiveKill, uncleared)
		}
		if resp.Breaker.ActiveToday != (resp.Breaker.Event != nil) {
			t.Fatalf("%s: breaker latch %v disagrees with event %+v", step, resp.Breaker.ActiveToday, resp.Breaker.Event)
		}
		if resp.LifecycleState != "paused" && resp.PausedFrom != nil {
			t.Fatalf("%s: paused_from %q on a %s strategy", step, *resp.PausedFrom, resp.LifecycleState)
		}
		return resp
	}
	assertConsistent("empty")
	if _, err := e.store.AppendStrategyKill(uid(60), strat1, "trader-1", at, false); err != nil {
		t.Fatalf("AppendStrategyKill: %v", err)
	}
	assertConsistent("after kill")
	if err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(61), Kind: "breaker", Scope: "strategy", StrategyID: &strat1,
		ActorID: "breaker-monitor", RecordedAt: at,
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	assertConsistent("after breaker")
	if _, _, err := e.store.AppendKillClearStrategy(uid(70), strat1, "admin-1", "resolved", 1, at); err != nil {
		t.Fatalf("AppendKillClearStrategy: %v", err)
	}
	if resp := assertConsistent("after clear"); resp.ActiveKill {
		t.Fatalf("after clear active_kill = true, want false")
	}
}

// TestSafetyStatus_PausedFrom pins OS11: a paused strategy reports its
// LC-7 provenance from_state; non-paused reports null; paused with no
// provenance row reports null (the client disables resume).
func TestSafetyStatus_PausedFrom(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "live_l1")
	if resp := getSafety(t, e, readTok, strat1); resp.PausedFrom != nil {
		t.Fatalf("live strategy paused_from = %q, want null", *resp.PausedFrom)
	}
	if err := e.store.AppendLifecycleTransition(store.LifecycleTransition{
		TransitionID: uid(80), StrategyID: strat1, FromState: "live_l1", ToState: "paused",
		ActorID: "trader-1", ActorRole: "trader", Reason: "pause", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if resp.LifecycleState != "paused" || resp.PausedFrom == nil || *resp.PausedFrom != "live_l1" {
		t.Fatalf("paused status = %s / %v, want paused / live_l1", resp.LifecycleState, resp.PausedFrom)
	}
	createStrategy(t, e.store, strat2, "paused") // no to_state='paused' audit row
	resp = getSafety(t, e, readTok, strat2)
	if resp.LifecycleState != "paused" || resp.PausedFrom != nil {
		t.Fatalf("unknown-provenance status = %s / %v, want paused / null", resp.LifecycleState, resp.PausedFrom)
	}
}

// TestSafetyStatus_NullEpochHidden pins OS12: a legacy NULL-epoch
// kind='kill' row is absent from kills, never flips active_kill, and
// renders no banner input.
func TestSafetyStatus_NullEpochHidden(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	if err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(60), Kind: "kill", Scope: "strategy", StrategyID: &strat1,
		ActorID: "legacy", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	resp := getSafety(t, e, readTok, strat1)
	if resp.ActiveKill || len(resp.Kills) != 0 {
		t.Fatalf("NULL-epoch row rendered: active %v, kills %+v, want false, []", resp.ActiveKill, resp.Kills)
	}
}

// TestSafetyStatus_BreakerEventScopeParity pins OS13: strategy-, tenant-,
// and platform-scope breaker rows each latch active_today true WITH a
// non-null matching event; event is null exactly when no row matches
// today (and a foreign tenant's rows never match).
func TestSafetyStatus_BreakerEventScopeParity(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	createTenant(t, e.store, "tenant-b")
	createTenantStrategy(t, e.store, strat1, "tenant-a", "paper")
	createTenantStrategy(t, e.store, strat2, "tenant-b", "paper")
	tenantA := "tenant-a"
	breaker := func(eventID string, sid, tid *string, recordedAt string) {
		t.Helper()
		if err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
			EventID: eventID, Kind: "breaker", Scope: "strategy",
			StrategyID: sid, TenantID: tid, ActorID: "breaker-monitor", RecordedAt: recordedAt,
		}); err != nil {
			t.Fatalf("AppendKillBreakerEvent(%s): %v", eventID, err)
		}
	}
	assertParity := func(step, strategyID string, wantEventID string) {
		t.Helper()
		resp := getSafety(t, e, readTok, strategyID)
		if wantEventID == "" {
			if resp.Breaker.ActiveToday || resp.Breaker.Event != nil {
				t.Fatalf("%s: breaker = %+v, want false with null event", step, resp.Breaker)
			}
			return
		}
		if !resp.Breaker.ActiveToday || resp.Breaker.Event == nil || resp.Breaker.Event.EventID != wantEventID {
			t.Fatalf("%s: breaker = %+v, want true with event %s", step, resp.Breaker, wantEventID)
		}
	}
	assertParity("no rows", strat1, "")
	breaker(uid(60), &strat1, &tenantA, "2026-07-04T09:00:00Z") // strategy scope
	assertParity("strategy scope", strat1, uid(60))
	assertParity("foreign tenant untouched", strat2, "")
	breaker(uid(61), nil, &tenantA, "2026-07-04T10:00:00Z") // tenant scope
	assertParity("tenant scope", strat1, uid(61))
	assertParity("tenant scope foreign", strat2, "")
	breaker(uid(62), nil, nil, "2026-07-04T11:00:00Z") // platform scope
	assertParity("platform scope", strat1, uid(62))
	assertParity("platform scope binds all", strat2, uid(62))
}
