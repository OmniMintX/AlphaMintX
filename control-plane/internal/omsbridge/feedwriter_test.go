package omsbridge

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
)

// TestRunFeedWriterStoresMarksAndFiresSweep: every tick drained from the
// Feed lands in the last-tick Store AND fires the trigger sweep with that
// tick's mark (market-data.md: every Store write runs the trigger checks).
// The ReplayFeed doubles as the offline fake feed.
func TestRunFeedWriterStoresMarksAndFiresSweep(t *testing.T) {
	series := map[string][]decimal.Decimal{
		"BTC/USDT": {decimal.RequireFromString("64000"), decimal.RequireFromString("63000")},
	}
	feed, err := marketdata.NewReplayFeed(series, testNow, 1, 2)
	if err != nil {
		t.Fatalf("NewReplayFeed: %v", err)
	}
	marks := newMarks(t)

	var sweeps []map[string]decimal.Decimal
	sweep := func(m map[string]decimal.Decimal) error {
		sweeps = append(sweeps, m)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RunFeedWriter(ctx, feed, []string{"BTC/USDT"}, marks, sweep, t.Logf); err != nil {
		t.Fatalf("RunFeedWriter: %v", err)
	}

	if len(sweeps) != 2 {
		t.Fatalf("sweep calls = %d, want 2 (one per Store write)", len(sweeps))
	}
	if !sweeps[0]["BTC/USDT"].Equal(decimal.RequireFromString("64000")) ||
		!sweeps[1]["BTC/USDT"].Equal(decimal.RequireFromString("63000")) {
		t.Errorf("sweep marks = %v, want [64000, 63000]", sweeps)
	}

	// The Store holds the LATEST tick, fresh at the second tick's time.
	now := testNow.Add(time.Second)
	mark, ts, ok := marks.Mark("BTC/USDT", now)
	if !ok || !mark.Equal(decimal.RequireFromString("63000")) || !ts.Equal(now) {
		t.Errorf("Mark = (%s, %s, %v), want (63000, %s, true)", mark, ts, ok, now)
	}
}
