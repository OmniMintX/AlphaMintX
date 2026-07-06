// Command betalog maintains the hash-chained beta operations log
// (docs/specs/beta-ops-tooling.md BL-1..BL-6, BP-9): `betalog append`
// takes an advisory flock, verifies the tail line only, and appends one
// JSONL entry whose prev is the SHA-256 of the previous line's exact
// bytes (excluding the trailing newline); `betalog verify` re-walks the
// whole chain and, with -prefix-of, checks the BL-6 byte-prefix custody
// property. Exit 0 clean, 1 findings/runtime failure, 2 usage error.
// betalog makes tampering DETECTABLE, not impossible (BL-5): daily
// off-host copies defeat regeneration, not cryptography. Entry text is
// written verbatim to the file only, never interpolated (BT-2).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// entry is one chain line (BL-1); the JSON field order is the declared
// order, and encoding/json sorts the refs map keys.
type entry struct {
	N    int               `json:"n"`
	Prev string            `json:"prev"`
	At   string            `json:"at"`
	Type string            `json:"type"`
	Text string            `json:"text"`
	Refs map[string]string `json:"refs"`
}

// genesisPrev is the prev of entry 1: 64 '0' characters.
var genesisPrev = strings.Repeat("0", 64)

// requiredRefs enforces BL-3: incident_ack/incident_resolve carry the
// notifier dedupe pair; limit_change joins V9 to the log via change_id.
// Types are otherwise an open set (correction is ordinary, BL-4a).
var requiredRefs = map[string][]string{
	"incident_ack":     {"source", "id"},
	"incident_resolve": {"source", "id"},
	"limit_change":     {"change_id"},
}

// refFlags is a repeatable -ref k=v flag.
type refFlags map[string]string

func (r refFlags) String() string { return "" }

func (r refFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("-ref wants k=v, got %q", v)
	}
	r[k] = val
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry (BT-1): dispatches the subcommand and
// returns the process exit code.
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "betalog: usage: betalog append|verify [flags]")
		return 2
	}
	switch args[0] {
	case "append":
		return runAppend(args[1:], out, errOut)
	case "verify":
		return runVerify(args[1:], out, errOut)
	}
	fmt.Fprintf(errOut, "betalog: unknown command %q (want append or verify)\n", args[0])
	return 2
}

// runAppend implements BL-2/BL-3: flock, tail check, single O_APPEND
// line, new entry's own SHA-256 to stdout.
func runAppend(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("betalog append", flag.ContinueOnError)
	fs.SetOutput(errOut)
	logPath := fs.String("log", "", "path to the beta log (REQUIRED)")
	typ := fs.String("type", "", "entry type (REQUIRED)")
	text := fs.String("text", "", "entry text (written verbatim to the file only)")
	refs := refFlags{}
	fs.Var(refs, "ref", "k=v reference (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *logPath == "" || *typ == "" {
		fmt.Fprintln(errOut, "betalog append: -log and -type are required")
		fs.Usage()
		return 2
	}
	for _, k := range requiredRefs[*typ] {
		if refs[k] == "" {
			fmt.Fprintf(errOut, "betalog append: type %q requires -ref %s=<value> (BL-3)\n", *typ, k)
			return 2
		}
	}
	sum, err := appendEntry(*logPath, *typ, *text, refs)
	if err != nil {
		fmt.Fprintf(errOut, "betalog append: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, sum)
	return 0
}

// appendEntry holds an exclusive advisory flock on the log for the
// whole read-tail/write cycle (two concurrent appends must not both
// read the same tail — BL-2) and returns the new line's own SHA-256.
func appendEntry(path, typ, text string, refs map[string]string) (string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("flock %s: %w", path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	n, prev := 1, genesisPrev
	if len(data) > 0 {
		if !bytes.HasSuffix(data, []byte("\n")) {
			return "", fmt.Errorf("%s: last line unterminated (truncated?); refusing to append", path)
		}
		last := lastLine(data)
		tail, err := parseEntry(last)
		if err != nil {
			return "", fmt.Errorf("%s: corrupt last line (%v); refusing to append", path, err)
		}
		n = tail.N + 1
		sum := sha256.Sum256(last)
		prev = hex.EncodeToString(sum[:])
	}
	// BL-4a: a duplicate ack is never appended — it would brick every
	// subsequent verify with no legal remediation (BL-5). The whole file
	// is already in memory under the flock; corrupt mid-file lines are
	// skipped here (tail-only append rule) and left for verify.
	if typ == "incident_ack" {
		for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			e, err := parseEntry(line)
			if err != nil {
				continue
			}
			if e.Type == "incident_ack" && e.Refs["source"] == refs["source"] && e.Refs["id"] == refs["id"] {
				return "", fmt.Errorf("duplicate incident_ack for (source=%s, id=%s): first ack is entry %d; append a correction instead (BL-4a)", refs["source"], refs["id"], e.N)
			}
		}
	}
	// at is tool-generated, never operator-supplied (BL-2).
	e := entry{N: n, Prev: prev, At: time.Now().UTC().Format(time.RFC3339Nano), Type: typ, Text: text, Refs: refs}
	line, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	own := sha256.Sum256(line)
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", fmt.Errorf("append %s: %w", path, err)
	}
	return hex.EncodeToString(own[:]), nil
}

// runVerify implements BL-4/BL-6: re-walk the whole chain; with
// -prefix-of, additionally check the byte-prefix custody property.
func runVerify(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("betalog verify", flag.ContinueOnError)
	fs.SetOutput(errOut)
	logPath := fs.String("log", "", "path to the beta log (REQUIRED)")
	prefixOf := fs.String("prefix-of", "", "later copy this log must be a byte-prefix of (BL-6)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *logPath == "" {
		fmt.Fprintln(errOut, "betalog verify: -log <path> is required")
		fs.Usage()
		return 2
	}
	data, err := os.ReadFile(*logPath)
	if err != nil {
		fmt.Fprintf(errOut, "betalog verify: %v\n", err)
		return 1
	}
	count, err := verifyChain(data, errOut)
	if err != nil {
		fmt.Fprintf(errOut, "betalog verify: %v\n", err)
		return 1
	}
	if *prefixOf != "" {
		other, err := os.ReadFile(*prefixOf)
		if err != nil {
			fmt.Fprintf(errOut, "betalog verify: %v\n", err)
			return 1
		}
		if !bytes.HasPrefix(other, data) {
			fmt.Fprintf(errOut, "betalog verify: %s is not a byte-prefix of %s (BL-6)\n", *logPath, *prefixOf)
			return 1
		}
	}
	fmt.Fprintf(out, "verify: ok (%d entries)\n", count)
	return 0
}

// verifyChain walks every line: parse, n = 1,2,3,... with no gap, prev
// matches the previous line's bytes, no duplicate (incident_ack,
// refs.source, refs.id) triple. A timestamp earlier than its
// predecessor is a WARNING on warn, not a failure (NTP slew is legal).
func verifyChain(data []byte, warn io.Writer) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		return 0, fmt.Errorf("line %d: unterminated (truncated?)", bytes.Count(data, []byte("\n"))+1)
	}
	lines := bytes.Split(data[:len(data)-1], []byte("\n"))
	prevHash := genesisPrev
	var prevAt time.Time
	acks := make(map[string]int)
	for i, line := range lines {
		ln := i + 1
		e, err := parseEntry(line)
		if err != nil {
			return 0, fmt.Errorf("line %d: %v", ln, err)
		}
		if e.N != ln {
			return 0, fmt.Errorf("line %d: n = %d, want %d", ln, e.N, ln)
		}
		if e.Prev != prevHash {
			return 0, fmt.Errorf("line %d: prev = %s, want %s (chain break)", ln, e.Prev, prevHash)
		}
		at, _ := time.Parse(time.RFC3339, e.At) // shape checked by parseEntry
		if ln > 1 && at.Before(prevAt) {
			fmt.Fprintf(warn, "warning: line %d: at %s earlier than predecessor (NTP slew?)\n", ln, e.At)
		}
		if e.Type == "incident_ack" {
			key := e.Refs["source"] + "\x00" + e.Refs["id"]
			if first, dup := acks[key]; dup {
				return 0, fmt.Errorf("line %d: duplicate incident_ack (source=%s, id=%s), first at line %d",
					ln, e.Refs["source"], e.Refs["id"], first)
			}
			acks[key] = ln
		}
		sum := sha256.Sum256(line)
		prevHash = hex.EncodeToString(sum[:])
		prevAt = at
	}
	return len(lines), nil
}

// parseEntry decodes one line strictly and checks the BL-1 shape:
// exactly the six declared fields, no more, no fewer.
func parseEntry(line []byte) (entry, error) {
	var e entry
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return e, err
	}
	for _, k := range []string{"n", "prev", "at", "type", "text", "refs"} {
		if _, ok := raw[k]; !ok {
			return e, fmt.Errorf("missing field %q (BL-1)", k)
		}
	}
	if len(raw) != 6 {
		return e, fmt.Errorf("%d fields, want exactly 6 (BL-1)", len(raw))
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return e, err
	}
	if dec.More() {
		return e, fmt.Errorf("trailing data after JSON object")
	}
	if e.N < 1 {
		return e, fmt.Errorf("n = %d, want >= 1", e.N)
	}
	if len(e.Prev) != 64 {
		return e, fmt.Errorf("prev has %d characters, want 64", len(e.Prev))
	}
	if _, err := hex.DecodeString(e.Prev); err != nil {
		return e, fmt.Errorf("prev is not hex: %v", err)
	}
	if e.Type == "" {
		return e, fmt.Errorf("empty type")
	}
	if _, err := time.Parse(time.RFC3339, e.At); err != nil {
		return e, fmt.Errorf("at: %v", err)
	}
	if !strings.HasSuffix(e.At, "Z") {
		return e, fmt.Errorf("at %q: not UTC Z (BT-1)", e.At)
	}
	return e, nil
}

// lastLine returns the final line's bytes, excluding the trailing
// newline (the BL-1 hash input).
func lastLine(data []byte) []byte {
	data = bytes.TrimSuffix(data, []byte("\n"))
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		return data[i+1:]
	}
	return data
}
