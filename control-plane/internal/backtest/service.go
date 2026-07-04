package backtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime/debug"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// MetaLine is the REQUIRED first proposals.jsonl line: the emitter's pinned
// run identity. Replay refuses to start unless it matches the runspec and
// the dataset actually read (fail-closed alignment; no line-index guessing).
type MetaLine struct {
	Kind          string `json:"kind"` // "backtest_meta"
	StrategyID    string `json:"strategy_id"`
	Symbol        string `json:"symbol"`
	Interval      string `json:"interval"`
	DatasetSHA256 string `json:"dataset_sha256"`
	Seed          int64  `json:"seed"`
	MaskLevel     string `json:"mask_level"`
	// Window and Scenario are the emitter's stage-1 knobs, recorded so the
	// proposals.jsonl artifact alone is enough to re-run stage 1
	// (reproducibility); the replay validates their presence, not their value.
	Window   int    `json:"window"`
	Scenario string `json:"scenario"`
}

// ProposalLine is one decision-tick line: the explicit tick_number is the
// alignment key (backtest-engine.md: no line-index alignment) and
// snapshot_sha256 is the recorded pipeline input hash consumed by the M2
// lookahead comparison (carried, not interpreted, by the replay).
type ProposalLine struct {
	TickNumber     *int              `json:"tick_number"`
	SnapshotSHA256 string            `json:"snapshot_sha256"`
	Proposal       contract.Proposal `json:"proposal"`
}

// Summary is the replay result.
type Summary struct {
	BacktestID    string
	DatasetSHA256 string
	ConfigHash    string
	Ticks         int
	Records       int
	Status        string
}

// Run replays proposals.jsonl over the dataset through a fresh gate + paper
// OMS, teeing byte-identical records to out (records.jsonl) and the
// backtest_records table. The run row is inserted 'running' up front and
// finishes 'complete' or 'failed'; a failed run keeps its records prefix.
func Run(spec *RunSpec, ds *Dataset, proposals io.Reader, out io.Writer, db *DB) (Summary, error) {
	if ds.Symbol != spec.Symbol || ds.Interval != spec.Interval {
		return Summary{}, fmt.Errorf("dataset (%s, %s) does not match runspec (%s, %s)",
			ds.Symbol, ds.Interval, spec.Symbol, spec.Interval)
	}
	meta, lines, err := readProposalLines(proposals, spec, ds)
	if err != nil {
		return Summary{}, err
	}
	backtestID := DeterministicID(fmt.Sprintf("backtest/%s/%s/%d/%s",
		meta.DatasetSHA256, spec.ConfigHash, spec.Seed, spec.MaskLevel))
	sum := Summary{BacktestID: backtestID, DatasetSHA256: ds.SHA256, ConfigHash: spec.ConfigHash, Ticks: ds.Ticks()}
	// created_at derives from the virtual clock (the grid origin), never
	// the wall clock: two runs over the same inputs are byte-identical.
	if err := db.InsertRun(RunRow{
		BacktestID: backtestID, StrategyID: spec.StrategyID, ConfigHash: spec.ConfigHash,
		DatasetSHA256: ds.SHA256, CodeVersion: codeVersion(), Seed: spec.Seed,
		MaskLevel: spec.MaskLevel, Status: StatusRunning,
		CreatedAt: time.UnixMilli(ds.FirstOpenTime()).UTC().Format("2006-01-02T15:04:05Z"),
	}); err != nil {
		return Summary{}, err
	}
	rec := &recorder{out: out, db: db, id: backtestID}
	if err := replay(spec, ds, lines, rec); err != nil {
		sum.Records, sum.Status = rec.seq, StatusFailed
		if ferr := db.FinishRun(backtestID, StatusFailed); ferr != nil {
			return sum, fmt.Errorf("%w (and marking failed: %v)", err, ferr)
		}
		return sum, err
	}
	sum.Records, sum.Status = rec.seq, StatusComplete
	return sum, db.FinishRun(backtestID, StatusComplete)
}

// readProposalLines parses and validates the meta line plus exactly one
// proposal line per grid tick, in tick order.
func readProposalLines(r io.Reader, spec *RunSpec, ds *Dataset) (MetaLine, []ProposalLine, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var meta MetaLine
	lines := make([]ProposalLine, 0, ds.Ticks())
	lineNo := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		lineNo++
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if lineNo == 1 {
			if err := dec.Decode(&meta); err != nil {
				return meta, nil, fmt.Errorf("proposals line 1 (meta): %w", err)
			}
			if err := checkMeta(meta, spec, ds); err != nil {
				return meta, nil, err
			}
			continue
		}
		var pl ProposalLine
		if err := dec.Decode(&pl); err != nil {
			return meta, nil, fmt.Errorf("proposals line %d: %w", lineNo, err)
		}
		if pl.TickNumber == nil {
			return meta, nil, fmt.Errorf("proposals line %d: tick_number is REQUIRED", lineNo)
		}
		if want := len(lines); *pl.TickNumber != want {
			return meta, nil, fmt.Errorf("proposals line %d: tick_number %d, want %d (one line per tick, in order)", lineNo, *pl.TickNumber, want)
		}
		if pl.Proposal.StrategyID != spec.StrategyID {
			return meta, nil, fmt.Errorf("proposals line %d: strategy_id %s, want %s", lineNo, pl.Proposal.StrategyID, spec.StrategyID)
		}
		// Single-symbol runs are a v1 invariant: the replay pumps marks and
		// tracks realized PnL for spec.Symbol only, so a foreign-symbol
		// proposal must fail closed here, not drift through the gate.
		if pl.Proposal.Symbol != spec.Symbol {
			return meta, nil, fmt.Errorf("proposals line %d: symbol %s, want %s", lineNo, pl.Proposal.Symbol, spec.Symbol)
		}
		// created_at is pinned to the virtual clock: T(t) + 1s exactly
		// (backtest-engine.md §Clock model). Anything else means the emitter
		// and replay disagree about the clock — fail closed, never let the
		// gate's coarse staleness window (60s) absorb the drift.
		wantCreated := time.UnixMilli(ds.FirstOpenTime() + int64(*pl.TickNumber+1)*ds.IntervalMS()).UTC().Add(time.Second)
		if !pl.Proposal.CreatedAt.Time().Equal(wantCreated) {
			return meta, nil, fmt.Errorf("proposals line %d: created_at %s, want %s (T+1s under the virtual clock)",
				lineNo, pl.Proposal.CreatedAt.Time().UTC().Format(time.RFC3339), wantCreated.Format(time.RFC3339))
		}
		lines = append(lines, pl)
	}
	if err := scanner.Err(); err != nil {
		return meta, nil, err
	}
	if lineNo == 0 {
		return meta, nil, fmt.Errorf("proposals: missing meta line")
	}
	if len(lines) != ds.Ticks() {
		return meta, nil, fmt.Errorf("proposals: %d proposal lines for %d dataset ticks", len(lines), ds.Ticks())
	}
	return meta, lines, nil
}

// checkMeta pins the emitter's identity against the runspec and dataset.
func checkMeta(m MetaLine, spec *RunSpec, ds *Dataset) error {
	switch {
	case m.Kind != "backtest_meta":
		return fmt.Errorf(`proposals meta: kind %q, want "backtest_meta"`, m.Kind)
	case m.StrategyID != spec.StrategyID:
		return fmt.Errorf("proposals meta: strategy_id %s, want %s", m.StrategyID, spec.StrategyID)
	case m.Symbol != spec.Symbol || m.Interval != spec.Interval:
		return fmt.Errorf("proposals meta: (%s, %s), want (%s, %s)", m.Symbol, m.Interval, spec.Symbol, spec.Interval)
	case m.DatasetSHA256 != ds.SHA256:
		return fmt.Errorf("proposals meta: dataset_sha256 %s does not match dataset %s", m.DatasetSHA256, ds.SHA256)
	case m.Seed != spec.Seed:
		return fmt.Errorf("proposals meta: seed %d, want %d", m.Seed, spec.Seed)
	case m.MaskLevel != spec.MaskLevel:
		return fmt.Errorf("proposals meta: mask_level %s, want %s", m.MaskLevel, spec.MaskLevel)
	case m.Window < 1:
		return fmt.Errorf("proposals meta: window %d must be >= 1", m.Window)
	case m.Scenario == "":
		return fmt.Errorf("proposals meta: scenario is REQUIRED")
	}
	return nil
}

// codeVersion is the VCS revision baked into the binary, or "unknown"
// (test binaries and non-VCS builds).
func codeVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
