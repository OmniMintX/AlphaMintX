package notifier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var testNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

// uid derives a deterministic contract-pattern UUID from an index.
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

// clock is the injectable engine time source (mutexed: Run reads it from
// the dispatcher goroutine).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuf captures the engine's operational log lines.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format, args...)
	l.b.WriteByte('\n')
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) count(sub string) int { return strings.Count(l.String(), sub) }

// safeBuf is a mutexed EventOut sink for the log-only marker lines.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// capture is one decoded AN-13 envelope as the receiver saw it, plus the
// status the receiver answered.
type capture struct {
	Schema      string         `json:"schema"`
	Source      string         `json:"source"`
	ID          string         `json:"id"`
	Seq         int64          `json:"seq"`
	DeliveredAt string         `json:"delivered_at"`
	Event       map[string]any `json:"event"`
	status      int
}

// receiver is the scripted webhook endpoint: status decides the answer
// per envelope (nil = 200); every request is captured in arrival order.
type receiver struct {
	t      *testing.T
	mu     sync.Mutex
	got    []capture
	status func(c capture) int
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{t: t}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			r.t.Errorf("receiver read: %v", err)
		}
		var c capture
		if err := json.Unmarshal(body, &c); err != nil {
			r.t.Errorf("receiver decode %q: %v", body, err)
		}
		r.mu.Lock()
		status := http.StatusOK
		if r.status != nil {
			status = r.status(c)
		}
		c.status = status
		r.got = append(r.got, c)
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) setStatus(f func(capture) int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = f
}

func (r *receiver) captures() []capture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capture(nil), r.got...)
}

// accepted returns the envelopes answered 2xx, in arrival order.
func (r *receiver) accepted() []capture {
	var out []capture
	for _, c := range r.captures() {
		if c.status >= 200 && c.status < 300 {
			out = append(out, c)
		}
	}
	return out
}

// harness is one engine test fixture: a real store, a captured log, a
// captured EventOut, and a settable clock.
type harness struct {
	t      *testing.T
	st     *store.Store
	dbPath string
	logs   *logBuf
	events *safeBuf
	clk    *clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "cp.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &harness{t: t, st: st, dbPath: dbPath,
		logs: &logBuf{}, events: &safeBuf{}, clk: &clock{t: testNow}}
}

// engine builds an Engine over the harness with test defaults; zero cfg
// fields are filled in (webhook URL must come from the caller unless
// LogOnly).
func (h *harness) engine(cfg Config) *Engine {
	h.t.Helper()
	if cfg.Store == nil {
		cfg.Store = h.st
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Poll == 0 {
		cfg.Poll = 10 * time.Millisecond
	}
	if cfg.MaxPerTick == 0 {
		cfg.MaxPerTick = 20
	}
	if cfg.Logf == nil {
		cfg.Logf = h.logs.logf
	}
	if cfg.EventOut == nil {
		cfg.EventOut = h.events
	}
	if cfg.Now == nil {
		cfg.Now = h.clk.Now
	}
	e, err := New(cfg)
	if err != nil {
		h.t.Fatalf("notifier.New: %v", err)
	}
	return e
}

// seeded builds an engine and runs Seed, returning both.
func (h *harness) seeded(cfg Config) (*Engine, map[string]int64) {
	h.t.Helper()
	e := h.engine(cfg)
	backlog, err := e.Seed()
	if err != nil {
		h.t.Fatalf("Seed: %v", err)
	}
	return e, backlog
}

// alert appends one safety_alerts row with AlertID uid(i).
func (h *harness) alert(i int, kind, details string) {
	h.t.Helper()
	if err := h.st.AppendSafetyAlert(store.SafetyAlert{
		AlertID: uid(i), Kind: kind, DetailsJSON: details,
		RecordedAt: formatTime(testNow),
	}); err != nil {
		h.t.Fatalf("AppendSafetyAlert: %v", err)
	}
}

// platformKill appends one platform-scope kill row and returns its epoch.
func (h *harness) platformKill(i int) int64 {
	h.t.Helper()
	epoch, err := h.st.AppendPlatformKill(uid(i), "op-1", formatTime(testNow), false)
	if err != nil {
		h.t.Fatalf("AppendPlatformKill: %v", err)
	}
	return epoch
}

// watermark reads one source's persisted watermark (must exist).
func (h *harness) watermark(source string) int64 {
	h.t.Helper()
	wm, ok, err := h.st.AlertDispatchWatermark(source)
	if err != nil || !ok {
		h.t.Fatalf("AlertDispatchWatermark(%s) ok=%v err=%v", source, ok, err)
	}
	return wm
}

// upsertFailStore injects AN-9 watermark-persist failures: the first
// `fail` upserts error, later ones pass through.
type upsertFailStore struct {
	*store.Store
	mu   sync.Mutex
	fail int
}

func (u *upsertFailStore) UpsertAlertDispatchWatermark(source string, lastRowid int64, updatedAt string) error {
	u.mu.Lock()
	if u.fail > 0 {
		u.fail--
		u.mu.Unlock()
		return errors.New("injected watermark persist failure")
	}
	u.mu.Unlock()
	return u.Store.UpsertAlertDispatchWatermark(source, lastRowid, updatedAt)
}
