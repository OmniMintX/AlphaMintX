package strategy

import (
	"slices"
	"testing"
)

func trader() Context { return Context{Actor: RoleTrader} }

func TestTransitionTable(t *testing.T) {
	tests := []struct {
		name string
		from State
		to   State
		ctx  Context
		ok   bool
	}{
		{"draft->paper with valid config", StateDraft, StatePaper,
			Context{Actor: RoleTrader, ConfigValid: true}, true},
		{"draft->paper without config", StateDraft, StatePaper, trader(), false},
		{"draft->paper by viewer", StateDraft, StatePaper,
			Context{Actor: RoleViewer, ConfigValid: true}, false},
		{"draft->live_l1 skips paper", StateDraft, StateLiveL1,
			Context{Actor: RoleTrader, PaperGatePassed: true, ExchangeKeysConfigured: true}, false},
		{"paper->live_l1 gate passed + keys", StatePaper, StateLiveL1,
			Context{Actor: RoleTrader, PaperGatePassed: true, ExchangeKeysConfigured: true}, true},
		{"paper->live_l1 without paper-gate", StatePaper, StateLiveL1,
			Context{Actor: RoleTrader, ExchangeKeysConfigured: true}, false},
		{"paper->live_l1 admin cannot waive paper-gate", StatePaper, StateLiveL1,
			Context{Actor: RoleAdmin, ExchangeKeysConfigured: true}, false},
		{"paper->live_l2 with envelope", StatePaper, StateLiveL2,
			Context{Actor: RoleTrader, PaperGatePassed: true, L2EnvelopeConfigured: true}, true},
		{"paper->live_l2 without envelope", StatePaper, StateLiveL2,
			Context{Actor: RoleTrader, PaperGatePassed: true}, false},
		{"paper->live_l3 with admin approval", StatePaper, StateLiveL3,
			Context{Actor: RoleTrader, PaperGatePassed: true, AdminApproval: true}, true},
		{"paper->live_l3 without admin approval", StatePaper, StateLiveL3,
			Context{Actor: RoleTrader, PaperGatePassed: true}, false},
		{"live_l1->live_l2 with envelope", StateLiveL1, StateLiveL2,
			Context{Actor: RoleTrader, L2EnvelopeConfigured: true}, true},
		{"live_l1->live_l3 with admin approval", StateLiveL1, StateLiveL3,
			Context{Actor: RoleTrader, AdminApproval: true}, true},
		{"live_l2->live_l3 without admin approval", StateLiveL2, StateLiveL3, trader(), false},
		{"live_l3->live_l1 demotion always allowed", StateLiveL3, StateLiveL1, trader(), true},
		{"live_l3->live_l2 demotion always allowed", StateLiveL3, StateLiveL2, trader(), true},
		{"live_l2->live_l1 demotion always allowed", StateLiveL2, StateLiveL1, trader(), true},
		{"live_l1->live_l2 without envelope", StateLiveL1, StateLiveL2, trader(), false},
		{"live->paper when flat", StateLiveL2, StatePaper,
			Context{Actor: RoleTrader, PositionsFlat: true}, true},
		{"live->paper with open positions", StateLiveL2, StatePaper, trader(), false},
		{"live->draft is illegal", StateLiveL1, StateDraft, trader(), false},
		{"paper->paused", StatePaper, StatePaused, trader(), true},
		{"live_l3->paused", StateLiveL3, StatePaused, trader(), true},
		{"draft->paused is illegal", StateDraft, StatePaused, trader(), false},
		{"paused->paused is illegal", StatePaused, StatePaused, trader(), false},
		{"any->killed by watchdog", StateLiveL3, StateKilled, Context{Actor: RoleSystem}, true},
		{"any->killed by trader", StatePaper, StateKilled, trader(), true},
		{"any->killed by viewer", StateLiveL1, StateKilled, Context{Actor: RoleViewer}, false},
		{"killed->paper full unlock", StateKilled, StatePaper,
			Context{Actor: RoleAdmin, PositionsFlat: true, ConfigValid: true,
				KillCleared: true, CountersReset: true, Reason: "post-mortem done"}, true},
		{"killed->paper not flat", StateKilled, StatePaper,
			Context{Actor: RoleAdmin, ConfigValid: true, KillCleared: true,
				CountersReset: true, Reason: "r"}, false},
		{"killed->paper by trader", StateKilled, StatePaper,
			Context{Actor: RoleTrader, PositionsFlat: true, ConfigValid: true,
				KillCleared: true, CountersReset: true, Reason: "r"}, false},
		{"killed->paper without counter reset", StateKilled, StatePaper,
			Context{Actor: RoleOwner, PositionsFlat: true, ConfigValid: true,
				KillCleared: true, Reason: "r"}, false},
		{"killed->paper while kill tier active", StateKilled, StatePaper,
			Context{Actor: RoleOwner, PositionsFlat: true, ConfigValid: true,
				CountersReset: true, Reason: "r"}, false},
		{"killed->paused for open positions", StateKilled, StatePaused,
			Context{Actor: RoleOwner, KillCleared: true, Reason: "manual resolution"}, true},
		{"killed->paused when flat", StateKilled, StatePaused,
			Context{Actor: RoleOwner, PositionsFlat: true, KillCleared: true, Reason: "r"}, false},
		{"killed->live_l3 never direct to live", StateKilled, StateLiveL3,
			Context{Actor: RoleOwner, PositionsFlat: true, ConfigValid: true,
				KillCleared: true, CountersReset: true, Reason: "r"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			i := NewAt(tc.from)
			_, err := i.Transition(tc.to, tc.ctx)
			if tc.ok && err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("want illegal transition error, got nil")
				}
				if i.State() != tc.from {
					t.Errorf("state changed on failed transition: %s", i.State())
				}
				return
			}
			if i.State() != tc.to {
				t.Errorf("state = %s, want %s", i.State(), tc.to)
			}
		})
	}
}

func TestPausedResumesToPreviousStateOnly(t *testing.T) {
	i := NewAt(StateLiveL2)
	effects, err := i.Transition(StatePaused, trader())
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Pausing cancels un-filled ENTRY orders; protective stops remain.
	if !slices.Contains(effects, EffectCancelEntryOrders) {
		t.Errorf("pause effects = %v, want cancel_entry_orders", effects)
	}
	if _, err := i.Transition(StateLiveL1, trader()); err == nil {
		t.Error("paused must resume only to the state it was paused from")
	}
	if _, err := i.Transition(StateLiveL2, trader()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if i.State() != StateLiveL2 {
		t.Errorf("state = %s, want live_l2", i.State())
	}
}

// Kill resurrection is closed: a strategy paused from live_l3, killed, then
// unlocked to paused must not resume to any live_* state. Pause provenance
// is neutralized on kill; the only exit from a post-kill pause is paper
// under the full killed->paper guard (paper-gate counters reset).
func TestKilledThenPausedCannotResumeToLive(t *testing.T) {
	i := NewAt(StateLiveL3)
	if _, err := i.Transition(StatePaused, trader()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := i.Transition(StateKilled, Context{Actor: RoleSystem}); err != nil {
		t.Fatalf("kill: %v", err)
	}
	unlock := Context{Actor: RoleAdmin, KillCleared: true, Reason: "manual resolution"}
	if _, err := i.Transition(StatePaused, unlock); err != nil {
		t.Fatalf("killed->paused unlock: %v", err)
	}
	full := Context{Actor: RoleOwner, ConfigValid: true, PaperGatePassed: true,
		ExchangeKeysConfigured: true, L2EnvelopeConfigured: true, AdminApproval: true,
		PositionsFlat: true, KillCleared: true, CountersReset: true, Reason: "r"}
	for _, to := range []State{StateLiveL1, StateLiveL2, StateLiveL3} {
		if _, err := i.Transition(to, trader()); err == nil {
			t.Fatalf("killed->paused->%s must be illegal (kill resurrection)", to)
		}
		if _, err := i.Transition(to, full); err == nil {
			t.Fatalf("killed->paused->%s must be illegal even for Owner", to)
		}
	}
	if i.State() != StatePaused {
		t.Fatalf("state = %s, want paused", i.State())
	}
	// The only exit is paper under the full killed->paper guard.
	if _, err := i.Transition(StatePaper, trader()); err == nil {
		t.Fatal("paused-after-kill -> paper must require the killed->paper guard")
	}
	if _, err := i.Transition(StatePaper, Context{Actor: RoleAdmin, PositionsFlat: true,
		ConfigValid: true, KillCleared: true, CountersReset: true, Reason: "post-mortem done"}); err != nil {
		t.Fatalf("paused-after-kill -> paper with full guard: %v", err)
	}
	if i.State() != StatePaper {
		t.Errorf("state = %s, want paper", i.State())
	}
}

func TestKillEffectsCancelEntriesOnly(t *testing.T) {
	i := NewAt(StateLiveL3)
	effects, err := i.Transition(StateKilled, Context{Actor: RoleSystem})
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !slices.Contains(effects, EffectCancelEntryOrders) {
		t.Errorf("kill effects = %v, want cancel_entry_orders (stops kept)", effects)
	}
}
