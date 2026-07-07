package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// seedFill persists one filled order + fill for the strategy at
// testNow + minuteOffset (inside the bootstrap paper window).
func seedFill(t *testing.T, e *testEnv, base int, strategyID, side, qty, price, fee string, minuteOffset int) {
	t.Helper()
	ts := formatTime(testNow.Add(time.Duration(minuteOffset) * time.Minute))
	if err := e.store.InsertOrder(store.Order{
		OrderID: uid(base), Origin: "kill", StrategyID: strategyID, Symbol: "BTC/USDT",
		Class: "ENTRY", Side: side, Type: "market", QtyBase: qty, Status: "filled",
		SubmittedAt: ts,
	}); err != nil {
		t.Fatalf("InsertOrder(%d): %v", base, err)
	}
	if err := e.store.InsertFill(store.Fill{
		FillID: uid(base + 1), OrderID: uid(base), QtyBase: qty, FillPrice: price,
		FeeQuote: fee, FillTS: ts,
	}); err != nil {
		t.Fatalf("InsertFill(%d): %v", base+1, err)
	}
}

// seedTrip is one full long round trip: buy then sell one minute apart.
func seedTrip(t *testing.T, e *testEnv, base int, strategyID, entry, exit string, minuteOffset int) {
	t.Helper()
	seedFill(t, e, base, strategyID, "buy", "1", entry, "0", minuteOffset)
	seedFill(t, e, base+2, strategyID, "sell", "1", exit, "0", minuteOffset+1)
}

func performancePath(strategyID string) string {
	return "/api/v1/strategies/" + strategyID + "/performance"
}

// TestPerformanceRead pins the happy path: the anchored equity curve
// (window start at the seed, then one post-fill sample per fill), the
// ADR-0003 decimal-string stats, tenant-scoped 404, and the null model.
func TestPerformanceRead(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper") // bootstrap window at testNow
	seedTrip(t, e, 100, strat1, "1000", "1010", 1)

	wantError(t, e.do(t, "GET", performancePath(uid(99)), readTok, nil), 404, codeUnknownStrategy)

	rec := e.do(t, "GET", performancePath(strat1), readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp performanceResponse
	decodeJSON(t, rec, &resp)
	if resp.StrategyID != strat1 || resp.Seed != "10000" ||
		resp.EvaluatedAt != formatTime(testNow) || resp.Model != nil {
		t.Fatalf("response head = %+v", resp)
	}
	if resp.WindowStartedAt == nil || *resp.WindowStartedAt != formatTime(testNow) {
		t.Fatalf("window_started_at = %v, want %q", resp.WindowStartedAt, formatTime(testNow))
	}
	want := []equityPointJSON{
		{TS: formatTime(testNow), Equity: "10000"},
		{TS: formatTime(testNow.Add(1 * time.Minute)), Equity: "10000"},
		{TS: formatTime(testNow.Add(2 * time.Minute)), Equity: "10010"},
	}
	if len(resp.EquityCurve) != len(want) {
		t.Fatalf("equity_curve = %+v, want %+v", resp.EquityCurve, want)
	}
	for i, p := range resp.EquityCurve {
		if p != want[i] {
			t.Errorf("equity_curve[%d] = %+v, want %+v", i, p, want[i])
		}
	}
	st := resp.Stats
	if st.RealizedPnL != "10" || st.ReturnPct != "0.1" || st.MaxDrawdownPct != "0" ||
		st.ClosedTrades != 1 || st.Wins != 1 || st.Losses != 0 || st.WinRatePct != "100" ||
		st.ProfitFactor != nil || st.FeesPaid != "0" {
		t.Errorf("stats = %+v", st)
	}
	if st.LastFillAt == nil || *st.LastFillAt != formatTime(testNow.Add(2*time.Minute)) {
		t.Errorf("last_fill_at = %v, want the newest fill", st.LastFillAt)
	}

	// A foreign-tenant viewer cannot see the strategy (404, not 403).
	createTenant(t, e.store, "tenant-2")
	foreign := seedUserToken(t, e.store, "tenant-2", RoleViewer, "db-viewer-2")
	wantError(t, e.do(t, "GET", performancePath(strat1), foreign, nil), 404, codeUnknownStrategy)
}

// TestPerformanceNoWindow pins the fail-closed window edge: a draft
// strategy (no qualifying paper entry) answers window_started_at null,
// equity_curve [], and all-zero stats — never an error.
func TestPerformanceNoWindow(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "draft")

	rec := e.do(t, "GET", performancePath(strat1), readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp performanceResponse
	decodeJSON(t, rec, &resp)
	if resp.WindowStartedAt != nil || len(resp.EquityCurve) != 0 {
		t.Fatalf("no-window response = %+v, want null window and [] curve", resp)
	}
	if resp.Stats.RealizedPnL != "0" || resp.Stats.ClosedTrades != 0 || resp.Stats.LastFillAt != nil {
		t.Errorf("stats = %+v, want all-zero", resp.Stats)
	}
	// The curve is a JSON array, never null.
	if body := rec.Body.String(); !strings.Contains(body, `"equity_curve":[]`) {
		t.Errorf("body %q missing \"equity_curve\":[]", body)
	}
}

// TestPerformanceMaxPoints pins the ?max_points downsampling: at most
// max_points samples with the anchor (window start) and the newest sample
// ALWAYS kept; values < 2 clamp to 2; larger-than-curve values are no-ops;
// non-numeric values fall back to the 500 default.
func TestPerformanceMaxPoints(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	for i := 0; i < 5; i++ {
		seedTrip(t, e, 100+i*4, strat1, "1000", "1010", 1+i*2)
	}
	// Full curve: anchor + 10 fills = 11 points.
	first := formatTime(testNow)
	last := formatTime(testNow.Add(10 * time.Minute))

	get := func(query string) performanceResponse {
		t.Helper()
		rec := e.do(t, "GET", performancePath(strat1)+query, readTok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (body %q)", query, rec.Code, rec.Body.String())
		}
		var resp performanceResponse
		decodeJSON(t, rec, &resp)
		return resp
	}

	for _, c := range []struct {
		query string
		want  int
	}{
		{"", 11}, {"?max_points=500", 11}, {"?max_points=4", 4},
		{"?max_points=2", 2}, {"?max_points=1", 2}, {"?max_points=abc", 11},
	} {
		resp := get(c.query)
		if len(resp.EquityCurve) != c.want {
			t.Errorf("%q: curve points = %d, want %d", c.query, len(resp.EquityCurve), c.want)
			continue
		}
		if resp.EquityCurve[0].TS != first || resp.EquityCurve[len(resp.EquityCurve)-1].TS != last {
			t.Errorf("%q: endpoints = (%s, %s), want (%s, %s)", c.query,
				resp.EquityCurve[0].TS, resp.EquityCurve[len(resp.EquityCurve)-1].TS, first, last)
		}
	}
	// Downsampling thins the curve only: the stats stay full-fidelity.
	if resp := get("?max_points=2"); resp.Stats.ClosedTrades != 5 || resp.Stats.RealizedPnL != "50" {
		t.Errorf("downsampled stats = %+v, want 5 trades / +50", resp.Stats)
	}
}

// TestPerformanceModelAttribution pins the model field: the newest
// node='trader' model_costs row's model, null before any trace.
func TestPerformanceModelAttribution(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	proposalID, _, runID := insertChain(t, e.store, 10, strat1, 0)
	if _, err := e.store.InsertTrace(testTraceEnvelope(t, strat1, runID, &proposalID), testNow); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	rec := e.do(t, "GET", performancePath(strat1), readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp performanceResponse
	decodeJSON(t, rec, &resp)
	if resp.Model == nil || *resp.Model != "stub" {
		t.Fatalf("model = %v, want \"stub\"", resp.Model)
	}
}

// TestPerformanceRateLimited pins the self-charged 60/min bucket (the
// paper-gate GET precedent): the 61st request on one token is 429.
func TestPerformanceRateLimited(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")

	for i := 0; i < 60; i++ {
		if rec := e.do(t, "GET", performancePath(strat1), readTok, nil); rec.Code != http.StatusOK {
			t.Fatalf("GET #%d: status = %d (body %q)", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, "GET", performancePath(strat1), readTok, nil)
	wantError(t, rec, 429, codeRateLimited)
	wantRetryAfter(t, rec)
}

// TestLeaderboard pins the ranking read: return_pct desc with the
// (realized_pnl desc, strategy_id asc) tie-break, dense 1-based ranks,
// per-row replay stats, model attribution, and tenant scoping — tenant
// principals see their own tenant only; env classes see the platform.
func TestLeaderboard(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createTenant(t, e.store, "tenant-2")
	// strat1 nets +10, strat2 nets -10, uid(3) (tenant-2) stays flat.
	createStrategy(t, e.store, strat1, "paper")
	createStrategy(t, e.store, strat2, "paper")
	createTenantStrategy(t, e.store, uid(3), "tenant-2", "paper")
	seedTrip(t, e, 100, strat1, "1000", "1010", 1)
	seedTrip(t, e, 110, strat2, "1000", "990", 1)
	proposalID, _, runID := insertChain(t, e.store, 10, strat1, 0)
	if _, err := e.store.InsertTrace(testTraceEnvelope(t, strat1, runID, &proposalID), testNow); err != nil {
		t.Fatalf("InsertTrace: %v", err)
	}

	rec := e.do(t, "GET", "/api/v1/arena/leaderboard", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp leaderboardResponse
	decodeJSON(t, rec, &resp)
	if resp.EvaluatedAt != formatTime(testNow) || len(resp.Items) != 3 {
		t.Fatalf("response = %+v, want 3 items at the fixed clock", resp)
	}
	for i, want := range []string{strat1, uid(3), strat2} {
		if it := resp.Items[i]; it.StrategyID != want || it.Rank != i+1 {
			t.Errorf("items[%d] = (%s, rank %d), want (%s, rank %d)", i, it.StrategyID, it.Rank, want, i+1)
		}
	}
	top := resp.Items[0]
	if top.Name != "strategy-"+strat1 || top.TenantID != "tenant-1" || top.LifecycleState != "paper" ||
		top.Seed != "10000" || top.Equity != "10010" || top.RealizedPnL != "10" ||
		top.ReturnPct != "0.1" || top.ClosedTrades != 1 || top.WinRatePct != "100" ||
		top.ProfitFactor != nil || top.MaxDrawdownPct != "0" {
		t.Errorf("top row = %+v", top)
	}
	if top.Model == nil || *top.Model != "stub" {
		t.Errorf("top model = %v, want \"stub\"", top.Model)
	}
	if top.LastFillAt == nil || *top.LastFillAt != formatTime(testNow.Add(2*time.Minute)) {
		t.Errorf("top last_fill_at = %v", top.LastFillAt)
	}
	if resp.Items[2].ReturnPct != "-0.1" || resp.Items[2].Model != nil {
		t.Errorf("bottom row = %+v, want return -0.1 and null model", resp.Items[2])
	}

	// Tenant viewer: own tenant only (§Lists — no foreign rows, ever).
	viewer := seedUserToken(t, e.store, "tenant-1", RoleViewer, "db-viewer")
	rec = e.do(t, "GET", "/api/v1/arena/leaderboard", viewer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var scoped leaderboardResponse
	decodeJSON(t, rec, &scoped)
	if len(scoped.Items) != 2 || scoped.Items[0].StrategyID != strat1 ||
		scoped.Items[1].StrategyID != strat2 || scoped.Items[1].Rank != 2 {
		t.Fatalf("tenant items = %+v, want [strat1, strat2] ranked 1..2", scoped.Items)
	}
}

// TestLeaderboardTieBreak pins the deterministic order on equal returns:
// realized_pnl desc first, then strategy_id ascending.
func TestLeaderboardTieBreak(t *testing.T) {
	e := gatedEnv(t)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat2, "paper") // flat; ids assert the order
	createStrategy(t, e.store, strat1, "paper") // flat

	rec := e.do(t, "GET", "/api/v1/arena/leaderboard", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp leaderboardResponse
	decodeJSON(t, rec, &resp)
	if len(resp.Items) != 2 || resp.Items[0].StrategyID != strat1 || resp.Items[1].StrategyID != strat2 {
		t.Fatalf("tie order = %+v, want strategy_id ascending", resp.Items)
	}
}

// TestLeaderboardEmpty pins the empty-DB shape: items is [] never null.
func TestLeaderboardEmpty(t *testing.T) {
	e := gatedEnv(t)
	rec := e.do(t, "GET", "/api/v1/arena/leaderboard", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("body %q missing \"items\":[]", rec.Body.String())
	}
}
