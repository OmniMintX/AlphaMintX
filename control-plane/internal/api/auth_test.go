package api

import (
	"testing"
	"time"
)

// TestRateLimiterRetryAfter pins the allow() wait: 0 on success; on
// rejection the time to refill to one token — (1 - tokens)/perSec — so
// perSec=1 waits ~1s after burst exhaustion, perSec=0.5 waits ~2s, and a
// half-refilled bucket waits only the remaining half.
func TestRateLimiterRetryAfter(t *testing.T) {
	now := testNow
	rl := newRateLimiter(func() time.Time { return now }, 1, 1)
	if ok, wait := rl.allow("k"); !ok || wait != 0 {
		t.Fatalf("first allow = (%v, %s), want (true, 0)", ok, wait)
	}
	if ok, wait := rl.allow("k"); ok || wait != time.Second {
		t.Fatalf("exhausted at perSec=1: allow = (%v, %s), want (false, 1s)", ok, wait)
	}

	// Half a refill later only half a second remains.
	now = now.Add(500 * time.Millisecond)
	if ok, wait := rl.allow("k"); ok || wait != 500*time.Millisecond {
		t.Fatalf("half-refilled: allow = (%v, %s), want (false, 500ms)", ok, wait)
	}

	now = testNow
	slow := newRateLimiter(func() time.Time { return now }, 1, 0.5)
	slow.allow("k")
	if ok, wait := slow.allow("k"); ok || wait != 2*time.Second {
		t.Fatalf("exhausted at perSec=0.5: allow = (%v, %s), want (false, 2s)", ok, wait)
	}
}
