package api

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// tenantIDPattern is the normative tenant_id shape ('default' reserved).
var tenantIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// createTenantRequest is the POST /api/v1/tenants body (env-admin ONLY).
type createTenantRequest struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
}

// createTenantResponse carries the tenant plus its FIRST owner token — the
// documented mint-ceiling exception, plaintext returned exactly once.
// Rotating this token SHOULD be the tenant's first act.
type createTenantResponse struct {
	store.Tenant
	OwnerToken mintedTokenResponse `json:"owner_token"`
}

// handleCreateTenant creates a tenant and atomically mints its first owner
// token (multi-tenant-rbac.md §Tenancy rules). Thereafter the tenant is
// self-service via that owner token.
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	var req createTenantRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.TenantID == "default" || !tenantIDPattern.MatchString(req.TenantID) {
		writeError(w, http.StatusBadRequest, codeInvalidTenantID,
			"tenant_id must match ^[a-z0-9][a-z0-9_-]{0,31}$ ('default' is reserved)")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "name is required")
		return
	}
	createdAt := formatTime(s.cfg.Now())
	tenant := store.Tenant{TenantID: req.TenantID, Name: req.Name, CreatedAt: createdAt}
	owner := RoleOwner
	tok := store.APIToken{
		TenantID:  req.TenantID,
		Principal: "user",
		Role:      &owner,
		Label:     "initial-owner",
		CreatedBy: s.actorID(pr),
		CreatedAt: createdAt,
	}
	for attempt := 0; attempt < 8; attempt++ {
		plaintext, hash, err := mintPlaintext()
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		tok.TokenID = uuid.NewString()
		err = s.cfg.Store.CreateTenantWithOwnerToken(tenant, tok, hash, uuid.NewString())
		switch {
		case errors.Is(err, store.ErrTenantExists):
			writeError(w, http.StatusConflict, codeTenantExists, "tenant_id already exists")
			return
		case errors.Is(err, store.ErrDuplicateTokenHash):
			continue
		case err != nil:
			s.writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, createTenantResponse{
			Tenant:     tenant,
			OwnerToken: mintedTokenResponse{APIToken: tok, Token: plaintext},
		})
		return
	}
	s.writeInternal(w, r, errors.New("token mint: exhausted hash-collision retries"))
}

// tenantKillResponse acknowledges the persisted tenant kill event.
type tenantKillResponse struct {
	EventID    string `json:"event_id"`
	TenantID   string `json:"tenant_id"`
	KillEpoch  int64  `json:"kill_epoch"`
	RecordedAt string `json:"recorded_at"`
	Flatten    bool   `json:"flatten"`
}

// handleTenantKill is the tenant-tier kill switch (multi-tenant-rbac.md
// §Tenant kill-switch, EXTENDED per safety-wiring.md §Kill endpoints):
// admin/owner OWN tenant only (a foreign tenant path is 404, no existence
// oracle); env-admin any existing tenant. The event is persisted and
// acknowledged BEFORE any side effect; the optional flatten field defaults
// to false (the v1 empty body stays valid — backward compatible) and
// effects run asynchronously through the SafetyDriver seam (the v1
// gate-block-only restriction is lifted).
func (s *Server) handleTenantKill(w http.ResponseWriter, r *http.Request) {
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
	var req killRequest
	if !decodeStrictOptional(w, r, &req) {
		return
	}
	now := s.cfg.Now()
	eventID := uuid.NewString()
	epoch, err := s.cfg.Store.AppendTenantKill(eventID, tenantID, s.actorID(pr), formatTime(now), req.Flatten)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tenantKillResponse{
		EventID:    eventID,
		TenantID:   tenantID,
		KillEpoch:  epoch,
		RecordedAt: formatTime(now),
		Flatten:    req.Flatten,
	})
	s.driveSafety()
}
