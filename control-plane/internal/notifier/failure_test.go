package notifier

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestPoisonRowSkip pins AN-4a: the SAME row failing 4xx on 12
// consecutive ticks is skipped with the mandatory ALERT DISPATCH SKIPPED
// line and the next row delivers in the same pass; the next row starts
// its own count.
func TestPoisonRowSkip(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	h.alert(40, "poison", "{}")
	h.alert(41, "healthy", "{}")
	r.setStatus(func(c capture) int {
		if c.ID == uid(40) {
			return http.StatusRequestEntityTooLarge // 413: deterministic 4xx
		}
		return http.StatusOK
	})
	for i := 0; i < 11; i++ {
		e.runPass(context.Background())
		if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
			t.Fatalf("tick %d: watermark = %d, want 0 (not yet skipped)", i+1, got)
		}
	}
	if h.logs.count("ALERT DISPATCH SKIPPED") != 0 {
		t.Fatal("skip line before the 12th consecutive 4xx")
	}
	e.runPass(context.Background()) // 12th attempt: skip + deliver row 2
	if h.logs.count("ALERT DISPATCH SKIPPED source=safety_alerts id="+uid(40)+" seq=1 status=413") != 1 {
		t.Fatalf("no SKIPPED line after the 12th 4xx; logs:\n%s", h.logs.String())
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 2 {
		t.Fatalf("watermark after skip = %d, want 2 (past the poison row AND row 2 delivered)", got)
	}
	acc := r.accepted()
	if len(acc) != 1 || acc[0].ID != uid(41) {
		t.Fatalf("accepted = %+v, want the healthy row exactly once", acc)
	}
}

// TestTransientNeverSkips pins the AN-4a split: a 5xx (or any transient
// class) row is NEVER skipped regardless of count.
func TestTransientNeverSkips(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	h.alert(40, "transient", "{}")
	r.setStatus(func(capture) int { return http.StatusInternalServerError })
	for i := 0; i < 24; i++ {
		e.runPass(context.Background())
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
		t.Fatalf("watermark = %d after 24 failed 5xx ticks, want 0 (retry forever)", got)
	}
	if h.logs.count("ALERT DISPATCH SKIPPED") != 0 {
		t.Fatalf("transient failure skipped; logs:\n%s", h.logs.String())
	}
}

// Test2xxOnlySuccess pins AN-16: 200/204 advance the watermark; 302, 400,
// and 500 do not (and log their status class).
func Test2xxOnlySuccess(t *testing.T) {
	cases := []struct {
		status  int
		advance bool
	}{
		{200, true}, {204, true}, {302, false}, {400, false}, {500, false},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			h := newHarness(t)
			r := newReceiver(t)
			e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
			h.alert(40, "status_case", "{}")
			r.setStatus(func(capture) int { return tc.status })
			e.runPass(context.Background())
			want := int64(0)
			if tc.advance {
				want = 1
			}
			if got := h.watermark(store.AlertSourceSafetyAlert); got != want {
				t.Fatalf("status %d: watermark = %d, want %d", tc.status, got, want)
			}
			if !tc.advance {
				want := fmt.Sprintf("alert dispatch failed source=safety_alerts class=status:%d", tc.status)
				if h.logs.count(want) != 1 {
					t.Fatalf("status %d: no failure line %q; logs:\n%s", tc.status, want, h.logs.String())
				}
			}
		})
	}
	// Timeout: transport never completes ⇒ failure, watermark holds.
	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t)
		block := make(chan struct{})
		defer close(block)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			<-block
		}))
		t.Cleanup(srv.Close)
		e, _ := h.seeded(Config{URL: srv.URL, Heartbeat: 0, Timeout: 50 * time.Millisecond})
		h.alert(40, "timeout_case", "{}")
		e.runPass(context.Background())
		if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
			t.Fatalf("watermark after timeout = %d, want 0", got)
		}
		if h.logs.count("alert dispatch failed source=safety_alerts class=timeout") != 1 {
			t.Fatalf("no timeout-class failure line; logs:\n%s", h.logs.String())
		}
	})
}

// TestWireTruncation pins the AN-13 8 KiB bound on details_json,
// trigger_ref, and reason: truncated on the wire with the marker suffix,
// DB rows unchanged.
func TestWireTruncation(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})

	big := strings.Repeat("x", truncateBytes+100)
	trigger := big
	if err := h.st.AppendKillBreakerEvent(store.KillBreakerEvent{
		EventID: uid(20), Kind: "breaker", Scope: "strategy", TriggerRef: &trigger,
		ActorID: "monitor", RecordedAt: formatTime(testNow),
	}); err != nil {
		t.Fatalf("AppendKillBreakerEvent: %v", err)
	}
	killEpoch := h.platformKill(21)
	if _, _, err := h.st.AppendKillClearPlatform(uid(30), "op-1", big, killEpoch, formatTime(testNow)); err != nil {
		t.Fatalf("AppendKillClearPlatform: %v", err)
	}
	h.alert(40, "oversized", big)

	e.runPass(context.Background())

	wantWire := big[:truncateBytes] + truncateSuffix
	seen := map[string]bool{}
	for _, c := range r.accepted() {
		var field string
		switch c.ID {
		case uid(20):
			field = "trigger_ref"
		case uid(30):
			field = "reason"
		case uid(40):
			field = "details_json"
		default:
			continue
		}
		seen[field] = true
		got, _ := c.Event[field].(string)
		if got != wantWire {
			t.Errorf("%s wire length = %d, want %d (8 KiB + suffix)", field, len(got), len(wantWire))
		}
	}
	for _, field := range []string{"trigger_ref", "reason", "details_json"} {
		if !seen[field] {
			t.Errorf("no delivery carried the oversized %s row", field)
		}
	}
	// DB rows stay complete.
	alerts, err := h.st.ListSafetyAlertsAfter(0, 100)
	if err != nil {
		t.Fatalf("ListSafetyAlertsAfter: %v", err)
	}
	found := false
	for _, a := range alerts {
		if a.AlertID == uid(40) {
			found = true
			if a.DetailsJSON != big {
				t.Errorf("DB details_json length = %d, want untouched %d", len(a.DetailsJSON), len(big))
			}
		}
	}
	if !found {
		t.Fatal("oversized alert row missing")
	}
}

// TestRestartNoGap pins the first AN-3 restart boundary: stop after a
// delivery AND its watermark persist, reopen the store, restart ⇒ the
// remaining rows deliver with no gap and no duplicate of the delivered
// row.
func TestRestartNoGap(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	h.alert(40, "first", "{}")
	h.alert(41, "second", "{}")
	r.setStatus(func(c capture) int {
		if c.ID == uid(41) {
			return http.StatusInternalServerError // stop after row 1 persisted
		}
		return http.StatusOK
	})
	e.runPass(context.Background())
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 1 {
		t.Fatalf("watermark before restart = %d, want 1", got)
	}
	// Restart: reopen the store, new engine, seed keeps the watermark.
	if err := h.st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := store.Open(h.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	h.st = st2
	r.setStatus(nil)
	e2, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	e2.runPass(context.Background())
	var ids []string
	for _, c := range r.accepted() {
		ids = append(ids, c.ID)
	}
	if len(ids) != 2 || ids[0] != uid(40) || ids[1] != uid(41) {
		t.Fatalf("accepted across restart = %v, want [%s %s] (no gap, no duplicate)", ids, uid(40), uid(41))
	}
}

// TestWatermarkPersistFailureRedelivers pins AN-9 and the second restart
// boundary: a POST succeeds but its watermark persist fails ⇒ the pass
// aborts and the next tick redelivers that exact row from the persisted
// watermark (duplicates, never loss).
func TestWatermarkPersistFailureRedelivers(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	fs := &upsertFailStore{Store: h.st}
	e, _ := h.seeded(Config{Store: fs, URL: r.srv.URL, Heartbeat: 0})
	h.alert(40, "redelivered", "{}")
	h.alert(41, "after", "{}")
	fs.mu.Lock()
	fs.fail = 1
	fs.mu.Unlock()
	e.runPass(context.Background()) // row 1 POSTs 200, persist fails, pass aborts
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 0 {
		t.Fatalf("watermark after failed persist = %d, want 0", got)
	}
	if got := len(r.accepted()); got != 1 {
		t.Fatalf("deliveries before redelivery = %d, want 1 (row 2 never attempted)", got)
	}
	if !strings.Contains(h.logs.String(), "alert dispatch watermark persist failed source=safety_alerts") {
		t.Fatalf("no persist-failure log; logs:\n%s", h.logs.String())
	}
	e.runPass(context.Background())
	var ids []string
	for _, c := range r.accepted() {
		ids = append(ids, c.ID)
	}
	if len(ids) != 3 || ids[0] != uid(40) || ids[1] != uid(40) || ids[2] != uid(41) {
		t.Fatalf("accepted = %v, want exact redelivery [40 40 41]", ids)
	}
	if got := h.watermark(store.AlertSourceSafetyAlert); got != 2 {
		t.Errorf("final watermark = %d, want 2", got)
	}
}

// TestDegradedEscalation pins AN-17: 12 consecutive failed ticks escalate
// to ALERT DISPATCH DEGRADED, rate-limited to once per minute, and a
// success clears the counter back to quiet operation.
func TestDegradedEscalation(t *testing.T) {
	h := newHarness(t)
	r := newReceiver(t)
	e, _ := h.seeded(Config{URL: r.srv.URL, Heartbeat: 0})
	h.alert(40, "degrading", "{}")
	r.setStatus(func(capture) int { return http.StatusBadGateway })
	for i := 0; i < 11; i++ {
		e.runPass(context.Background())
	}
	if h.logs.count("ALERT DISPATCH DEGRADED") != 0 {
		t.Fatalf("DEGRADED before the 12th failed tick; logs:\n%s", h.logs.String())
	}
	if h.logs.count("alert dispatch failed source=safety_alerts class=status:502") != 11 {
		t.Fatalf("plain failure lines != 11; logs:\n%s", h.logs.String())
	}
	e.runPass(context.Background()) // 12th: escalates
	if h.logs.count("ALERT DISPATCH DEGRADED source=safety_alerts class=status:502") != 1 {
		t.Fatalf("no DEGRADED line at the 12th failed tick; logs:\n%s", h.logs.String())
	}
	// Within the same minute the DEGRADED line repeats at most once.
	e.runPass(context.Background())
	e.runPass(context.Background())
	if h.logs.count("ALERT DISPATCH DEGRADED") != 1 {
		t.Fatalf("DEGRADED repeated within a minute; logs:\n%s", h.logs.String())
	}
	h.clk.Advance(2 * time.Minute)
	e.runPass(context.Background())
	if h.logs.count("ALERT DISPATCH DEGRADED") != 2 {
		t.Fatalf("DEGRADED not re-logged after a minute; logs:\n%s", h.logs.String())
	}
	// Success clears: the next failure logs the plain line again.
	r.setStatus(nil)
	e.runPass(context.Background())
	r.setStatus(func(capture) int { return http.StatusBadGateway })
	h.alert(41, "fresh", "{}")
	e.runPass(context.Background())
	if h.logs.count("alert dispatch failed source=safety_alerts class=status:502") != 12 {
		t.Fatalf("counter not cleared by success; logs:\n%s", h.logs.String())
	}
}
