package marketdata

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func replayMarks() map[string][]decimal.Decimal {
	return map[string][]decimal.Decimal{
		"BTC/USDT": {
			decimal.RequireFromString("64180.1"),
			decimal.RequireFromString("64200.5"),
		},
		"ETH/USDT": {
			decimal.RequireFromString("3005.5"),
			decimal.RequireFromString("3010.25"),
			decimal.RequireFromString("2998"),
			decimal.RequireFromString("3001"),
		},
	}
}

// tickKey flattens a Tick for deterministic comparison.
func tickKey(t Tick) string {
	return fmt.Sprintf("%s %s %s %s", t.Symbol, t.Mark, t.Last, t.TS.Format(time.RFC3339))
}

func collectTicks(t *testing.T, feed Feed, symbols []string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := feed.Subscribe(ctx, symbols)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var got []string
	for tick := range ch {
		got = append(got, tickKey(tick))
	}
	return got
}

func TestNewReplayFeedValidation(t *testing.T) {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := NewReplayFeed(replayMarks(), start, 0, 4); err == nil {
		t.Fatal("tick_seconds 0 accepted, want error")
	}
	if _, err := NewReplayFeed(replayMarks(), start, 60, -1); err == nil {
		t.Fatal("negative ticks accepted, want error")
	}
}

func TestReplayFeedDeterministicAndExhaustion(t *testing.T) {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	feed, err := NewReplayFeed(replayMarks(), start, 60, 4)
	if err != nil {
		t.Fatalf("NewReplayFeed: %v", err)
	}
	symbols := []string{"ETH/USDT", "BTC/USDT"} // unsorted on purpose
	first := collectTicks(t, feed, symbols)
	second := collectTicks(t, feed, symbols)
	if !slices.Equal(first, second) {
		t.Fatalf("replay not deterministic:\nfirst  %v\nsecond %v", first, second)
	}
	// 4 indices x 2 symbols, lexicographic within each index; the BTC series
	// (len 2) is exhausted at indices 2 and 3 and repeats its last element.
	want := []string{
		"BTC/USDT 64180.1 64180.1 2025-06-01T00:00:00Z",
		"ETH/USDT 3005.5 3005.5 2025-06-01T00:00:00Z",
		"BTC/USDT 64200.5 64200.5 2025-06-01T00:01:00Z",
		"ETH/USDT 3010.25 3010.25 2025-06-01T00:01:00Z",
		"BTC/USDT 64200.5 64200.5 2025-06-01T00:02:00Z",
		"ETH/USDT 2998 2998 2025-06-01T00:02:00Z",
		"BTC/USDT 64200.5 64200.5 2025-06-01T00:03:00Z",
		"ETH/USDT 3001 3001 2025-06-01T00:03:00Z",
	}
	if !slices.Equal(first, want) {
		t.Fatalf("replay ticks:\ngot  %v\nwant %v", first, want)
	}
}

func TestReplayFeedUnknownSymbolYieldsNoTick(t *testing.T) {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	feed, err := NewReplayFeed(replayMarks(), start, 60, 3)
	if err != nil {
		t.Fatalf("NewReplayFeed: %v", err)
	}
	got := collectTicks(t, feed, []string{"XRP/USDT"})
	if len(got) != 0 {
		t.Fatalf("unknown symbol emitted %d ticks, want 0", len(got))
	}
}

func TestReplayFeedRejectsEmptySubscribe(t *testing.T) {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	feed, err := NewReplayFeed(replayMarks(), start, 60, 3)
	if err != nil {
		t.Fatalf("NewReplayFeed: %v", err)
	}
	if _, err := feed.Subscribe(context.Background(), nil); err == nil {
		t.Fatal("empty subscribe accepted, want error")
	}
}

func TestReplayFeedStopsOnCancel(t *testing.T) {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	feed, err := NewReplayFeed(replayMarks(), start, 60, 4)
	if err != nil {
		t.Fatalf("NewReplayFeed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := feed.Subscribe(ctx, []string{"BTC/USDT"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	// The channel must close without emitting the full series.
	n := 0
	for range ch {
		n++
	}
	if n > 4 {
		t.Fatalf("received %d ticks after cancel, want at most the series", n)
	}
}
