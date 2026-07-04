package backtest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// Kline is one dataset line. The struct field order IS the wire key order
// (encoding/json marshals in declaration order): the canonical dataset file
// must be byte-identical on both planes. open_time is ms since the Unix
// epoch; close_time is DERIVED (open_time + interval), never stored. Prices
// and volume are verbatim decimal strings (ADR-0003).
type Kline struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	OpenTime int64  `json:"open_time"`
	Open     string `json:"open"`
	High     string `json:"high"`
	Low      string `json:"low"`
	Close    string `json:"close"`
	Volume   string `json:"volume"`
}

// Dataset is a validated, ordered candle series for one (symbol, interval).
// Klines may be GAPPED (missing grid slots); tick numbers index the full
// grid from the first candle, so a gap is a tick with no candle.
type Dataset struct {
	Symbol   string
	Interval string
	SHA256   string // hex sha256 of the exact file bytes
	Klines   []Kline
}

// IntervalMS is the candle duration in ms.
func (d *Dataset) IntervalMS() int64 {
	return intervalSeconds[d.Interval] * 1000
}

// FirstOpenTime is the grid origin (ms).
func (d *Dataset) FirstOpenTime() int64 { return d.Klines[0].OpenTime }

// Ticks is the number of decision ticks: one per grid slot from the first
// through the last candle, INCLUDING gapped slots.
func (d *Dataset) Ticks() int {
	last := d.Klines[len(d.Klines)-1].OpenTime
	return int((last-d.FirstOpenTime())/d.IntervalMS()) + 1
}

// TickOf is the grid index of a candle open_time.
func (d *Dataset) TickOf(openTime int64) int {
	return int((openTime - d.FirstOpenTime()) / d.IntervalMS())
}

// ReadDataset reads and validates a canonical dataset file, recording the
// sha256 of its exact bytes.
func ReadDataset(path string) (*Dataset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	ds := &Dataset{SHA256: hex.EncodeToString(sum[:])}
	scanner := bufio.NewScanner(bytes.NewReader(b))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			return nil, fmt.Errorf("dataset %s line %d: empty line", path, lineNo)
		}
		var k Kline
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&k); err != nil {
			return nil, fmt.Errorf("dataset %s line %d: %w", path, lineNo, err)
		}
		if err := ds.appendValidated(k); err != nil {
			return nil, fmt.Errorf("dataset %s line %d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(ds.Klines) == 0 {
		return nil, fmt.Errorf("dataset %s: no klines", path)
	}
	return ds, nil
}

// appendValidated enforces the dataset invariants line by line: one
// (symbol, interval) per file, parseable decimals, strictly ascending
// grid-aligned open times.
func (ds *Dataset) appendValidated(k Kline) error {
	if len(ds.Klines) == 0 {
		if !symbolPattern.MatchString(k.Symbol) {
			return fmt.Errorf("symbol %q is not canonical BASE/QUOTE", k.Symbol)
		}
		if _, err := IntervalSeconds(k.Interval); err != nil {
			return err
		}
		ds.Symbol, ds.Interval = k.Symbol, k.Interval
	} else {
		if k.Symbol != ds.Symbol || k.Interval != ds.Interval {
			return fmt.Errorf("(%s, %s) does not match dataset (%s, %s)", k.Symbol, k.Interval, ds.Symbol, ds.Interval)
		}
		prev := ds.Klines[len(ds.Klines)-1].OpenTime
		if k.OpenTime <= prev {
			return fmt.Errorf("open_time %d not strictly ascending (previous %d)", k.OpenTime, prev)
		}
		if (k.OpenTime-ds.FirstOpenTime())%ds.IntervalMS() != 0 {
			return fmt.Errorf("open_time %d off the %s grid anchored at %d", k.OpenTime, ds.Interval, ds.FirstOpenTime())
		}
	}
	for _, f := range []struct{ name, v string }{
		{"open", k.Open}, {"high", k.High}, {"low", k.Low}, {"close", k.Close}, {"volume", k.Volume},
	} {
		// The strict ADR-0003 form (no signs, exponents, or leading zeros) —
		// the same rule the Python plane's DecimalStr enforces, so a file can
		// never pass one plane and fail the other.
		if _, err := contract.ParseDecimal(f.v); err != nil {
			return fmt.Errorf("%s %q: %w", f.name, f.v, err)
		}
	}
	ds.Klines = append(ds.Klines, k)
	return nil
}

// WriteDataset writes klines as the canonical dataset form: one compact-JSON
// line per candle in the pinned key order, LF-terminated. It returns the
// sha256 hex of the bytes written.
func WriteDataset(w io.Writer, klines []Kline) (string, error) {
	var buf bytes.Buffer
	ds := &Dataset{}
	for i, k := range klines {
		if err := ds.appendValidated(k); err != nil {
			return "", fmt.Errorf("kline %d: %w", i, err)
		}
		b, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if len(klines) == 0 {
		return "", fmt.Errorf("no klines to write")
	}
	sum := sha256.Sum256(buf.Bytes())
	if _, err := w.Write(buf.Bytes()); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}
