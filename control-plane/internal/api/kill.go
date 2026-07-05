package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// platformKillAck is the mandatory case-sensitive acknowledgment literal:
// it prevents a fat-fingered platform-wide halt (safety-wiring.md §Kill
// endpoints — the same explicit-literal pattern as
// CONTROLPLANE_LIVE_PROD_ACK).
const platformKillAck = "KILL-PLATFORM"

// killRequest is the optional strategy/tenant kill body. flatten is the
// operator's trigger-time choice and the WIRE default is false: an absent
// body or field never flattens (safety-wiring.md §Kill endpoints).
type killRequest struct {
	Flatten bool `json:"flatten"`
}

// platformKillRequest additionally carries the mandatory ack literal.
type platformKillRequest struct {
	Ack     string `json:"ack"`
	Flatten bool   `json:"flatten"`
}

// strategyKillResponse acknowledges the persisted strategy kill event —
// persistence only, never effect completion (safety-wiring.md invariant 1).
type strategyKillResponse struct {
	EventID    string `json:"event_id"`
	StrategyID string `json:"strategy_id"`
	KillEpoch  int64  `json:"kill_epoch"`
	RecordedAt string `json:"recorded_at"`
	Flatten    bool   `json:"flatten"`
}

// platformKillResponse acknowledges the persisted platform kill event.
type platformKillResponse struct {
	EventID    string `json:"event_id"`
	KillEpoch  int64  `json:"kill_epoch"`
	RecordedAt string `json:"recorded_at"`
	Flatten    bool   `json:"flatten"`
}

// handleStrategyKill is the strategy-tier kill switch (safety-wiring.md
// §Kill endpoints): trader/admin/owner own tenant — the path strategy is
// tenant-resolved FIRST, so a foreign or absent strategy is 404
// UNKNOWN_STRATEGY identical to absence (no existence oracle) — env-admin
// any strategy. The event is persisted and acknowledged BEFORE any side
// effect; effects run asynchronously through the SafetyDriver seam.
func (s *Server) handleStrategyKill(w http.ResponseWriter, r *http.Request) {
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
	var req killRequest
	if !decodeStrictOptional(w, r, &req) {
		return
	}
	now := s.cfg.Now()
	eventID := uuid.NewString()
	epoch, err := s.cfg.Store.AppendStrategyKill(eventID, strategyID, s.actorID(pr), formatTime(now), req.Flatten)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, strategyKillResponse{
		EventID:    eventID,
		StrategyID: strategyID,
		KillEpoch:  epoch,
		RecordedAt: formatTime(now),
		Flatten:    req.Flatten,
	})
	s.driveSafety()
}

// handlePlatformKill is the platform-tier kill switch (safety-wiring.md
// §Kill endpoints): env-admin ONLY (permission matrix — no tenant role may
// kill the platform). The body MUST carry the literal acknowledgment
// "ack": "KILL-PLATFORM"; anything else is 400 PLATFORM_KILL_ACK_REQUIRED
// and NO row is written. The row's both-NULL scope ids make the existing
// 3-clause kill predicate bind every strategy of every tenant.
func (s *Server) handlePlatformKill(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	var req platformKillRequest
	if !decodeStrictOptional(w, r, &req) {
		return
	}
	if req.Ack != platformKillAck {
		writeError(w, http.StatusBadRequest, codePlatformKillAckRequired,
			`platform kill requires the acknowledgment "ack": "KILL-PLATFORM"`)
		return
	}
	now := s.cfg.Now()
	eventID := uuid.NewString()
	epoch, err := s.cfg.Store.AppendPlatformKill(eventID, s.actorID(pr), formatTime(now), req.Flatten)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, platformKillResponse{
		EventID:    eventID,
		KillEpoch:  epoch,
		RecordedAt: formatTime(now),
		Flatten:    req.Flatten,
	})
	s.driveSafety()
}

// driveSafety invokes the optional SafetyDriver seam in a detached
// goroutine AFTER the response is written (safety-wiring.md §API surface):
// the handler never waits on effects, driver errors are logged and never
// surfaced, and a panic is recovered so the server never crashes from it.
// A drive orphaned by server shutdown is safe: unserved rows re-drive on
// restart (crash-resumable by design, safety-wiring.md invariant 4).
func (s *Server) driveSafety() {
	d := s.cfg.SafetyDriver
	if d == nil {
		return
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				s.cfg.Logf("api: safety driver panic: %v", p)
			}
		}()
		if err := d.DriveSafetyEffects(context.Background()); err != nil {
			s.cfg.Logf("api: safety driver: %v", err)
		}
	}()
}
