package safety

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var testNow = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// fakeStore is an in-memory Store fake; the mutex keeps it race-safe under
// the Run/Poke tests. events doubles as the unserved set (nothing marks
// rows served here).
type fakeStore struct {
	mu         sync.Mutex
	strategies []store.Strategy
	positions  map[string][]store.Position
	orders     []store.LiveOrder
	events     []store.KillBreakerEvent
	alerts     []store.SafetyAlert
	// Error injection for the watchdog fail-safe branch tests
	// (watchdog.md WD-17/WD-20): each, when set, fails the matching call.
	ordersErr     error
	alertErr      error
	alertTodayErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{positions: map[string][]store.Position{}}
}

func (f *fakeStore) ListStrategies(page, limit int) ([]store.Strategy, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := (page - 1) * limit
	if start >= len(f.strategies) {
		return nil, len(f.strategies), nil
	}
	end := start + limit
	if end > len(f.strategies) {
		end = len(f.strategies)
	}
	return append([]store.Strategy(nil), f.strategies[start:end]...), len(f.strategies), nil
}

func (f *fakeStore) ListPositions(strategyID string) ([]store.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Position(nil), f.positions[strategyID]...), nil
}

func (f *fakeStore) ListNonTerminalLiveOrders() ([]store.LiveOrder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ordersErr != nil {
		return nil, f.ordersErr
	}
	return append([]store.LiveOrder(nil), f.orders...), nil
}

func (f *fakeStore) BreakerActiveToday(strategyID, utcDate string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.Kind == "breaker" && e.StrategyID != nil && *e.StrategyID == strategyID &&
			strings.HasPrefix(e.RecordedAt, utcDate) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) AppendKillBreakerEvent(e store.KillBreakerEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) AppendSafetyAlert(a store.SafetyAlert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alertErr != nil {
		return f.alertErr
	}
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *fakeStore) HasSafetyAlertToday(kind, strategyID, refID, utcDate string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.alertTodayErr != nil {
		return false, f.alertTodayErr
	}
	for _, a := range f.alerts {
		if a.Kind == kind && strings.HasPrefix(a.RecordedAt, utcDate) &&
			matchNullable(a.StrategyID, strategyID) && matchNullable(a.RefID, refID) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) ListUnservedSafetyEvents() ([]store.KillBreakerEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.KillBreakerEvent(nil), f.events...), nil
}

func (f *fakeStore) GlobalMaxKillEpoch(strategyID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var max int64
	for _, e := range f.events {
		if e.Kind != "kill" || e.KillEpoch == nil {
			continue
		}
		global := e.StrategyID == nil && e.TenantID == nil
		if (global || (e.StrategyID != nil && *e.StrategyID == strategyID)) && *e.KillEpoch > max {
			max = *e.KillEpoch
		}
	}
	return max, nil
}

func (f *fakeStore) AppendStrategyKill(eventID, strategyID, actorID, recordedAt string, flatten bool) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var epoch int64
	for _, e := range f.events {
		if e.KillEpoch != nil && *e.KillEpoch > epoch {
			epoch = *e.KillEpoch
		}
	}
	epoch++
	sid, tid := strategyID, "tenant-1"
	f.events = append(f.events, store.KillBreakerEvent{
		EventID: eventID, Kind: "kill", Scope: "strategy",
		StrategyID: &sid, TenantID: &tid, KillEpoch: &epoch,
		Flatten: &flatten, ActorID: actorID, RecordedAt: recordedAt,
	})
	return epoch, nil
}

func (f *fakeStore) HasSafetyAlert(kind, strategyID, refID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.alerts {
		if a.Kind == kind && matchNullable(a.StrategyID, strategyID) && matchNullable(a.RefID, refID) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) LatestStrategyKillEvent(strategyID string) (string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		e := f.events[i]
		if e.Kind == "kill" && e.Scope == "strategy" &&
			e.StrategyID != nil && *e.StrategyID == strategyID {
			return e.EventID, e.ActorID, true, nil
		}
	}
	return "", "", false, nil
}

// matchNullable is the store's empty-matches-NULL dedupe rule.
func matchNullable(v *string, arg string) bool {
	if arg == "" {
		return v == nil
	}
	return v != nil && *v == arg
}

// breakerEvents returns the appended kind='breaker' rows.
func (f *fakeStore) breakerEvents() []store.KillBreakerEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.KillBreakerEvent
	for _, e := range f.events {
		if e.Kind == "breaker" {
			out = append(out, e)
		}
	}
	return out
}

// killEvents returns the appended kind='kill' rows.
func (f *fakeStore) killEvents() []store.KillBreakerEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.KillBreakerEvent
	for _, e := range f.events {
		if e.Kind == "kill" {
			out = append(out, e)
		}
	}
	return out
}

// alertsOf returns the alerts of one kind matching refID (""
// matches NULL and any).
func (f *fakeStore) alertsOf(kind, refID string) []store.SafetyAlert {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.SafetyAlert
	for _, a := range f.alerts {
		if a.Kind != kind {
			continue
		}
		if refID != "" && (a.RefID == nil || *a.RefID != refID) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// fakePnL yields per-strategy daily PnL; calls (when non-nil) signals every
// DailyPnL invocation for the Poke test.
type fakePnL struct {
	mu    sync.Mutex
	pnl   map[string]decimal.Decimal
	err   error
	calls chan struct{}
}

func (f *fakePnL) DailyPnL(strategyID string, _ time.Time) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != nil {
		select {
		case f.calls <- struct{}{}:
		default:
		}
	}
	if f.err != nil {
		return decimal.Zero, f.err
	}
	return f.pnl[strategyID], nil
}

func (f *fakePnL) set(strategyID, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pnl[strategyID] = decimal.RequireFromString(v)
}

// fakeLimits yields per-strategy daily-loss limits; panics=true makes every
// Limits call panic (the tick-panic drill).
type fakeLimits struct {
	mu     sync.Mutex
	limits map[string]decimal.Decimal
	panics bool
}

func (f *fakeLimits) Limits(strategyID string) riskgate.RiskLimits {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panics {
		panic("limits provider exploded")
	}
	return riskgate.RiskLimits{DailyLossLimitQuote: f.limits[strategyID]}
}

// fakeMarks marks every symbol fresh at 100 unless flagged stale.
type fakeMarks struct {
	mu    sync.Mutex
	stale map[string]bool
}

func (f *fakeMarks) Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale[symbol] {
		return decimal.Zero, time.Time{}, false
	}
	return decimal.NewFromInt(100), now, true
}

// fakeDriver records DriveSafetyEffects invocations and signals each one.
type fakeDriver struct {
	mu    sync.Mutex
	calls int
	ch    chan struct{}
}

func newFakeDriver() *fakeDriver { return &fakeDriver{ch: make(chan struct{}, 16)} }

func (f *fakeDriver) DriveSafetyEffects(context.Context) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	f.ch <- struct{}{}
	return nil
}

func (f *fakeDriver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// waitDrive blocks until one DriveSafetyEffects call lands (fire is async).
func (f *fakeDriver) waitDrive(t *testing.T) {
	t.Helper()
	select {
	case <-f.ch:
	case <-time.After(2 * time.Second):
		t.Fatal("DriveSafetyEffects was not invoked")
	}
}

type fakeRecon struct {
	mu sync.Mutex
	ok bool
}

func (f *fakeRecon) Reconciled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ok
}

func (f *fakeRecon) set(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ok = v
}

// fakeEntries records the watchdog's CancelOpenEntries sweeps; err (when
// set) is returned on every call.
type fakeEntries struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeEntries) CancelOpenEntries(_ context.Context, strategyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strategyID)
	return f.err
}

func (f *fakeEntries) sweeps(strategyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.calls {
		if id == strategyID {
			n++
		}
	}
	return n
}

// fakeFilters yields fixed venue minimums; ok=false simulates the
// ErrFilterUnavailable condition (WD-20 exclusion).
type fakeFilters struct {
	mu                  sync.Mutex
	minQty, minNotional decimal.Decimal
	miss                bool
}

func (f *fakeFilters) MinFilters(string) (decimal.Decimal, decimal.Decimal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.miss {
		return decimal.Decimal{}, decimal.Decimal{}, false
	}
	return f.minQty, f.minNotional, true
}

// harness wires a Monitor over the fakes with an injected clock that
// starts at testNow and only moves when a test calls advance.
type harness struct {
	t       *testing.T
	st      *fakeStore
	pnl     *fakePnL
	limits  *fakeLimits
	marks   *fakeMarks
	driver  *fakeDriver
	recon   *fakeRecon
	entries *fakeEntries
	filters *fakeFilters
	clockMu sync.Mutex
	clock   time.Time
	m       *Monitor
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		st:      newFakeStore(),
		pnl:     &fakePnL{pnl: map[string]decimal.Decimal{}},
		limits:  &fakeLimits{limits: map[string]decimal.Decimal{}},
		marks:   &fakeMarks{stale: map[string]bool{}},
		driver:  newFakeDriver(),
		recon:   &fakeRecon{ok: true},
		entries: &fakeEntries{},
		filters: &fakeFilters{minQty: decimal.RequireFromString("0.00001"), minNotional: decimal.NewFromInt(5)},
		clock:   testNow,
	}
	m, err := New(Config{
		Store: h.st, PnL: h.pnl, Limits: h.limits, Marks: h.marks,
		Driver: h.driver, Recon: h.recon,
		Entries: h.entries, Filters: h.filters,
		Now: func() time.Time {
			h.clockMu.Lock()
			defer h.clockMu.Unlock()
			return h.clock
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.m = m
	return h
}

// advance moves the injected clock forward (the watchdog silence tests).
func (h *harness) advance(d time.Duration) {
	h.clockMu.Lock()
	h.clock = h.clock.Add(d)
	h.clockMu.Unlock()
}

// addStrategy registers one strategy; posQty != "" opens a BTC/USDT
// position and limit != "" sets daily_loss_limit_quote.
func (h *harness) addStrategy(id, state, limit, posQty string) {
	h.st.mu.Lock()
	h.st.strategies = append(h.st.strategies, store.Strategy{
		StrategyID: id, TenantID: "tenant-1", Name: id, LifecycleState: state,
	})
	if posQty != "" {
		h.st.positions[id] = []store.Position{{StrategyID: id, Symbol: "BTC/USDT", QtyBase: posQty}}
	}
	h.st.mu.Unlock()
	if limit != "" {
		h.limits.mu.Lock()
		h.limits.limits[id] = decimal.RequireFromString(limit)
		h.limits.mu.Unlock()
	}
}

// tick runs one panic-isolated tick and returns the selected cadence.
func (h *harness) tick() time.Duration {
	h.t.Helper()
	return h.m.safeTick(context.Background())
}
