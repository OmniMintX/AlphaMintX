package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// approvalRequest is the POST body: {verdict_id, approved}.
type approvalRequest struct {
	VerdictID string `json:"verdict_id"`
	Approved  bool   `json:"approved"`
}

// approvalResponse is the POST response: the recorded Approval plus the OMS
// submission status. Submitted is set only when a submission was attempted
// (outcome=approved with a Submitter wired), so the UI can distinguish
// approved-and-submitted from approved-but-submit-failed.
type approvalResponse struct {
	store.Approval
	Submitted       *bool  `json:"submitted,omitempty"`
	SubmitErrorCode string `json:"submit_error_code,omitempty"`
}

// handlePostApproval records the L1 decision for a verdict
// (persistence-and-api.md §L0/L1): one decision per verdict, ever; 409 with
// the recorded outcome on any second decision; approved:true runs the
// preflight and appends approved or approved_but_blocked.
func (s *Server) handlePostApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if !decodeStrict(w, r, &req) {
		return
	}

	meta, err := s.cfg.Store.GetVerdictMeta(req.VerdictID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && meta.StrategyID != r.PathValue("id")) {
		// verdict -> proposal -> strategy match is REQUIRED; a mismatch is
		// indistinguishable from an unknown verdict.
		writeError(w, http.StatusNotFound, codeUnknownVerdict, "unknown verdict")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}

	now := s.cfg.Now()
	timeoutSeconds := 0
	if p, ok, perr := s.cfg.Store.GetPendingApproval(req.VerdictID); perr != nil {
		s.writeInternal(w, r, perr)
		return
	} else if ok {
		secs, serr := pendingTimeoutSeconds(p)
		if serr != nil {
			s.writeInternal(w, r, serr)
			return
		}
		timeoutSeconds = secs
	}

	outcome := store.OutcomeRejected
	var reasons []string
	if req.Approved {
		if reasons, err = s.preflight(meta, now); err != nil {
			s.writeInternal(w, r, err)
			return
		}
		outcome = store.OutcomeApproved
		if len(reasons) > 0 {
			outcome = store.OutcomeApprovedButBlocked
		}
	}

	recorded, inserted, err := s.cfg.Store.ResolveApproval(store.Approval{
		ApprovalID:       uuid.NewString(),
		VerdictID:        meta.VerdictID,
		ProposalID:       meta.ProposalID,
		Outcome:          outcome,
		PreflightReasons: reasons,
		DecidedBy:        s.cfg.OperatorPrincipal,
		DecidedAt:        formatTime(now),
		TimeoutSeconds:   timeoutSeconds,
	})
	if errors.Is(err, store.ErrNotPending) {
		writeError(w, http.StatusUnprocessableEntity, codeNotPending, "verdict is not pending approval")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if !inserted {
		writeJSON(w, http.StatusConflict, errorBody{
			Code:     codeAlreadyDecided,
			Message:  "verdict already decided",
			Recorded: &recorded,
		})
		return
	}

	// The OMS submits at most once, on the single winning approved row. A
	// submission failure after the approval row is committed is persisted
	// (rejected_submissions, reason SUBMIT_FAILED) and surfaced in the
	// response — never logged-and-forgotten: the audit trail must not claim
	// an execution that did not happen.
	resp := approvalResponse{Approval: recorded}
	if recorded.Outcome == store.OutcomeApproved && s.cfg.Submitter != nil {
		submitted := true
		if err := s.cfg.Submitter.SubmitApproved(meta); err != nil {
			submitted = false
			resp.SubmitErrorCode = codeSubmitFailed
			s.cfg.Logf("api: submit approved verdict %s: %v", meta.VerdictID, err)
			// The approval row is already committed: a persistence failure
			// here must not mask the recorded outcome (a retried POST would
			// 409), so it is logged and the response still says
			// submitted=false.
			if perr := s.recordSubmitFailure(meta, recorded, err, now); perr != nil {
				s.cfg.Logf("api: record submit failure for verdict %s: %v", meta.VerdictID, perr)
			}
		}
		resp.Submitted = &submitted
	}
	writeJSON(w, http.StatusOK, resp)
}

// recordSubmitFailure appends the post-approval OMS submission failure to
// the rejected_submissions audit surface, keyed back to the approval and
// verdict, so approved-but-not-executed decisions stay visible.
func (s *Server) recordSubmitFailure(meta store.VerdictMeta, recorded store.Approval, submitErr error, now time.Time) error {
	payload, err := json.Marshal(map[string]string{
		"approval_id": recorded.ApprovalID,
		"verdict_id":  meta.VerdictID,
		"proposal_id": meta.ProposalID,
		"error":       submitErr.Error(),
	})
	if err != nil {
		return err
	}
	strategyID := meta.StrategyID
	return s.cfg.Store.AppendRejectedSubmission(store.RejectedSubmission{
		RejectionID: uuid.NewString(),
		StrategyID:  &strategyID,
		ReceivedAt:  formatTime(now),
		Reason:      codeSubmitFailed + ": " + submitErr.Error(),
		PayloadJSON: string(payload),
	})
}

// preflight is the lightweight decision-time re-check (§Approval preflight):
// it never re-runs riskgate.Evaluate. Empty reasons == pass.
func (s *Server) preflight(meta store.VerdictMeta, now time.Time) ([]string, error) {
	var reasons []string

	st, err := s.cfg.Store.GetStrategy(meta.StrategyID)
	if err != nil {
		return nil, err
	}
	if st.LifecycleState != "live_l1" {
		reasons = append(reasons, reasonStrategyNotLive)
	}

	// Kill-epoch unchanged since the verdict: any persisted kill event after
	// evaluated_at means the epoch moved.
	epoch, err := s.cfg.Store.MaxKillEpoch(meta.StrategyID, meta.EvaluatedAt)
	if err != nil {
		return nil, err
	}
	if epoch > 0 {
		reasons = append(reasons, reasonKillSwitchActive)
	}

	// Freshness is the MARK's age (<= max_age_seconds), never the proposal's
	// created_at: PROPOSAL_STALE does not kill the approval window.
	if s.cfg.Marks == nil {
		reasons = append(reasons, reasonMarkPriceUnavailable)
	} else if _, _, ok := s.cfg.Marks.Mark(meta.Symbol, now); !ok {
		reasons = append(reasons, reasonMarkPriceUnavailable)
	}

	if s.cfg.DailyLossBreached != nil {
		breached, err := s.cfg.DailyLossBreached(meta.StrategyID, now)
		if err != nil {
			return nil, err
		}
		if breached {
			reasons = append(reasons, reasonDailyLossLimitBreach)
		}
	}

	// No Submitter wired (L0/paper deployments, or serve mode before the
	// OMS seam is built): an approval could never reach the OMS, so it is
	// blocked instead of recorded as a false "submitted" execution.
	if s.cfg.Submitter == nil {
		reasons = append(reasons, reasonSubmitterUnavailable)
	}
	return reasons, nil
}

// pendingTimeoutSeconds derives approvals.timeout_seconds from the pending
// row's persisted window (deadline_at - created_at).
func pendingTimeoutSeconds(p store.PendingApproval) (int, error) {
	created, err := time.Parse(time.RFC3339Nano, p.CreatedAt)
	if err != nil {
		return 0, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, p.DeadlineAt)
	if err != nil {
		return 0, err
	}
	return int(deadline.Sub(created) / time.Second), nil
}

// decodeStrict decodes a POST body with unknown fields rejected; it writes
// 413 on an oversized body and 400 on any other decode failure.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		// Exactly one JSON value.
		if dec.More() {
			err = errors.New("trailing data after JSON body")
		} else {
			return true
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "body exceeds 1 MiB")
		return false
	}
	writeError(w, http.StatusBadRequest, codeSchemaInvalid, "malformed request body: "+err.Error())
	return false
}
