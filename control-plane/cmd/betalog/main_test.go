package main

// betalog CLI tests (docs/specs/beta-ops-tooling.md §Acceptance,
// betalog bullet): round-trip, tamper detection with line numbers,
// broken-tail refusal, required refs, duplicate acks, -prefix-of, and
// flock-serialized concurrent appends.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func appendArgs(log, typ, text string, refs ...string) []string {
	args := []string{"append", "-log", log, "-type", typ, "-text", text}
	for _, r := range refs {
		args = append(args, "-ref", r)
	}
	return args
}

// mustAppend runs one append, requires exit 0, and returns the hash
// printed to stdout.
func mustAppend(t *testing.T, log, typ, text string, refs ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := run(appendArgs(log, typ, text, refs...), &out, &errOut); code != 0 {
		t.Fatalf("append exit = %d (stderr %q), want 0", code, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func runVerifyArgs(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(append([]string{"verify"}, args...), &out, &errOut)
	return code, errOut.String()
}

// buildChain appends n note entries and returns the log path.
func buildChain(t *testing.T, n int) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "beta.log")
	for i := 1; i <= n; i++ {
		mustAppend(t, log, "note", fmt.Sprintf("entry-%d", i))
	}
	return log
}

func readLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func writeLines(t *testing.T, log string, lines []string) {
	t.Helper()
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestAppendVerifyRoundTrip: several entries including refs; the
// printed hash is the new line's own SHA-256; verify exits 0.
func TestAppendVerifyRoundTrip(t *testing.T) {
	log := filepath.Join(t.TempDir(), "beta.log")
	mustAppend(t, log, "note", "beta start")
	mustAppend(t, log, "incident_ack", "acked pd alert", "source=pagerduty", "id=42")
	sum := mustAppend(t, log, "limit_change", "cap tightened", "change_id=ch-1")

	lines := readLines(t, log)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	want := sha256.Sum256([]byte(lines[2]))
	if sum != hex.EncodeToString(want[:]) {
		t.Errorf("stdout hash %s != last line SHA-256 %s", sum, hex.EncodeToString(want[:]))
	}
	var first, second entry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1: %v", err)
	}
	if first.N != 1 || first.Prev != strings.Repeat("0", 64) {
		t.Errorf("entry 1 = {n:%d prev:%s}, want genesis", first.N, first.Prev)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2: %v", err)
	}
	if second.Refs["source"] != "pagerduty" || second.Refs["id"] != "42" {
		t.Errorf("refs did not round-trip: %v", second.Refs)
	}
	if code, stderr := runVerifyArgs(t, "-log", log); code != 0 {
		t.Fatalf("verify exit = %d (stderr %q), want 0", code, stderr)
	}
}

// TestTamperDetection: byte edit mid-file, line delete, line reorder,
// and truncation are each detected by verify with a line number.
func TestTamperDetection(t *testing.T) {
	cases := []struct {
		name     string
		tamper   func([]string) []string
		wantLine string
	}{
		{"byte edit mid-file", func(lines []string) []string {
			lines[1] = strings.Replace(lines[1], "entry-2", "entry-X", 1)
			return lines
		}, "line 3"},
		{"line delete", func(lines []string) []string {
			return append(lines[:1], lines[2:]...)
		}, "line 2"},
		{"line reorder", func(lines []string) []string {
			lines[1], lines[2] = lines[2], lines[1]
			return lines
		}, "line 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := buildChain(t, 5)
			writeLines(t, log, tc.tamper(readLines(t, log)))
			code, stderr := runVerifyArgs(t, "-log", log)
			if code != 1 {
				t.Fatalf("verify exit = %d (stderr %q), want 1", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantLine) {
				t.Errorf("stderr %q missing %q", stderr, tc.wantLine)
			}
		})
	}

	t.Run("truncation", func(t *testing.T) {
		log := buildChain(t, 5)
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if err := os.WriteFile(log, data[:len(data)-10], 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		code, stderr := runVerifyArgs(t, "-log", log)
		if code != 1 {
			t.Fatalf("verify exit = %d (stderr %q), want 1", code, stderr)
		}
		if !strings.Contains(stderr, "line 5") {
			t.Errorf("stderr %q missing %q", stderr, "line 5")
		}
	})
}

// TestBrokenTailAppendRefusal: a corrupt last line makes append refuse
// (exit 1) and leave the file unchanged.
func TestBrokenTailAppendRefusal(t *testing.T) {
	log := buildChain(t, 3)
	lines := readLines(t, log)
	lines[2] = `{"not": "an entry"`
	writeLines(t, log, lines)
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := run(appendArgs(log, "note", "after corruption"), &out, &errOut); code != 1 {
		t.Fatalf("append exit = %d (stderr %q), want 1", code, errOut.String())
	}
	after, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("file changed on refused append")
	}
}

// TestRequiredRefsEnforcement: incident_ack/incident_resolve without
// source+id and limit_change without change_id exit 2 with nothing
// appended (BL-3).
func TestRequiredRefsEnforcement(t *testing.T) {
	log := buildChain(t, 1)
	before, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	cases := [][]string{
		appendArgs(log, "incident_ack", "no refs"),
		appendArgs(log, "incident_ack", "missing id", "source=pagerduty"),
		appendArgs(log, "incident_resolve", "missing source", "id=42"),
		appendArgs(log, "limit_change", "no change_id"),
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) exit = %d (stderr %q), want 2", args, code, errOut.String())
		}
	}
	after, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("file changed on refused appends")
	}
}

// TestDuplicateIncidentAckDetected: append allows the duplicate (it
// verifies the tail only); verify reports it with the line number.
func TestDuplicateIncidentAckDetected(t *testing.T) {
	log := filepath.Join(t.TempDir(), "beta.log")
	mustAppend(t, log, "incident_ack", "first ack", "source=pagerduty", "id=42")
	mustAppend(t, log, "note", "between")
	mustAppend(t, log, "incident_ack", "dup ack", "source=pagerduty", "id=42")

	code, stderr := runVerifyArgs(t, "-log", log)
	if code != 1 {
		t.Fatalf("verify exit = %d (stderr %q), want 1", code, stderr)
	}
	if !strings.Contains(stderr, "duplicate incident_ack") || !strings.Contains(stderr, "line 3") {
		t.Errorf("stderr %q missing duplicate report with line number", stderr)
	}
}

// TestPrefixOf: a daily copy is a byte-prefix of the later final log
// (exit 0); a regenerated chain — same entries re-appended fresh, so
// different at/hashes — fails the prefix property (exit 1, BL-6).
func TestPrefixOf(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "final.log")
	for i := 1; i <= 3; i++ {
		mustAppend(t, final, "note", fmt.Sprintf("entry-%d", i))
	}
	daily := filepath.Join(dir, "daily.log")
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(daily, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustAppend(t, final, "note", "entry-4")
	mustAppend(t, final, "note", "entry-5")

	if code, stderr := runVerifyArgs(t, "-log", daily, "-prefix-of", final); code != 0 {
		t.Fatalf("true prefix: verify exit = %d (stderr %q), want 0", code, stderr)
	}

	regen := filepath.Join(dir, "regen.log")
	for i := 1; i <= 5; i++ {
		mustAppend(t, regen, "note", fmt.Sprintf("entry-%d", i))
	}
	code, stderr := runVerifyArgs(t, "-log", daily, "-prefix-of", regen)
	if code != 1 {
		t.Fatalf("regenerated chain: verify exit = %d (stderr %q), want 1", code, stderr)
	}
	if !strings.Contains(stderr, "byte-prefix") {
		t.Errorf("stderr %q missing prefix report", stderr)
	}
}

// TestConcurrentAppends: two goroutines x 20 appends; the flock keeps
// the chain intact — verify passes and n is exactly 1..40.
func TestConcurrentAppends(t *testing.T) {
	log := filepath.Join(t.TempDir(), "beta.log")
	var wg sync.WaitGroup
	errs := make(chan string, 40)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				var out, errOut bytes.Buffer
				if code := run(appendArgs(log, "note", fmt.Sprintf("g%d-%d", g, i)), &out, &errOut); code != 0 {
					errs <- fmt.Sprintf("append exit = %d (stderr %q)", code, errOut.String())
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	if code, stderr := runVerifyArgs(t, "-log", log); code != 0 {
		t.Fatalf("verify exit = %d (stderr %q), want 0", code, stderr)
	}
	lines := readLines(t, log)
	if len(lines) != 40 {
		t.Fatalf("got %d lines, want 40", len(lines))
	}
	seen := make(map[int]bool, 40)
	for i, line := range lines {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if seen[e.N] {
			t.Errorf("duplicate n = %d", e.N)
		}
		seen[e.N] = true
	}
	for n := 1; n <= 40; n++ {
		if !seen[n] {
			t.Errorf("missing n = %d", n)
		}
	}
}

// TestCorrectionEntry: type=correction with -ref supersedes=<n> is an
// ordinary entry (BL-4a) — appends fine, verify stays clean.
func TestCorrectionEntry(t *testing.T) {
	log := filepath.Join(t.TempDir(), "beta.log")
	mustAppend(t, log, "incident_ack", "acked with wrong runbook", "source=pagerduty", "id=7")
	mustAppend(t, log, "correction", "entry 1 named the wrong runbook", "supersedes=1")

	if code, stderr := runVerifyArgs(t, "-log", log); code != 0 {
		t.Fatalf("verify exit = %d (stderr %q), want 0", code, stderr)
	}
	var e entry
	if err := json.Unmarshal([]byte(readLines(t, log)[1]), &e); err != nil {
		t.Fatalf("line 2: %v", err)
	}
	if e.Type != "correction" || e.Refs["supersedes"] != "1" {
		t.Errorf("correction entry = {type:%s refs:%v}, want supersedes=1", e.Type, e.Refs)
	}
}
