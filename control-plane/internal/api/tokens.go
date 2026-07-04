package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// mintPlaintext generates the amx_ + 64-lowercase-hex credential (32 CSPRNG
// bytes) and its storage hash. The plaintext leaves this package exactly
// once, in the mint response; only the hash is persisted.
func mintPlaintext() (plaintext, hash string, err error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	plaintext = "amx_" + hex.EncodeToString(buf[:])
	return plaintext, hashToken(plaintext), nil
}

// mintTokenRequest is the POST /api/v1/tokens body. tenant_id is carried
// by env-admin requests only (DB principals mint in their own tenant).
type mintTokenRequest struct {
	TenantID   string `json:"tenant_id,omitempty"`
	Principal  string `json:"principal"`
	Role       string `json:"role,omitempty"`
	StrategyID string `json:"strategy_id,omitempty"`
	Label      string `json:"label"`
}

// mintedTokenResponse is the create response: metadata plus the plaintext,
// returned exactly once — no endpoint returns it (or the hash) ever again.
type mintedTokenResponse struct {
	store.APIToken
	Token string `json:"token"`
}

// handleMintToken mints a DB token (multi-tenant-rbac.md §Token lifecycle):
// admin/owner for their own tenant, env-admin for any tenant. The mint
// ceiling binds user roles (at or below the creator's own; env-admin mints
// owner ONLY as recovery at zero unrevoked owners) and agent tokens bind to
// an own-tenant strategy (foreign is 404).
func (s *Server) handleMintToken(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	var req mintTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	tenantID, ok := s.resolveMintTenant(w, r, pr, req.TenantID)
	if !ok {
		return
	}
	if req.Label == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "label is required")
		return
	}
	tok := store.APIToken{
		TenantID:  tenantID,
		Principal: req.Principal,
		Label:     req.Label,
		CreatedBy: s.actorID(pr),
		CreatedAt: formatTime(s.cfg.Now()),
	}
	switch req.Principal {
	case "user":
		if _, valid := roleRank[req.Role]; !valid || req.StrategyID != "" {
			writeError(w, http.StatusBadRequest, codeInvalidRole, "user tokens carry a valid role and no strategy_id")
			return
		}
		// Mint ceiling: at or below the creator's own role; the env-admin
		// owner path is recovery-only (zero unrevoked owner tokens, checked
		// transactionally at insert time so concurrent mints cannot race).
		if pr.class == classUser && roleRank[req.Role] > roleRank[pr.role] {
			writeError(w, http.StatusForbidden, codeForbidden, "mint ceiling: role above the creator's own")
			return
		}
		tok.Role = &req.Role
	case "agent":
		if req.Role != "" || req.StrategyID == "" {
			writeError(w, http.StatusBadRequest, codeInvalidRole, "agent tokens carry a strategy_id and no role")
			return
		}
		// The agent token's tenant MUST equal its strategy's tenant at
		// mint time; a foreign-tenant strategy is indistinguishable from
		// absence.
		if _, err := s.cfg.Store.GetStrategyInTenant(req.StrategyID, tenantID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
				return
			}
			s.writeInternal(w, r, err)
			return
		}
		tok.StrategyID = &req.StrategyID
	default:
		writeError(w, http.StatusBadRequest, codeInvalidRole, "principal must be user or agent")
		return
	}

	recoveryOnly := pr.class == classEnvAdmin && req.Principal == "user" && req.Role == RoleOwner
	minted, plaintext, err := s.insertMintedToken(tok, recoveryOnly)
	if errors.Is(err, store.ErrOwnerExists) {
		writeError(w, http.StatusForbidden, codeForbidden, "owner recovery mint requires zero unrevoked owner tokens")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mintedTokenResponse{APIToken: minted, Token: plaintext})
}

// resolveMintTenant resolves the target tenant: DB principals mint in
// their own tenant only (a body tenant_id naming another is 403);
// env-admin names any EXISTING tenant in the body.
func (s *Server) resolveMintTenant(w http.ResponseWriter, r *http.Request, pr principal, bodyTenant string) (string, bool) {
	if pr.tenantBound() {
		if bodyTenant != "" && bodyTenant != pr.tenantID {
			writeError(w, http.StatusForbidden, codeForbidden, "tenant_id outside the token's tenant")
			return "", false
		}
		return pr.tenantID, true
	}
	if bodyTenant == "" {
		writeError(w, http.StatusBadRequest, codeInvalidTenantID, "tenant_id is required")
		return "", false
	}
	if _, err := s.cfg.Store.GetTenant(bodyTenant); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownTenant, "unknown tenant")
			return "", false
		}
		s.writeInternal(w, r, err)
		return "", false
	}
	return bodyTenant, true
}

// insertMintedToken persists a fresh CSPRNG credential; a token_hash
// UNIQUE collision is retried internally with a fresh value, never
// surfaced (minting provides no existence oracle). recoveryOnly routes
// through the transactional zero-owner gate (env-admin owner recovery).
func (s *Server) insertMintedToken(tok store.APIToken, recoveryOnly bool) (store.APIToken, string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		plaintext, hash, err := mintPlaintext()
		if err != nil {
			return store.APIToken{}, "", err
		}
		tok.TokenID = uuid.NewString()
		if recoveryOnly {
			err = s.cfg.Store.InsertOwnerRecoveryToken(tok, hash, uuid.NewString())
		} else {
			err = s.cfg.Store.InsertAPIToken(tok, hash, uuid.NewString())
		}
		if errors.Is(err, store.ErrDuplicateTokenHash) {
			continue
		}
		if err != nil {
			return store.APIToken{}, "", err
		}
		return tok, plaintext, nil
	}
	return store.APIToken{}, "", errors.New("token mint: exhausted hash-collision retries")
}

// handleListTokens lists token METADATA only — never token_hash nor
// plaintext (no-read-back invariant). DB principals list their own tenant;
// env-admin lists any tenant (?tenant_id filter, "" = all).
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	tenantID := pr.tenantID
	if !pr.tenantBound() {
		tenantID = r.URL.Query().Get("tenant_id")
	}
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListAPITokens(tenantID, pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

// handleRevokeToken revokes a token (multi-tenant-rbac.md §Token
// lifecycle): a foreign or absent token_id is 404 UNKNOWN_TOKEN (no
// cross-tenant existence oracle); the revoke ceiling allows only tokens at
// or below the caller's role (owner tokens by owner only, agent tokens by
// any admin+; env-admin revokes any). Idempotent: the first revocation
// stands.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	tokenID := r.PathValue("token_id")
	tok, err := s.cfg.Store.GetAPIToken(tokenID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && pr.tenantBound() && tok.TenantID != pr.tenantID) {
		writeError(w, http.StatusNotFound, codeUnknownToken, "unknown token")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if pr.class == classUser && tok.Role != nil && roleRank[*tok.Role] > roleRank[pr.role] {
		writeError(w, http.StatusForbidden, codeForbidden, "revoke ceiling: role above the caller's own")
		return
	}
	now := formatTime(s.cfg.Now())
	revoked, err := s.cfg.Store.RevokeAPIToken(tokenID, now, uuid.NewString(), s.actorID(pr))
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if revoked {
		tok.RevokedAt = &now
	}
	writeJSON(w, http.StatusOK, tok)
}
