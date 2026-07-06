package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/vault"
)

// vaultEnv is newEnv plus a fixed-key vault, so the four secret routes are
// live (a nil Config.Vault answers 503).
func vaultEnv(t *testing.T) (*testEnv, *vault.Vault) {
	t.Helper()
	v, err := vault.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	e := newEnv(t, func(cfg *Config) { cfg.Vault = v })
	return e, v
}

// secretItems decodes a {"items":[secretMetaView...]} response.
func secretItems(t *testing.T, rec *httptest.ResponseRecorder) []secretMetaView {
	t.Helper()
	var body struct {
		Items []secretMetaView `json:"items"`
	}
	decodeJSON(t, rec, &body)
	return body.Items
}

const (
	testBinanceKey    = "binance-api-key-0001"
	testBinanceSecret = "binance-api-secret-XyZ9"
	testLLMKey        = "sk-test-llm-key-9876"
)

// TestPlatformSecretsSetAndList: the env-admin sets both kinds, the list
// returns metadata views sorted by kind with the correct last4, a rotation
// replaces the metadata in place, and the empty vault lists {"items":[]}.
func TestPlatformSecretsSetAndList(t *testing.T) {
	e, _ := vaultEnv(t)

	rec := e.do(t, "GET", "/api/v1/platform/secrets", adminTok, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("empty list = %d %q, want 200 {\"items\":[]}", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "POST", "/api/v1/platform/secrets/binance", adminTok, map[string]any{
		"env": "testnet", "api_key": testBinanceKey, "api_secret": testBinanceSecret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set binance = %d (body %q)", rec.Code, rec.Body.String())
	}
	var set setSecretResponse
	decodeJSON(t, rec, &set)
	var bMeta binanceMeta
	if err := json.Unmarshal(set.Secret.Meta, &bMeta); err != nil {
		t.Fatalf("decode binance meta: %v", err)
	}
	if set.Secret.Kind != "binance" || bMeta.Env != "testnet" || bMeta.APIKeyLast4 != "0001" ||
		set.Secret.UpdatedBy != "env-admin" {
		t.Fatalf("binance view = %+v meta %+v, want testnet last4 0001 by env-admin", set.Secret, bMeta)
	}

	rec = e.do(t, "POST", "/api/v1/platform/secrets/llm", adminTok, map[string]any{
		"base_url": "https://llm.example/v1", "api_key": testLLMKey,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set llm = %d (body %q)", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &set)
	var lMeta llmMeta
	if err := json.Unmarshal(set.Secret.Meta, &lMeta); err != nil {
		t.Fatalf("decode llm meta: %v", err)
	}
	if lMeta.BaseURL != "https://llm.example/v1" || lMeta.APIKeyLast4 != "9876" ||
		lMeta.TimeoutSeconds != 30 {
		t.Fatalf("llm meta = %+v, want default timeout 30 and last4 9876", lMeta)
	}

	items := secretItems(t, e.do(t, "GET", "/api/v1/platform/secrets", adminTok, nil))
	if len(items) != 2 || items[0].Kind != "binance" || items[1].Kind != "llm" {
		t.Fatalf("list = %+v, want [binance llm] sorted by kind", items)
	}

	// Rotation: same kind, new values; the listed metadata follows.
	rec = e.do(t, "POST", "/api/v1/platform/secrets/binance", adminTok, map[string]any{
		"env": "prod", "api_key": "rotated-key-7777", "api_secret": "rotated-secret",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate binance = %d (body %q)", rec.Code, rec.Body.String())
	}
	items = secretItems(t, e.do(t, "GET", "/api/v1/platform/secrets", adminTok, nil))
	if err := json.Unmarshal(items[0].Meta, &bMeta); err != nil {
		t.Fatalf("decode rotated meta: %v", err)
	}
	if bMeta.Env != "prod" || bMeta.APIKeyLast4 != "7777" {
		t.Fatalf("rotated meta = %+v, want prod last4 7777", bMeta)
	}
}

// TestPlatformSecretsNeverEchoPayload pins the plane boundary: NO response
// from the binance POST, the secrets list, or the agent llm-config route
// carries the binance api_secret or full api_key — the binance payload
// never leaves the process (platform-secrets.md §Threat model).
func TestPlatformSecretsNeverEchoPayload(t *testing.T) {
	e, _ := vaultEnv(t)
	set := e.do(t, "POST", "/api/v1/platform/secrets/binance", adminTok, map[string]any{
		"env": "testnet", "api_key": testBinanceKey, "api_secret": testBinanceSecret,
	})
	if set.Code != http.StatusOK {
		t.Fatalf("set binance = %d (body %q)", set.Code, set.Body.String())
	}
	llm := e.do(t, "POST", "/api/v1/platform/secrets/llm", adminTok, map[string]any{
		"base_url": "https://llm.example", "api_key": testLLMKey,
	})
	if llm.Code != http.StatusOK {
		t.Fatalf("set llm = %d (body %q)", llm.Code, llm.Body.String())
	}
	bodies := map[string]string{
		"POST binance":   set.Body.String(),
		"POST llm":       llm.Body.String(),
		"GET list":       e.do(t, "GET", "/api/v1/platform/secrets", adminTok, nil).Body.String(),
		"GET llm-config": e.do(t, "GET", "/api/v1/agent/llm-config", agent1Tok, nil).Body.String(),
	}
	for name, body := range bodies {
		if strings.Contains(body, testBinanceSecret) {
			t.Errorf("%s response leaks api_secret", name)
		}
		if strings.Contains(body, testBinanceKey) {
			t.Errorf("%s response leaks the full binance api_key", name)
		}
	}
}

// TestAgentLLMConfig: 404 NOT_CONFIGURED before the first set; after the
// admin sets the llm secret, ANY agent token (env or DB — the route has no
// {id}, so no strategy-scope check) reads the full payload; the env-admin
// itself is 403 — the ONE secret-returning route is agent-only.
func TestAgentLLMConfig(t *testing.T) {
	e, _ := vaultEnv(t)
	wantError(t, e.do(t, "GET", "/api/v1/agent/llm-config", agent1Tok, nil), 404, codeNotConfigured)

	rec := e.do(t, "POST", "/api/v1/platform/secrets/llm", adminTok, map[string]any{
		"base_url": "https://llm.example/v1", "api_key": testLLMKey, "timeout_seconds": 120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set llm = %d (body %q)", rec.Code, rec.Body.String())
	}

	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	dbAgent := seedAgentToken(t, e.store, "tenant-1", strat1, "db-agent-token")
	for _, tok := range []string{agent1Tok, dbAgent} {
		rec = e.do(t, "GET", "/api/v1/agent/llm-config", tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("llm-config = %d (body %q), want 200", rec.Code, rec.Body.String())
		}
		var got llmPayload
		decodeJSON(t, rec, &got)
		if got.BaseURL != "https://llm.example/v1" || got.APIKey != testLLMKey || got.TimeoutSeconds != 120 {
			t.Fatalf("llm-config payload = %+v, want the sealed payload verbatim", got)
		}
	}
	wantError(t, e.do(t, "GET", "/api/v1/agent/llm-config", adminTok, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/agent/llm-config", readTok, nil), 403, codeForbidden)
}

// TestSecretsVaultUnavailable: with Config.Vault nil every secret route
// answers 503 VAULT_UNAVAILABLE (the routes stay registered — the matrix
// is the total route set).
func TestSecretsVaultUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	routes := []struct{ method, path, token string }{
		{"GET", "/api/v1/platform/secrets", adminTok},
		{"POST", "/api/v1/platform/secrets/binance", adminTok},
		{"POST", "/api/v1/platform/secrets/llm", adminTok},
		{"GET", "/api/v1/agent/llm-config", agent1Tok},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			wantError(t, e.do(t, rt.method, rt.path, rt.token, nil), 503, codeVaultUnavailable)
		})
	}
}

// TestSecretsValidation: strict decode plus the pinned value rules — env
// literal, 1..256 credential lengths, http(s) base_url, 1..600 timeout.
func TestSecretsValidation(t *testing.T) {
	e, _ := vaultEnv(t)
	long := strings.Repeat("k", 257)
	tests := []struct {
		name, path string
		body       map[string]any
	}{
		{"binance bad env", "/api/v1/platform/secrets/binance",
			map[string]any{"env": "mainnet", "api_key": "k", "api_secret": "s"}},
		{"binance empty key", "/api/v1/platform/secrets/binance",
			map[string]any{"env": "testnet", "api_key": "", "api_secret": "s"}},
		{"binance long key", "/api/v1/platform/secrets/binance",
			map[string]any{"env": "testnet", "api_key": long, "api_secret": "s"}},
		{"binance empty secret", "/api/v1/platform/secrets/binance",
			map[string]any{"env": "testnet", "api_key": "k", "api_secret": ""}},
		{"binance unknown field", "/api/v1/platform/secrets/binance",
			map[string]any{"env": "testnet", "api_key": "k", "api_secret": "s", "bogus": 1}},
		{"llm bad scheme", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "ftp://llm.example", "api_key": "k"}},
		{"llm not a url", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "not-a-url", "api_key": "k"}},
		{"llm empty key", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "https://llm.example", "api_key": ""}},
		{"llm long key", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "https://llm.example", "api_key": long}},
		{"llm timeout low", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "https://llm.example", "api_key": "k", "timeout_seconds": 0}},
		{"llm timeout high", "/api/v1/platform/secrets/llm",
			map[string]any{"base_url": "https://llm.example", "api_key": "k", "timeout_seconds": 601}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, e.do(t, "POST", tc.path, adminTok, tc.body), 400, codeSchemaInvalid)
		})
	}
	if items := secretItems(t, e.do(t, "GET", "/api/v1/platform/secrets", adminTok, nil)); len(items) != 0 {
		t.Fatalf("rejected bodies persisted %d secrets", len(items))
	}
}

// TestLoadBinanceSecret: the startup helper decrypts the stored binance
// credentials in-process; ok=false with no vault or no stored secret.
func TestLoadBinanceSecret(t *testing.T) {
	e, v := vaultEnv(t)
	if _, _, _, ok, err := LoadBinanceSecret(e.store, v); err != nil || ok {
		t.Fatalf("empty store LoadBinanceSecret ok=%v err=%v, want false, nil", ok, err)
	}
	if _, _, _, ok, err := LoadBinanceSecret(e.store, nil); err != nil || ok {
		t.Fatalf("nil vault LoadBinanceSecret ok=%v err=%v, want false, nil", ok, err)
	}
	rec := e.do(t, "POST", "/api/v1/platform/secrets/binance", adminTok, map[string]any{
		"env": "prod", "api_key": testBinanceKey, "api_secret": testBinanceSecret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set binance = %d (body %q)", rec.Code, rec.Body.String())
	}
	env, key, secret, ok, err := LoadBinanceSecret(e.store, v)
	if err != nil || !ok || env != "prod" || key != testBinanceKey || secret != testBinanceSecret {
		t.Fatalf("LoadBinanceSecret = %q %q %q ok=%v err=%v, want the stored credentials", env, key, secret, ok, err)
	}
}
