// Command deadman is the reference notifier receiver and dead-man alarm
// (docs/specs/beta-ops-tooling.md DM-1..DM-5; BP-2 item 4; AN-14a). It
// accepts the notifier's webhook POSTs behind a mandatory bearer
// (env DEADMAN_BEARER only — BT-2), appends every delivery to an
// append-only JSONL raw log (the BP-1 acknowledgment tiebreaker) BEFORE
// acknowledging 200, and alarms when notifier heartbeats
// (source=="notifier") stay silent longer than H+1 hours. It is a
// separate trust domain: no control-plane tokens, no venue keys, no TLS
// (operators front it with their reverse proxy — DM-5).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// maxBody bounds each request body at 1 MiB (DM-1): an unbounded body is
// a disk-exhaustion attack on the evidence file.
const maxBody = 1 << 20

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry (BT-1): exit 0 success, 1 runtime failure or
// failed selftest, 2 usage error.
func run(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("deadman", flag.ContinueOnError)
	fs.SetOutput(errOut)
	listen := fs.String("listen", "", "address to serve the notifier webhook on")
	rawLog := fs.String("raw-log", "", "path to the append-only JSONL raw log")
	hbHours := fs.Int("heartbeat-hours", 0, "notifier heartbeat_hours H; alarm after H+1 hours of silence")
	selftest := fs.Bool("selftest", false, "send one synthetic envelope to -target and exit (DM-4)")
	target := fs.String("target", "", "running deadman instance URL for -selftest")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bearer := os.Getenv("DEADMAN_BEARER")
	if bearer == "" {
		fmt.Fprintln(errOut, "deadman: DEADMAN_BEARER must be set and non-empty (env only, never argv)")
		fs.Usage()
		return 2
	}
	if *selftest != (*target != "") {
		fmt.Fprintln(errOut, "deadman: -selftest and -target must be used together")
		fs.Usage()
		return 2
	}
	if *selftest {
		return runSelftest(*target, bearer, out, errOut)
	}
	if *listen == "" || *rawLog == "" || *hbHours <= 0 {
		fmt.Fprintln(errOut, "deadman: -listen, -raw-log and -heartbeat-hours (> 0) are required")
		fs.Usage()
		return 2
	}
	s, err := newServer(serverConfig{
		bearer:         bearer,
		logPath:        *rawLog,
		heartbeatHours: *hbHours,
		alarmURL:       os.Getenv("DEADMAN_ALARM_URL"),
		now:            time.Now,
		aliveEvery:     10 * time.Minute,
		alarmEvery:     30 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(errOut, "deadman: %v\n", err)
		return 1
	}
	defer s.close()
	ctx := context.Background()
	go s.aliveLoop(ctx)
	go s.alarmLoop(ctx)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(errOut, "deadman: %v\n", err)
		return 1
	}
	return 0
}

// Raw-log line shapes. Field order of the structs below is the wire
// order (encoding/json marshals struct fields in declaration order).

// receivedLine is one accepted POST (DM-2): Envelope carries the body
// verbatim when it is valid JSON (compacted onto one line), RawB64 the
// base64 of a body that is not.
type receivedLine struct {
	ReceivedAt string          `json:"received_at"`
	Envelope   json.RawMessage `json:"envelope,omitempty"`
	RawB64     string          `json:"raw_b64,omitempty"`
}

// markLine is a DM-2a lifecycle mark (receiver_start / receiver_alive):
// a gap in receiver_alive marks is receiver downtime, distinguishable
// from notifier silence.
type markLine struct {
	Mark string `json:"mark"`
	At   string `json:"at"`
}

// alarmLine is the DM-3 dead-man alarm; fired_at is the SLA clock start
// for the heartbeat-gap and control-plane-down classes (BP-2 item 4).
type alarmLine struct {
	Alarm       string `json:"alarm"`
	SilentSince string `json:"silent_since"`
	FiredAt     string `json:"fired_at"`
}

// authRejectLine records a rejected request (DM-1): remote addr and time
// only — NO header echo, so brute-force attempts are themselves evidence
// without the log ever holding presented credentials.
type authRejectLine struct {
	AuthReject authRejectBody `json:"auth_reject"`
}

type authRejectBody struct {
	RemoteAddr string `json:"remote_addr"`
	At         string `json:"at"`
}

// serverConfig wires a server; tests inject a fake clock and short loop
// intervals. heartbeatHours is H — the DM-3 alarm threshold is H+1
// hours. alarmURL is the optional DM-3 alarm POST target. aliveEvery is
// the DM-2a receiver_alive cadence; alarmEvery the DM-3 tick cadence.
type serverConfig struct {
	bearer         string
	logPath        string
	heartbeatHours int
	alarmURL       string
	now            func() time.Time
	aliveEvery     time.Duration
	alarmEvery     time.Duration
}

// server is the webhook receiver. ALL raw-log appends are serialized
// through mu (DM-1: concurrent O_APPEND JSONL interleaving would corrupt
// the evidence); the DM-3 tracker shares the same lock.
type server struct {
	expect       []byte // "Bearer <value>"; compared constant-time, never logged
	silenceAfter time.Duration
	alarmURL     string
	now          func() time.Time
	aliveEvery   time.Duration
	alarmEvery   time.Duration
	alarmClient  *http.Client

	mu           sync.Mutex
	log          *os.File
	lastNotifier time.Time // newest source=="notifier" arrival (DM-3)
	alarmed      bool      // alarm line appended for the current episode
	alarmPayload []byte    // the episode's alarm JSON, re-POSTed each tick
}

// newServer seeds the DM-3 tracker from the existing raw log, opens the
// log for append, and writes the receiver_start mark (DM-2a).
func newServer(cfg serverConfig) (*server, error) {
	s := &server{
		expect:       []byte("Bearer " + cfg.bearer),
		silenceAfter: time.Duration(cfg.heartbeatHours+1) * time.Hour,
		alarmURL:     cfg.alarmURL,
		now:          cfg.now,
		aliveEvery:   cfg.aliveEvery,
		alarmEvery:   cfg.alarmEvery,
		alarmClient:  &http.Client{Timeout: 5 * time.Second},
	}
	seed, err := newestNotifierArrival(cfg.logPath)
	if err != nil {
		return nil, err
	}
	if seed.IsZero() {
		// No notifier arrival on record: seed = receiver start (first
		// boot only — DM-3).
		seed = s.now()
	}
	s.lastNotifier = seed
	f, err := os.OpenFile(cfg.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s.log = f
	if err := s.appendJSON(markLine{Mark: "receiver_start", At: rfc3339(s.now())}); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// newestNotifierArrival scans an existing raw log for the newest entry
// whose envelope has source=="notifier" (DM-3 seeding): a receiver
// restarted after long silence must alarm immediately, not get a fresh
// H+1 grace window. The zero time means no notifier arrival on record.
func newestNotifierArrival(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	defer f.Close()
	var newest time.Time
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*maxBody)
	for sc.Scan() {
		var line struct {
			ReceivedAt string          `json:"received_at"`
			Envelope   json.RawMessage `json:"envelope"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil || len(line.Envelope) == 0 {
			continue
		}
		var env struct {
			Source string `json:"source"`
		}
		if json.Unmarshal(line.Envelope, &env) != nil || env.Source != "notifier" {
			continue
		}
		at, err := time.Parse(time.RFC3339, line.ReceivedAt)
		if err != nil {
			continue
		}
		if at.After(newest) {
			newest = at
		}
	}
	if err := sc.Err(); err != nil {
		return time.Time{}, fmt.Errorf("scan %s: %w", path, err)
	}
	return newest, nil
}

// appendJSON marshals v and appends it as one JSONL line; every raw-log
// write funnels through here or appendLocked under the single mutex.
func (s *server) appendJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(b)
}

func (s *server) appendLocked(line []byte) error {
	_, err := s.log.Write(append(line, '\n'))
	return err
}

// ServeHTTP is the DM-1/DM-2 receive path: constant-time bearer check
// (401 + auth_reject line on mismatch), 1 MiB body cap, and the
// append-BEFORE-200 evidence ordering. Failure to append = 500
// (at-least-once: the notifier redelivers; losing evidence is worse
// than a retry).
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(got), s.expect) != 1 {
		_ = s.appendJSON(authRejectLine{authRejectBody{RemoteAddr: r.RemoteAddr, At: rfc3339(s.now())}})
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	receivedAt := s.now()
	if !json.Valid(body) {
		// The notifier never legally sends this, so it must be visible,
		// not dropped (DM-2).
		line := receivedLine{ReceivedAt: rfc3339(receivedAt), RawB64: base64.StdEncoding.EncodeToString(body)}
		if err := s.appendJSON(line); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := s.appendJSON(receivedLine{ReceivedAt: rfc3339(receivedAt), Envelope: body}); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var env struct {
		Source string `json:"source"`
	}
	if json.Unmarshal(body, &env) == nil && env.Source == "notifier" {
		// Only notifier heartbeats feed the tracker: a selftest envelope
		// can never mask a real gap (DM-4). Arrival re-arms the alarm.
		s.mu.Lock()
		s.lastNotifier = receivedAt
		s.alarmed = false
		s.alarmPayload = nil
		s.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

// checkAlarm is one DM-3 tick: when notifier silence exceeds H+1 hours
// it appends the heartbeat_silence line ONCE per episode and, while the
// episode lasts, best-effort POSTs that same JSON to DEADMAN_ALARM_URL
// on every tick. The next notifier arrival re-arms (ServeHTTP).
func (s *server) checkAlarm() {
	now := s.now()
	s.mu.Lock()
	if now.Sub(s.lastNotifier) <= s.silenceAfter {
		s.mu.Unlock()
		return
	}
	if !s.alarmed {
		b, err := json.Marshal(alarmLine{
			Alarm:       "heartbeat_silence",
			SilentSince: rfc3339(s.lastNotifier),
			FiredAt:     rfc3339(now),
		})
		if err != nil || s.appendLocked(b) != nil {
			s.mu.Unlock()
			return // not marked alarmed: the next tick retries the append
		}
		s.alarmed = true
		s.alarmPayload = b
	}
	url, payload := s.alarmURL, s.alarmPayload
	s.mu.Unlock()
	if url != "" && len(payload) > 0 {
		s.postAlarm(url, payload)
	}
}

// postAlarm is the best-effort DM-3 alarm POST (5 s timeout via
// alarmClient); failures are retried by the next tick, never logged.
func (s *server) postAlarm(url string, payload []byte) {
	resp, err := s.alarmClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// aliveLoop appends the DM-2a receiver_alive mark every tick.
func (s *server) aliveLoop(ctx context.Context) {
	t := time.NewTicker(s.aliveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.appendJSON(markLine{Mark: "receiver_alive", At: rfc3339(s.now())})
		}
	}
}

// alarmLoop drives DM-3 ticks.
func (s *server) alarmLoop(ctx context.Context) {
	t := time.NewTicker(s.alarmEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkAlarm()
		}
	}
}

// close releases the raw-log handle (tests; the real server runs until
// the process is killed).
func (s *server) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.Close()
}

// runSelftest sends one synthetic envelope (DM-4) with source
// "selftest", so it can never satisfy the DM-3 tracker or mask a real
// gap. Prints the HTTP status; exit 0 iff 200.
func runSelftest(target, bearer string, out, errOut io.Writer) int {
	now := time.Now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(struct {
		Schema      string            `json:"schema"`
		Source      string            `json:"source"`
		ID          string            `json:"id"`
		Seq         int64             `json:"seq"`
		DeliveredAt string            `json:"delivered_at"`
		Event       map[string]string `json:"event"`
	}{
		Schema:      "alphamintx.safety-event.v1",
		Source:      "selftest",
		ID:          "selftest-" + now,
		Seq:         0,
		DeliveredAt: now,
		Event:       map[string]string{"kind": "deadman_selftest"},
	})
	if err != nil {
		fmt.Fprintf(errOut, "deadman: %v\n", err)
		return 1
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(errOut, "deadman: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errOut, "deadman: selftest POST failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(out, "selftest: %s\n", resp.Status)
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}

// rfc3339 renders the BT-1 timestamp form: RFC 3339 UTC with Z.
func rfc3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
