package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Platform secrets (docs/specs/platform-secrets.md): the three admin routes
// are env-admin ONLY (env admin token + platform_admin sessions — NOT
// tenant owners in v1) and return non-secret metadata views only; the ONE
// endpoint that returns a secret value is GET /api/v1/agent/llm-config
// (agent tokens), and it returns ONLY the llm payload — the binance
// payload never leaves the process.

// SecretVault is the AES-256-GCM seal/open seam (*vault.Vault satisfies
// it): Seal returns base64(nonce||ciphertext); Open reverses it. nil
// answers all four secret routes 503 VAULT_UNAVAILABLE.
type SecretVault interface {
	Seal(plaintext []byte) (string, error)
	Open(sealed string) ([]byte, error)
}

// Secret kinds and validation bounds (platform-secrets.md).
const (
	secretKindBinance = "binance"
	secretKindLLM     = "llm"
	maxSecretChars    = 256
	llmTimeoutDefault = 30
	llmTimeoutMin     = 1
	llmTimeoutMax     = 600
	llmModelMaxChars  = 128
	// Model defaults mirror agent-plane DEFAULT_ROLE_MODELS (llm/mintrouter.py):
	// the expensive model for the trader role, the cheap one for analyst roles.
	llmTraderModelDefault  = "gpt-4o"
	llmDefaultModelDefault = "gpt-4o-mini"
)

// secretMetaView is the API view of one stored secret: kind, the decoded
// non-secret metadata, and audit fields — NEVER the payload.
type secretMetaView struct {
	Kind      string          `json:"kind"`
	Meta      json.RawMessage `json:"meta"`
	UpdatedAt string          `json:"updated_at"`
	UpdatedBy string          `json:"updated_by"`
}

// setSecretResponse acknowledges a set/rotate with the metadata view only.
type setSecretResponse struct {
	Secret secretMetaView `json:"secret"`
}

// vaultReady answers 503 VAULT_UNAVAILABLE when no vault is wired.
func (s *Server) vaultReady(w http.ResponseWriter) bool {
	if s.cfg.Vault == nil {
		writeError(w, http.StatusServiceUnavailable, codeVaultUnavailable, "secrets vault is not configured")
		return false
	}
	return true
}

// last4 is the non-secret display suffix recorded in meta_json: the
// trailing 4 characters (the whole value only when it is already <= 4
// characters — a degenerate key).
func last4(v string) string {
	if len(v) <= 4 {
		return v
	}
	return v[len(v)-4:]
}

// handleListPlatformSecrets is GET /api/v1/platform/secrets (env-admin
// ONLY): metadata views sorted by kind; an empty vault is {"items":[]}.
func (s *Server) handleListPlatformSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	metas, err := s.cfg.Store.ListPlatformSecretMeta()
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	items := make([]secretMetaView, 0, len(metas))
	for _, m := range metas {
		items = append(items, secretMetaView{
			Kind: m.Kind, Meta: json.RawMessage(m.MetaJSON),
			UpdatedAt: m.UpdatedAt, UpdatedBy: m.UpdatedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string][]secretMetaView{"items": items})
}

// storeSealedSecret marshals + seals the payload, upserts snapshot + audit
// in one store transaction, and answers with the metadata view.
func (s *Server) storeSealedSecret(w http.ResponseWriter, r *http.Request, kind string, payload, meta any) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	sealed, err := s.cfg.Vault.Seal(plaintext)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	now := formatTime(s.cfg.Now())
	actor := s.actorID(principalFrom(r))
	if err := s.cfg.Store.UpsertPlatformSecret(kind, sealed, string(metaJSON), actor, uuid.NewString(), now); err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, setSecretResponse{Secret: secretMetaView{
		Kind: kind, Meta: metaJSON, UpdatedAt: now, UpdatedBy: actor,
	}})
}

// binanceSecretRequest is the POST /api/v1/platform/secrets/binance body:
// all three fields required, env pinned to testnet|prod.
type binanceSecretRequest struct {
	Env       string `json:"env"`
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

// binancePayload is the sealed plaintext: the credentials ONLY (env lives
// in the non-secret metadata).
type binancePayload struct {
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

// binanceMeta is the non-secret display metadata for kind binance.
type binanceMeta struct {
	Env         string `json:"env"`
	APIKeyLast4 string `json:"api_key_last4"`
}

// handleSetBinanceSecret is POST /api/v1/platform/secrets/binance
// (env-admin ONLY): seals {"api_key","api_secret"} and answers the
// metadata view — the plaintext is NEVER echoed.
func (s *Server) handleSetBinanceSecret(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	var req binanceSecretRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Env != "testnet" && req.Env != "prod" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "env must be 'testnet' or 'prod'")
		return
	}
	if req.APIKey == "" || len(req.APIKey) > maxSecretChars {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "api_key must be 1..256 characters")
		return
	}
	if req.APISecret == "" || len(req.APISecret) > maxSecretChars {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "api_secret must be 1..256 characters")
		return
	}
	s.storeSealedSecret(w, r, secretKindBinance,
		binancePayload{APIKey: req.APIKey, APISecret: req.APISecret},
		binanceMeta{Env: req.Env, APIKeyLast4: last4(req.APIKey)})
}

// llmSecretRequest is the POST /api/v1/platform/secrets/llm body:
// timeout_seconds is optional (default 30, bounds 1..600); trader_model and
// default_model are optional (defaults gpt-4o / gpt-4o-mini, 1..128 chars).
type llmSecretRequest struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	TimeoutSeconds *int   `json:"timeout_seconds"`
	TraderModel    string `json:"trader_model"`
	DefaultModel   string `json:"default_model"`
}

// llmPayload is BOTH the sealed plaintext and the GET
// /api/v1/agent/llm-config response shape — the one secret that crosses an
// API boundary, to agent tokens only.
type llmPayload struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	TraderModel    string `json:"trader_model"`
	DefaultModel   string `json:"default_model"`
}

// llmMeta is the non-secret display metadata for kind llm.
type llmMeta struct {
	BaseURL        string `json:"base_url"`
	APIKeyLast4    string `json:"api_key_last4"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	TraderModel    string `json:"trader_model"`
	DefaultModel   string `json:"default_model"`
}

// handleSetLLMSecret is POST /api/v1/platform/secrets/llm (env-admin
// ONLY): seals {"base_url","api_key","timeout_seconds"} and answers the
// metadata view — the api_key is NEVER echoed.
func (s *Server) handleSetLLMSecret(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	var req llmSecretRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	u, err := url.Parse(req.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "base_url must be an http(s) URL")
		return
	}
	if req.APIKey == "" || len(req.APIKey) > maxSecretChars {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "api_key must be 1..256 characters")
		return
	}
	timeout := llmTimeoutDefault
	if req.TimeoutSeconds != nil {
		timeout = *req.TimeoutSeconds
		if timeout < llmTimeoutMin || timeout > llmTimeoutMax {
			writeError(w, http.StatusBadRequest, codeSchemaInvalid, "timeout_seconds must be 1..600")
			return
		}
	}
	traderModel := req.TraderModel
	if traderModel == "" {
		traderModel = llmTraderModelDefault
	}
	defaultModel := req.DefaultModel
	if defaultModel == "" {
		defaultModel = llmDefaultModelDefault
	}
	if len(traderModel) > llmModelMaxChars || len(defaultModel) > llmModelMaxChars {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "trader_model and default_model must be 1..128 characters")
		return
	}
	s.storeSealedSecret(w, r, secretKindLLM,
		llmPayload{BaseURL: req.BaseURL, APIKey: req.APIKey, TimeoutSeconds: timeout,
			TraderModel: traderModel, DefaultModel: defaultModel},
		llmMeta{BaseURL: req.BaseURL, APIKeyLast4: last4(req.APIKey), TimeoutSeconds: timeout,
			TraderModel: traderModel, DefaultModel: defaultModel})
}

// handleAgentLLMConfig is GET /api/v1/agent/llm-config (agent tokens ONLY,
// any agent): decrypts and returns the llm payload — the SINGLE endpoint
// that returns a secret value. 404 NOT_CONFIGURED before the first set.
func (s *Server) handleAgentLLMConfig(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	ciphertext, _, _, _, err := s.cfg.Store.GetPlatformSecret(secretKindLLM)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeNotConfigured, "llm secret is not configured")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	plaintext, err := s.cfg.Vault.Open(ciphertext)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	var payload llmPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		s.writeInternal(w, r, err)
		return
	}
	// Payloads sealed before the model fields existed lack them: agents
	// always see concrete models.
	if payload.TraderModel == "" {
		payload.TraderModel = llmTraderModelDefault
	}
	if payload.DefaultModel == "" {
		payload.DefaultModel = llmDefaultModelDefault
	}
	writeJSON(w, http.StatusOK, payload)
}

// LoadBinanceSecret decrypts the stored binance credentials for startup
// wiring (cmd/controlplane): ok=false when no vault is configured or no
// binance secret is stored. The plaintext stays in-process — no API route
// ever returns it, and callers MUST NOT log it.
func LoadBinanceSecret(st *store.Store, v SecretVault) (env, apiKey, apiSecret string, ok bool, err error) {
	if v == nil {
		return "", "", "", false, nil
	}
	ciphertext, metaJSON, _, _, err := st.GetPlatformSecret(secretKindBinance)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	plaintext, err := v.Open(ciphertext)
	if err != nil {
		return "", "", "", false, err
	}
	var payload binancePayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return "", "", "", false, err
	}
	var meta binanceMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return "", "", "", false, err
	}
	return meta.Env, payload.APIKey, payload.APISecret, true, nil
}
