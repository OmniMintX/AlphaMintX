package backtest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDatasetRoundTripCanonicalBytes(t *testing.T) {
	klines := []Kline{
		kl(testT0, "100.50", "101.00", "99.90", "100.10"),
		flat(1, "100.10"),
		flat(3, "100.20"), // gap at slot 2 is legal (grid-aligned)
	}
	var buf bytes.Buffer
	sha, err := WriteDataset(&buf, klines)
	if err != nil {
		t.Fatalf("WriteDataset: %v", err)
	}
	// Pinned wire key order: identical bytes on both planes.
	first, _, _ := strings.Cut(buf.String(), "\n")
	want := `{"symbol":"BTC/USDT","interval":"1m","open_time":1783122840000,` +
		`"open":"100.50","high":"101.00","low":"99.90","close":"100.10","volume":"1"}`
	if first != want {
		t.Errorf("canonical line:\n got %s\nwant %s", first, want)
	}

	path := filepath.Join(t.TempDir(), "ds.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ds, err := ReadDataset(path)
	if err != nil {
		t.Fatalf("ReadDataset: %v", err)
	}
	if ds.SHA256 != sha {
		t.Errorf("sha256 = %s, want %s (WriteDataset)", ds.SHA256, sha)
	}
	if ds.Symbol != "BTC/USDT" || ds.Interval != "1m" {
		t.Errorf("(%s, %s), want (BTC/USDT, 1m)", ds.Symbol, ds.Interval)
	}
	if !reflect.DeepEqual(ds.Klines, klines) {
		t.Errorf("klines round-trip mismatch: %+v", ds.Klines)
	}
	if got := ds.Ticks(); got != 4 { // slots 0..3 including the gap
		t.Errorf("Ticks() = %d, want 4", got)
	}
	if got := ds.TickOf(testT0 + 3*60_000); got != 3 {
		t.Errorf("TickOf(slot 3) = %d, want 3", got)
	}
}

func TestReadDatasetRejections(t *testing.T) {
	valid := func() []Kline { return []Kline{flat(0, "100"), flat(1, "101")} }
	tests := []struct {
		name    string
		mutate  func([]Kline) []Kline
		wantErr string
	}{
		{"descending", func(k []Kline) []Kline { k[1].OpenTime = k[0].OpenTime - 60_000; return k },
			"strictly ascending"},
		{"duplicate open_time", func(k []Kline) []Kline { k[1].OpenTime = k[0].OpenTime; return k },
			"strictly ascending"},
		{"off grid", func(k []Kline) []Kline { k[1].OpenTime += 1; return k }, "off the 1m grid"},
		{"symbol mismatch", func(k []Kline) []Kline { k[1].Symbol = "ETH/USDT"; return k },
			"does not match dataset"},
		{"interval mismatch", func(k []Kline) []Kline { k[1].Interval = "5m"; return k },
			"does not match dataset"},
		{"bad decimal", func(k []Kline) []Kline { k[0].Low = "oops"; return k }, "low"},
		{"negative decimal", func(k []Kline) []Kline { k[0].Low = "-1"; return k }, "low"},
		{"exponent decimal", func(k []Kline) []Kline { k[0].High = "1e5"; return k }, "high"},
		{"bad symbol", func(k []Kline) []Kline { k[0].Symbol = "btcusdt"; return k },
			"not canonical"},
		{"bad interval", func(k []Kline) []Kline { k[0].Interval = "7m"; return k },
			"unknown interval"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			for _, k := range tc.mutate(valid()) {
				b, err := json.Marshal(k)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				buf.Write(b)
				buf.WriteByte('\n')
			}
			path := filepath.Join(t.TempDir(), "ds.jsonl")
			if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := ReadDataset(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ReadDataset error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	for name, content := range map[string]string{
		"empty file":  "",
		"empty line":  "\n" + `{"symbol":"BTC/USDT"}` + "\n",
		"unknown key": `{"symbol":"BTC/USDT","interval":"1m","open_time":1,"open":"1","high":"1","low":"1","close":"1","volume":"1","close_time":2}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ds.jsonl")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := ReadDataset(path); err == nil {
				t.Fatal("accepted, want error")
			}
		})
	}
}

func TestWriteDatasetRejectsInvalid(t *testing.T) {
	if _, err := WriteDataset(&bytes.Buffer{}, nil); err == nil {
		t.Fatal("empty klines accepted, want error")
	}
	bad := []Kline{flat(0, "100"), flat(0, "100")}
	if _, err := WriteDataset(&bytes.Buffer{}, bad); err == nil {
		t.Fatal("duplicate open_time accepted, want error")
	}
}
