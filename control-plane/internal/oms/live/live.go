// Package live is the Phase-3 live Binance spot OMS
// (docs/specs/live-oms-and-reconciler.md): the write-ahead intent journal
// with idempotent placement, the exchange-is-truth Reconciler (startup
// reconcile, orphan handling, fill-gap backfill), the user-data stream
// consumer, and the monotone order FSM. It fills the same Submitter seam as
// the paper omsbridge and writes the SAME orders/fills/positions/
// strategy_state rows through the identical accounting math (invariant 10).
package live

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/exchange"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Error-code registry additions (spec §API surface); order-level codes are
// recorded on order status and oms_recon_events, never new HTTP shapes.
var (
	// ErrReconcilePending: no order is sent before the startup reconcile
	// completes, or while a detected venue reset awaits operator
	// acknowledgment (invariant 2, §Venue epochs).
	ErrReconcilePending = errors.New("RECONCILE_PENDING")
	// ErrFilterUnavailable: filters not loaded or expired — never trade
	// unfiltered (fail closed, Reconciler R1).
	ErrFilterUnavailable = errors.New("FILTER_UNAVAILABLE")
	// ErrBelowMinNotional: post-rounding qty < minQty or notional below the
	// venue minimum (the EXISTING registry code).
	ErrBelowMinNotional = errors.New("BELOW_MIN_NOTIONAL")
	// ErrFilterRejected: a non-notional filter violation (maxQty, bounds).
	ErrFilterRejected = errors.New("FILTER_REJECTED")
	// ErrExchangeRejected: the venue processed and refused the placement
	// (DefiniteReject); the intent is terminal, no retry.
	ErrExchangeRejected = errors.New("EXCHANGE_REJECTED")
	// ErrKillEpochStale: a newer kill epoch arrived between journal and
	// send; the order is dropped (risk-limits.md OMS kill re-check).
	ErrKillEpochStale = errors.New("KILL_SWITCH_ACTIVE: kill-epoch stale at submission")
	// ErrKillSwitchActive: a standing kill binds the strategy
	// (GlobalMaxKillEpoch > 0) — fresh ENTRY submissions are rejected;
	// safety-origin flatten/protective submissions are exempt and rely on
	// the transmit-loop staleness comparison (safety-wiring.md
	// invariant 15).
	ErrKillSwitchActive = errors.New("KILL_SWITCH_ACTIVE: a standing kill binds the strategy; ENTRY submissions rejected")
	// ErrBreakerActive: the circuit breaker binds the strategy on the
	// current UTC day — ENTRY submissions halt (protectives and reduce-only
	// continue); derived from kill_breaker_events, auto-reset at 00:00 UTC.
	ErrBreakerActive = errors.New("DAILY_LOSS_LIMIT_BREACHED: circuit breaker active; ENTRY submissions halted")
	// ErrReconRunning: a reconcile run is already in progress (409 on the
	// POST; internal triggers coalesce instead).
	ErrReconRunning = errors.New("RECON_RUNNING")
	// ErrVenueUnreachable: an ambiguous placement could not be resolved by
	// query; the order stays pending_new and the Reconciler owns it.
	ErrVenueUnreachable = errors.New("VENUE_UNREACHABLE: placement unresolved; reconciler owns the intent")
)

// MarkSource is the freshness-checked last-tick cache (market-entry sizing,
// R5 commission conversion); *marketdata.Store satisfies it.
type MarkSource interface {
	Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool)
}

// Config wires the live OMS. Store, Exchange, Symbols, Marks, and a
// positive AllocatedCapitalQuote are required.
type Config struct {
	Store    *store.Store
	Exchange exchange.Exchange
	// Symbols are the configured canonical BASE/QUOTE symbols; the venue
	// mapping never leaks out of the adapter boundary.
	Symbols []string
	Marks   MarkSource
	// AllocatedCapitalQuote seeds a strategy's realized-equity snapshot on
	// its first fill (risk-limits.md Definitions), as in omsbridge.
	AllocatedCapitalQuote decimal.Decimal
	// VenueEnv names the venue environment ("testnet" or "prod") echoed in
	// the recon-status payload (§API surface); informational only.
	VenueEnv string
	// Tuning: the zero value means DefaultTuning().
	Tuning Tuning
	Now    func() time.Time
	// TokenReader is the intent-token CSPRNG source, injectable so scenario
	// tests are reproducible (spec §Config); defaults to crypto/rand.Reader.
	TokenReader io.Reader
	// Sleep is the backoff/throttle sleeper; defaults to time.Sleep.
	Sleep func(time.Duration)
	// Rand drives the ambiguous-resolution backoff jitter, injectable so
	// scenario tests stay deterministic; defaults to a time-seeded source.
	Rand *mrand.Rand
	// Logf defaults to log.Printf. MUST NOT be handed secrets or URLs.
	Logf func(format string, args ...any)
	// OnFill is an OPTIONAL hook invoked after booking ANY fill (stream
	// and backfill alike) — the breaker monitor's Poke seam
	// (safety-wiring.md §Evaluation loop). It MUST return promptly; a
	// panic is recovered and logged, never propagated.
	OnFill func(strategyID string)
}

// OMS is the live order manager. One mutex guards the gate state (startup
// reconcile, venue-reset pending, filters, run exclusion); the store is the
// durable source for everything else.
type OMS struct {
	st        *store.Store
	ex        exchange.Exchange
	marks     MarkSource
	symbols   []string          // canonical, as configured
	venueOf   map[string]string // canonical -> venue symbol
	symbolOf  map[string]string // venue symbol -> canonical
	allocated decimal.Decimal
	venueEnv  string
	tuning    Tuning
	now       func() time.Time
	tokens    io.Reader
	sleep     func(time.Duration)
	rng       *mrand.Rand // jitter source; o.mu serializes draws
	logf      func(format string, args ...any)
	onFill    func(strategyID string)

	mu sync.Mutex
	// reconciled: the MANDATORY startup reconcile completed; until then
	// every submission preflight-fails RECONCILE_PENDING (invariant 2).
	reconciled bool
	// resetPending: a venue reset was detected; ALL sends are refused until
	// an operator acknowledges via TriggerRun(accept_venue_reset).
	resetPending bool
	venueEpoch   store.VenueEpoch
	filters      map[string]symbolFilters // by venue symbol
	filtersAt    time.Time                // last successful load
	// filtersStale: a venue filter-violation reject flagged the loaded
	// filters as out of date; the next reconcile's R1 refreshes them even
	// before their normal expiry (cleared by a successful load).
	filtersStale bool
	running      bool // one reconcile run at a time
	reconFails   int  // consecutive startup-grade failures
	// lastReconcileEnd: completion time of the last full reconcile run —
	// the safety drive's served-marker gate (safety-wiring.md
	// invariant 16).
	lastReconcileEnd time.Time

	// driveMu serializes protective drives: the reconcile-completion and
	// stream-fill triggers may overlap (§Protective order lifecycle).
	driveMu sync.Mutex

	// afterJournal is a test seam invoked between the journal commit and
	// the send claim (scenario S14: kill epoch bumps between journal and
	// send), mirroring the paper OMS's protective-placement hook pattern.
	afterJournal func()
}

// New builds the live OMS and ensures the epoch-0 venue_epochs row exists
// (its insertion at first live start IS the initial epoch transition). The
// OMS starts CLOSED: no order is accepted until the startup reconcile
// completes (Run or TriggerRun).
func New(cfg Config) (*OMS, error) {
	if cfg.Store == nil || cfg.Exchange == nil || cfg.Marks == nil {
		return nil, errors.New("live: Store, Exchange, and Marks are required")
	}
	if len(cfg.Symbols) == 0 {
		return nil, errors.New("live: at least one configured symbol is required")
	}
	if cfg.AllocatedCapitalQuote.Sign() <= 0 {
		return nil, errors.New("live: AllocatedCapitalQuote must be > 0 (equity seed, risk-limits.md)")
	}
	if cfg.Tuning == (Tuning{}) {
		cfg.Tuning = DefaultTuning()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TokenReader == nil {
		cfg.TokenReader = rand.Reader
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Rand == nil {
		cfg.Rand = mrand.New(mrand.NewSource(time.Now().UnixNano()))
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	return newOMS(cfg)
}

func newOMS(cfg Config) (*OMS, error) {
	o := &OMS{
		st:        cfg.Store,
		ex:        cfg.Exchange,
		marks:     cfg.Marks,
		symbols:   append([]string(nil), cfg.Symbols...),
		venueOf:   make(map[string]string, len(cfg.Symbols)),
		symbolOf:  make(map[string]string, len(cfg.Symbols)),
		allocated: cfg.AllocatedCapitalQuote,
		venueEnv:  cfg.VenueEnv,
		tuning:    cfg.Tuning,
		now:       cfg.Now,
		tokens:    cfg.TokenReader,
		sleep:     cfg.Sleep,
		rng:       cfg.Rand,
		logf:      cfg.Logf,
		onFill:    cfg.OnFill,
	}
	for _, sym := range o.symbols {
		venue, err := marketdata.ToBinance(sym)
		if err != nil {
			return nil, fmt.Errorf("live: %w", err)
		}
		o.venueOf[sym] = venue
		o.symbolOf[venue] = sym
	}
	epoch, ok, err := o.st.CurrentVenueEpoch()
	if err != nil {
		return nil, fmt.Errorf("live: read venue epoch: %w", err)
	}
	if !ok {
		epoch = store.VenueEpoch{
			VenueEpoch: 0, StartedAt: formatTime(o.now()),
			Reason: "initial", DetailsJSON: "{}",
		}
		if err := o.st.InsertVenueEpoch(epoch); err != nil {
			return nil, fmt.Errorf("live: insert initial venue epoch: %w", err)
		}
	}
	o.venueEpoch = epoch
	return o, nil
}

// Reconciled reports whether the OMS accepts submissions: the startup
// reconcile completed and no venue reset awaits acknowledgment.
func (o *OMS) Reconciled() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reconciled && !o.resetPending
}

// currentEpoch is the epoch stamped on every venue fill row.
func (o *OMS) currentEpoch() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.venueEpoch.VenueEpoch
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// utcDate is the YYYY-MM-DD UTC day of t (00:00 UTC boundary).
func utcDate(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

func parseDec(field, v string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("live: %s %q: %w", field, v, err)
	}
	return d, nil
}

// round8 is the single normative rounding rule: half away from zero to 8
// decimal places (market-data.md §Rounding), matching the paper OMS.
func round8(d decimal.Decimal) decimal.Decimal { return d.Round(8) }
