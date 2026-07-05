package api

// Operator-surface read endpoints (docs/specs/operator-surface.md): the
// OS-7 composite safety status, the OS-16 per-strategy alert feed, and the
// OS-21 global feed. All three are read-only — no store write, no alert
// append, no drive, no rate-bucket charge (OS-13; the guard charges
// non-GET requests only).

import (
	"errors"
	"net/http"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// WatchdogLiveness is the OS-12 heartbeat-liveness seam: the
// safety.Monitor's LastBeat satisfies it — api declares the interface,
// main.go wires the implementation (the HeartbeatSink/SafetyDriver
// precedent; no api → safety import). nil (paper mode, or watchdog
// disabled) renders {"enabled": false} with nulls.
type WatchdogLiveness interface {
	LastBeat(strategyID string) (at time.Time, ok bool)
}

// safetyKillCleared is the OS-7 cleared object (nested wire DTO).
type safetyKillCleared struct {
	ClearID      string `json:"clear_id"`
	ActorID      string `json:"actor_id"`
	Reason       string `json:"reason"`
	RecordedAt   string `json:"recorded_at"`
	ClearedEpoch int64  `json:"cleared_epoch"`
}

// safetyKill is the OS-8a wire DTO: EXACTLY the OS-7 kill keys — the
// store's BoundKill json tags never reach the response (no tenant_id, no
// trigger_ref, no kind).
type safetyKill struct {
	EventID    string             `json:"event_id"`
	Scope      string             `json:"scope"`
	KillEpoch  int64              `json:"kill_epoch"`
	Flatten    bool               `json:"flatten"`
	ActorID    string             `json:"actor_id"`
	RecordedAt string             `json:"recorded_at"`
	Cleared    *safetyKillCleared `json:"cleared"`
}

type safetyBreakerEvent struct {
	EventID    string  `json:"event_id"`
	RecordedAt string  `json:"recorded_at"`
	TriggerRef *string `json:"trigger_ref"`
}

type safetyBreaker struct {
	ActiveToday bool                `json:"active_today"`
	Event       *safetyBreakerEvent `json:"event"`
}

type safetyWatchdog struct {
	Enabled         bool    `json:"enabled"`
	LastHeartbeatAt *string `json:"last_heartbeat_at"`
	SecondsSince    *int64  `json:"seconds_since"`
}

// safetyStatusResponse is the OS-7 composite envelope.
type safetyStatusResponse struct {
	StrategyID     string         `json:"strategy_id"`
	LifecycleState string         `json:"lifecycle_state"`
	PausedFrom     *string        `json:"paused_from"`
	ActiveKill     bool           `json:"active_kill"`
	Kills          []safetyKill   `json:"kills"`
	Breaker        safetyBreaker  `json:"breaker"`
	Watchdog       safetyWatchdog `json:"watchdog"`
}

// killScope derives the response scope from id NULL-ness (OS-8): never the
// legacy scope column, so Phase-1 global rows report platform (LC-26).
func killScope(e store.KillBreakerEvent) string {
	switch {
	case e.StrategyID != nil:
		return "strategy"
	case e.TenantID != nil:
		return "tenant"
	default:
		return "platform"
	}
}

// handleGetSafetyStatus is GET /api/v1/strategies/{id}/safety
// (OS-5..OS-14): tenant-scoped resolution per OS-6, then the OS-10a
// single-snapshot store read; the watchdog object stays OUTSIDE the
// snapshot (a different truth source), with seconds_since derived from
// the SAME now reading that names the UTC date.
func (s *Server) handleGetSafetyStatus(w http.ResponseWriter, r *http.Request) {
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
	now := s.cfg.Now()
	st, err := s.cfg.Store.SafetyStatus(strategyID, now.UTC().Format("2006-01-02"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	resp := safetyStatusResponse{
		StrategyID:     strategyID,
		LifecycleState: st.LifecycleState,
		PausedFrom:     st.PausedFrom,
		ActiveKill:     st.ActiveKill,
		Kills:          []safetyKill{},
	}
	for _, k := range st.Kills {
		wire := safetyKill{
			EventID: k.EventID, Scope: killScope(k.KillBreakerEvent),
			ActorID: k.ActorID, RecordedAt: k.RecordedAt,
		}
		if k.KillEpoch != nil {
			wire.KillEpoch = *k.KillEpoch
		}
		// A NULL flatten column (pre-flatten-era rows) renders false (OS-8).
		if k.Flatten != nil {
			wire.Flatten = *k.Flatten
		}
		if k.Cleared != nil {
			wire.Cleared = &safetyKillCleared{
				ClearID: k.Cleared.ClearID, ActorID: k.Cleared.ActorID,
				Reason: k.Cleared.Reason, RecordedAt: k.Cleared.RecordedAt,
				ClearedEpoch: k.Cleared.ClearedEpoch,
			}
		}
		resp.Kills = append(resp.Kills, wire)
	}
	resp.Breaker = safetyBreaker{ActiveToday: st.BreakerActiveToday}
	if st.BreakerEvent != nil {
		resp.Breaker.Event = &safetyBreakerEvent{
			EventID:    st.BreakerEvent.EventID,
			RecordedAt: st.BreakerEvent.RecordedAt,
			TriggerRef: st.BreakerEvent.TriggerRef,
		}
	}
	resp.Watchdog = s.watchdogLiveness(strategyID, now)
	writeJSON(w, http.StatusOK, resp)
}

// watchdogLiveness renders the OS-12 object: seam nil ⇒ enabled false with
// nulls; wired with no beat ⇒ enabled true with nulls (never a baseline —
// invariant 7); a beat ⇒ its instant plus floor(now − beat) whole seconds,
// clamped to 0 on clock skew.
func (s *Server) watchdogLiveness(strategyID string, now time.Time) safetyWatchdog {
	wd := safetyWatchdog{Enabled: s.cfg.Watchdog != nil}
	if s.cfg.Watchdog == nil {
		return wd
	}
	at, ok := s.cfg.Watchdog.LastBeat(strategyID)
	if !ok {
		return wd
	}
	beat := formatTime(at)
	secs := int64(now.Sub(at) / time.Second)
	if secs < 0 {
		secs = 0
	}
	wd.LastHeartbeatAt, wd.SecondsSince = &beat, &secs
	return wd
}

// handleGetStrategyAlerts is GET /api/v1/strategies/{id}/alerts
// (OS-15..OS-18): tenant-scoped resolution per OS-6, then the standard
// pagination envelope over strategy_id = {id} rows only, newest first.
func (s *Server) handleGetStrategyAlerts(w http.ResponseWriter, r *http.Request) {
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(principalFrom(r), strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListSafetyAlertsByStrategyPage(strategyID, pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

// handleGetGlobalAlerts is GET /api/v1/alerts (OS-19..OS-21): env read and
// env-admin classes only (the matrix row 403s every DB principal before
// this handler runs); all rows including NULL strategy_id, optional exact
// ?kind= filter on the open set.
func (s *Server) handleGetGlobalAlerts(w http.ResponseWriter, r *http.Request) {
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListSafetyAlertsGlobalPage(r.URL.Query().Get("kind"), pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}
