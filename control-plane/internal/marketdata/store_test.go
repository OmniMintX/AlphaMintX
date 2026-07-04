package marketdata

import (
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestNewStoreRejectsNonPositiveMaxAge(t *testing.T) {
	for _, maxAge := range []time.Duration{0, -time.Second} {
		if _, err := NewStore(maxAge); err == nil {
			t.Fatalf("NewStore(%s) = nil error, want rejection", maxAge)
		}
	}
}

func TestStoreMarkFreshness(t *testing.T) {
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	maxAge := 10 * time.Second
	tests := []struct {
		name string
		age  time.Duration // now - ts
		ok   bool
	}{
		{"fresh", 0, true},
		{"mid window", 3 * time.Second, true},
		{"exactly max_age", 10 * time.Second, true},
		{"just past max_age", 10*time.Second + time.Nanosecond, false},
		{"future within skew", -3 * time.Second, true},
		{"exactly -5s skew bound", -5 * time.Second, true},
		{"future past skew", -5*time.Second - time.Nanosecond, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewStore(maxAge)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			mark := decimal.RequireFromString("64180.1")
			ts := now.Add(-tc.age)
			store.Put(Tick{Symbol: "BTC/USDT", Mark: mark, Last: mark, TS: ts})
			price, gotTS, ok := store.Mark("BTC/USDT", now)
			if ok != tc.ok {
				t.Fatalf("Mark ok = %v, want %v (age %s)", ok, tc.ok, tc.age)
			}
			if !gotTS.Equal(ts) {
				t.Fatalf("Mark ts = %s, want %s", gotTS, ts)
			}
			if tc.ok && !price.Equal(mark) {
				t.Fatalf("Mark price = %s, want %s", price, mark)
			}
			if !tc.ok && !price.IsZero() {
				t.Fatalf("stale mark leaked price %s, want zero", price)
			}
		})
	}
}

func TestStoreMarkUnknownSymbol(t *testing.T) {
	store, err := NewStore(10 * time.Second)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	price, ts, ok := store.Mark("ETH/USDT", time.Now())
	if ok {
		t.Fatal("Mark ok = true for unknown symbol, want false")
	}
	if !price.IsZero() || !ts.IsZero() {
		t.Fatalf("unknown symbol = (%s, %s), want zero values", price, ts)
	}
}

func TestStoreLatestTickWins(t *testing.T) {
	store, err := NewStore(10 * time.Second)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	store.Put(Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("100"), TS: now.Add(-2 * time.Second)})
	store.Put(Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("101"), TS: now.Add(-time.Second)})
	price, _, ok := store.Mark("BTC/USDT", now)
	if !ok || price.String() != "101" {
		t.Fatalf("Mark = (%s, %v), want (101, true)", price, ok)
	}
}

func TestStoreConcurrent(t *testing.T) {
	store, err := NewStore(10 * time.Second)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				store.Put(Tick{
					Symbol: "BTC/USDT",
					Mark:   decimal.NewFromInt(int64(g*1000 + i)),
					TS:     now,
				})
			}
		}(g)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				store.Mark("BTC/USDT", now)
			}
		}()
	}
	wg.Wait()
	if _, _, ok := store.Mark("BTC/USDT", now); !ok {
		t.Fatal("Mark ok = false after concurrent writes, want true")
	}
}
