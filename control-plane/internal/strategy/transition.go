package strategy

// Transition applies the normative transition table. On success it returns
// the side effects the caller MUST apply; on failure the state is unchanged.
func (i *Instance) Transition(to State, ctx Context) ([]Effect, error) {
	from := i.state

	// any -> killed: kill-switch (any tier) or watchdog escalation.
	if to == StateKilled {
		if !ctx.Actor.traderPlus() && ctx.Actor != RoleSystem {
			return nil, illegal(from, to, "kill requires Trader+ or the watchdog")
		}
		i.state = StateKilled
		// Neutralize pause provenance: a kill invalidates any remembered
		// resume target so a later unlock can never restore live_*.
		i.pausedFrom = ""
		// ENTRY orders canceled; protective stops kept unless flattened
		// (flatten choice is the kill procedure's, not the machine's).
		return []Effect{EffectCancelEntryOrders}, nil
	}

	// any of paper, live_* -> paused.
	if to == StatePaused && (from == StatePaper || from.IsLive()) {
		if !ctx.Actor.traderPlus() {
			return nil, illegal(from, to, "pause requires Trader+")
		}
		i.pausedFrom = from
		i.state = StatePaused
		return []Effect{EffectCancelEntryOrders}, nil
	}

	var err error
	switch from {
	case StateDraft:
		err = guardDraft(to, ctx)
	case StatePaper:
		err = guardPaper(to, ctx)
	case StateLiveL1, StateLiveL2, StateLiveL3:
		err = guardLive(from, to, ctx)
	case StatePaused:
		if i.pausedFrom == StateKilled || i.pausedFrom == "" {
			// Paused entered via a killed unlock (or unknown provenance):
			// never back to live_* (strategy-lifecycle.md kill-reset rule).
			// The only exit is paper under the full killed->paper guard,
			// including the paper-gate counter reset.
			if to != StatePaper {
				return nil, illegal(from, to, "paused-after-kill resumes only to paper with counters reset")
			}
			err = guardKilled(to, ctx)
		} else {
			// Resume to the exact state it was paused from.
			if to != i.pausedFrom {
				return nil, illegal(from, to, "paused resumes only to its previous state")
			}
			if !ctx.Actor.traderPlus() {
				return nil, illegal(from, to, "resume requires Trader+")
			}
		}
	case StateKilled:
		err = guardKilled(to, ctx)
	default:
		return nil, illegal(from, to, "unknown state")
	}
	if err != nil {
		return nil, err
	}
	i.state = to
	if from == StatePaused {
		i.pausedFrom = ""
	}
	if from == StateKilled && to == StatePaused {
		// Record kill provenance so a later resume cannot reach live_*.
		i.pausedFrom = StateKilled
	}
	return nil, nil
}

func guardDraft(to State, ctx Context) error {
	if to != StatePaper {
		return illegal(StateDraft, to, "not in the transition table")
	}
	if !ctx.Actor.traderPlus() {
		return illegal(StateDraft, to, "requires Trader+")
	}
	if !ctx.ConfigValid {
		return illegal(StateDraft, to, "config invalid or RiskLimits not set (whitelist, caps)")
	}
	return nil
}

func guardPaper(to State, ctx Context) error {
	if !to.IsLive() {
		return illegal(StatePaper, to, "not in the transition table")
	}
	if !ctx.Actor.traderPlus() {
		return illegal(StatePaper, to, "requires Trader+")
	}
	// Paper-gate cannot be waived by any role (invariant 3).
	if !ctx.PaperGatePassed {
		return illegal(StatePaper, to, "paper-gate not passed")
	}
	switch to {
	case StateLiveL1:
		if !ctx.ExchangeKeysConfigured {
			return illegal(StatePaper, to, "exchange keys not configured")
		}
	case StateLiveL2:
		if !ctx.L2EnvelopeConfigured {
			return illegal(StatePaper, to, "L2 envelope not configured")
		}
	case StateLiveL3:
		if !ctx.AdminApproval {
			return illegal(StatePaper, to, "Admin approval not recorded")
		}
	}
	return nil
}

func guardLive(from, to State, ctx Context) error {
	if !ctx.Actor.traderPlus() {
		return illegal(from, to, "requires Trader+")
	}
	switch {
	case to == StatePaper:
		if !ctx.PositionsFlat {
			return illegal(from, to, "positions not flat")
		}
	case from == StateLiveL1 && to == StateLiveL2:
		if !ctx.L2EnvelopeConfigured {
			return illegal(from, to, "L2 envelope not configured")
		}
	case (from == StateLiveL1 || from == StateLiveL2) && to == StateLiveL3:
		if !ctx.AdminApproval {
			return illegal(from, to, "Admin approval not recorded")
		}
	case from == StateLiveL3 && (to == StateLiveL2 || to == StateLiveL1),
		from == StateLiveL2 && to == StateLiveL1:
		// Demotion; always allowed for Trader+.
	default:
		return illegal(from, to, "not in the transition table")
	}
	return nil
}

// guardKilled: human unlock only (Admin or Owner), never automatic, never
// directly back to live_*; blocked while the triggering kill tier is active.
func guardKilled(to State, ctx Context) error {
	if to != StatePaper && to != StatePaused {
		return illegal(StateKilled, to, "killed unlocks only to paper (flat) or paused (not flat)")
	}
	if !ctx.Actor.adminPlus() {
		return illegal(StateKilled, to, "unlock requires Admin or Owner")
	}
	if !ctx.KillCleared {
		return illegal(StateKilled, to, "triggering kill tier still active")
	}
	if ctx.Reason == "" {
		return illegal(StateKilled, to, "unlock requires a recorded reason")
	}
	if to == StatePaper {
		if !ctx.PositionsFlat {
			return illegal(StateKilled, to, "positions not flat (or flatten not confirmed)")
		}
		if !ctx.ConfigValid {
			return illegal(StateKilled, to, "draft->paper guard conditions do not hold")
		}
		if !ctx.CountersReset {
			return illegal(StateKilled, to, "paper-gate counters not reset")
		}
	} else if ctx.PositionsFlat {
		return illegal(StateKilled, to, "killed->paused is for manual resolution of open positions; flat unlocks go to paper")
	}
	return nil
}
