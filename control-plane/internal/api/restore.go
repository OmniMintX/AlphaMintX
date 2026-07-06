package api

// Restore-gate API surface (docs/specs/deploy-and-survive.md DS-5/DS-6):
// the status read renders WHY proposals/approvals 503 (read + env-admin)
// and the ack clears the gate (env-admin ONLY). Both routes are always
// registered when a Store exists — unlike the backup routes they do NOT
// require CONTROLPLANE_BACKUP_DIR: a restore can happen on a deployment
// whose backup dir is not (yet) configured.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// restoreStatusResponse is the DS-6 body.
type restoreStatusResponse struct {
	Engaged bool `json:"engaged"`
}

// handleGetRestoreStatus is GET /api/v1/ops/restore (DS-6): platform
// operational data, never tenant-confidential (the GET /api/v1/alerts
// OS-19 precedent).
func (s *Server) handleGetRestoreStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, restoreStatusResponse{Engaged: s.cfg.Store.RestoreGateEngaged()})
}

// handlePostRestoreAck is POST /api/v1/ops/restore/ack (DS-5): empty
// body, never parsed. Exactly one concurrent ack wins (the store CAS);
// user_version = 0 and the cleared alert commit in one transaction. The
// loser — and any ack when the gate is not engaged — is 409
// RESTORE_GATE_NOT_ENGAGED (catches acks aimed at the wrong deployment).
func (s *Server) handlePostRestoreAck(w http.ResponseWriter, r *http.Request) {
	actor := s.actorID(principalFrom(r))
	details, err := json.Marshal(map[string]string{"actor_id": actor})
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	err = s.cfg.Store.ClearRestoreGate(store.SafetyAlert{
		AlertID:     uuid.NewString(),
		Kind:        "restore_gate_cleared",
		DetailsJSON: string(details),
		RecordedAt:  formatTime(s.cfg.Now()),
	})
	if errors.Is(err, store.ErrRestoreGateNotEngaged) {
		writeError(w, http.StatusConflict, codeRestoreGateNotEngaged, "restore gate is not engaged")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	s.cfg.Logf("restore gate cleared by %s (deploy-and-survive.md DS-5)", actor)
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
