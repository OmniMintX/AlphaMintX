package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/papergate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/strategy"
)

// lifecycleRequest is the REQUIRED, strictly-decoded LC-4 body.
type lifecycleRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// lifecycleResponse is the LC-13 success envelope.
type lifecycleResponse struct {
	StrategyID   string `json:"strategy_id"`
	FromState    string `json:"from_state"`
	ToState      string `json:"to_state"`
	TransitionID string `json:"transition_id"`
	RecordedAt   string `json:"recorded_at"`
}

// paperGateErrorBody is the PAPER_GATE_FAILED envelope: the standard error
// shape plus the full LC-23 condition report (LC-11).
type paperGateErrorBody struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	PaperGate papergate.Report `json:"paper_gate"`
}

// lifecycleStates is the canonical state-name set; anything else is 400
// INVALID_LIFECYCLE_STATE (LC-4).
var lifecycleStates = map[string]strategy.State{
	"draft":   strategy.StateDraft,
	"paper":   strategy.StatePaper,
	"live_l1": strategy.StateLiveL1,
	"live_l2": strategy.StateLiveL2,
	"live_l3": strategy.StateLiveL3,
	"paused":  strategy.StatePaused,
	"killed":  strategy.StateKilled,
}

// machineRole maps the authenticated principal to the machine's actor role
// (LC-6): env-admin acts as Owner; RoleSystem is unreachable via the API.
func machineRole(pr principal) strategy.Role {
	if pr.class == classEnvAdmin {
		return strategy.RoleOwner
	}
	switch pr.role {
	case RoleTrader:
		return strategy.RoleTrader
	case RoleAdmin:
		return strategy.RoleAdmin
	case RoleOwner:
		return strategy.RoleOwner
	default:
		return strategy.RoleViewer
	}
}

// auditRole is the actor_role audit column value (LC-10): the principal's
// role string, or "env-admin" for the env-admin class; 'system' remains
// exclusively the safety driver's and the bootstrap writer's.
func auditRole(pr principal) string {
	if pr.class == classEnvAdmin {
		return "env-admin"
	}
	return pr.role
}

// handlePostLifecycle performs one lifecycle transition (lifecycle-api.md
// LC-1..LC-14): tenant-scoped resolution, the strictly-decoded LC-4 body,
// the machine rehydrated from persisted state (paused provenance from the
// audit trail), guard inputs derived from persisted state only, CAS
// persistence, and persist-then-execute effects.
func (s *Server) handlePostLifecycle(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(pr, strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	var req lifecycleRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	to, known := lifecycleStates[req.To]
	if !known {
		writeError(w, http.StatusBadRequest, codeInvalidLifecycleState,
			fmt.Sprintf("unknown lifecycle state %q", req.To))
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "reason is required")
		return
	}
	if to == strategy.StateKilled {
		// Kills flow ONLY through the kill endpoints: their
		// kill_breaker_events row is the activation record (LC-5).
		writeError(w, http.StatusUnprocessableEntity, codeUseKillEndpoint,
			"use the kill endpoints: the lifecycle endpoint never mints a killed state")
		return
	}

	// One lifecycle evaluation+CAS per strategy at a time, never
	// concurrent with a gate evaluation reading lifecycle_state (LC-14).
	lock := s.strategyLock(strategyID)
	lock.Lock()
	defer lock.Unlock()

	st, err := s.rootStrategy(pr, strategyID) // fresh read under the lock
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	s.runLifecycleTransition(w, r, pr, st, to, req.Reason)
}

// runLifecycleTransition is the under-lock half of the handler: machine
// rehydration (LC-7), guard-input derivation (LC-8), the live-target kill
// guard, the paper-gate carve-out (LC-11), CAS persistence (LC-9), and the
// LC-12 effects.
func (s *Server) runLifecycleTransition(w http.ResponseWriter, r *http.Request, pr principal, st store.Strategy, to strategy.State, reason string) {
	strategyID, now := st.StrategyID, s.cfg.Now()
	from := strategy.State(st.LifecycleState)

	var machine *strategy.Instance
	if from == strategy.StatePaused {
		// Provenance from the audit trail: the newest to_state='paused'
		// row's from_state; none found means unknown (paper-only exit).
		prev, ok, err := s.cfg.Store.PausedProvenance(strategyID)
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		if !ok {
			prev = ""
		}
		machine = strategy.NewPausedFrom(strategy.State(prev))
	} else {
		machine = strategy.NewAt(from)
	}

	ctx, err := s.lifecycleContext(pr, strategyID, reason)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	// Live-target kill guard (LC-8, the invariant-9 carve-out): the
	// machine consumes KillCleared only on killed-exits, so live targets
	// are enforced here — promotion AND paused-resume alike.
	if to.IsLive() && !ctx.KillCleared {
		writeError(w, http.StatusUnprocessableEntity, codeIllegalTransition, "kill tier active")
		return
	}

	var report papergate.Report
	evalGate := from == strategy.StatePaper && to.IsLive()
	if evalGate {
		// The gate is evaluated synchronously in-request from persisted
		// rows only; no cached pass, no waiver (LC-15).
		if report, err = s.paperGateReport(strategyID, now); err != nil {
			s.writeInternal(w, r, err)
			return
		}
		ctx.PaperGatePassed = report.Passed
	}

	effects, terr := machine.Transition(to, ctx)
	if terr != nil {
		if evalGate && !report.Passed {
			// PAPER_GATE_FAILED only when the gate is the SOLE failure
			// (LC-11): a fresh machine with the gate forced true decides.
			passCtx := ctx
			passCtx.PaperGatePassed = true
			if _, err2 := strategy.NewAt(from).Transition(to, passCtx); err2 == nil {
				writeJSON(w, http.StatusUnprocessableEntity, paperGateErrorBody{
					Code: codePaperGateFailed, Message: "paper-gate not passed", PaperGate: report,
				})
				return
			}
		}
		// The machine is the single source of transition-table truth: its
		// error text verbatim (LC-11).
		writeError(w, http.StatusUnprocessableEntity, codeIllegalTransition, terr.Error())
		return
	}

	row := store.LifecycleTransition{
		TransitionID: uuid.NewString(),
		StrategyID:   strategyID,
		FromState:    string(from),
		ToState:      string(to),
		ActorID:      s.actorID(pr),
		ActorRole:    auditRole(pr),
		Reason:       reason,
		RecordedAt:   formatTime(now),
	}
	committed, err := s.cfg.Store.AppendLifecycleTransitionCAS(row, to.IsLive())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if errors.Is(err, store.ErrKillActive) {
		// A kill landed between the LC-8 pre-check's read and the commit:
		// the in-transaction re-check answers identically (LC-9).
		writeError(w, http.StatusUnprocessableEntity, codeIllegalTransition, "kill tier active")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if !committed {
		writeError(w, http.StatusConflict, codeLifecycleConflict,
			"lifecycle state changed concurrently; nothing written")
		return
	}
	// Persist-then-execute: effects run AFTER the commit and never roll
	// the transition back (LC-12).
	for _, eff := range effects {
		if eff == strategy.EffectCancelEntryOrders {
			s.cancelEntryOrders(r.Context(), strategyID, row.TransitionID, now)
		}
	}
	writeJSON(w, http.StatusOK, lifecycleResponse{
		StrategyID:   strategyID,
		FromState:    row.FromState,
		ToState:      row.ToState,
		TransitionID: row.TransitionID,
		RecordedAt:   row.RecordedAt,
	})
}

// lifecycleContext derives the strategy.Context guard inputs from
// persisted state and deployment wiring, never from request fields (LC-8).
func (s *Server) lifecycleContext(pr principal, strategyID, reason string) (strategy.Context, error) {
	actor := machineRole(pr)
	ctx := strategy.Context{
		Actor: actor,
		// The acting Admin/Owner's transition row IS the recorded
		// approval in v1 (approval-by-reference is deferred).
		AdminApproval:          actor == strategy.RoleAdmin || actor == strategy.RoleOwner,
		ExchangeKeysConfigured: s.cfg.ExchangeKeysConfigured,
		// Every QUALIFYING re-entry into paper restarts the window and a
		// binding kill closes it (LC-16): satisfied by construction.
		CountersReset: true,
		Reason:        reason,
	}
	if s.limits != nil {
		lim := s.limits.Limits(strategyID)
		ctx.ConfigValid = len(lim.SymbolWhitelist) > 0 &&
			lim.PerPositionNotionalCapQuote.Sign() > 0 &&
			lim.DailyLossLimitQuote.Sign() > 0 &&
			lim.MaxDrawdownPct.Sign() > 0
		ctx.L2EnvelopeConfigured = lim.L2Envelope != nil
	}
	flat, err := s.positionsFlat(strategyID)
	if err != nil {
		return ctx, err
	}
	ctx.PositionsFlat = flat
	active, err := s.cfg.Store.ActiveKill(strategyID)
	if err != nil {
		return ctx, err
	}
	ctx.KillCleared = !active
	return ctx, nil
}

// positionsFlat reports whether every positions row of the strategy is
// numerically zero; no rows means flat (LC-8).
func (s *Server) positionsFlat(strategyID string) (bool, error) {
	positions, err := s.cfg.Store.ListPositions(strategyID)
	if err != nil {
		return false, err
	}
	for _, p := range positions {
		qty, err := decimal.NewFromString(p.QtyBase)
		if err != nil {
			return false, fmt.Errorf("positions.qty_base %q: %w", p.QtyBase, err)
		}
		if !qty.IsZero() {
			return false, nil
		}
	}
	return true, nil
}

// cancelEntryOrders executes EffectCancelEntryOrders through the optional
// EntryCanceler seam (LC-12): a nil seam or an effect error never rolls
// back the transition — it logs, appends the lifecycle_entry_cancel_failed
// alert, and the handler still answers 200.
func (s *Server) cancelEntryOrders(ctx context.Context, strategyID, transitionID string, now time.Time) {
	var err error
	if s.cfg.EntryCanceler == nil {
		err = errors.New("no EntryCanceler wired")
	} else {
		err = s.cfg.EntryCanceler.CancelOpenEntries(ctx, strategyID)
	}
	if err == nil {
		return
	}
	s.cfg.Logf("api: lifecycle entry-cancel for %s: %v (paused state already demotes autonomy)", strategyID, err)
	details, _ := json.Marshal(map[string]string{"error": err.Error()})
	alert := store.SafetyAlert{
		AlertID:     uuid.NewString(),
		Kind:        "lifecycle_entry_cancel_failed",
		StrategyID:  &strategyID,
		RefID:       &transitionID,
		DetailsJSON: string(details),
		RecordedAt:  formatTime(now),
	}
	if aerr := s.cfg.Store.AppendSafetyAlert(alert); aerr != nil {
		s.cfg.Logf("api: lifecycle_entry_cancel_failed alert append: %v", aerr)
	}
}

// paperGateReport evaluates the LC-23 report from persisted rows and the
// CURRENT effective limits; it never caches and never persists (LC-15).
func (s *Server) paperGateReport(strategyID string, now time.Time) (papergate.Report, error) {
	in := papergate.Input{Now: now, Seed: s.cfg.AllocatedCapitalQuote}
	if s.limits != nil {
		lim := s.limits.Limits(strategyID)
		in.LimitsOK = true
		in.NotionalCap = lim.PerPositionNotionalCapQuote
		in.MaxDrawdownPct = lim.MaxDrawdownPct
	}
	startStr, ok, err := s.cfg.Store.PaperWindowStart(strategyID)
	if err != nil {
		return papergate.Report{}, err
	}
	if ok {
		start, perr := time.Parse(time.RFC3339, startStr)
		if perr != nil {
			return papergate.Report{}, fmt.Errorf("paper window start %q: %w", startStr, perr)
		}
		rows, err := s.cfg.Store.ListPaperGateFills(strategyID, startStr)
		if err != nil {
			return papergate.Report{}, err
		}
		fills := make([]papergate.Fill, 0, len(rows))
		for _, row := range rows {
			f := papergate.Fill{Symbol: row.Symbol, Side: row.Side, ReduceOnly: row.ReduceOnly}
			if f.QtyBase, err = decimal.NewFromString(row.QtyBase); err != nil {
				return papergate.Report{}, fmt.Errorf("fills.qty_base %q: %w", row.QtyBase, err)
			}
			if f.FillPrice, err = decimal.NewFromString(row.FillPrice); err != nil {
				return papergate.Report{}, fmt.Errorf("fills.fill_price %q: %w", row.FillPrice, err)
			}
			if f.FeeQuote, err = decimal.NewFromString(row.FeeQuote); err != nil {
				return papergate.Report{}, fmt.Errorf("fills.fee_quote %q: %w", row.FeeQuote, err)
			}
			fills = append(fills, f)
		}
		in.WindowOK, in.WindowStart, in.Fills = true, start, fills
	}
	return papergate.Evaluate(in), nil
}

// handleGetPaperGate returns the LC-23 report read-only (LC-24, promotion
// visibility). The evaluation is O(window fills): the accepted cost pin is
// the per-token 60/min bucket every authenticated route charges — the
// guard charges it on POSTs only, so this GET charges it here.
func (s *Server) handleGetPaperGate(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	if ok, retryAfter := s.rl.allow(pr.rateKey); !ok {
		writeRateLimited(w, retryAfter, "rate limit exceeded (60 req/min per token)")
		return
	}
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(pr, strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	report, err := s.paperGateReport(strategyID, s.cfg.Now())
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}
