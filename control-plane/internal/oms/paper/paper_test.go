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

// zeroFillModel keeps fills at the raw mark/limit price (no fee, no
// slippage) for the tests that exercise order-class semantics only.
func zeroFillModel() FillModel {
	return FillModel{MarketSlippageBps: "0", TakerFeeBps: "0", MakerFeeBps: "0"}
}

func newTestOMS(t *testing.T, fm FillModel) *OMS {
	t.Helper()
	o, err := New(fm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
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
	o := newTestOMS(t, zeroFillModel())
	if _, err := o.Flatten(stratID, sym, decimal.NewFromInt(100)); err == nil {
		t.Fatal("want error flattening a flat book")
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("reduce-only flatten must never open a position")
	}
}

// ---- Fill model v2 (docs/specs/market-data.md §Fill model v2) ----

// The fill model is REQUIRED at construction: missing, negative, or
// malformed bps fields are a construction error, never a silent zero.
func TestFillModelValidation(t *testing.T) {
	cases := []struct {
		name    string
		fm      FillModel
		wantErr bool
	}{
		{"valid zero", FillModel{"0", "0", "0"}, false},
		{"valid fractional", FillModel{"7.5", "10", "2"}, false},
		{"missing slippage", FillModel{"", "10", "2"}, true},
		{"missing taker", FillModel{"5", "", "2"}, true},
		{"missing maker", FillModel{"5", "10", ""}, true},
		{"negative slippage", FillModel{"-1", "10", "2"}, true},
		{"negative taker", FillModel{"5", "-10", "2"}, true},
		{"negative maker", FillModel{"5", "10", "-2"}, true},
		{"not a decimal", FillModel{"abc", "10", "2"}, true},
		{"float notation", FillModel{"1e-4", "10", "2"}, true},
		// 10000 bps = 100% slippage: a sell-side fill price of exactly 0
		// (and negative beyond) — rejected at construction, never a
		// division-by-zero panic in qtyForNotional.
		{"slippage just under bound", FillModel{"9999.99999999", "10", "2"}, false},
		{"slippage at 10000", FillModel{"10000", "10", "2"}, true},
		{"slippage above 10000", FillModel{"10001", "10", "2"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.fm)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%+v) err = %v, wantErr %v", tc.fm, err, tc.wantErr)
			}
		})
	}
}

// Directional slippage, all four cases: entries pay market slippage on the
// entry side, exits (flatten) on the closing side; shorts are symmetric.
func TestSlippageDirection(t *testing.T) {
	fm := FillModel{MarketSlippageBps: "10", TakerFeeBps: "0", MakerFeeBps: "0"}
	cases := []struct {
		name      string
		entrySide Side
		wantEntry string // 100 × (1 ± 0.001)
		wantExit  string
	}{
		{"long entry buys high, long exit sells low", SideBuy, "100.1", "99.9"},
		{"short entry sells low, short exit buys high", SideSell, "99.9", "100.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newTestOMS(t, fm)
			req := marketEntry()
			req.Side = tc.entrySide
			if tc.entrySide == SideSell {
				req.StopPrice = decimal.NewFromInt(102)
			}
			entry, err := o.SubmitEntry(req)
			if err != nil {
				t.Fatalf("SubmitEntry: %v", err)
			}
			if !entry.FillPrice.Equal(d(tc.wantEntry)) {
				t.Errorf("entry fill price = %s, want %s", entry.FillPrice, tc.wantEntry)
			}
			exit, err := o.Flatten(stratID, sym, decimal.NewFromInt(100))
			if err != nil {
				t.Fatalf("Flatten: %v", err)
			}
			if !exit.FillPrice.Equal(d(tc.wantExit)) {
				t.Errorf("exit fill price = %s, want %s", exit.FillPrice, tc.wantExit)
			}
		})
	}
}

// Fees are rounded half away from zero to 8 decimal places and recorded
// separately on the fill; entry notional here is exactly 1000.
func TestFeeRoundingEdges(t *testing.T) {
	cases := []struct {
		takerBps string
		wantFee  string
	}{
		{"10", "1"},                  // 1000 × 0.001
		{"0.00000014", "0.00000001"}, // 1.4e-8 rounds down
		{"0.00000015", "0.00000002"}, // 1.5e-8 half rounds AWAY from zero
		{"0.00000025", "0.00000003"}, // 2.5e-8 half away, never banker's
		{"0.000000004", "0"},         // 4e-10 rounds to zero
	}
	for _, tc := range cases {
		t.Run(tc.takerBps, func(t *testing.T) {
			o := newTestOMS(t, FillModel{MarketSlippageBps: "0", TakerFeeBps: tc.takerBps, MakerFeeBps: "0"})
			ord, err := o.SubmitEntry(marketEntry())
			if err != nil {
				t.Fatalf("SubmitEntry: %v", err)
			}
			if !ord.FeeQuote.Equal(d(tc.wantFee)) {
				t.Errorf("fee = %s, want %s", ord.FeeQuote, tc.wantFee)
			}
			if !ord.FillPrice.Equal(decimal.NewFromInt(100)) {
				t.Errorf("fee must never be baked into the fill price: %s", ord.FillPrice)
			}
		})
	}
}

// A limit that crosses the mark at submission executes immediately as a
// TAKER at the limit price; a resting limit later fills as a MAKER at the
// limit price with no slippage.
func TestMarketableLimitTakerRestingMaker(t *testing.T) {
	fm := FillModel{MarketSlippageBps: "10", TakerFeeBps: "10", MakerFeeBps: "5"}

	t.Run("marketable at placement is taker", func(t *testing.T) {
		o := newTestOMS(t, fm)
		req := limitEntry()
		req.LimitPrice = decimal.NewFromInt(100)
		req.MarkPrice = decimal.NewFromInt(99) // buy: mark ≤ limit ⇒ marketable
		ord, err := o.SubmitEntry(req)
		if err != nil {
			t.Fatalf("SubmitEntry: %v", err)
		}
		if ord.Status != StatusFilled || !ord.FillPrice.Equal(decimal.NewFromInt(100)) {
			t.Fatalf("order = %+v, want immediate fill at limit 100", ord)
		}
		// taker: 10 × 100 × 10bps = 1 (qty 1000/100 = 10, no slippage on limits).
		if !ord.FeeQuote.Equal(decimal.NewFromInt(1)) {
			t.Errorf("taker fee = %s, want 1", ord.FeeQuote)
		}
	})

	t.Run("resting fill is maker", func(t *testing.T) {
		o := newTestOMS(t, fm)
		resting, err := o.SubmitEntry(limitEntry()) // buy 99, mark 100 ⇒ rests
		if err != nil {
			t.Fatalf("SubmitEntry: %v", err)
		}
		if resting.Status != StatusOpen {
			t.Fatalf("order = %+v, want resting", resting)
		}
		fills, err := o.ProcessTick(map[string]decimal.Decimal{sym: decimal.NewFromInt(98)})
		if err != nil {
			t.Fatalf("ProcessTick: %v", err)
		}
		if len(fills) != 1 || fills[0].ID != resting.ID {
			t.Fatalf("fills = %+v, want the resting entry", fills)
		}
		got := fills[0]
		if !got.FillPrice.Equal(decimal.NewFromInt(99)) {
			t.Errorf("fill price = %s, want limit 99 exactly (no slippage)", got.FillPrice)
		}
		// maker: qty 10.10101010 × 99 × 5bps = 0.49999999995 ⇒ 0.5.
		if !got.QtyBase.Equal(d("10.10101010")) || !got.FeeQuote.Equal(d("0.5")) {
			t.Errorf("qty/fee = %s/%s, want 10.10101010/0.5", got.QtyBase, got.FeeQuote)
		}
	})
}

// The approved/clipped size_quote is a NOTIONAL cap at the fill price:
// quantity rounding may never push qty × fill_price above it.
func TestClipNotionalNeverExceeded(t *testing.T) {
	cases := []struct {
		name              string
		size, mark, slip  string
		wantQty, wantFill string
	}{
		// 2/3 = 0.6666… rounds half-away UP at 8dp (would exceed the cap),
		// so it must step DOWN one unit at the 8th decimal instead.
		{"round half up would exceed", "2", "3", "0", "0.66666666", "3"},
		{"slippage included in cap", "1000", "100", "10", "9.99000999", "100.1"},
		{"exact division", "1000", "100", "0", "10", "100"},
		{"repeating decimal", "1000", "99", "0", "10.10101010", "99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newTestOMS(t, FillModel{MarketSlippageBps: tc.slip, TakerFeeBps: "0", MakerFeeBps: "0"})
			req := marketEntry()
			req.SizeQuote = d(tc.size)
			req.MarkPrice = d(tc.mark)
			req.StopPrice = d(tc.mark).Div(decimal.NewFromInt(2))
			ord, err := o.SubmitEntry(req)
			if err != nil {
				t.Fatalf("SubmitEntry: %v", err)
			}
			if !ord.QtyBase.Equal(d(tc.wantQty)) || !ord.FillPrice.Equal(d(tc.wantFill)) {
				t.Errorf("qty/fill = %s/%s, want %s/%s", ord.QtyBase, ord.FillPrice, tc.wantQty, tc.wantFill)
			}
			if ord.QtyBase.Mul(ord.FillPrice).GreaterThan(d(tc.size)) {
				t.Errorf("notional %s exceeds cap %s", ord.QtyBase.Mul(ord.FillPrice), tc.size)
			}
		})
	}

	t.Run("size that rounds to zero qty is rejected", func(t *testing.T) {
		o := newTestOMS(t, zeroFillModel())
		req := marketEntry()
		req.SizeQuote = d("0.00000001")
		req.MarkPrice = decimal.NewFromInt(3)
		if _, err := o.SubmitEntry(req); err == nil {
			t.Error("want error when qty rounds to zero at 8dp, never a zero-qty fill")
		}
	})
}

// A protective stop gapped through fills at the observed (gapped) mark ±
// slippage with the taker fee — never at stop_price itself.
func TestGapThroughStopFillsAtGappedMark(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "10", TakerFeeBps: "10", MakerFeeBps: "0"})
	entry, err := o.SubmitEntry(marketEntry()) // long, stop 98
	if err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	// Gap straight through the stop: 100 → 90 with no intra-tick path.
	fills, err := o.ProcessTick(map[string]decimal.Decimal{sym: decimal.NewFromInt(90)})
	if err != nil {
		t.Fatalf("ProcessTick: %v", err)
	}
	if len(fills) != 1 || fills[0].Type != "stop" {
		t.Fatalf("fills = %+v, want the protective stop", fills)
	}
	stop := fills[0]
	if !stop.FillPrice.Equal(d("89.91")) { // 90 × (1 − 0.001), NOT 98
		t.Errorf("stop fill price = %s, want gapped 89.91", stop.FillPrice)
	}
	if !stop.QtyBase.Equal(entry.QtyBase) {
		t.Errorf("stop qty = %s, want filled entry qty %s", stop.QtyBase, entry.QtyBase)
	}
	wantFee := stop.QtyBase.Mul(d("89.91")).Mul(d("0.001")).Round(8)
	if !stop.FeeQuote.Equal(wantFee) {
		t.Errorf("stop fee = %s, want taker %s", stop.FeeQuote, wantFee)
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flat after the stop fill")
	}
}

// An exit with no usable mark is QUEUED, never filled at zero; it fills on
// the next fresh tick at that tick's mark ± slippage.
func TestZeroMarkExitQueuesThenFills(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "10", TakerFeeBps: "10", MakerFeeBps: "0"})
	if _, err := o.SubmitEntry(marketEntry()); err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	queued, err := o.Flatten(stratID, sym, decimal.Zero)
	if err != nil {
		t.Fatalf("Flatten with no usable mark must queue, not error: %v", err)
	}
	if queued.Status != StatusOpen || !queued.FillPrice.IsZero() {
		t.Fatalf("queued exit = %+v, want open and unfilled", queued)
	}
	if _, ok := o.Position(stratID, sym); !ok {
		t.Fatal("position must stay open while the exit is queued")
	}
	for _, ord := range o.Orders() {
		if ord.Type == "stop" && ord.Status != StatusOpen {
			t.Errorf("protective stop must stay armed while the exit is queued: %+v", ord)
		}
	}

	fills, err := o.ProcessTick(map[string]decimal.Decimal{sym: decimal.NewFromInt(105)})
	if err != nil {
		t.Fatalf("ProcessTick: %v", err)
	}
	if len(fills) != 1 || fills[0].ID != queued.ID {
		t.Fatalf("fills = %+v, want the queued flatten", fills)
	}
	if !fills[0].FillPrice.Equal(d("104.895")) { // 105 × (1 − 0.001)
		t.Errorf("queued exit fill price = %s, want 104.895", fills[0].FillPrice)
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flat after the queued exit fills")
	}
	for _, ord := range o.Orders() {
		if ord.Type == "stop" && ord.Status != StatusCanceled {
			t.Errorf("protective stop not canceled after the flatten fill: %+v", ord)
		}
	}
}

// A take-profit has limit semantics: it fills at the TP price exactly, no
// slippage, with the maker fee, and the sibling stop is canceled after.
func TestTakeProfitFillsAtLimitMakerFee(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "10", TakerFeeBps: "10", MakerFeeBps: "5"})
	req := marketEntry()
	req.TakeProfit = decimal.NewFromInt(102)
	entry, err := o.SubmitEntry(req)
	if err != nil {
		t.Fatalf("SubmitEntry: %v", err)
	}
	fills, err := o.ProcessTick(map[string]decimal.Decimal{sym: decimal.NewFromInt(103)})
	if err != nil {
		t.Fatalf("ProcessTick: %v", err)
	}
	if len(fills) != 1 || fills[0].Type != "take_profit" {
		t.Fatalf("fills = %+v, want the take-profit", fills)
	}
	tp := fills[0]
	if !tp.FillPrice.Equal(decimal.NewFromInt(102)) {
		t.Errorf("TP fill price = %s, want 102 exactly (no slippage)", tp.FillPrice)
	}
	wantFee := entry.QtyBase.Mul(decimal.NewFromInt(102)).Mul(d("0.0005")).Round(8)
	if !tp.FeeQuote.Equal(wantFee) {
		t.Errorf("TP fee = %s, want maker %s", tp.FeeQuote, wantFee)
	}
	if _, ok := o.Position(stratID, sym); ok {
		t.Error("position must be flat after the TP fill")
	}
	for _, ord := range o.Orders() {
		if ord.Type == "stop" && ord.Status != StatusCanceled {
			t.Errorf("sibling stop not canceled after the TP fill: %+v", ord)
		}
	}
}

// Per-tick trigger ordering is deterministic: symbols lexicographic; within
// a symbol stops before take-profits before entry limits; ties within a
// class by order_id lexicographic. A tick crossing both SL and TP fills the
// stop (pessimistic) and cancels the sibling TP.
func TestTriggerOrderingDeterminism(t *testing.T) {
	symA, symB := "AAA/USDT", "BBB/USDT"
	o := newTestOMS(t, zeroFillModel())
	for _, s := range []string{symA, symB} {
		// s1: open long whose SL (100) and TP (99, inverted on purpose) BOTH
		// trigger at mark 99.5 — the stop must win and cancel the TP.
		if _, err := o.SubmitEntry(EntryRequest{
			StrategyID: "s1", Symbol: s, Side: SideBuy, Type: "market",
			SizeQuote: decimal.NewFromInt(1000), MarkPrice: decimal.NewFromInt(100),
			StopPrice: decimal.NewFromInt(100), TakeProfit: decimal.NewFromInt(99),
		}); err != nil {
			t.Fatalf("market entry %s: %v", s, err)
		}
		// s2, s3: resting buy limits at 99.6, crossed by mark 99.5.
		for _, strat := range []string{"s2", "s3"} {
			if _, err := o.SubmitEntry(EntryRequest{
				StrategyID: strat, Symbol: s, Side: SideBuy, Type: "limit",
				LimitPrice: d("99.6"), SizeQuote: decimal.NewFromInt(1000),
				MarkPrice: decimal.NewFromInt(100), StopPrice: decimal.NewFromInt(90),
			}); err != nil {
				t.Fatalf("limit entry %s %s: %v", strat, s, err)
			}
		}
	}

	mark := d("99.5")
	fills, err := o.ProcessTick(map[string]decimal.Decimal{symB: mark, symA: mark})
	if err != nil {
		t.Fatalf("ProcessTick: %v", err)
	}
	wantKinds := []struct {
		symbol string
		typ    string
	}{
		{symA, "stop"}, {symA, "limit"}, {symA, "limit"},
		{symB, "stop"}, {symB, "limit"}, {symB, "limit"},
	}
	if len(fills) != len(wantKinds) {
		t.Fatalf("fills = %d, want %d", len(fills), len(wantKinds))
	}
	for i, w := range wantKinds {
		if fills[i].Symbol != w.symbol || fills[i].Type != w.typ {
			t.Errorf("fill[%d] = %s %s, want %s %s", i, fills[i].Symbol, fills[i].Type, w.symbol, w.typ)
		}
	}
	// Ties within a class break by order_id lexicographic.
	if fills[1].ID > fills[2].ID || fills[4].ID > fills[5].ID {
		t.Error("entry-limit fills not ordered by order_id lexicographic")
	}
	// The crossed sibling TPs were canceled, not filled.
	for _, ord := range o.Orders() {
		if ord.Type == "take_profit" && ord.Status != StatusCanceled {
			t.Errorf("sibling TP not canceled after the stop fill: %+v", ord)
		}
	}
}

// Positions accumulate the fees paid over their lifecycle (fees_quote).
func TestCumulativeFeesOnPosition(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "0", TakerFeeBps: "10", MakerFeeBps: "5"})
	if _, err := o.SubmitEntry(marketEntry()); err != nil { // fee 1000 × 0.001 = 1
		t.Fatalf("first entry: %v", err)
	}
	add := marketEntry()
	add.SizeQuote = decimal.NewFromInt(500) // fee 0.5
	if _, err := o.SubmitEntry(add); err != nil {
		t.Fatalf("second entry: %v", err)
	}
	pos, ok := o.Position(stratID, sym)
	if !ok || !pos.FeesQuote.Equal(d("1.5")) {
		t.Fatalf("position fees = %s (ok=%v), want 1.5", pos.FeesQuote, ok)
	}
	if _, err := o.Flatten(stratID, sym, decimal.NewFromInt(100)); err != nil { // fee 15 × 100 × 0.001 = 1.5
		t.Fatalf("Flatten: %v", err)
	}
	// Flat round trip at one price: realized PnL is exactly −Σ fees.
	if got := o.RealizedPnL(stratID, sym); !got.Equal(d("-3")) {
		t.Errorf("realized PnL = %s, want -3", got)
	}
}

// Realized PnL = exit proceeds − entry cost − Σ fees (entry + exit), longs
// and shorts symmetric.
func TestRealizedPnLSubtractsFees(t *testing.T) {
	cases := []struct {
		name         string
		side         Side
		stop         string
		exitMark     string
		wantRealized string
	}{
		// long: gross (110−100)×10 = 100; fees 1 + 10×110×0.001 = 2.1
		{"long", SideBuy, "98", "110", "97.9"},
		// short: gross (100−90)×10 = 100; fees 1 + 10×90×0.001 = 1.9
		{"short", SideSell, "102", "90", "98.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := newTestOMS(t, FillModel{MarketSlippageBps: "0", TakerFeeBps: "10", MakerFeeBps: "5"})
			req := marketEntry()
			req.Side = tc.side
			req.StopPrice = d(tc.stop)
			if _, err := o.SubmitEntry(req); err != nil {
				t.Fatalf("SubmitEntry: %v", err)
			}
			if _, err := o.Flatten(stratID, sym, d(tc.exitMark)); err != nil {
				t.Fatalf("Flatten: %v", err)
			}
			if got := o.RealizedPnL(stratID, sym); !got.Equal(d(tc.wantRealized)) {
				t.Errorf("realized PnL = %s, want %s", got, tc.wantRealized)
			}
		})
	}
}

// Increasing a position re-averages the entry price quantity-weighted
// (orders.go applyFill, opening/increasing branch); the round-trip PnL
// prices the whole lot at that average.
func TestWeightedAverageEntryOnIncrease(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "0", TakerFeeBps: "10", MakerFeeBps: "5"})
	if _, err := o.SubmitEntry(marketEntry()); err != nil { // 10 @ 100, fee 1
		t.Fatalf("first entry: %v", err)
	}
	add := marketEntry()
	add.SizeQuote = decimal.NewFromInt(1200)
	add.MarkPrice = decimal.NewFromInt(120)
	if _, err := o.SubmitEntry(add); err != nil { // 10 @ 120, fee 1.2
		t.Fatalf("second entry: %v", err)
	}
	pos, ok := o.Position(stratID, sym)
	if !ok || !pos.QtyBase.Equal(d("20")) || !pos.EntryPrice.Equal(d("110")) {
		t.Fatalf("position qty=%s entry=%s (ok=%v), want 20 @ 110", pos.QtyBase, pos.EntryPrice, ok)
	}
	if _, err := o.Flatten(stratID, sym, decimal.NewFromInt(130)); err != nil { // fee 20 × 130 × 0.001 = 2.6
		t.Fatalf("Flatten: %v", err)
	}
	// gross (130−110) × 20 = 400; fees 1 + 1.2 + 2.6 = 4.8
	if got := o.RealizedPnL(stratID, sym); !got.Equal(d("395.2")) {
		t.Errorf("realized PnL = %s, want 395.2", got)
	}
}

// An opposite-side entry larger than the open position realizes PnL on the
// closed quantity ONLY and re-opens the remainder at the fill price
// (orders.go applyFill, reducing/flipping branch).
func TestFlipRealizesClosedQtyAndReopensAtFillPrice(t *testing.T) {
	o := newTestOMS(t, FillModel{MarketSlippageBps: "0", TakerFeeBps: "10", MakerFeeBps: "5"})
	if _, err := o.SubmitEntry(marketEntry()); err != nil { // long 10 @ 100, fee 1
		t.Fatalf("long entry: %v", err)
	}
	flip := marketEntry()
	flip.Side = SideSell
	flip.SizeQuote = decimal.NewFromInt(3000)
	flip.MarkPrice = decimal.NewFromInt(120)
	flip.StopPrice = decimal.NewFromInt(126)
	if _, err := o.SubmitEntry(flip); err != nil { // sell 25 @ 120, fee 3
		t.Fatalf("flip entry: %v", err)
	}
	pos, ok := o.Position(stratID, sym)
	if !ok || !pos.QtyBase.Equal(d("-15")) || !pos.EntryPrice.Equal(d("120")) {
		t.Fatalf("position qty=%s entry=%s (ok=%v), want -15 @ 120", pos.QtyBase, pos.EntryPrice, ok)
	}
	// Realized on the closed 10 only: gross (120−100) × 10 = 200; fees 1 + 3.
	if got := o.RealizedPnL(stratID, sym); !got.Equal(d("196")) {
		t.Errorf("realized PnL = %s, want 196", got)
	}
	// The flipped lot keeps accumulating lifecycle fees (never went flat).
	if !pos.FeesQuote.Equal(d("4")) {
		t.Errorf("fees_quote = %s, want 4", pos.FeesQuote)
	}
}
