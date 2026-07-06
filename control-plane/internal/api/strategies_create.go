package api

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// createStrategyRequest is the POST /api/v1/strategies body
// (strategy-provisioning.md SP-2). No id field: strategy_id is
// server-generated (SP-4).
type createStrategyRequest struct {
	TenantID       string `json:"tenant_id"`
	Name           string `json:"name"`
	LifecycleState string `json:"lifecycle_state"`
}

// validateStrategyName enforces the SP-2 content rules on the TRIMMED
// name: 1-128 bytes, valid UTF-8, no C0/C1 controls, no bidi overrides
// (names render in the operator's strategy list; bidi controls could
// spoof it). U+FFFD is rejected too: encoding/json COERCES invalid UTF-8
// and lone surrogates to U+FFFD before this code runs, so the
// replacement char is the only observable artifact of a mangled name —
// rejecting it is how "invalid UTF-8 => 400" stays enforceable through
// decodeStrict. Returns "" when valid, else the 400 message.
func validateStrategyName(name string) string {
	if name == "" {
		return "name is required (1-128 bytes after trimming)"
	}
	if len(name) > 128 {
		return "name exceeds 128 bytes after trimming"
	}
	if !utf8.ValidString(name) {
		return "name must be valid UTF-8"
	}
	for _, r := range name {
		switch {
		case r < 0x20, r >= 0x7F && r <= 0x9F:
			return "name must not contain control characters"
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return "name must not contain bidi control characters"
		case r == utf8.RuneError:
			return "name must be valid UTF-8 (no replacement characters)"
		}
	}
	return ""
}

// handleCreateStrategy is POST /api/v1/strategies
// (strategy-provisioning.md): tenant owner/admin create in their own
// tenant, env-admin in any existing tenant (resolveMintTenant semantics,
// SP-3). Initial state is draft (default) or paper ONLY — every live_*
// tier is 400, the paper gate cannot be bypassed at birth (SP-2). NOT
// gated by the restore gate and not blocked by standing kills: creation
// is not trading intent (SP-6).
func (s *Server) handleCreateStrategy(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	var req createStrategyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	tenantID, ok := s.resolveMintTenant(w, r, pr, req.TenantID)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	if msg := validateStrategyName(name); msg != "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, msg)
		return
	}
	state := req.LifecycleState
	if state == "" {
		state = "draft"
	}
	if state != "draft" && state != "paper" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid,
			"lifecycle_state must be draft or paper (live tiers require the lifecycle endpoint and its paper gate)")
		return
	}
	now := formatTime(s.cfg.Now())
	st := store.Strategy{
		StrategyID:     uuid.NewString(),
		TenantID:       tenantID,
		Name:           name,
		LifecycleState: state,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	err := s.cfg.Store.CreateStrategyProvisioned(st, s.actorID(pr), s.cfg.MaxStrategiesPerTenant)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrStrategyNameTaken):
		writeError(w, http.StatusConflict, codeStrategyNameTaken,
			"a strategy with this name already exists in the tenant")
		return
	case errors.Is(err, store.ErrStrategyLimitReached):
		writeError(w, http.StatusConflict, codeStrategyLimitReached,
			"tenant strategy limit reached")
		return
	default:
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
