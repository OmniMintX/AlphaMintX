package live

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

// S6: a duplicated/replayed executionReport and the R5 backfill overlap all
// converge on the SAME fill row — booking is idempotent (invariant 8).
func TestStream_DuplicateFillNoop(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	ch, err := e.venue.StreamUserData(context.Background(), "key")
	if err != nil {
		t.Fatalf("StreamUserData: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	ev := <-ch
	if ev.Kind != exchange.UserEventExecutionReport || ev.ExecType != "TRADE" {
		t.Fatalf("unexpected event %+v", ev)
	}
	// The report arrives, then is REPLAYED verbatim.
	if err := e.oms.handleUserEvent(ev); err != nil {
		t.Fatalf("handleUserEvent: %v", err)
	}
	if err := e.oms.handleUserEvent(ev); err != nil {
		t.Fatalf("handleUserEvent replay: %v", err)
	}
	if fills := e.fills(idN(1, 0)); len(fills) != 1 {
		t.Fatalf("fills after replay = %d, want 1", len(fills))
	}
	pos, ok := e.position()
	if !ok || pos.QtyBase != "0.005" {
		t.Fatalf("position = %+v ok=%v, want 0.005 booked ONCE", pos, ok)
	}
	// The R5 overlap (same trade id in MyTrades) is likewise a no-op.
	e.reconcile()
	if fills := e.fills(idN(1, 0)); len(fills) != 1 {
		t.Errorf("fills after R5 overlap = %d, want still 1", len(fills))
	}
	if pos, _ := e.position(); pos.QtyBase != "0.005" {
		t.Errorf("position after R5 overlap = %s, want 0.005", pos.QtyBase)
	}
	if evs := e.events("fill_backfilled"); len(evs) != 0 {
		t.Errorf("fill_backfilled events = %d, want 0 (stream already booked it)", len(evs))
	}
	if ord := e.order(idN(1, 0)); ord.Status != "partially_filled" {
		t.Errorf("order status = %s, want partially_filled", ord.Status)
	}
}

// S12: stream SILENCE — no received frame of any kind for the timeout —
// forces a reconnect (stream_reconnect with reason silence_timeout) and a
// full reconcile run.
func TestStream_SilenceForcesReconcile(t *testing.T) {
	e := newEnv(t)
	tun := DefaultTuning()
	tun.WSSilenceTimeoutSeconds = 1
	e.oms = e.newOMSWith(tun)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.oms.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		silenced := false
		for _, ev := range e.events("stream_reconnect") {
			if strings.Contains(ev.DetailsJSON, "silence_timeout") {
				silenced = true
			}
		}
		// >= 2 completed runs: the startup reconcile plus the mandatory
		// post-reconnect run.
		if silenced && len(e.events("run_completed")) >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no silence-driven reconnect+reconcile within %s", 15*time.Second)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
