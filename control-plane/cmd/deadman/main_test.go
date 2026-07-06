package main

// deadman tests (docs/specs/beta-ops-tooling.md §Acceptance, deadman
// bullet): bearer gate, raw-log line shapes and append-before-200,
// JSONL integrity under concurrent POSTs, DM-3 alarm episodes under a
// fake clock, tracker seeding from an existing raw log, and the DM-4
// selftest that can never reset the tracker.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBearer = "test-bearer-2f0c1a"

// fakeClock is a goroutine-safe manual clock for the DM-3 tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newDeadman builds a server (H=1, so the DM-3 threshold is 2h) over
// logPath with long loop intervals — tests drive ticks directly.
func newDeadman(t *testing.T, now func() time.Time, logPath string) (*server, *httptest.Server) {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	s, err := newServer(serverConfig{
		bearer:         testBearer,
		logPath:        logPath,
		heartbeatHours: 1,
		now:            now,
		aliveEvery:     time.Hour,
		alarmEvery:     time.Hour,
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() { s.close() })
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

// post sends one request; auth is the literal Authorization header value
// (empty = no header).
func post(t *testing.T, url, auth string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// logLines parses the raw log, failing on any line that is not a JSON
// object — JSONL integrity is itself an assertion.
func logLines(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var out []map[string]json.RawMessage
	for i, ln := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ln, &m); err != nil {
			t.Fatalf("raw-log line %d is not a JSON object: %v (%q)", i+1, err, ln)
		}
		out = append(out, m)
	}
	return out
}

func countKey(lines []map[string]json.RawMessage, key string) int {
	n := 0
	for _, m := range lines {
		if _, ok := m[key]; ok {
			n++
		}
	}
	return n
}

// heartbeatBody is an AN-14a notifier heartbeat envelope.
func heartbeatBody(ts string) []byte {
	return []byte(fmt.Sprintf(`{"schema":"alphamintx.safety-event.v1","source":"notifier","id":"heartbeat-%s","seq":0,"delivered_at":%q,"event":{"kind":"notifier_heartbeat"}}`, ts, ts))
}

// TestUsageErrors: missing DEADMAN_BEARER refuses to start (exit 2,
// DM-1); -selftest/-target must travel together (DM-4); missing server
// flags are usage errors.
func TestUsageErrors(t *testing.T) {
	t.Setenv("DEADMAN_BEARER", "")
	var out, errOut bytes.Buffer
	args := []string{"-listen", "127.0.0.1:0", "-raw-log", filepath.Join(t.TempDir(), "raw.jsonl"), "-heartbeat-hours", "24"}
	if code := run(args, &out, &errOut); code != 2 {
		t.Fatalf("no-bearer exit = %d, want 2", code)
	}
	t.Setenv("DEADMAN_BEARER", testBearer)
	for _, args := range [][]string{
		{"-target", "http://127.0.0.1:1"},
		{"-selftest"},
		{"-listen", "127.0.0.1:0"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}

// TestBearerAcceptReject: the exact bearer gets 200 with the envelope
// line already on disk when the response returns; a wrong or missing
// bearer gets 401 plus an auth_reject line, no envelope line, and no
// header echo (DM-1, DM-2, BT-2).
func TestBearerAcceptReject(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	_, ts := newDeadman(t, nil, logPath)
	body := heartbeatBody("2026-07-06T00:00:00Z")
	if code := post(t, ts.URL, "Bearer "+testBearer, body); code != http.StatusOK {
		t.Fatalf("accept status = %d, want 200", code)
	}
	lines := logLines(t, logPath)
	if got := countKey(lines, "envelope"); got != 1 {
		t.Fatalf("envelope lines = %d, want 1", got)
	}
	last := lines[len(lines)-1]
	if _, ok := last["received_at"]; !ok {
		t.Error("envelope line missing received_at")
	}
	var env struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(last["envelope"], &env); err != nil || env.Source != "notifier" {
		t.Errorf("logged envelope = %s (err %v), want the verbatim notifier body", last["envelope"], err)
	}

	if code := post(t, ts.URL, "Bearer wrong-bearer-value", body); code != http.StatusUnauthorized {
		t.Fatalf("reject status = %d, want 401", code)
	}
	if code := post(t, ts.URL, "", body); code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", code)
	}
	lines = logLines(t, logPath)
	if got := countKey(lines, "auth_reject"); got != 2 {
		t.Fatalf("auth_reject lines = %d, want 2", got)
	}
	if got := countKey(lines, "envelope"); got != 1 {
		t.Errorf("envelope lines after rejects = %d, want 1", got)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "wrong-bearer-value") {
		t.Error("auth_reject echoed the presented header (DM-1 forbids)")
	}
	if strings.Contains(string(raw), testBearer) {
		t.Error("raw log contains the bearer (BT-2)")
	}
}

// TestOversizedBody: a body over 1 MiB is rejected non-200 and no
// received_at line is appended (DM-1).
func TestOversizedBody(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	_, ts := newDeadman(t, nil, logPath)
	body := bytes.Repeat([]byte("a"), maxBody+1)
	if code := post(t, ts.URL, "Bearer "+testBearer, body); code == http.StatusOK {
		t.Fatalf("oversized body status = %d, want non-200", code)
	}
	if got := countKey(logLines(t, logPath), "received_at"); got != 0 {
		t.Errorf("received_at lines = %d, want 0 (oversized body must not be logged as a delivery)", got)
	}
}

// TestInvalidJSONBody: a non-JSON body is still logged (base64 under
// raw_b64) and answered 400 (DM-2).
func TestInvalidJSONBody(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	_, ts := newDeadman(t, nil, logPath)
	body := []byte("this is not json")
	if code := post(t, ts.URL, "Bearer "+testBearer, body); code != http.StatusBadRequest {
		t.Fatalf("invalid-JSON status = %d, want 400", code)
	}
	lines := logLines(t, logPath)
	if got := countKey(lines, "envelope"); got != 0 {
		t.Errorf("envelope lines = %d, want 0", got)
	}
	if got := countKey(lines, "raw_b64"); got != 1 {
		t.Fatalf("raw_b64 lines = %d, want 1", got)
	}
	last := lines[len(lines)-1]
	var b64 string
	if err := json.Unmarshal(last["raw_b64"], &b64); err != nil {
		t.Fatalf("raw_b64 not a string: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Errorf("raw_b64 decodes to %q (err %v), want %q", decoded, err, body)
	}
	if _, ok := last["received_at"]; !ok {
		t.Error("raw_b64 line missing received_at")
	}
}

// TestConcurrentPosts: 10 goroutines × 20 POSTs; the raw log stays valid
// JSONL (single mutex-guarded writer) and every delivery is present.
func TestConcurrentPosts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	_, ts := newDeadman(t, nil, logPath)
	const goroutines, per = 10, 20
	statuses := make(chan int, goroutines*per)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				body := []byte(fmt.Sprintf(`{"source":"notifier","id":"hb-%d-%d","seq":0}`, g, i))
				req, err := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
				if err != nil {
					statuses <- -1
					continue
				}
				req.Header.Set("Authorization", "Bearer "+testBearer)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					statuses <- -1
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				statuses <- resp.StatusCode
			}
		}(g)
	}
	wg.Wait()
	close(statuses)
	for code := range statuses {
		if code != http.StatusOK {
			t.Fatalf("concurrent POST status = %d, want 200", code)
		}
	}
	// logLines fails the test on any corrupt (interleaved) line.
	if got := countKey(logLines(t, logPath), "envelope"); got != goroutines*per {
		t.Errorf("envelope lines = %d, want %d", got, goroutines*per)
	}
}

// TestAlarmEpisodes: silence past H+1 hours appends the alarm line
// exactly once per episode while the DEADMAN_ALARM_URL POST repeats each
// tick; the next notifier arrival re-arms; a second silence produces a
// second alarm line (DM-3).
func TestAlarmEpisodes(t *testing.T) {
	t0 := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	clk := newFakeClock(t0)
	var postsMu sync.Mutex
	var posts [][]byte
	alarmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		postsMu.Lock()
		posts = append(posts, b)
		postsMu.Unlock()
	}))
	defer alarmSrv.Close()
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	s, err := newServer(serverConfig{
		bearer:         testBearer,
		logPath:        logPath,
		heartbeatHours: 1, // threshold H+1 = 2h
		alarmURL:       alarmSrv.URL,
		now:            clk.Now,
		aliveEvery:     time.Hour,
		alarmEvery:     time.Hour,
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()
	ts := httptest.NewServer(s)
	defer ts.Close()

	post(t, ts.URL, "Bearer "+testBearer, heartbeatBody("2026-07-06T00:00:00Z")) // arrives at t0
	s.checkAlarm()
	if got := countKey(logLines(t, logPath), "alarm"); got != 0 {
		t.Fatalf("alarm lines before silence = %d, want 0", got)
	}

	clk.Advance(2*time.Hour + time.Minute)
	s.checkAlarm()
	lines := logLines(t, logPath)
	if got := countKey(lines, "alarm"); got != 1 {
		t.Fatalf("alarm lines after silence = %d, want 1", got)
	}
	for _, m := range lines {
		if _, ok := m["alarm"]; !ok {
			continue
		}
		var since string
		if err := json.Unmarshal(m["silent_since"], &since); err != nil || since != "2026-07-06T00:00:00Z" {
			t.Errorf("silent_since = %q (err %v), want the last notifier arrival", since, err)
		}
	}
	for i := 0; i < 3; i++ {
		s.checkAlarm() // still silent: no new line, but the URL POST repeats
	}
	if got := countKey(logLines(t, logPath), "alarm"); got != 1 {
		t.Fatalf("alarm lines after extra ticks = %d, want 1 (once per episode)", got)
	}

	post(t, ts.URL, "Bearer "+testBearer, heartbeatBody("2026-07-06T02:01:00Z")) // re-arms
	s.checkAlarm()
	if got := countKey(logLines(t, logPath), "alarm"); got != 1 {
		t.Fatalf("alarm lines after re-arm = %d, want 1", got)
	}
	clk.Advance(2*time.Hour + time.Minute)
	s.checkAlarm()
	if got := countKey(logLines(t, logPath), "alarm"); got != 2 {
		t.Fatalf("alarm lines after second silence = %d, want 2", got)
	}

	postsMu.Lock()
	defer postsMu.Unlock()
	if len(posts) != 5 { // episode 1: 1+3 silent ticks; episode 2: 1
		t.Errorf("alarm URL POSTs = %d, want 5 (retried each silent tick)", len(posts))
	}
	for _, p := range posts {
		if !bytes.Contains(p, []byte(`"heartbeat_silence"`)) {
			t.Errorf("alarm POST body = %q, want the alarm JSON", p)
		}
	}
}

// TestTrackerSeedsFromRawLog: on restart the tracker seeds from the raw
// log's NEWEST notifier arrival, so a receiver restarted after long
// silence alarms on the first tick — no fresh H+1 grace window (DM-3).
func TestTrackerSeedsFromRawLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	seed := `{"received_at":"2026-01-01T00:00:00Z","envelope":{"source":"notifier","id":"heartbeat-2026-01-01T00:00:00Z","seq":0}}
{"received_at":"2026-01-02T00:00:00Z","envelope":{"source":"notifier","id":"heartbeat-2026-01-02T00:00:00Z","seq":0}}
{"received_at":"2026-01-03T00:00:00Z","envelope":{"source":"selftest","id":"selftest-2026-01-03T00:00:00Z","seq":0}}
`
	if err := os.WriteFile(logPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	clk := newFakeClock(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	s, _ := newDeadman(t, clk.Now, logPath)
	s.checkAlarm() // first tick: alarms immediately
	lines := logLines(t, logPath)
	if got := countKey(lines, "alarm"); got != 1 {
		t.Fatalf("alarm lines on first tick after restart = %d, want 1", got)
	}
	for _, m := range lines {
		if _, ok := m["alarm"]; !ok {
			continue
		}
		var since string
		if err := json.Unmarshal(m["silent_since"], &since); err != nil || since != "2026-01-02T00:00:00Z" {
			t.Errorf("silent_since = %q (err %v), want the newest NOTIFIER arrival (not the selftest line)", since, err)
		}
	}
}

// TestSelftestDoesNotResetTracker: a source=="selftest" envelope is
// accepted and logged but never feeds the DM-3 tracker — the alarm still
// fires H+1 after the last real notifier arrival (DM-4).
func TestSelftestDoesNotResetTracker(t *testing.T) {
	t0 := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	clk := newFakeClock(t0)
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	s, ts := newDeadman(t, clk.Now, logPath)
	post(t, ts.URL, "Bearer "+testBearer, heartbeatBody("2026-07-06T00:00:00Z")) // notifier at t0
	clk.Advance(90 * time.Minute)
	if code := post(t, ts.URL, "Bearer "+testBearer, []byte(`{"source":"selftest","id":"selftest-x","seq":0}`)); code != http.StatusOK {
		t.Fatalf("selftest envelope status = %d, want 200", code)
	}
	clk.Advance(45 * time.Minute) // 2h15m since the notifier, 45m since the selftest
	s.checkAlarm()
	lines := logLines(t, logPath)
	if got := countKey(lines, "alarm"); got != 1 {
		t.Fatalf("alarm lines = %d, want 1 (selftest must not mask the gap)", got)
	}
	for _, m := range lines {
		if _, ok := m["alarm"]; !ok {
			continue
		}
		var since string
		if err := json.Unmarshal(m["silent_since"], &since); err != nil || since != "2026-07-06T00:00:00Z" {
			t.Errorf("silent_since = %q (err %v), want the notifier arrival, not the selftest", since, err)
		}
	}
}

// TestSelftestRoundTrip: `deadman -selftest -target <url>` delivers one
// source=="selftest" envelope with the env bearer, prints the status,
// exits 0 on 200 and 1 otherwise (DM-4).
func TestSelftestRoundTrip(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	_, ts := newDeadman(t, nil, logPath)
	t.Setenv("DEADMAN_BEARER", testBearer)
	var out, errOut bytes.Buffer
	if code := run([]string{"-selftest", "-target", ts.URL}, &out, &errOut); code != 0 {
		t.Fatalf("selftest exit = %d (stderr %q), want 0", code, errOut.String())
	}
	if !strings.Contains(out.String(), "200") {
		t.Errorf("selftest output = %q, want the HTTP status", out.String())
	}
	found := false
	for _, m := range logLines(t, logPath) {
		raw, ok := m["envelope"]
		if !ok {
			continue
		}
		var env struct {
			Source string `json:"source"`
			ID     string `json:"id"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Source == "selftest" && strings.HasPrefix(env.ID, "selftest-") {
			found = true
		}
	}
	if !found {
		t.Error("selftest envelope not logged with source selftest and selftest-<ts> id")
	}

	t.Setenv("DEADMAN_BEARER", "not-the-bearer")
	var out2, errOut2 bytes.Buffer
	if code := run([]string{"-selftest", "-target", ts.URL}, &out2, &errOut2); code != 1 {
		t.Fatalf("selftest with wrong bearer exit = %d, want 1 (401)", code)
	}
}

// TestLifecycleMarks: receiver_start is appended on start and
// receiver_alive appears at the injected cadence (DM-2a).
func TestLifecycleMarks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	s, err := newServer(serverConfig{
		bearer:         testBearer,
		logPath:        logPath,
		heartbeatHours: 1,
		now:            time.Now,
		aliveEvery:     5 * time.Millisecond,
		alarmEvery:     time.Hour,
	})
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	defer s.close()
	lines := logLines(t, logPath)
	if len(lines) == 0 || !bytes.Equal(lines[0]["mark"], []byte(`"receiver_start"`)) {
		t.Fatalf("first line = %v, want the receiver_start mark", lines)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // runs before s.close (LIFO), stopping the loop first
	go s.aliveLoop(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if bytes.Contains(raw, []byte(`"receiver_alive"`)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no receiver_alive mark within 5s at 5ms cadence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAuthRejectSuppression: rejects beyond maxRejectLines within one
// rejectWindow stop producing verbatim lines; when the window rolls,
// the overflow is folded into one auth_reject_suppressed count line
// (DM-1: unauthenticated appends must stay bounded, without losing the
// brute-force evidence).
func TestAuthRejectSuppression(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	clk := newFakeClock(time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC))
	_, ts := newDeadman(t, clk.Now, logPath)

	for i := 0; i < maxRejectLines+7; i++ {
		if code := post(t, ts.URL, "Bearer wrong", []byte(`{}`)); code != http.StatusUnauthorized {
			t.Fatalf("reject %d: status = %d, want 401", i, code)
		}
	}
	lines := logLines(t, logPath)
	if got := countKey(lines, "auth_reject"); got != maxRejectLines {
		t.Fatalf("auth_reject lines = %d, want cap %d", got, maxRejectLines)
	}
	if got := countKey(lines, "auth_reject_suppressed"); got != 0 {
		t.Fatalf("suppressed lines before window roll = %d, want 0", got)
	}

	clk.Advance(rejectWindow + time.Second)
	if code := post(t, ts.URL, "Bearer wrong", []byte(`{}`)); code != http.StatusUnauthorized {
		t.Fatal("post after window roll should still be 401")
	}
	lines = logLines(t, logPath)
	if got := countKey(lines, "auth_reject_suppressed"); got != 1 {
		t.Fatalf("suppressed summary lines = %d, want 1", got)
	}
	for _, m := range lines {
		raw, ok := m["auth_reject_suppressed"]
		if !ok {
			continue
		}
		var body struct {
			Count int `json:"count"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Count != 7 {
			t.Errorf("suppressed count = %d (err %v), want 7", body.Count, err)
		}
	}
	if got := countKey(lines, "auth_reject"); got != maxRejectLines+1 {
		t.Fatalf("auth_reject lines after roll = %d, want %d (new window)", got, maxRejectLines+1)
	}
}

// TestSeedFallsBackToOldestMark: with no notifier arrival on record,
// restart seeds from the OLDEST lifecycle mark — a crash-looping
// receiver cannot re-grant itself a fresh H+1 window every restart
// (DM-3); the alarm fires on the first tick.
func TestSeedFallsBackToOldestMark(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "raw.jsonl")
	seed := `{"mark":"receiver_start","at":"2026-01-01T00:00:00Z"}
{"mark":"receiver_alive","at":"2026-01-01T00:10:00Z"}
`
	if err := os.WriteFile(logPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	clk := newFakeClock(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	s, _ := newDeadman(t, clk.Now, logPath)
	s.checkAlarm()
	lines := logLines(t, logPath)
	if got := countKey(lines, "alarm"); got != 1 {
		t.Fatalf("alarm lines on first tick = %d, want 1", got)
	}
	for _, m := range lines {
		if _, ok := m["alarm"]; !ok {
			continue
		}
		var since string
		if err := json.Unmarshal(m["silent_since"], &since); err != nil || since != "2026-01-01T00:00:00Z" {
			t.Errorf("silent_since = %q (err %v), want the OLDEST mark", since, err)
		}
	}
}
