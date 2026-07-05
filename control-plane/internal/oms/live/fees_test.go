package live

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
)

// S23: a non-base/quote commission with NO fresh mark books the fill and
// the position quantity immediately, DEFERS the fee-dependent accounting,
// and converts it on a later run (fee_conversion_applied) once a fresh mark
// exists — no fee is ever silently zero, and never applied twice.
func TestFees_DeferredConversion(t *testing.T) {
	e := newEnv(t)
	e.reconcile()
	if err := e.submitEntry(10); err != nil {
		t.Fatalf("SubmitApproved: %v", err)
	}
	if err := e.venue.FillWithCommission(idN(1, 0), "0.005", "64000", "0.01", "BNB"); err != nil {
		t.Fatalf("FillWithCommission: %v", err)
	}
	e.reconcile()

	pos, ok := e.position()
	if !ok || pos.QtyBase != "0.005" || pos.FeesQuote != "0" || pos.RealizedPnLQuote != "0" {
		t.Fatalf("position = %+v ok=%v, want qty booked with the fee deferred", pos, ok)
	}
	if pend, err := e.st.ListUnconvertedPendingFillFees(); err != nil || len(pend) != 1 {
		t.Fatalf("unconverted fees = %d err=%v, want 1", len(pend), err)
	}
	if evs := e.events("commission_asset_anomaly"); len(evs) != 1 ||
		!strings.Contains(evs[0].DetailsJSON, `"deferred":true`) {
		t.Errorf("commission_asset_anomaly events = %+v, want one deferred row", evs)
	}
	if evs := e.events("fee_conversion_applied"); len(evs) != 0 {
		t.Fatalf("fee_conversion_applied = %d, want 0 before a fresh mark", len(evs))
	}

	// A fresh BNB/USDT mark arrives; the next run converts: 0.01 x 600 = 6.
	e.marks.Put(marketdata.Tick{Symbol: "BNB/USDT",
		Mark: decimal.RequireFromString("600"), TS: e.now})
	e.reconcile()

	evs := e.events("fee_conversion_applied")
	if len(evs) != 1 || !strings.Contains(evs[0].DetailsJSON, `"fee_quote":"6"`) {
		t.Fatalf("fee_conversion_applied = %+v, want one row converting to 6", evs)
	}
	pos, _ = e.position()
	if pos.FeesQuote != "6" || pos.RealizedPnLQuote != "-6" {
		t.Errorf("position after conversion = fees %s realized %s, want 6 / -6",
			pos.FeesQuote, pos.RealizedPnLQuote)
	}
	state, ok, err := e.st.GetStrategyState(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetStrategyState: ok=%v err=%v", ok, err)
	}
	if state.EquityQuote != "9994" || state.DailyRealizedPnLQuote != "-6" {
		t.Errorf("strategy_state = equity %s daily %s, want 9994 / -6",
			state.EquityQuote, state.DailyRealizedPnLQuote)
	}
	if pend, err := e.st.ListUnconvertedPendingFillFees(); err != nil || len(pend) != 0 {
		t.Errorf("unconverted fees after conversion = %d err=%v, want 0", len(pend), err)
	}

	// Replay run: the conversion never double-applies.
	e.reconcile()
	if pos, _ := e.position(); pos.FeesQuote != "6" {
		t.Errorf("fees after replay run = %s, want still 6", pos.FeesQuote)
	}
	if evs := e.events("fee_conversion_applied"); len(evs) != 1 {
		t.Errorf("fee_conversion_applied after replay = %d, want still 1", len(evs))
	}
}
