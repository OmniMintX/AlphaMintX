package backtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

const testStrategyID = "b0000000-0000-4000-8000-000000000001"

// testT0 is the fixture grid origin: 2026-07-03T23:54:00Z (six 1m candles
// reach the UTC midnight rollover).
var testT0 = time.Date(2026, 7, 3, 23, 54, 0, 0, time.UTC).UnixMilli()

func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func mustDec(t *testing.T, v string) contract.Decimal {
	t.Helper()
	d, err := contract.ParseDecimal(v)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", v, err)
	}
	return d
}

// kl is a 1m BTC/USDT fixture candle.
func kl(openTime int64, o, h, l, c string) Kline {
	return Kline{Symbol: "BTC/USDT", Interval: "1m", OpenTime: openTime,
		Open: o, High: h, Low: l, Close: c, Volume: "1"}
}

// flat is a flat fixture candle at price p on grid slot i.
func flat(i int, p string) Kline { return kl(testT0+int64(i)*60_000, p, p, p, p) }

// writeTestDataset writes klines canonically and reads them back.
func writeTestDataset(t *testing.T, klines []Kline) *Dataset {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dataset.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	if _, err := WriteDataset(f, klines); err != nil {
		t.Fatalf("WriteDataset: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close dataset: %v", err)
	}
	ds, err := ReadDataset(path)
	if err != nil {
		t.Fatalf("ReadDataset: %v", err)
	}
	return ds
}

const testRunSpecJSON = `{
  "strategy_id": "b0000000-0000-4000-8000-000000000001",
  "symbol": "BTC/USDT",
  "interval": "1m",
  "mask_level": "M0",
  "seed": 42,
  "quote_currency": "USDT",
  "fill_model": {"market_slippage_bps": "0", "taker_fee_bps": "0", "maker_fee_bps": "0"},
  "max_age_seconds": 60,
  "limits": {
    "symbol_whitelist": ["BTC/USDT"],
    "max_open_positions": 3,
    "per_position_notional_cap_quote": "2000",
    "daily_loss_limit_quote": "500",
    "max_drawdown_pct": "50",
    "max_loss_at_stop_quote": "450",
    "min_stop_distance_pct": "0.1",
    "max_stop_distance_pct": "25",
    "max_orders_per_minute": 60,
    "require_stop_loss": true,
    "allocated_capital_quote": "10000",
    "accounting_quote": "USDT"
  }
}`

// loadSpec writes raw as a runspec file and loads it.
func loadSpec(t *testing.T, raw string) (*RunSpec, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runspec.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write runspec: %v", err)
	}
	return LoadRunSpec(path)
}

func testSpec(t *testing.T) *RunSpec {
	t.Helper()
	spec, err := loadSpec(t, testRunSpecJSON)
	if err != nil {
		t.Fatalf("LoadRunSpec: %v", err)
	}
	return spec
}

// prop builds one schema-valid proposal line for a decision tick, with
// created_at pinned to the virtual decision time close_time + 1s.
func prop(t *testing.T, ds *Dataset, tick int, action contract.Action, size, stop string) ProposalLine {
	t.Helper()
	decision := time.UnixMilli(ds.FirstOpenTime() + int64(tick+1)*ds.IntervalMS()).UTC().Add(time.Second)
	sum := contract.AnalystSummary{Signal: "neutral", Confidence: 0.5, Summary: "flat"}
	p := contract.Proposal{
		SchemaVersion: contract.SchemaVersion,
		ProposalID:    uid(1000 + tick),
		StrategyID:    testStrategyID,
		AgentTraceID:  uid(2000 + tick),
		CreatedAt:     contract.NewUTCTime(decision),
		Symbol:        "BTC/USDT",
		Action:        action,
		SizeQuote:     mustDec(t, size),
		Entry:         contract.Entry{Type: "market"},
		TimeInForce:   "gtc",
		Confidence:    0.9,
		Reasoning:     "backtest",
		AnalystSummaries: contract.AnalystSummaries{
			Market: sum, News: sum, Fundamental: sum,
		},
	}
	if stop != "" {
		d := mustDec(t, stop)
		p.StopLoss = &d
	}
	n := tick
	return ProposalLine{TickNumber: &n, Proposal: p}
}

// proposalsJSONL renders the meta line plus proposal lines.
func proposalsJSONL(t *testing.T, spec *RunSpec, ds *Dataset, lines []ProposalLine) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	meta := MetaLine{Kind: "backtest_meta", StrategyID: spec.StrategyID, Symbol: spec.Symbol,
		Interval: spec.Interval, DatasetSHA256: ds.SHA256, Seed: spec.Seed, MaskLevel: spec.MaskLevel,
		Window: 24, Scenario: "bullish"}
	enc := json.NewEncoder(buf)
	if err := enc.Encode(meta); err != nil {
		t.Fatalf("encode meta: %v", err)
	}
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatalf("encode proposal line: %v", err)
		}
	}
	return buf
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "backtest.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
