package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// proposalResponse is the 200 submission envelope (persistence-and-api.md
// §HTTP API): verdict always (the persisted canonical bytes); submitted
// only when THIS request attempted an OMS submission; submit_error_code
// only on a failed attempt; pending_approval only when THIS request armed
// the L1/escalation timer. A verbatim duplicate carries the stored verdict
// alone — it re-reports nothing it did not do.
type proposalResponse struct {
	Verdict         json.RawMessage `json:"verdict"`
	Submitted       *bool           `json:"submitted,omitempty"`
	SubmitErrorCode string          `json:"submit_error_code,omitempty"`
	PendingApproval bool            `json:"pending_approval,omitempty"`
}

// handlePostProposal ingests one TradeProposal submission envelope
// (docs/ARCHITECTURE.md plane rules; persistence-and-api.md Row rules):
// at-least-once delivery made idempotent by the atomic proposal_id insert —
// a duplicate answers the ORIGINAL verdict verbatim, never re-evaluated,
// never a second order; a same-id different-payload (or different-tick)
// submission is 409 IDEMPOTENCY_CONFLICT and a run/tick contradiction is
// 409 RUN_TICK_CONFLICT. Malformed or out-of-scope submissions are recorded
// append-only in rejected_submissions and NEVER earn a verdict (gate step 0a).
func (s *Server) handlePostProposal(w http.ResponseWriter, r *http.Request) {
	strategyID := r.PathValue("id")
	now := s.cfg.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "body exceeds 1 MiB")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	var sub store.ProposalSubmission
	if msg := decodeSubmission(body, &sub); msg != "" {
		s.rejectSubmission(w, r, strategyID, body, codeSchemaInvalid, msg, http.StatusBadRequest, now)
		return
	}
	// The middleware matched the token scope to the path {id}; the
	// proposal's strategy_id must agree. Auth failures never produce
	// verdicts (STRATEGY_SCOPE_MISMATCH, recorded rejected).
	if sub.Proposal.StrategyID != strategyID {
		s.rejectSubmission(w, r, strategyID, body, codeStrategyScopeMismatch,
			"proposal strategy_id outside the token scope", http.StatusForbidden, now)
		return
	}

	// Gate evaluations for a strategy are serialized (risk-limits.md); the
	// duplicate check and the rate-limit charge sit under the same lock so
	// a verbatim retry never burns a token and never races an evaluation.
	lock := s.strategyLock(strategyID)
	lock.Lock()
	defer lock.Unlock()

	// The strategy row is read UNDER the lock (LC-14): a lifecycle
	// transition committed while this request waited — a pause, say — is
	// observed; a stale pre-lock snapshot could route a submission for a
	// strategy that is no longer live.
	st, err := s.cfg.Store.GetStrategy(strategyID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}

	dup, err := s.cfg.Store.IsDuplicateProposal(sub)
	if err != nil && !errors.Is(err, store.ErrIdempotencyConflict) {
		s.writeInternal(w, r, err)
		return
	}
	if dup {
		payload, verr := s.cfg.Store.GetVerdictByProposalID(sub.Proposal.ProposalID)
		if verr == nil {
			// At-least-once retry: the ORIGINAL verdict verbatim, free of
			// charge — no rate-limit token, no re-evaluation, no flags.
			writeJSON(w, http.StatusOK, proposalResponse{Verdict: payload})
			return
		}
		if !errors.Is(verr, store.ErrNotFound) {
			s.writeInternal(w, r, verr)
			return
		}
		// Proposal persisted but verdict missing (crash between the two
		// inserts): fall through and evaluate now — still one verdict ever.
	}

	// Per-strategy proposal rate limit (default 30/min): fresh evaluations
	// and conflicts charge; excess is 429 with NO persisted verdict.
	if !s.prl.allow(strategyID) {
		writeError(w, http.StatusTooManyRequests, codeRateLimited,
			"proposal rate limit exceeded (30/min per strategy)")
		return
	}

	if _, err := s.cfg.Store.InsertProposal(sub, now); err != nil {
		switch {
		case errors.Is(err, store.ErrIdempotencyConflict):
			writeError(w, http.StatusConflict, codeIdempotencyConflict,
				"proposal_id already ingested with a different payload or tick")
		case errors.Is(err, store.ErrRunTickConflict):
			writeError(w, http.StatusConflict, codeRunTickConflict,
				"a different run already owns this (strategy_id, tick_number)")
		default:
			s.writeInternal(w, r, err)
		}
		return
	}

	resp, err := s.evaluateAndRoute(st, sub.Proposal, now)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// evaluateAndRoute runs the deterministic gate over the hydrated runtime
// state, persists the single verdict, and applies the L0–L3 execution
// semantics to gate-approved outcomes. The response envelope carries the
// same canonical verdict bytes the store persisted.
func (s *Server) evaluateAndRoute(st store.Strategy, p *contract.Proposal, now time.Time) (proposalResponse, error) {
	state, err := s.cfg.RuntimeState.State(st.StrategyID, st.LifecycleState, p.Symbol, now)
	if err != nil {
		return proposalResponse{}, err
	}
	// The LimitsProvider is the single read path for effective limits
	// (multi-tenant-rbac.md §Runtime limit changes): the gate observes a
	// runtime change on the next evaluation.
	verdict := riskgate.Evaluate(p, s.limits.Limits(st.StrategyID), state, now)
	if _, err := s.cfg.Store.InsertVerdict(&verdict); err != nil {
		return proposalResponse{}, err
	}
	raw, err := json.Marshal(&verdict)
	if err != nil {
		return proposalResponse{}, err
	}
	resp := proposalResponse{Verdict: raw}
	if err := s.routeExecution(st, p, &verdict, now, &resp); err != nil {
		return proposalResponse{}, err
	}
	return resp, nil
}

// routeExecution decides what happens to a persisted verdict
// (strategy-lifecycle.md autonomy ladder): escalate and live_l1
// approve/clip verdicts arm the restart-safe L1 approval timer; paper and
// live_l2/l3 approve/clip verdicts submit to the OMS at once (paper OMS
// only in Phase 1); everything else — hold, reject, and the L0 floor
// (draft/paused/killed) — is persisted and shown, never submitted. The
// outcome (submitted / pending_approval) is reported on resp.
func (s *Server) routeExecution(st store.Strategy, p *contract.Proposal, v *contract.Verdict, now time.Time, resp *proposalResponse) error {
	if p.Action == contract.ActionHold {
		return nil // approve verdict, no order (gate rule)
	}
	switch v.Decision {
	case contract.DecisionEscalate:
		return s.armPendingApproval(v, st.StrategyID, now, resp)
	case contract.DecisionApprove, contract.DecisionClip:
	default:
		return nil // reject: no order, ever
	}
	switch st.LifecycleState {
	case "live_l1":
		return s.armPendingApproval(v, st.StrategyID, now, resp)
	case "paper", "live_l2", "live_l3":
		if st.LifecycleState == "paper" && !s.cfg.PaperSubmitter {
			// Live-mode paper floor (lifecycle-api.md LC-14a): paper track
			// records are built against the paper OMS, never the live
			// venue — the verdict persists, nothing submits.
			return nil
		}
		if s.cfg.Submitter == nil {
			return nil // no OMS wired: persisted, never falsely "submitted"
		}
		meta := store.VerdictMeta{
			VerdictID:   v.VerdictID,
			ProposalID:  v.ProposalID,
			StrategyID:  st.StrategyID,
			Symbol:      p.Symbol,
			Decision:    string(v.Decision),
			EvaluatedAt: v.EvaluatedAt.String(),
		}
		submitted := true
		if err := s.cfg.Submitter.SubmitApproved(meta); err != nil {
			// The verdict stays valid; the failed submission is persisted
			// to the audit surface, never logged-and-forgotten.
			submitted = false
			resp.SubmitErrorCode = codeSubmitFailed
			s.cfg.Logf("api: submit proposal %s: %v", p.ProposalID, err)
			if err := s.recordSubmitFailure(meta, "", err, now); err != nil {
				return err
			}
		}
		resp.Submitted = &submitted
	}
	return nil
}

// armPendingApproval arms the L1/escalation timer and reports it on resp.
func (s *Server) armPendingApproval(v *contract.Verdict, strategyID string, now time.Time, resp *proposalResponse) error {
	if err := s.createPendingApproval(v, strategyID, now); err != nil {
		return err
	}
	resp.PendingApproval = true
	return nil
}

// createPendingApproval arms the restart-safe L1/escalation approval timer.
func (s *Server) createPendingApproval(v *contract.Verdict, strategyID string, now time.Time) error {
	timeout := riskgate.DefaultL1ApprovalTimeoutSeconds
	if t := s.limits.Limits(strategyID).L1ApprovalTimeoutSeconds; t > 0 {
		timeout = t
	}
	return s.cfg.Store.CreatePendingApproval(v.VerdictID, strategyID, now, timeout)
}

// rejectSubmission records a submission that never earns a verdict
// (malformed or out of token scope) and answers with its error.
func (s *Server) rejectSubmission(w http.ResponseWriter, r *http.Request, strategyID string, body []byte, code, msg string, status int, now time.Time) {
	if err := s.cfg.Store.AppendRejectedSubmission(store.RejectedSubmission{
		RejectionID: uuid.NewString(),
		StrategyID:  &strategyID,
		ReceivedAt:  formatTime(now),
		Reason:      code + ": " + msg,
		PayloadJSON: string(body),
	}); err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeError(w, status, code, msg)
}

// submissionEnvelope mirrors store.ProposalSubmission with tick_number
// presence detection: a missing tick_number is a 400, never silently tick 0
// (the run row would be mis-keyed).
type submissionEnvelope struct {
	TickNumber *int               `json:"tick_number"`
	Proposal   *contract.Proposal `json:"proposal"`
}

// decodeSubmission strictly decodes the submission envelope and enforces
// the gate step-0a identity requirements (missing/invalid ids never earn a
// verdict); "" means valid enough to evaluate.
func decodeSubmission(body []byte, sub *store.ProposalSubmission) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var env submissionEnvelope
	if err := dec.Decode(&env); err != nil {
		return "malformed submission envelope: " + err.Error()
	}
	if dec.More() {
		return "trailing data after JSON body"
	}
	if env.Proposal == nil {
		return "proposal is required"
	}
	if env.TickNumber == nil {
		return "tick_number is required"
	}
	if *env.TickNumber < 0 {
		return "tick_number must be >= 0"
	}
	p := env.Proposal
	if !uuidPattern.MatchString(p.ProposalID) {
		return "proposal_id is not a lowercase UUID"
	}
	if !uuidPattern.MatchString(p.StrategyID) {
		return "strategy_id is not a lowercase UUID"
	}
	if !uuidPattern.MatchString(p.AgentTraceID) {
		return "agent_trace_id is not a lowercase UUID"
	}
	sub.TickNumber, sub.Proposal = *env.TickNumber, env.Proposal
	return ""
}
