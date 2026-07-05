package api

import (
	"net/http"
	"time"
)

// HeartbeatSink receives heartbeat beats (docs/specs/watchdog.md §Wiring
// seams): the safety.Monitor's Beat satisfies it — api declares the
// interface, main.go wires the implementation (the SafetyDriver
// precedent). nil in paper mode; the handler answers 200 either way
// (WD-3).
type HeartbeatSink interface {
	Beat(strategyID string, at time.Time)
}

// heartbeatRequest is the WD-4 body: `{}` — no fields in v1; unknown
// fields and trailing data are rejected by decodeStrictOptional's strict
// decode, and an EMPTY body is accepted as `{}`.
type heartbeatRequest struct{}

// heartbeatResponse acknowledges RECEIPT only — the server clock instant
// recorded as the beat, never watchdog evaluation state (WD-5).
type heartbeatResponse struct {
	ReceivedAt string `json:"received_at"`
}

// handleHeartbeat is POST /api/v1/strategies/{id}/heartbeat (watchdog.md
// WD-1..WD-7): agent class only, own strategy — the guard already
// enforced auth, class, and the strategy scope. NO lifecycle predicate
// and NO store write: a beat for a paused, killed, or paper strategy is
// accepted identically (the WATCH SET decides what silence means), and
// receipt is an in-memory timestamp update on the Monitor when one is
// wired. The per-strategy proposal limiter is NEVER charged (WD-6).
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if !decodeStrictOptional(w, r, &req) {
		return
	}
	now := s.cfg.Now()
	if s.cfg.Heartbeats != nil {
		s.cfg.Heartbeats.Beat(r.PathValue("id"), now)
	}
	writeJSON(w, http.StatusOK, heartbeatResponse{ReceivedAt: formatTime(now)})
}
