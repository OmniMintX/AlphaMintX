package backtest

import (
	"testing"
	"time"
)

func TestSubTicksPinnedOrderAndTimestamps(t *testing.T) {
	tests := []struct {
		name       string
		o, h, l, c string
		wantLegs   [4]string
		wantMarks  [4]string
	}{
		{"bullish", "100", "110", "95", "105",
			[4]string{"open", "low", "high", "close"}, [4]string{"100", "95", "110", "105"}},
		{"doji close == open is bullish", "100", "110", "95", "100",
			[4]string{"open", "low", "high", "close"}, [4]string{"100", "95", "110", "100"}},
		{"bearish", "105", "110", "95", "100",
			[4]string{"open", "high", "low", "close"}, [4]string{"105", "110", "95", "100"}},
	}
	const ivlMS = int64(60_000)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subs, err := SubTicks("BTC/USDT", kl(testT0, tc.o, tc.h, tc.l, tc.c), ivlMS)
			if err != nil {
				t.Fatalf("SubTicks: %v", err)
			}
			wantTS := [4]time.Time{
				time.UnixMilli(testT0).UTC(),
				time.UnixMilli(testT0 + 15_000).UTC(),
				time.UnixMilli(testT0 + 30_000).UTC(),
				time.UnixMilli(testT0 + 60_000).UTC(),
			}
			for i, sub := range subs {
				if sub.Leg != tc.wantLegs[i] {
					t.Errorf("leg[%d] = %s, want %s", i, sub.Leg, tc.wantLegs[i])
				}
				if sub.Tick.Mark.String() != tc.wantMarks[i] {
					t.Errorf("mark[%d] = %s, want %s", i, sub.Tick.Mark, tc.wantMarks[i])
				}
				if !sub.Tick.TS.Equal(wantTS[i]) {
					t.Errorf("ts[%d] = %s, want %s", i, sub.Tick.TS, wantTS[i])
				}
				if sub.Tick.Symbol != "BTC/USDT" {
					t.Errorf("symbol[%d] = %s", i, sub.Tick.Symbol)
				}
			}
		})
	}
}

func TestSubTicksRejectsBadOHLC(t *testing.T) {
	if _, err := SubTicks("BTC/USDT", kl(testT0, "100", "99", "98", "100"), 60_000); err == nil {
		t.Fatal("high below open accepted, want error")
	}
	if _, err := SubTicks("BTC/USDT", kl(testT0, "100", "101", "100.5", "101"), 60_000); err == nil {
		t.Fatal("low above open accepted, want error")
	}
	bad := kl(testT0, "x", "1", "1", "1")
	if _, err := SubTicks("BTC/USDT", bad, 60_000); err == nil {
		t.Fatal("non-decimal open accepted, want error")
	}
}
