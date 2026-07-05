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
	st     Store
	pnl    PnLSource
	limits LimitsProvider
	marks  MarkSource
	driver Driver
	recon  ReconGate
	active time.Duration
	idle   time.Duration
	stall  time.Duration
	now    func() time.Time
	logf   func(format string, args ...any)
	poke   chan struct{}
}

// New builds the Monitor.
func New(cfg Config) (*Monitor, error) {
	if cfg.Store == nil || cfg.PnL == nil || cfg.Limits == nil ||
		cfg.Marks == nil || cfg.Driver == nil || cfg.Recon == nil {
		return nil, errors.New("safety: Store, PnL, Limits, Marks, Driver, and Recon are required")
	}
	m := &Monitor{
		st: cfg.Store, pnl: cfg.PnL, limits: cfg.Limits, marks: cfg.Marks,
		driver: cfg.Driver, recon: cfg.Recon,
		active: cfg.ActiveInterval, idle: cfg.IdleInterval, stall: cfg.StallThreshold,
		now: cfg.Now, logf: cfg.Logf,
		poke: make(chan struct{}, 1),
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
	return m, nil
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
