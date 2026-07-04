// Package strategy implements the lifecycle state machine of
// docs/specs/strategy-lifecycle.md. Anything not in the transition table is
// illegal: an error, not a warning.
package strategy

import "fmt"

// State is a canonical lifecycle state.
type State string

const (
	StateDraft  State = "draft"
	StatePaper  State = "paper"
	StateLiveL1 State = "live_l1"
	StateLiveL2 State = "live_l2"
	StateLiveL3 State = "live_l3"
	StatePaused State = "paused"
	StateKilled State = "killed"
)

// IsLive reports whether s is one of the live_* states.
func (s State) IsLive() bool {
	return s == StateLiveL1 || s == StateLiveL2 || s == StateLiveL3
}

// Role of the transition actor. Viewer can never transition; "Trader+" is
// Trader, Admin, or Owner; killed unlocks require Admin or Owner; RoleSystem
// covers watchdog/kill-switch escalation paths.
type Role int

const (
	RoleViewer Role = iota
	RoleTrader
	RoleAdmin
	RoleOwner
	RoleSystem
)

func (r Role) traderPlus() bool { return r == RoleTrader || r == RoleAdmin || r == RoleOwner }
func (r Role) adminPlus() bool  { return r == RoleAdmin || r == RoleOwner }

// Context carries the guard inputs of the transition table. Callers assert
// the facts; the machine enforces the combinations.
type Context struct {
	Actor Role
	// ConfigValid: RiskLimits set by Admin (whitelist non-empty, caps set).
	ConfigValid            bool
	ExchangeKeysConfigured bool
	PaperGatePassed        bool
	L2EnvelopeConfigured   bool
	AdminApproval          bool
	PositionsFlat          bool
	// KillCleared: the triggering kill tier's standing condition is cleared.
	KillCleared bool
	// CountersReset: paper-gate counters reset after the kill/breach event.
	CountersReset bool
	// Reason recorded for killed unlocks (append-only audit).
	Reason string
}

// Effect is a side effect the caller MUST apply on a successful transition.
type Effect string

const (
	// EffectCancelEntryOrders cancels un-filled ENTRY orders only;
	// protective reduce-only SL/TP remain and positions stay managed.
	EffectCancelEntryOrders Effect = "cancel_entry_orders"
)

// Instance is one strategy instance's lifecycle state.
type Instance struct {
	state State
	// pausedFrom remembers the state to resume to (paused -> previous).
	pausedFrom State
}

// New returns a strategy instance in draft.
func New() *Instance { return &Instance{state: StateDraft} }

// NewAt returns an instance at a given state (tests, rehydration).
func NewAt(state State) *Instance { return &Instance{state: state} }

// State returns the current lifecycle state.
func (i *Instance) State() State { return i.state }

func illegal(from, to State, why string) error {
	return fmt.Errorf("illegal transition %s -> %s: %s", from, to, why)
}
