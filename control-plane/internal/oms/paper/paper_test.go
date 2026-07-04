package paper

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

const (
	stratID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6e"
	sym     = "BTC/USDT"
)

func marketEntry() EntryRequest {
	return EntryRequest{
		StrategyID: stratID,
		Symbol:     sym,
		Side:       SideBuy,
		Type:       "market",
		SizeQuote:  decimal.NewFromInt(1000),
		MarkPrice:  decimal.NewFromInt(100),
		StopPrice:  decimal.NewFromInt(98),
	}
}

func limitEntry() EntryRequest {
	r := marketEntry()
	r.Type = "limit"
	r.LimitPrice = decimal.NewFromInt(99)
	return r
}

func ordersByClass(o *OMS, c Class) []Order {
	var out []Order
	for _, ord := range o.Orders() {
		if ord.Class == c {
			out = append(out, ord)
		}
	}
	return out
}

// Market entry fills immediately and places a reduce-only PROTECTIVE stop on
// the opposite side for the full filled quantity (invariant 2).
func TestMarketEntryPlacesProtectiveStop(t *testing.T) {
	o := New()
	ord, err := o.SubmitEntry(marketEntry())
	if err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	if ord.Status != StatusFilled || !ord.QtyBase.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("entry = %+v, want filled qty 10", ord)
	}
	pos, ok := o.Position(stratID, sym)
	if !ok || !pos.QtyBase.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("position = %+v ok=%v, want long 10", pos, ok)
	}
	stops := ordersByClass(o, ClassProtective)
	if len(stops) != 1 {
		t.Fatalf("protective orders = %d, want 1", len(stops))
	}
	s := stops[0]
	if !s.ReduceOnly || s.Side != SideSell || s.Status != StatusOpen ||
		!s.QtyBase.Equal(ord.QtyBase) || !s.StopPrice.Equal(decimal.NewFromInt(98)) {
		t.Errorf("protective stop = %+v, want reduce-only sell 10 @98 open", s)
	}
}

// Kill cancels un-filled ENTRY orders ONLY; the protective stop guarding the
// open position survives (risk-limits.md, order classes).
func TestKillCancelsEntriesOnlyStopSurvives(t *testing.T) {
	o := New()
	if _, err := o.SubmitEntry(marketEntry()); err != nil {
		t.Fatalf("market entry: %v", err)
	}
	resting, err := o.SubmitEntry(limitEntry())
	if err != nil {
		t.Fatalf("limit entry: %v", err)
	}

	o.Kill(1)

	for _, ord := range o.Orders() {
		switch {
		case ord.ID == resting.ID && ord.Status != StatusCanceled:
			t.Errorf("resting ENTRY not canceled by kill: %+v", ord)
		case ord.Class == ClassProtective && ord.Status != StatusOpen:
			t.Errorf("protective order did not survive kill: %+v", ord)
		}
	}
	if _, ok := o.Position(stratID, sym); !ok {
		t.Error("kill must not touch the open position itself")
	}
	if o.KillEpoch() != 1 {
		t.Errorf("kill epoch = %d, want 1", o.KillEpoch())
	}
}

// Submissions carrying a kill-epoch older than the current one are rejected
// (the OMS-boundary kill re-check).
func TestStaleKillEpochRejected(t *testing.T) {
	o := New()
	o.Kill(2)
	req := marketEntry()
	req.KillEpoch = 1
	if _, err := o.SubmitEntry(req); !errors.Is(err, ErrKillEpochStale) {
		t.Fatalf("err = %v, want ErrKillEpochStale", err)
	}
	if len(o.Orders()) != 0 {
		t.Error("rejected submission must not create orders")
	}
}

// SL placement contingency: if the protective stop cannot be placed, the
// filled quantity is closed with a reduce-only market order (never naked).
func TestProtectivePlacementFailureFlattens(t *testing.T) {
	o := New()
	o.SetProtectivePlacementHook(func(*Order) error { return errors.New("exchange down") })
	if _, err := o.SubmitEntry(marketEntry()); err == nil {
		t.Fatal("want error when protective placement fails")
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flattened after SL placement failure")
	}
	for _, ord := range o.Orders() {
		if ord.Class == ClassProtective && ord.Status == StatusFilled && !ord.ReduceOnly {
			t.Errorf("flatten order must be reduce-only: %+v", ord)
		}
	}
}

// Limit entries carry the protective-stop obligation on the resting order
// and place the stop when the entry fills (naked-position invariant).
func TestLimitEntryFillPlacesProtectiveStop(t *testing.T) {
	o := New()
	resting, err := o.SubmitEntry(limitEntry())
	if err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	if resting.Status != StatusOpen || !resting.StopPrice.Equal(decimal.NewFromInt(98)) {
		t.Fatalf("resting entry = %+v, want open with stop obligation 98", resting)
	}
	if got := ordersByClass(o, ClassProtective); len(got) != 0 {
		t.Fatalf("protective orders before fill = %d, want 0", len(got))
	}

	filled, err := o.FillLimitEntry(resting.ID)
	if err != nil {
		t.Fatalf("FillLimitEntry: %v", err)
	}
	if filled.Status != StatusFilled || !filled.FillPrice.Equal(decimal.NewFromInt(99)) {
		t.Fatalf("filled entry = %+v, want filled @99", filled)
	}
	pos, ok := o.Position(stratID, sym)
	if !ok || pos.QtyBase.Sign() <= 0 {
		t.Fatalf("position = %+v ok=%v, want long", pos, ok)
	}
	stops := ordersByClass(o, ClassProtective)
	if len(stops) != 1 {
		t.Fatalf("protective orders after fill = %d, want 1", len(stops))
	}
	s := stops[0]
	if !s.ReduceOnly || s.Side != SideSell || s.Status != StatusOpen ||
		!s.QtyBase.Equal(filled.QtyBase) || !s.StopPrice.Equal(decimal.NewFromInt(98)) {
		t.Errorf("protective stop = %+v, want reduce-only sell %s @98 open", s, filled.QtyBase)
	}
}

// The SL placement contingency also covers limit-entry fills: if the stop
// cannot be placed on fill, the filled quantity is flattened reduce-only.
func TestLimitEntryFillProtectiveFailureFlattens(t *testing.T) {
	o := New()
	resting, err := o.SubmitEntry(limitEntry())
	if err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	o.SetProtectivePlacementHook(func(*Order) error { return errors.New("exchange down") })
	if _, err := o.FillLimitEntry(resting.ID); err == nil {
		t.Fatal("want error when protective placement fails on fill")
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flattened after SL placement failure")
	}
}

// Non-positive prices and sizes are rejected with errors, never a decimal
// division-by-zero panic (same class as the gate's zero-mark guard).
func TestZeroPriceAndSizeGuards(t *testing.T) {
	o := New()
	m := marketEntry()
	m.MarkPrice = decimal.Zero
	if _, err := o.SubmitEntry(m); err == nil {
		t.Error("market entry with zero mark price must error, not panic")
	}
	l := limitEntry()
	l.LimitPrice = decimal.Zero
	if _, err := o.SubmitEntry(l); err == nil {
		t.Error("limit entry with zero limit price must error, not panic")
	}
	z := marketEntry()
	z.SizeQuote = decimal.Zero
	if _, err := o.SubmitEntry(z); err == nil {
		t.Error("entry with zero size_quote must error, not panic")
	}
	if got := len(o.Orders()); got != 0 {
		t.Errorf("rejected submissions must not create orders, got %d", got)
	}
}

// Flatten closes the position reduce-only and cancels the now-orphaned
// protective stop only AFTER the flatten fill (stops-after-flatten ordering).
func TestFlattenReduceOnlyAndCancelsStops(t *testing.T) {
	o := New()
	if _, err := o.SubmitEntry(marketEntry()); err != nil {
		t.Fatalf("entry: %v", err)
	}
	ord, err := o.Flatten(stratID, sym, decimal.NewFromInt(101))
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if !ord.ReduceOnly || ord.Side != SideSell || ord.Class != ClassProtective ||
		!ord.QtyBase.Equal(decimal.NewFromInt(10)) {
		t.Errorf("flatten order = %+v, want reduce-only sell 10", ord)
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flat after Flatten")
	}
	for _, other := range o.Orders() {
		if other.Type == "stop" && other.Status != StatusCanceled {
			t.Errorf("protective stop not canceled after flatten: %+v", other)
		}
	}
}

// Flatten on a flat book errors; reduce-only can never open a position.
func TestFlattenWithoutPositionErrors(t *testing.T) {
	o := New()
	if _, err := o.Flatten(stratID, sym, decimal.NewFromInt(100)); err == nil {
		t.Fatal("want error flattening a flat book")
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("reduce-only flatten must never open a position")
	}
}
