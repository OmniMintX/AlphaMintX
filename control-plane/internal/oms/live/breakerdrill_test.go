package live

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/safety"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// countingSafetyStore wraps the real store as the monitor's safety.Store,
// counting the per-tick latch reads and the breaker appends so the drill
// observes tick completion and the once-per-day dedupe deterministically.
type countingSafetyStore struct {
	*store.Store
	latchReads atomic.Int64
	appends    atomic.Int64
}

func (s *countingSafetyStore) BreakerActiveToday(strategyID, utcDate string) (bool, error) {
	s.latchReads.Add(1)
	return s.Store.BreakerActiveToday(strategyID, utcDate)
}

func (s *countingSafetyStore) AppendKillBreakerEvent(ev store.KillBreakerEvent) error {
	s.appends.Add(1)
	return s.Store.AppendKillBreakerEvent(ev)
}

// stubPnL injects the folded daily figure the monitor evaluates.
type stubPnL struct{ pnl decimal.Decimal }

func (p stubPnL) DailyPnL(string, time.Time) (decimal.Decimal, error) { return p.pnl, nil }

// stubLimits injects daily_loss_limit_quote for every strategy.
type stubLimits struct{ limit decimal.Decimal }

func (l stubLimits) Limits(string) riskgate.RiskLimits {
	return riskgate.RiskLimits{DailyLossLimitQuote: l.limit}
}

// waitFor polls cond to a deadline (the monitor fires and drives async).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// BD1: a monitor tick with loss >= limit fires EXACTLY once — the second
// tick dedupes on the BreakerActiveToday latch — and the effects run in
// order: entry cancels, the reduce-only market flatten (origin breaker),
// and stops-after-flatten only AFTER the covering fill. Same-day ENTRY
// submissions halt with ErrBreakerActive while 'close' is still allowed.
func TestBreakerDrill_FiresOnceWithEffects(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, withStops(t, "60000", "")); err != nil { // token 1
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.venue.SetBalance("BTC", "0.01562", "0")
	e.reconcile()                             // books the fill; the drive places the SL (token 2)
	if err := e.submitEntry(20); err != nil { // resting entry (token 3)
		t.Fatalf("SubmitApproved: %v", err)
	}

	cst := &countingSafetyStore{Store: e.st}
	m, err := safety.New(safety.Config{
		Store: cst, PnL: stubPnL{decimal.NewFromInt(-600)},
		Limits: stubLimits{decimal.NewFromInt(500)},
		Marks:  e.marks, Driver: e.oms, Recon: e.oms,
		WatchdogDisabled: true, // the breaker drill isolates the breaker
		ActiveInterval:   time.Hour, IdleInterval: time.Hour,
		Now:  func() time.Time { return e.now },
		Logf: func(string, ...any) {}, // never log after the test ends
	})
	if err != nil {
		t.Fatalf("safety.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	// The startup tick fires the breaker row and drives effects async;
	// the flatten (token 4) journaled AND resting at the venue marks the
	// drive's destructive work complete (journal-before-send).
	waitFor(t, "the breaker row", func() bool { return cst.appends.Load() == 1 })
	waitFor(t, "the drive's flatten", func() bool {
		if _, err := e.st.GetLiveOrderByClientOrderID(idN(4, 0)); err != nil {
			return false
		}
		for _, vo := range e.venueOpen() {
			if vo.Type == "MARKET" {
				return true
			}
		}
		return false
	})
	// Tick #2 (Poke): the BreakerActiveToday latch dedupes — the tick
	// reads the latch and appends NO second row.
	m.Poke(uid(1))
	waitFor(t, "the second tick", func() bool { return cst.latchReads.Load() >= 2 })
	cancel()
	<-done
	if got := cst.appends.Load(); got != 1 {
		t.Fatalf("breaker rows appended = %d, want 1 (the second tick dedupes)", got)
	}

	// Effects in order: the resting entry canceled; the reduce-only market
	// flatten (origin breaker) resting; the SL PRESERVED until the
	// covering fill (stops-after-flatten owns its cancel).
	if ord := e.order(idN(3, 0)); ord.Status != "canceled" {
		t.Errorf("resting entry status = %s, want canceled", ord.Status)
	}
	if fl := e.order(idN(4, 0)); fl.Origin != "breaker" || !fl.ReduceOnly || fl.Type != "market" {
		t.Errorf("flatten order = %+v, want reduce-only market origin breaker", fl.Order)
	}
	types := map[string]int{}
	for _, vo := range e.venueOpen() {
		types[vo.Type]++
	}
	if types["STOP_LOSS"] != 1 || types["MARKET"] != 1 || len(types) != 2 {
		t.Fatalf("venue open types = %+v, want the preserved SL plus the resting flatten", types)
	}
	// Same-day ENTRY halt.
	if err := e.submitEntry(30); !errors.Is(err, ErrBreakerActive) {
		t.Errorf("entry err = %v, want ErrBreakerActive", err)
	}

	if err := e.venue.Fill(idN(4, 0), "0.01562", "64000"); err != nil {
		t.Fatalf("Fill flatten: %v", err)
	}
	e.now = e.now.Add(time.Second) // serve needs a reconcile STRICTLY after recorded_at
	e.reconcile()                  // books flat; stops-after-flatten cancels the SL; served
	if got := e.venueOpen(); len(got) != 0 {
		t.Errorf("venue open orders = %d, want 0 (SL canceled AFTER the covering fill)", len(got))
	}
	if got := e.unserved(); len(got) != 0 {
		t.Errorf("unserved events = %d, want 0", len(got))
	}
	// 'close' is still allowed under the halt: the ActionClose path
	// carries no breaker check — it reaches the flatten logic (which
	// reports the already-flat book), never ErrBreakerActive.
	err = e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "proposal", nil)
	if err == nil || errors.Is(err, ErrBreakerActive) || !strings.Contains(err.Error(), "no open position") {
		t.Errorf("close under the halt err = %v, want the flat-book error, never ErrBreakerActive", err)
	}
}
