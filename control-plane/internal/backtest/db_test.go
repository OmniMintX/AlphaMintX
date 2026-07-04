package backtest

import (
	"reflect"
	"testing"
)

func TestKlinesCacheIdempotentAndRangeOrdered(t *testing.T) {
	db := openTestDB(t)
	klines := []Kline{flat(0, "100"), flat(1, "101"), flat(2, "102")}
	if err := db.InsertKlines(klines); err != nil {
		t.Fatalf("InsertKlines: %v", err)
	}
	// Refetch is a no-op, never a mutation (append-only cache).
	mutated := []Kline{flat(1, "999")}
	if err := db.InsertKlines(mutated); err != nil {
		t.Fatalf("InsertKlines refetch: %v", err)
	}
	got, err := db.Klines("BTC/USDT", "1m", testT0, testT0+60_000)
	if err != nil {
		t.Fatalf("Klines: %v", err)
	}
	if !reflect.DeepEqual(got, klines[:2]) {
		t.Errorf("range query = %+v, want first two originals", got)
	}
	if got, err := db.Klines("ETH/USDT", "1m", testT0, testT0+60_000); err != nil || got != nil {
		t.Errorf("unknown symbol = %+v, %v; want nil, nil", got, err)
	}
}

func TestRunLifecycle(t *testing.T) {
	db := openTestDB(t)
	row := RunRow{
		BacktestID: uid(1), StrategyID: testStrategyID, ConfigHash: "c", DatasetSHA256: "d",
		CodeVersion: "v", Seed: 42, MaskLevel: "M0", Status: StatusRunning,
		CreatedAt: "2026-07-03T23:54:00Z",
	}
	if err := db.InsertRun(row); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	if err := db.InsertRun(row); err == nil {
		t.Fatal("duplicate backtest_id accepted, want error")
	}
	got, ok, err := db.GetRun(uid(1))
	if err != nil || !ok {
		t.Fatalf("GetRun: ok=%v err=%v", ok, err)
	}
	if got != row {
		t.Errorf("GetRun = %+v, want %+v", got, row)
	}
	if err := db.FinishRun(uid(1), "exploded"); err == nil {
		t.Fatal("illegal terminal status accepted, want error")
	}
	if err := db.FinishRun(uid(1), StatusComplete); err != nil {
		t.Fatalf("FinishRun complete: %v", err)
	}
	// Terminal statuses are immutable.
	if err := db.FinishRun(uid(1), StatusFailed); err == nil {
		t.Fatal("complete -> failed accepted, want error")
	}
	if _, ok, err := db.GetRun(uid(99)); err != nil || ok {
		t.Fatalf("GetRun missing: ok=%v err=%v, want false, nil", ok, err)
	}
}

func TestRecordsAppendOnlyOrderedAndFKGuarded(t *testing.T) {
	db := openTestDB(t)
	if err := db.AppendRecord(uid(1), 0, "proposal", []byte(`{}`)); err == nil {
		t.Fatal("record without a run row accepted, want FK error")
	}
	if err := db.InsertRun(RunRow{BacktestID: uid(1), StrategyID: testStrategyID,
		ConfigHash: "c", DatasetSHA256: "d", CodeVersion: "v", Seed: 1, MaskLevel: "M0",
		Status: StatusRunning, CreatedAt: "2026-07-03T23:54:00Z"}); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	for i, kind := range []string{"proposal", "verdict", "order"} {
		if err := db.AppendRecord(uid(1), i, kind, []byte(`{"kind":"`+kind+`"}`)); err != nil {
			t.Fatalf("AppendRecord %d: %v", i, err)
		}
	}
	if err := db.AppendRecord(uid(1), 1, "verdict", []byte(`{}`)); err == nil {
		t.Fatal("duplicate (backtest_id, seq) accepted, want error")
	}
	got, err := db.Records(uid(1))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Records = %d rows, want 3", len(got))
	}
}
