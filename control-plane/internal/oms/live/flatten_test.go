package live

import (
	"context"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// S22: a flatten whose venue free balance is SHORT of the local position
// sizes to the min(), appends flatten_short_balance + operator alert, and
// never sells beyond the venue balance.
func TestFlatten_ShortBalance(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0.004", "0")

	if err := e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "kill", nil); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	evs := e.events("flatten_short_balance")
	if len(evs) != 1 {
		t.Fatalf("flatten_short_balance events = %d, want 1", len(evs))
	}
	for _, want := range []string{`"local_position":"0.01"`, `"venue_free":"0.004"`} {
		if !strings.Contains(evs[0].DetailsJSON, want) {
			t.Errorf("flatten_short_balance details = %s, want %s", evs[0].DetailsJSON, want)
		}
	}
	fl := e.order(idN(2, 0))
	if fl.QtyBase != "0.004" || !fl.ReduceOnly || fl.Origin != "kill" ||
		fl.Class != "PROTECTIVE" || fl.Type != "market" || fl.Side != "sell" {
		t.Errorf("flatten order = %+v, want reduce-only market sell for min() 0.004", fl.Order)
	}
	for _, vo := range e.venueOpen() {
		if vo.Type == "MARKET" && vo.OrigQty != "0.004" {
			t.Errorf("venue flatten qty = %s, want 0.004 (never oversells)", vo.OrigQty)
		}
	}
}

// M1: a SHORT position flatten is a BUY — the venue free BASE balance
// never bounds a buyback: the full local position closes, with no false
// flatten_short_balance clamp or flatten_dust.
func TestFlatten_ShortPositionBuyback(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntryWith(10, func(p *contract.Proposal) {
		p.Action = contract.ActionOpenShort
	}); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.Fill(idN(1, 0), "0.01", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0", "0") // no base inventory: irrelevant to a buy

	if err := e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "kill", nil); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	if evs := e.events("flatten_short_balance"); len(evs) != 0 {
		t.Errorf("flatten_short_balance events = %d, want 0 (base balance never bounds a buy)", len(evs))
	}
	if evs := e.events("flatten_dust"); len(evs) != 0 {
		t.Errorf("flatten_dust events = %d, want 0", len(evs))
	}
	fl := e.order(idN(2, 0))
	if fl.Side != "buy" || fl.QtyBase != "0.01" || !fl.ReduceOnly ||
		fl.Class != "PROTECTIVE" || fl.Type != "market" {
		t.Errorf("flatten order = %+v, want reduce-only market buy for the full 0.01", fl.Order)
	}
}

// m3: a remainder above minQty but below the venue minNotional (at the
// fresh mark) is dust on spot — flatten_dust carries the min_notional
// threshold and NO order is sent.
func TestFlatten_DustMinNotional(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	// 0.00005 BTC at the 64000 mark = 3.2 quote < minNotional 5.
	if err := e.venue.Fill(idN(1, 0), "0.00005", "64000"); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	e.reconcile()
	e.venue.SetBalance("BTC", "0.00005", "0")

	if err := e.oms.Flatten(context.Background(), uid(1), "BTC/USDT", "kill", nil); err != nil {
		t.Fatalf("Flatten: %v (dust is evented, never an error)", err)
	}

	evs := e.events("flatten_dust")
	if len(evs) != 1 {
		t.Fatalf("flatten_dust events = %d, want 1", len(evs))
	}
	for _, want := range []string{`"remaining":"0.00005"`, `"min_notional":"5"`} {
		if !strings.Contains(evs[0].DetailsJSON, want) {
			t.Errorf("flatten_dust details = %s, want %s", evs[0].DetailsJSON, want)
		}
	}
	for _, vo := range e.venueOpen() {
		if vo.Type == "MARKET" {
			t.Errorf("venue market order sent for dust: %+v", vo)
		}
	}
}
