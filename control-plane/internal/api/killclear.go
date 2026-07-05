package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// platformClearAck is the mandatory case-sensitive acknowledgment literal
// for platform-wide clears (LC-30, the KILL-PLATFORM ack pattern).
const platformClearAck = "CLEAR-PLATFORM"

// clearRequest is the REQUIRED, strictly-decoded LC-30 body; observed_epoch
// is a pointer so absence is detectable (400 SCHEMA_INVALID, never a
// silent 0).
type clearRequest struct {
	Reason        string `json:"reason"`
	ObservedEpoch *int64 `json:"observed_epoch"`
}

// platformClearRequest additionally carries the mandatory ack literal.
type platformClearRequest struct {
	Reason        string `json:"reason"`
	ObservedEpoch *int64 `json:"observed_epoch"`
	Ack           string `json:"ack"`
}

// clearResponse is the LC-33 envelope; SupersededEventIDs is never null
// and the scope-id fields render only on their tier.
type clearResponse struct {
	ClearID            string   `json:"clear_id"`
	Scope              string   `json:"scope"`
	StrategyID         string   `json:"strategy_id,omitempty"`
	TenantID           string   `json:"tenant_id,omitempty"`
	ClearedEpoch       int64    `json:"cleared_epoch"`
	RecordedAt         string   `json:"recorded_at"`
	SupersededEventIDs []string `json:"superseded_event_ids"`
}

// decodeClearBody enforces the LC-30 base requirements; false means the
// error is already written.
func decodeClearBody(w http.ResponseWriter, reason string, observed *int64) bool {
	if reason == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "reason is required")
		return false
	}
	if observed == nil {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "observed_epoch is required")
		return false
	}
	return true
}

// writeClearError maps the append sentinels: the active check answers
// before the epoch verification (LC-27/LC-31 order).
func (s *Server) writeClearError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNoActiveKill):
		writeError(w, http.StatusUnprocessableEntity, codeNoActiveKill,
			"no active kill at this scope; nothing written")
	case errors.Is(err, store.ErrClearConflict):
		writeError(w, http.StatusConflict, codeClearConflict,
			"observed_epoch is stale: a kill landed since the read; nothing written")
	default:
		s.writeInternal(w, r, err)
	}
}

// handleStrategyKillClear clears the strategy-scope standing kill (LC-29:
// admin/owner own tenant, env-admin any strategy — one level stricter than
// kill). Resolution precedes body semantics (LC-31); clears never drive
// safety effects (LC-38 — supersession happens inside the append).
func (s *Server) handleStrategyKillClear(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	strategyID := r.PathValue("id")
	st, err := s.rootStrategy(pr, strategyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	var req clearRequest
	if !decodeStrict(w, r, &req) || !decodeClearBody(w, req.Reason, req.ObservedEpoch) {
		return
	}
	now, clearID := s.cfg.Now(), uuid.NewString()
	epoch, superseded, err := s.cfg.Store.AppendKillClearStrategy(
		clearID, strategyID, s.actorID(pr), req.Reason, *req.ObservedEpoch, formatTime(now))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if err != nil {
		s.writeClearError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clearResponse{
		ClearID: clearID, Scope: "strategy",
		StrategyID: strategyID, TenantID: st.TenantID,
		ClearedEpoch: epoch, RecordedAt: formatTime(now),
		SupersededEventIDs: nonNil(superseded),
	})
}

// handleTenantKillClear clears the tenant-scope standing kill: admin/owner
// OWN tenant only (a foreign tenant path is 404, no existence oracle);
// env-admin any existing tenant — the tenant kill handler's resolution.
func (s *Server) handleTenantKillClear(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	tenantID := r.PathValue("tenant_id")
	if pr.tenantBound() {
		if tenantID != pr.tenantID {
			writeError(w, http.StatusNotFound, codeUnknownTenant, "unknown tenant")
			return
		}
	} else if _, err := s.cfg.Store.GetTenant(tenantID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownTenant, "unknown tenant")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	var req clearRequest
	if !decodeStrict(w, r, &req) || !decodeClearBody(w, req.Reason, req.ObservedEpoch) {
		return
	}
	now, clearID := s.cfg.Now(), uuid.NewString()
	epoch, superseded, err := s.cfg.Store.AppendKillClearTenant(
		clearID, tenantID, s.actorID(pr), req.Reason, *req.ObservedEpoch, formatTime(now))
	if err != nil {
		s.writeClearError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clearResponse{
		ClearID: clearID, Scope: "tenant", TenantID: tenantID,
		ClearedEpoch: epoch, RecordedAt: formatTime(now),
		SupersededEventIDs: nonNil(superseded),
	})
}

// handlePlatformKillClear clears the platform-scope standing kill —
// env-admin ONLY (LC-29). The body additionally REQUIRES the literal
// "ack": "CLEAR-PLATFORM"; anything else is 400 and NO row is written
// (LC-30). Phase-1 global kill rows (both ids NULL) classify as platform.
func (s *Server) handlePlatformKillClear(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	var req platformClearRequest
	if !decodeStrict(w, r, &req) || !decodeClearBody(w, req.Reason, req.ObservedEpoch) {
		return
	}
	if req.Ack != platformClearAck {
		writeError(w, http.StatusBadRequest, codePlatformClearAckRequired,
			`platform clear requires the acknowledgment "ack": "CLEAR-PLATFORM"`)
		return
	}
	now, clearID := s.cfg.Now(), uuid.NewString()
	epoch, superseded, err := s.cfg.Store.AppendKillClearPlatform(
		clearID, s.actorID(pr), req.Reason, *req.ObservedEpoch, formatTime(now))
	if err != nil {
		s.writeClearError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, clearResponse{
		ClearID: clearID, Scope: "platform",
		ClearedEpoch: epoch, RecordedAt: formatTime(now),
		SupersededEventIDs: nonNil(superseded),
	})
}

// nonNil keeps superseded_event_ids a JSON array, never null (LC-33).
func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
