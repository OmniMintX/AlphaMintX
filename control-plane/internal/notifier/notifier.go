// Package notifier implements the alert notifier
// (docs/specs/alert-notifier.md): an at-least-once, per-source in-order,
// rowid-watermark-resumable dispatcher that POSTs each new safety event
// from the three AN-1 source tables to one operator-configured webhook —
// or, in log-only mode, emits a stable SAFETY-EVENT marker line. It is a
// read-only poller: safety write paths behave identically with the
// notifier on, off, failing, or absent (AN-5).
package notifier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Schema is the AN-13 wire envelope version pin; any shape change bumps it.
const Schema = "alphamintx.safety-event.v1"

const (
	// truncateBytes bounds details_json/trigger_ref/reason on the wire
	// (AN-13); the DB row stays complete.
	truncateBytes  = 8 * 1024
	truncateSuffix = "…[truncated]"
	// maxBodyRead bounds the discarded response-body read (AN-16).
	maxBodyRead = 4 * 1024
	// degradeAfter escalates the per-source failure log after this many
	// consecutive failed ticks (AN-17); degradeEvery rate-limits the
	// escalated line.
	degradeAfter = 12
	degradeEvery = time.Minute
	// poisonAfter skips one row after this many consecutive 4xx delivery
	// attempts of the SAME row (AN-4a); transient classes never skip.
	poisonAfter = 12
)

// sources is the fixed AN-6a pass order: a kill is never queued behind
// lower-urgency sources.
var sources = []string{
	store.AlertSourceKillBreaker,
	store.AlertSourceKillClear,
	store.AlertSourceSafetyAlert,
}

// Store is the narrow store surface the engine needs; *store.Store
// satisfies it.
type Store interface {
	SeedAlertDispatchWatermark(source, updatedAt string) error
	AlertDispatchWatermark(source string) (int64, bool, error)
	UpsertAlertDispatchWatermark(source string, lastRowid int64, updatedAt string) error
	MaxAlertSourceRowid(source string) (int64, error)
	ListKillBreakerEventsAfter(after int64, limit int) ([]store.NotifyKillBreakerEvent, error)
	ListKillClearEventsAfter(after int64, limit int) ([]store.NotifyKillClearEvent, error)
	ListSafetyAlertsAfter(after int64, limit int) ([]store.NotifySafetyAlert, error)
}

// Config wires the engine; values arrive pre-validated from the AN-10
// parser in cmd wiring. URL and Bearer are secrets: they never leave the
// engine except inside the HTTP request itself (AN-11).
type Config struct {
	Store      Store
	URL        string        // webhook endpoint; unused when LogOnly
	Bearer     string        // optional Authorization: Bearer value
	Timeout    time.Duration // per-delivery timeout (AN-15)
	Poll       time.Duration // tick cadence = retry cadence (AN-17)
	MaxPerTick int           // per-source batch bound (AN-6)
	Heartbeat  time.Duration // AN-14a cadence; 0 disables
	LogOnly    bool          // AN-14 marker lines instead of POSTs
	// Logf is the operational log (default log.Printf); it never receives
	// the URL, the bearer, or a raw delivery error (AN-11).
	Logf func(format string, args ...any)
	// EventOut receives the log-only SAFETY-EVENT lines through a
	// dedicated zero-flag logger (default os.Stderr, AN-14).
	EventOut io.Writer
	Now      func() time.Time
}

// Engine is the dispatcher. One goroutine (Run) executes passes, so
// passes never overlap (AN-6a); all mutable state below is owned by it.
type Engine struct {
	st         Store
	url        string
	bearer     string
	timeout    time.Duration
	poll       time.Duration
	maxPerTick int
	hb         time.Duration
	logOnly    bool
	logf       func(format string, args ...any)
	events     *log.Logger
	now        func() time.Time
	client     *http.Client

	nextHB       time.Time
	fail         map[string]int       // consecutive failed ticks per source
	lastDegraded map[string]time.Time // last DEGRADED line per source
	poison       map[string]*poisonState
}

// poisonState counts consecutive 4xx delivery attempts of ONE row; it is
// in-memory only — a restart can delay a skip, never hasten it (AN-4a).
type poisonState struct {
	rowid int64
	count int
}

// New builds the engine. It validates the wiring only; AN-10 field
// validation happens in the cmd parser before values reach here.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil {
		return nil, errors.New("notifier: Store is required")
	}
	if !cfg.LogOnly && cfg.URL == "" {
		return nil, errors.New("notifier: url is required unless log_only")
	}
	if cfg.Timeout <= 0 || cfg.Poll <= 0 || cfg.MaxPerTick <= 0 {
		return nil, errors.New("notifier: Timeout, Poll, and MaxPerTick must be positive")
	}
	e := &Engine{
		st: cfg.Store, url: cfg.URL, bearer: cfg.Bearer,
		timeout: cfg.Timeout, poll: cfg.Poll, maxPerTick: cfg.MaxPerTick,
		hb: cfg.Heartbeat, logOnly: cfg.LogOnly,
		logf: cfg.Logf, now: cfg.Now,
		fail:         make(map[string]int),
		lastDegraded: make(map[string]time.Time),
		poison:       make(map[string]*poisonState),
	}
	if e.logf == nil {
		e.logf = log.Printf
	}
	if cfg.EventOut == nil {
		cfg.EventOut = os.Stderr
	}
	e.events = log.New(cfg.EventOut, "", 0)
	if e.now == nil {
		e.now = time.Now
	}
	e.client = &http.Client{
		// Proxy nil: an HTTP_PROXY environment variable would otherwise
		// route the full URL and headers through a proxy (AN-11);
		// ErrUseLastResponse surfaces a 3xx as a failed status without
		// ever constructing a url.Error for the redirect target.
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return e, nil
}

// Seed runs the AN-8 seed-at-enable SYNCHRONOUSLY — the caller runs it
// after store.Open and BEFORE any goroutine that can write a source
// table starts. Sources without a watermark row seed at MAX(rowid); an
// existing watermark above MAX(rowid) is clamped down and logged loudly
// (AN-8a). Returns the per-source backlog (MAX(rowid) − last_rowid) for
// the AN-17a startup summary; a nonzero backlog IS dispatched and is
// logged here before dispatch begins.
func (e *Engine) Seed() (map[string]int64, error) {
	backlog := make(map[string]int64, len(sources))
	now := formatTime(e.now())
	for _, src := range sources {
		if err := e.st.SeedAlertDispatchWatermark(src, now); err != nil {
			return nil, fmt.Errorf("notifier: seed %s: %w", src, err)
		}
		wm, ok, err := e.st.AlertDispatchWatermark(src)
		if err != nil {
			return nil, fmt.Errorf("notifier: watermark %s: %w", src, err)
		}
		if !ok {
			return nil, fmt.Errorf("notifier: watermark %s missing after seed", src)
		}
		max, err := e.st.MaxAlertSourceRowid(src)
		if err != nil {
			return nil, fmt.Errorf("notifier: max rowid %s: %w", src, err)
		}
		if wm > max {
			e.logf("ALERT DISPATCH watermark clamped source=%s last_rowid=%d max_rowid=%d (AN-8a: a stale watermark would silently skip new events)",
				src, wm, max)
			if err := e.st.UpsertAlertDispatchWatermark(src, max, now); err != nil {
				return nil, fmt.Errorf("notifier: clamp %s: %w", src, err)
			}
			wm = max
		}
		backlog[src] = max - wm
		if backlog[src] > 0 {
			e.logf("alert dispatch backlog source=%s events=%d (dispatching from watermark)",
				src, backlog[src])
		}
	}
	if e.hb > 0 {
		e.nextHB = e.now() // AN-14a: first heartbeat on start
	}
	return backlog, nil
}

// Run drives the dispatcher until ctx is cancelled (AN-18): one pass
// immediately (the first AN-14a heartbeat and any AN-8a backlog do not
// wait a full interval), then a start-anchored ticker at Poll cadence.
// One goroutine executes passes, so passes never overlap; a tick that
// fired during a pass is drained afterwards — skip, never queue (AN-6a).
func (e *Engine) Run(ctx context.Context) {
	e.runPass(ctx)
	ticker := time.NewTicker(e.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.runPass(ctx)
			select {
			case <-ticker.C:
			default:
			}
		}
	}
}

// runPass dispatches every source in the fixed AN-6a order, then the
// heartbeat (AN-14a: after the sources, never aborting them).
func (e *Engine) runPass(ctx context.Context) {
	for _, src := range sources {
		if ctx.Err() != nil {
			return
		}
		e.dispatchSource(ctx, src)
	}
	if ctx.Err() == nil {
		e.heartbeatPass(ctx)
	}
}

// dispatchSource runs one source's slice of a pass: materialize the batch
// (AN-2a), deliver in rowid order, advance the watermark after each
// success (AN-3), stop on the first failure (AN-4) except the loudly
// logged AN-4a poison-row skip.
func (e *Engine) dispatchSource(ctx context.Context, src string) {
	// The watermark is re-read from the store every pass: the in-memory
	// position is never trusted across a failed persist (AN-9).
	wm, ok, err := e.st.AlertDispatchWatermark(src)
	if err != nil {
		e.logf("alert dispatch watermark read failed source=%s: %v", src, err)
		return
	}
	if !ok {
		e.logf("alert dispatch watermark missing source=%s (Seed not run?)", src)
		return
	}
	batch, err := e.loadBatch(src, wm)
	if err != nil {
		e.logf("alert dispatch read failed source=%s: %v", src, err)
		return
	}
	// The batch is fully materialized and every store resource released
	// BEFORE the first network attempt (AN-2a): a POST can never hold
	// the pool-of-one connection against a safety write.
	failedTick := false
	recordFailure := func(class string) {
		if !failedTick {
			failedTick = true
			e.tickFailed(src, class)
		}
	}
	for _, it := range batch {
		env := envelope{Schema: Schema, Source: src, ID: it.id, Seq: it.rowid,
			DeliveredAt: formatTime(e.now()), Event: it.event}
		class, status, delivered := e.deliver(ctx, env)
		if delivered {
			if err := e.st.UpsertAlertDispatchWatermark(src, it.rowid, formatTime(e.now())); err != nil {
				// AN-9: abort the pass; the next tick redelivers from the
				// last PERSISTED watermark (duplicates, never loss).
				e.logf("alert dispatch watermark persist failed source=%s: %v", src, err)
				return
			}
			if ps := e.poison[src]; ps != nil && ps.rowid == it.rowid {
				delete(e.poison, src)
			}
			e.tickSucceeded(src)
			if !e.logOnly {
				e.logf("alert dispatched source=%s id=%s seq=%d", src, it.id, it.rowid)
			}
			continue
		}
		if ctx.Err() != nil {
			return // shutdown, not a receiver failure
		}
		if status >= 400 && status < 500 {
			// AN-4a: a deterministic 4xx of the SAME row on poisonAfter
			// consecutive attempts advances the watermark past that ONE
			// row, loudly; the next row starts its own count.
			ps := e.poison[src]
			if ps == nil || ps.rowid != it.rowid {
				ps = &poisonState{rowid: it.rowid}
				e.poison[src] = ps
			}
			ps.count++
			if ps.count >= poisonAfter {
				e.logf("ALERT DISPATCH SKIPPED source=%s id=%s seq=%d status=%d", src, it.id, it.rowid, status)
				delete(e.poison, src)
				if err := e.st.UpsertAlertDispatchWatermark(src, it.rowid, formatTime(e.now())); err != nil {
					e.logf("alert dispatch watermark persist failed source=%s: %v", src, err)
					return
				}
				recordFailure(class)
				continue
			}
		} else if ps := e.poison[src]; ps != nil && ps.rowid == it.rowid {
			// A transient failure breaks the row's consecutive-4xx chain.
			delete(e.poison, src)
		}
		recordFailure(class)
		return // AN-4 stop-on-failure: the watermark holds
	}
}

// heartbeatPass attempts the AN-14a synthetic envelope at most once per
// due pass: source "notifier", seq 0, NO watermark involvement; a failure
// feeds the notifier's own degradation counter and never aborts or
// delays any source's dispatch.
func (e *Engine) heartbeatPass(ctx context.Context) {
	if e.hb == 0 {
		return
	}
	now := e.now()
	if now.Before(e.nextHB) {
		return
	}
	id := "heartbeat-" + now.UTC().Truncate(time.Hour).Format("2006-01-02T15:04:05Z")
	env := envelope{Schema: Schema, Source: "notifier", ID: id, Seq: 0,
		DeliveredAt: formatTime(now), Event: map[string]string{"kind": "notifier_heartbeat"}}
	class, _, delivered := e.deliver(ctx, env)
	if !delivered {
		if ctx.Err() == nil {
			e.tickFailed("notifier", class)
		}
		return // still due: the next pass retries (same-hour ids dedupe)
	}
	e.tickSucceeded("notifier")
	if !e.logOnly {
		e.logf("alert dispatched source=notifier id=%s seq=0", id)
	}
	// Interval-anchored: advance from the original anchor past now.
	for !e.nextHB.After(now) {
		e.nextHB = e.nextHB.Add(e.hb)
	}
}

// tickSucceeded clears a source's consecutive-failure state (AN-17).
func (e *Engine) tickSucceeded(src string) {
	delete(e.fail, src)
	delete(e.lastDegraded, src)
}

// tickFailed logs one line per failed tick per source — the AN-11 derived
// class only — escalating to ALERT DISPATCH DEGRADED after degradeAfter
// consecutive failed ticks, then at most once per degradeEvery (AN-17).
func (e *Engine) tickFailed(src, class string) {
	e.fail[src]++
	n := e.fail[src]
	if n < degradeAfter {
		e.logf("alert dispatch failed source=%s class=%s", src, class)
		return
	}
	now := e.now()
	if n == degradeAfter || now.Sub(e.lastDegraded[src]) >= degradeEvery {
		e.logf("ALERT DISPATCH DEGRADED source=%s class=%s consecutive_ticks=%d", src, class, n)
		e.lastDegraded[src] = now
	}
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
