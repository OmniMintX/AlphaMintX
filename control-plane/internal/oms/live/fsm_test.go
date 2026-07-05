package live

import (
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
)

func statusEvent(clientOrderID, venueStatus string) exchange.UserEvent {
	return exchange.UserEvent{
		Kind: exchange.UserEventExecutionReport, VenueSymbol: "BTCUSDT",
		ClientOrderID: clientOrderID, ExecType: "NEW", OrderStatus: venueStatus,
		EventTime: testNow,
	}
}

// S13: the FSM is monotone in rank — a stale/regressive update is dropped
// (with a stale_update_dropped event), terminal statuses are immutable, and
// PENDING_CANCEL is never a regression.
func TestFSM_Monotone(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	// PENDING_CANCEL maps to the CURRENT non-terminal status: no change, no
	// stale-drop event.
	if err := e.oms.handleUserEvent(statusEvent(idN(1, 0), "PENDING_CANCEL")); err != nil {
		t.Fatalf("handleUserEvent PENDING_CANCEL: %v", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "open" {
		t.Fatalf("status after PENDING_CANCEL = %s, want open kept", ord.Status)
	}
	if evs := e.events("stale_update_dropped"); len(evs) != 0 {
		t.Fatalf("stale_update_dropped after PENDING_CANCEL = %d, want 0", len(evs))
	}
	// Advance to FILLED (rank 3, terminal).
	if err := e.oms.handleUserEvent(statusEvent(idN(1, 0), "FILLED")); err != nil {
		t.Fatalf("handleUserEvent FILLED: %v", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "filled" {
		t.Fatalf("status = %s, want filled", ord.Status)
	}
	// A stale NEW replay is regressive: dropped and evented.
	if err := e.oms.handleUserEvent(statusEvent(idN(1, 0), "NEW")); err != nil {
		t.Fatalf("handleUserEvent stale NEW: %v", err)
	}
	if ord := e.order(idN(1, 0)); ord.Status != "filled" {
		t.Errorf("status after stale NEW = %s, want filled kept", ord.Status)
	}
	if evs := e.events("stale_update_dropped"); len(evs) != 1 {
		t.Errorf("stale_update_dropped events = %d, want 1", len(evs))
	}
	// Terminal immutability: even a same-rank write is a no-op.
	now, err := e.st.RecordOrderStatus(e.order(idN(1, 0)).OrderID, "canceled")
	if err != nil || now != "filled" {
		t.Errorf("RecordOrderStatus(canceled) = %s, %v; want filled kept", now, err)
	}
}
