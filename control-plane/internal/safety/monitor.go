// Package safety implements the live PnL circuit-breaker monitor
// (docs/specs/safety-wiring.md §Live PnL circuit-breaker monitor): one
// goroutine evaluates every monitored strategy's daily-loss condition on a
// timer (ACTIVE/IDLE cadence) and on every booked fill (Poke), appends the
// breaker row persist-then-execute, and runs the TIME-based stall scan for
// unserved safety events on every tick. The package declares narrow
// consumer-side interfaces — *runstate.Hydrator, *api.LimitsProvider,
// *marketdata.Store, the live OMS, and *store.Store satisfy them — and
// deliberately never imports oms/live (spec §Placement).
package safety

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Normative defaults (spec §Config). Bounds are enforced fail-closed at
// env parse time in cmd/controlplane; zero Config values mean these.
const (
	DefaultActiveInterval = 5 * time.Second
	DefaultIdleInterval   = 60 * time.Second
	DefaultStallThreshold = 600 * time.Second
)

// Watchdog thresholds (docs/specs/watchdog.md WD-27): CONSTANTS, not
// env-tunable — risk-limits.md §Watchdog pins 90 s / 10 min normatively,
// and tunability would let one misconfigured env var silently defeat the
// ladder. Loosening requires a spec change, deliberately.
const (
	WatchdogSilenceThreshold    = 90 * time.Second
	WatchdogEscalationThreshold = 600 * time.Second
)

// PnLSource is the folded daily figure (realized after rollover +
// unrealized at fresh marks); *runstate.Hydrator satisfies it.
type PnLSource interface {
	DailyPnL(strategyID string, now time.Time) (decimal.Decimal, error)
}

// LimitsProvider yields the CURRENT effective limits per strategy — never
// a startup capture (multi-tenant-rbac.md §Runtime limit changes);
// *api.LimitsProvider satisfies it.
type LimitsProvider interface {
	Limits(strategyID string) riskgate.RiskLimits
}

// MarkSource is the freshness-checked last-tick cache the monitor consults
// DIRECTLY for step 4 (Hydrator.DailyPnL silently folds zero unrealized
// for stale marks); *marketdata.Store satisfies it.
type MarkSource interface {
	Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool)
}

// Driver re-drives unserved kill/breaker effects — the same seam as
// api.SafetyDriver; the live OMS satisfies it.
type Driver interface {
	DriveSafetyEffects(ctx context.Context) error
}

// ReconGate reports whether the live OMS completed its startup reconcile
// with no venue reset pending (evaluation-loop step 1); the live OMS
// satisfies it.
type ReconGate interface {
	Reconciled() bool
}

// EntryCanceller is the watchdog's rung-1 ENTRY-cancel sweep seam
// (watchdog.md WD-18): the EXISTING kill-driver sweep, reused with its
// exact semantics — ENTRY class only, NotFound is success, claimed-unsent
// intents claim-revoked first, Ambiguous left for the next tick. The live
// OMS's CancelOpenEntries satisfies it.
type EntryCanceller interface {
	CancelOpenEntries(ctx context.Context, strategyID string) error
}

// FiltersProvider yields one canonical symbol's venue minimums for the
// WD-20 dust carve-out; ok=false (filters unloaded/expired, or the symbol
// unconfigured) EXCLUDES the position — fail toward PROTECTED. The live
// OMS's MinFilters satisfies it.
type FiltersProvider interface {
	MinFilters(symbol string) (minQty, minNotional decimal.Decimal, ok bool)
}

// Store is the narrow store surface the monitor needs; *store.Store
// satisfies it.
type Store interface {
	ListStrategies(page, limit int) ([]store.Strategy, int, error)
	ListPositions(strategyID string) ([]store.Position, error)
	ListNonTerminalLiveOrders() ([]store.LiveOrder, error)
	BreakerActiveToday(strategyID, utcDate string) (bool, error)
	AppendKillBreakerEvent(e store.KillBreakerEvent) error
	AppendSafetyAlert(a store.SafetyAlert) error
	HasSafetyAlertToday(kind, strategyID, refID, utcDate string) (bool, error)
	ListUnservedSafetyEvents() ([]store.KillBreakerEvent, error)
	// Watchdog surface (watchdog.md §Wiring seams): the standing-kill
	// skip — on the LC-28 ActiveKill predicate since lifecycle-api.md
	// LC-34: after a clear the watchdog is RE-ARMED and may kill again
	// on fresh silence — the rung-2 escalation append, the per-kill-event
	// alert dedupe, and the WD-16 back-fill read.
	ActiveKill(strategyID string) (bool, error)
	AppendStrategyKill(eventID, strategyID, actorID, recordedAt string, flatten bool) (int64, error)
	HasSafetyAlert(kind, strategyID, refID string) (bool, error)
	LatestStrategyKillEvent(strategyID string) (eventID, actorID string, ok bool, err error)
}

// Config wires the Monitor. Store, PnL, Limits, Marks, Driver, and Recon
// are required; zero durations mean the normative defaults.
type Config struct {
	Store  Store
	PnL    PnLSource
	Limits LimitsProvider
	Marks  MarkSource
	Driver Driver
	Recon  ReconGate
	// Entries is the watchdog rung-1 sweep seam (watchdog.md WD-18);
	// required unless WatchdogDisabled — a disabled watchdog must not
	// demand a seam it never calls.
	Entries EntryCanceller
	// Filters backs the WD-20 dust carve-out; required unless
	// WatchdogDisabled.
	Filters FiltersProvider
	// WatchdogDisabled is the CONTROLPLANE_WATCHDOG_DISABLED escape hatch
	// (watchdog.md §Config): it turns off watchdog EVALUATION only —
	// heartbeat RECEIPT stays with the API layer.
	WatchdogDisabled bool
	// ActiveInterval ticks while any monitored strategy has a nonzero
	// position or a non-terminal live order; IdleInterval when all are
	// flat and quiet (spec §Config; bounds are the parser's contract).
	ActiveInterval time.Duration
	IdleInterval   time.Duration
	// StallThreshold is safety_effect_stall_seconds (§Safety-effects
	// driver step 5).
	StallThreshold time.Duration
	// Now defaults to time.Now; tests inject a fixed clock.
	Now func() time.Time
	// Logf defaults to log.Printf.
	Logf func(format string, args ...any)
}

// Monitor is the breaker monitor: one Run goroutine, poked on fills.
type Monitor struct {
	st          Store
	pnl         PnLSource
	limits      LimitsProvider
	marks       MarkSource
	driver      Driver
	recon       ReconGate
	entries     EntryCanceller
	filters     FiltersProvider
	watchdogOff bool
	active      time.Duration
	idle        time.Duration
	stall       time.Duration
	now         func() time.Time
	logf        func(format string, args ...any)
	poke        chan struct{}

	// Watchdog heartbeat state (watchdog.md WD-8/WD-9): NEVER persisted —
	// lastSeen per strategy from Beat, firstWatched stamped on watch-set
	// entry, startedAt the monitor's construction instant; the silence
	// baseline is max(startedAt, firstWatched). A restart loses lastSeen
	// and grants a fresh, bounded window (the accepted WD-9 liveness gap).
	// Leaving the watch set deletes BOTH entries and re-entry re-stamps
	// (lifecycle-api.md LC-34b): re-promotion after a kill clear starts a
	// fresh baseline, never a pre-kill staleness escalation.
	startedAt    time.Time
	hbMu         sync.Mutex
	lastSeen     map[string]time.Time
	firstWatched map[string]time.Time
}

// New builds the Monitor.
func New(cfg Config) (*Monitor, error) {
	if cfg.Store == nil || cfg.PnL == nil || cfg.Limits == nil ||
		cfg.Marks == nil || cfg.Driver == nil || cfg.Recon == nil {
		return nil, errors.New("safety: Store, PnL, Limits, Marks, Driver, and Recon are required")
	}
	if !cfg.WatchdogDisabled && (cfg.Entries == nil || cfg.Filters == nil) {
		return nil, errors.New("safety: Entries and Filters are required unless WatchdogDisabled (watchdog.md §Wiring seams)")
	}
	m := &Monitor{
		st: cfg.Store, pnl: cfg.PnL, limits: cfg.Limits, marks: cfg.Marks,
		driver: cfg.Driver, recon: cfg.Recon,
		entries: cfg.Entries, filters: cfg.Filters, watchdogOff: cfg.WatchdogDisabled,
		active: cfg.ActiveInterval, idle: cfg.IdleInterval, stall: cfg.StallThreshold,
		now: cfg.Now, logf: cfg.Logf,
		poke:         make(chan struct{}, 1),
		lastSeen:     make(map[string]time.Time),
		firstWatched: make(map[string]time.Time),
	}
	if m.active <= 0 {
		m.active = DefaultActiveInterval
	}
	if m.idle <= 0 {
		m.idle = DefaultIdleInterval
	}
	if m.stall <= 0 {
		m.stall = DefaultStallThreshold
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logf == nil {
		m.logf = log.Printf
	}
	m.startedAt = m.now()
	return m, nil
}

// Beat records one heartbeat receipt (watchdog.md WD-8): a mutex-guarded
// in-memory timestamp update — no store row, safe from any goroutine,
// never blocking a tick (the api.HeartbeatSink seam). An out-of-order
// receipt never regresses lastSeen.
func (m *Monitor) Beat(strategyID string, at time.Time) {
	m.hbMu.Lock()
	defer m.hbMu.Unlock()
	if prev, ok := m.lastSeen[strategyID]; !ok || at.After(prev) {
		m.lastSeen[strategyID] = at
	}
}

// Poke nudges an immediate evaluation tick — the live OMS's Config.OnFill
// seam, the "on every fill" half of the rule (spec §Cadence). Non-blocking
// and coalescing: pokes arriving during a tick queue at most one extra
// tick, and the poked tick evaluates every monitored strategy (a superset
// of the poked one).
func (m *Monitor) Poke(string) {
	select {
	case m.poke <- struct{}{}:
	default:
	}
}

// Run is the evaluation loop: an immediate startup tick, then a tick at
// the per-tick-selected ACTIVE/IDLE cadence or on a Poke, until ctx is
// done (spec §Lifecycle: started by cmd/controlplane iff
// CONTROLPLANE_OMS_MODE=live, stopped with server shutdown). A tick panic
// is recovered, logged, and alerted (invariant 14): the monitor and the
// process keep running.
func (m *Monitor) Run(ctx context.Context) {
	for {
		next := m.safeTick(ctx)
		timer := time.NewTimer(next)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-m.poke:
			timer.Stop()
		}
	}
}
