package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/live"
)

// handleGetReconStatus returns the reconciliation status, tenant-filtered
// by the principal's scope (live-oms-and-reconciler.md §API surface): env
// classes ("" tenant) see the full account-level payload; tenant principals
// only the restricted subset plus their own strategies' counts.
func (s *Server) handleGetReconStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.cfg.ReconStatus.Status(listTenant(principalFrom(r)))
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// reconRunRequest is the OPTIONAL POST /api/v1/oms/recon/run body:
// accept_venue_reset acknowledges a detected venue reset and bumps the
// venue epoch (§Venue epochs).
type reconRunRequest struct {
	AcceptVenueReset bool `json:"accept_venue_reset"`
}

// handlePostReconRun runs R1-R7 synchronously (env-admin ONLY, a deployer
// act like the billing POSTs): 200 with the run_completed counters, 409
// RECON_RUNNING when a run is in progress. The body is optional; unknown
// fields are rejected.
func (s *Server) handlePostReconRun(w http.ResponseWriter, r *http.Request) {
	var req reconRunRequest
	if !decodeStrictOptional(w, r, &req) {
		return
	}
	if err := s.cfg.ReconStatus.TriggerRun(r.Context(), req.AcceptVenueReset); err != nil {
		if errors.Is(err, live.ErrReconRunning) {
			writeError(w, http.StatusConflict, codeReconRunning, "a reconcile run is in progress")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	status, err := s.cfg.ReconStatus.Status("")
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	run := status.LastRun
	if run == nil {
		run = &live.ReconRun{}
	}
	writeJSON(w, http.StatusOK, run)
}

// decodeStrictOptional is decodeStrict for routes whose body is OPTIONAL:
// an empty body leaves v at its zero value; anything present must decode
// strictly.
func decodeStrictOptional(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	switch {
	case errors.Is(err, io.EOF):
		return true // empty body
	case err == nil && dec.More():
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "trailing data after JSON body")
		return false
	case err != nil:
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codeBodyTooLarge, "body exceeds 1 MiB")
			return false
		}
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "malformed request body: "+err.Error())
		return false
	}
	return true
}
