package backtest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/e2e"
)

// runOnce replays the fixture against a fresh DB.
func runOnce(t *testing.T, spec *RunSpec, ds *Dataset, lines []ProposalLine) (Summary, string, *DB, error) {
	t.Helper()
	db := openTestDB(t)
	var out bytes.Buffer
	sum, err := Run(spec, ds, proposalsJSONL(t, spec, ds, lines), &out, db)
	return sum, out.String(), db, err
}

// parsed splits records.jsonl into the per-kind typed lines, in order.
type parsed struct {
	kinds     []string
	verdicts  []contract.Verdict
	orders    []e2e.OrderRecord
	positions []e2e.PositionRecord
}

func parseRecords(t *testing.T, records string) parsed {
	t.Helper()
	var p parsed
	for i, line := range strings.Split(strings.TrimSuffix(records, "\n"), "\n") {
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		p.kinds = append(p.kinds, probe.Kind)
		var err error
		switch probe.Kind {
		case "verdict":
			var vr e2e.VerdictRecord
			if err = json.Unmarshal([]byte(line), &vr); err == nil {
				p.verdicts = append(p.verdicts, *vr.Verdict)
			}
		case "order":
			var or e2e.OrderRecord
			if err = json.Unmarshal([]byte(line), &or); err == nil {
				p.orders = append(p.orders, or)
			}
		case "position":
			var pr e2e.PositionRecord
			if err = json.Unmarshal([]byte(line), &pr); err == nil {
				p.positions = append(p.positions, pr)
			}
		}
		if err != nil {
			t.Fatalf("record %d (%s): %v", i, probe.Kind, err)
		}
	}
	return p
}

// rolloverFixture: six 1m candles 23:54-23:59 UTC. Tick 1 opens long 100 @
// mark 100 with stop 94; candle 2's low sub-tick (93) fires the stop for a
// -7 realized loss; tick 5's decision lands at 00:00:01 the NEXT UTC day.
func rolloverFixture(t *testing.T) (*RunSpec, *Dataset, []ProposalLine) {
	t.Helper()
	spec := testSpec(t)
	ds := writeTestDataset(t, []Kline{
		flat(0, "100"), flat(1, "100"),
		kl(testT0+2*60_000, "100", "100", "93", "100"),
		flat(3, "100"), flat(4, "100"), flat(5, "100"),
	})
	lines := []ProposalLine{
		prop(t, ds, 0, contract.ActionHold, "0", ""),
		prop(t, ds, 1, contract.ActionOpenLong, "100", "94"),
		prop(t, ds, 2, contract.ActionHold, "0", ""),
		prop(t, ds, 3, contract.ActionHold, "0", ""),
		prop(t, ds, 4, contract.ActionHold, "0", ""),
		prop(t, ds, 5, contract.ActionHold, "0", ""),
	}
	return spec, ds, lines
}

func TestRunDeterministicByteIdenticalAndTeed(t *testing.T) {
	spec, ds, lines := rolloverFixture(t)
	sum1, rec1, db1, err := runOnce(t, spec, ds, lines)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	sum2, rec2, _, err := runOnce(t, spec, ds, lines)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if rec1 != rec2 {
		t.Errorf("two runs are not byte-identical:\n--- run 1 ---\n%s--- run 2 ---\n%s", rec1, rec2)
	}
	if !reflect.DeepEqual(sum1, sum2) {
		t.Errorf("summaries differ: %+v vs %+v", sum1, sum2)
	}
	if sum1.Status != StatusComplete || sum1.Ticks != 6 || sum1.DatasetSHA256 != ds.SHA256 {
		t.Errorf("summary = %+v", sum1)
	}

	// DB tee: backtest_records payloads are the exact record lines.
	rows, err := db1.Records(sum1.BacktestID)
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if joined := strings.Join(rows, "\n") + "\n"; joined != rec1 {
		t.Errorf("backtest_records tee is not byte-identical to records.jsonl")
	}
	row, ok, err := db1.GetRun(sum1.BacktestID)
	if err != nil || !ok {
		t.Fatalf("GetRun: ok=%v err=%v", ok, err)
	}
	if row.Status != StatusComplete || row.Seed != 42 || row.MaskLevel != "M0" ||
		row.DatasetSHA256 != ds.SHA256 || row.ConfigHash != spec.ConfigHash ||
		row.StrategyID != testStrategyID || row.CodeVersion == "" ||
		row.CreatedAt != "2026-07-03T23:54:00Z" {
		t.Errorf("run row = %+v", row)
	}
}

func TestIntraCandleStopFillAndUTCRollover(t *testing.T) {
	spec, ds, lines := rolloverFixture(t)
	_, records, _, err := runOnce(t, spec, ds, lines)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	p := parseRecords(t, records)

	// Stable per-tick record order; tick 2's stop fill (sub-tick pump) is
	// recorded BEFORE tick 2's proposal.
	wantKinds := []string{
		"proposal", "verdict", // t0 hold
		"proposal", "verdict", "order", "position", // t1 entry
		"order", "proposal", "verdict", // t2: pumped stop fill, then hold
		"proposal", "verdict", "proposal", "verdict", "proposal", "verdict", // t3-t5
	}
	if !reflect.DeepEqual(p.kinds, wantKinds) {
		t.Fatalf("kinds = %v\nwant %v", p.kinds, wantKinds)
	}

	entry, stop := p.orders[0], p.orders[1]
	if entry.OrderID != DeterministicID("order/"+uid(1001)+"/entry") ||
		entry.Class != "ENTRY" || entry.Status != "filled" ||
		entry.FillPrice != "100" || entry.QtyBase != "1" || entry.ProposalID != uid(1001) {
		t.Errorf("entry order = %+v", entry)
	}
	if pos := p.positions[0]; pos.QtyBase != "1" || pos.EntryPrice != "100" {
		t.Errorf("position = %+v", pos)
	}
	// The stop is a PROTECTIVE stop-market: it triggers on the low sub-tick
	// and fills AT the observed low (93), zero slippage in this fixture.
	if stop.OrderID != DeterministicID("order/tick/2/low/0") ||
		stop.Class != "PROTECTIVE" || stop.Type != "stop" || !stop.ReduceOnly ||
		stop.Status != "filled" || stop.FillPrice != "93" || stop.ProposalID != "" {
		t.Errorf("stop order = %+v", stop)
	}

	v := p.verdicts
	// Tick 2 decision (23:57:01Z, same UTC day): the -7 realized loss and
	// the equity drop are visible to the gate.
	snap2 := v[2].LimitsSnapshot
	if snap2.DailyRealizedPnlQuote.Decimal().String() != "-7" ||
		snap2.EquityQuote.Decimal().String() != "9993" ||
		snap2.PeakEquityQuote.Decimal().String() != "10000" {
		t.Errorf("tick 2 snapshot = daily %s equity %s peak %s",
			snap2.DailyRealizedPnlQuote, snap2.EquityQuote, snap2.PeakEquityQuote)
	}
	if snap2.OpenPositionsCount != 0 {
		t.Errorf("tick 2 open positions = %d, want 0 (stop closed the book)", snap2.OpenPositionsCount)
	}
	// Tick 5 decision lands at 00:00:01Z the NEXT UTC day: daily realized
	// PnL rolls to zero, equity carries over.
	if v[5].EvaluatedAt.String() != "2026-07-04T00:00:01Z" {
		t.Errorf("tick 5 evaluated_at = %s", v[5].EvaluatedAt)
	}
	snap5 := v[5].LimitsSnapshot
	if snap5.DailyRealizedPnlQuote.Decimal().String() != "0" ||
		snap5.EquityQuote.Decimal().String() != "9993" {
		t.Errorf("tick 5 snapshot = daily %s equity %s (want rollover to 0, equity 9993)",
			snap5.DailyRealizedPnlQuote, snap5.EquityQuote)
	}
	for i, d := range []contract.Decision{"approve", "approve", "approve", "approve", "approve", "approve"} {
		if v[i].Decision != d {
			t.Errorf("verdict %d decision = %s, want %s", i, v[i].Decision, d)
		}
	}
}

func TestBoundaryMarkHasNoLookahead(t *testing.T) {
	spec := testSpec(t)
	// close_time(0) == open_time(1) and candle 1 gaps up to 110: the tick-0
	// decision MUST see close(0)=101, never candle 1's open.
	ds := writeTestDataset(t, []Kline{
		kl(testT0, "100", "101", "99", "101"),
		flat(1, "110"),
	})
	lines := []ProposalLine{
		prop(t, ds, 0, contract.ActionOpenLong, "100", "96"),
		prop(t, ds, 1, contract.ActionHold, "0", ""),
	}
	_, records, _, err := runOnce(t, spec, ds, lines)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	p := parseRecords(t, records)
	if got := p.verdicts[0].LimitsSnapshot.MarkPrice.Decimal().String(); got != "101" {
		t.Errorf("tick 0 mark = %s, want 101 (lookahead into candle 1's open)", got)
	}
	if p.orders[0].FillPrice != "101" {
		t.Errorf("entry fill = %s, want 101", p.orders[0].FillPrice)
	}
}

func TestGappedDatasetFailsClosed(t *testing.T) {
	spec := testSpec(t)
	ds := writeTestDataset(t, []Kline{flat(0, "100"), flat(1, "100"), flat(3, "100")})
	lines := []ProposalLine{
		prop(t, ds, 0, contract.ActionHold, "0", ""),
		prop(t, ds, 1, contract.ActionHold, "0", ""),
		prop(t, ds, 2, contract.ActionOpenLong, "100", "94"), // gap tick: stale mark
		prop(t, ds, 3, contract.ActionOpenLong, "100", "94"),
	}
	sum, records, _, err := runOnce(t, spec, ds, lines)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Status != StatusComplete {
		t.Fatalf("status = %s", sum.Status)
	}
	p := parseRecords(t, records)
	v2 := p.verdicts[2]
	if v2.Decision != contract.DecisionReject || len(v2.Reasons) == 0 ||
		v2.Reasons[0].Code != contract.CodeMarkPriceUnavailable {
		t.Errorf("gap tick verdict = %s %+v, want reject MARK_PRICE_UNAVAILABLE", v2.Decision, v2.Reasons)
	}
	v3 := p.verdicts[3]
	if v3.Decision != contract.DecisionApprove {
		t.Errorf("post-gap verdict = %s %+v, want approve (fresh mark resumes)", v3.Decision, v3.Reasons)
	}
	if len(p.orders) != 1 || p.orders[0].FillPrice != "100" {
		t.Errorf("orders = %+v, want single post-gap entry filled at 100", p.orders)
	}
}

func TestReplayErrorMarksRunFailed(t *testing.T) {
	spec := testSpec(t)
	ds := writeTestDataset(t, []Kline{flat(0, "100")})
	// Gate-approved but unexecutable: the size rounds to a zero base qty at
	// the mark, so SubmitEntry errors mid-replay.
	lines := []ProposalLine{prop(t, ds, 0, contract.ActionOpenLong, "0.00000001", "94")}
	sum, records, db, err := runOnce(t, spec, ds, lines)
	if err == nil {
		t.Fatal("run succeeded, want mid-replay error")
	}
	if sum.Status != StatusFailed {
		t.Errorf("summary status = %s, want failed", sum.Status)
	}
	row, ok, gerr := db.GetRun(sum.BacktestID)
	if gerr != nil || !ok || row.Status != StatusFailed {
		t.Errorf("run row = %+v ok=%v err=%v, want status failed", row, ok, gerr)
	}
	// The records prefix up to the failure is preserved in both sinks.
	p := parseRecords(t, records)
	if !reflect.DeepEqual(p.kinds, []string{"proposal", "verdict"}) {
		t.Errorf("kinds = %v, want [proposal verdict]", p.kinds)
	}
	if rows, err := db.Records(sum.BacktestID); err != nil || len(rows) != 2 {
		t.Errorf("db records = %d, %v; want 2 rows", len(rows), err)
	}
}

func TestRunRejectsMisalignedInputs(t *testing.T) {
	spec := testSpec(t)
	ds := writeTestDataset(t, []Kline{flat(0, "100"), flat(1, "100")})
	holds := func() []ProposalLine {
		return []ProposalLine{
			prop(t, ds, 0, contract.ActionHold, "0", ""),
			prop(t, ds, 1, contract.ActionHold, "0", ""),
		}
	}
	run := func(t *testing.T, proposals string) error {
		t.Helper()
		var out bytes.Buffer
		_, err := Run(spec, ds, strings.NewReader(proposals), &out, openTestDB(t))
		return err
	}
	valid := proposalsJSONL(t, spec, ds, holds()).String()

	t.Run("dataset sha mismatch", func(t *testing.T) {
		bad := strings.Replace(valid, ds.SHA256, strings.Repeat("0", 64), 1)
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "dataset_sha256") {
			t.Fatalf("error = %v, want dataset_sha256 mismatch", err)
		}
	})
	t.Run("missing meta line", func(t *testing.T) {
		_, rest, _ := strings.Cut(valid, "\n")
		if err := run(t, rest); err == nil {
			t.Fatal("accepted, want meta error")
		}
	})
	t.Run("tick out of order", func(t *testing.T) {
		lines := holds()
		lines[0], lines[1] = lines[1], lines[0]
		bad := proposalsJSONL(t, spec, ds, lines).String()
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "tick_number") {
			t.Fatalf("error = %v, want tick_number misalignment", err)
		}
	})
	t.Run("missing tick line", func(t *testing.T) {
		bad := proposalsJSONL(t, spec, ds, holds()[:1]).String()
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "proposal lines") {
			t.Fatalf("error = %v, want line-count mismatch", err)
		}
	})
	t.Run("foreign strategy", func(t *testing.T) {
		lines := holds()
		lines[1].Proposal.StrategyID = uid(999)
		bad := proposalsJSONL(t, spec, ds, lines).String()
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "strategy_id") {
			t.Fatalf("error = %v, want strategy_id mismatch", err)
		}
	})
	t.Run("foreign symbol", func(t *testing.T) {
		lines := holds()
		lines[1].Proposal.Symbol = "ETH/USDT"
		bad := proposalsJSONL(t, spec, ds, lines).String()
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "symbol") {
			t.Fatalf("error = %v, want per-line symbol mismatch", err)
		}
	})
	t.Run("created_at not T+1s", func(t *testing.T) {
		lines := holds()
		lines[1].Proposal.CreatedAt = contract.NewUTCTime(lines[1].Proposal.CreatedAt.Time().Add(time.Second))
		bad := proposalsJSONL(t, spec, ds, lines).String()
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "created_at") {
			t.Fatalf("error = %v, want created_at pin violation", err)
		}
	})
	t.Run("meta window missing or zero", func(t *testing.T) {
		bad := strings.Replace(valid, `"window":24`, `"window":0`, 1)
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "window") {
			t.Fatalf("error = %v, want meta window error", err)
		}
	})
	t.Run("meta scenario missing", func(t *testing.T) {
		bad := strings.Replace(valid, `"scenario":"bullish"`, `"scenario":""`, 1)
		if err := run(t, bad); err == nil || !strings.Contains(err.Error(), "scenario") {
			t.Fatalf("error = %v, want meta scenario error", err)
		}
	})
	t.Run("dataset does not match runspec", func(t *testing.T) {
		eth := writeTestDataset(t, []Kline{{Symbol: "ETH/USDT", Interval: "1m",
			OpenTime: testT0, Open: "1", High: "1", Low: "1", Close: "1", Volume: "1"}})
		var out bytes.Buffer
		_, err := Run(spec, eth, proposalsJSONL(t, spec, eth, nil), &out, openTestDB(t))
		if err == nil || !strings.Contains(err.Error(), "does not match runspec") {
			t.Fatalf("error = %v, want dataset/runspec mismatch", err)
		}
	})
}
