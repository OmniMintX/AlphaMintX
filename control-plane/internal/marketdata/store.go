package marketdata

import (
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// futureSkewTolerance is the normative lower bound of the staleness window:
// a tick timestamped more than 5 s in the future is NOT usable.
const futureSkewTolerance = 5 * time.Second

// Store is the last-tick cache consumed at proposal evaluation. It retains
// only the latest tick per symbol; a writer goroutine drains a Feed channel
// into it via Put. The Store never fabricates or extrapolates prices.
type Store struct {
	mu     sync.Mutex
	maxAge time.Duration
	ticks  map[string]Tick
}

// NewStore builds a Store with the REQUIRED max_age_seconds freshness bound.
// There is no default: a non-positive maxAge is a configuration error.
func NewStore(maxAge time.Duration) (*Store, error) {
	if maxAge <= 0 {
		return nil, fmt.Errorf("marketdata: max_age_seconds must be > 0, got %s", maxAge)
	}
	return &Store{maxAge: maxAge, ticks: make(map[string]Tick)}, nil
}

// Put records the latest tick for its symbol, replacing any previous one.
func (s *Store) Put(t Tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ticks[t.Symbol] = t
}

// Mark returns the cached mark for symbol iff it is usable at now:
// −5s ≤ now−ts ≤ max_age, both bounds inclusive (the −5s is the future-skew
// tolerance). A stale or future-skewed mark returns (decimal.Zero, ts, false):
// the price never leaks, while the real ts lets the call site record the
// stale mark's age in the verdict limits_snapshot. An unknown symbol returns
// (decimal.Zero, time.Time{}, false).
func (s *Store) Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool) {
	s.mu.Lock()
	t, ok := s.ticks[symbol]
	s.mu.Unlock()
	if !ok {
		return decimal.Zero, time.Time{}, false
	}
	age := now.Sub(t.TS)
	if age < -futureSkewTolerance || age > s.maxAge {
		return decimal.Zero, t.TS, false
	}
	return t.Mark, t.TS, true
}
