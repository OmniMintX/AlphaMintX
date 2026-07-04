package marketdata

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

// ReplayFeed emits marks from an injected series under an index-based clock:
// one tick per subscribed symbol per index, TS = clockStart + index ×
// tickSeconds. It never reads the wall clock, sleeps, or opens a network
// connection, keeping e2e runs byte-deterministic and offline.
type ReplayFeed struct {
	marks       map[string][]decimal.Decimal
	clockStart  time.Time
	tickSeconds int
	ticks       int
}

var _ Feed = (*ReplayFeed)(nil)

// NewReplayFeed builds a replay feed over the runspec marks series. ticks is
// the number of indices to emit; tickSeconds is the runspec tick interval.
func NewReplayFeed(marks map[string][]decimal.Decimal, clockStart time.Time, tickSeconds, ticks int) (*ReplayFeed, error) {
	if tickSeconds <= 0 {
		return nil, fmt.Errorf("marketdata: tick_seconds must be > 0, got %d", tickSeconds)
	}
	if ticks < 0 {
		return nil, fmt.Errorf("marketdata: ticks must be >= 0, got %d", ticks)
	}
	return &ReplayFeed{marks: marks, clockStart: clockStart, tickSeconds: tickSeconds, ticks: ticks}, nil
}

// Subscribe streams ticks index by index, symbols in lexicographic order
// within each index. Exhausted series repeat their last element (matching
// the e2e markAt fallback); unknown symbols yield no tick, so their mark
// stays zero and market entries reject MARK_PRICE_UNAVAILABLE.
func (f *ReplayFeed) Subscribe(ctx context.Context, symbols []string) (<-chan Tick, error) {
	if len(symbols) == 0 {
		return nil, fmt.Errorf("marketdata: no symbols to subscribe")
	}
	sorted := slices.Clone(symbols)
	slices.Sort(sorted)
	ch := make(chan Tick)
	go func() {
		defer close(ch)
		for i := 0; i < f.ticks; i++ {
			ts := f.clockStart.Add(time.Duration(i*f.tickSeconds) * time.Second)
			for _, sym := range sorted {
				series := f.marks[sym]
				if len(series) == 0 {
					continue
				}
				idx := min(i, len(series)-1)
				mark := series[idx]
				select {
				case ch <- Tick{Symbol: sym, Mark: mark, Last: mark, TS: ts}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}
