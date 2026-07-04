package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

var plaintextPattern = regexp.MustCompile(`^amx_[0-9a-f]{64}$`)

func tokenEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-1")
	createStrategy(t, e.store, strat1, "paper")
	return e
}

func mintVia(t *testing.T, e *testEnv, token string, body mintTokenRequest) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, "POST", "/api/v1/tokens", token, body)
}

func decodeMinted(t *testing.T, rec *httptest.ResponseRecorder) mintedTokenResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("mint status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var m mintedTokenResponse
	decodeJSON(t, rec, &m)
	if !plaintextPattern.MatchString(m.Token) {
		t.Fatalf("plaintext %q does not match ^amx_[0-9a-f]{64}$", m.Token)
	}
	return m
}

// TestTokenMintRevokeLifecycle: mint -> use -> revoke -> 401 on the very
// next request, with the created/revoked audit trail appended.
func TestTokenMintRevokeLifecycle(t *testing.T) {
	e := tokenEnv(t)
	admin := seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin-token")

	minted := decodeMinted(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "user", Role: RoleViewer, Label: "ops-viewer"}))
	if minted.TenantID != "tenant-1" || minted.Role == nil || *minted.Role != RoleViewer {
		t.Fatalf("minted metadata = %+v", minted.APIToken)
	}

	if rec := e.do(t, "GET", "/api/v1/strategies", minted.Token, nil); rec.Code != http.StatusOK {
		t.Fatalf("minted viewer GET: status = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec := e.do(t, "POST", "/api/v1/tokens/"+minted.TokenID+"/revoke", admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var revoked store.APIToken
	decodeJSON(t, rec, &revoked)
	if revoked.RevokedAt == nil {
		t.Fatal("revoked_at not set after revoke")
	}
	// Revoked token is 401 on the NEXT request (lookup observes revoked_at).
	wantError(t, e.do(t, "GET", "/api/v1/strategies", minted.Token, nil), 401, codeUnauthorized)
	// Second revoke: idempotent, the first revocation stands.
	rec = e.do(t, "POST", "/api/v1/tokens/"+minted.TokenID+"/revoke", admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second revoke status = %d", rec.Code)
	}
	var again store.APIToken
	decodeJSON(t, rec, &again)
	if again.RevokedAt == nil || *again.RevokedAt != *revoked.RevokedAt {
		t.Errorf("second revoke moved revoked_at: %v -> %v", *revoked.RevokedAt, again.RevokedAt)
	}
	// Absent and (later, in the isolation tests) foreign token_id are 404.
	wantError(t, e.do(t, "POST", "/api/v1/tokens/"+uid(77)+"/revoke", admin, nil), 404, codeUnknownToken)
}

// TestMintCeiling: a creator mints user roles only at or below its own
// role — admin cannot mint owner — and agent tokens only for own-tenant
// strategies (foreign is 404: no existence oracle).
func TestMintCeiling(t *testing.T) {
	e := tokenEnv(t)
	admin := seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin-token")

	wantError(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "user", Role: RoleOwner, Label: "escalation"}), 403, codeForbidden)
	decodeMinted(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "user", Role: RoleAdmin, Label: "peer-admin"}))

	createTenant(t, e.store, "tenant-b")
	createTenantStrategy(t, e.store, strat2, "tenant-b", "paper")
	wantError(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "agent", StrategyID: strat2, Label: "foreign-agent"}), 404, codeUnknownStrategy)
	decodeMinted(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "agent", StrategyID: strat1, Label: "own-agent"}))

	wantError(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "user", Label: "role-missing"}), 400, codeInvalidRole)
	wantError(t, mintVia(t, e, admin, mintTokenRequest{
		Principal: "robot", Role: RoleViewer, Label: "bad-principal"}), 400, codeInvalidRole)
	// A DB principal cannot mint into another tenant.
	wantError(t, mintVia(t, e, admin, mintTokenRequest{
		TenantID: "tenant-b", Principal: "user", Role: RoleViewer, Label: "cross"}), 403, codeForbidden)
}

// TestEnvAdminCannotMintOwnerWhenOwnerExists: the env-admin owner mint is
// RECOVERY only — allowed at exactly zero unrevoked owner tokens.
func TestEnvAdminCannotMintOwnerWhenOwnerExists(t *testing.T) {
	e := tokenEnv(t)
	owner := decodeMinted(t, mintVia(t, e, adminTok, mintTokenRequest{
		TenantID: "tenant-1", Principal: "user", Role: RoleOwner, Label: "first-owner"}))

	wantError(t, mintVia(t, e, adminTok, mintTokenRequest{
		TenantID: "tenant-1", Principal: "user", Role: RoleOwner, Label: "second-owner"}), 403, codeForbidden)

	if rec := e.do(t, "POST", "/api/v1/tokens/"+owner.TokenID+"/revoke", adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("env-admin revoke status = %d", rec.Code)
	}
	decodeMinted(t, mintVia(t, e, adminTok, mintTokenRequest{
		TenantID: "tenant-1", Principal: "user", Role: RoleOwner, Label: "recovered-owner"}))
}

// TestAdminCannotRevokeOwner: the revoke ceiling — owner tokens are
// revocable by owner (and env-admin) only.
func TestAdminCannotRevokeOwner(t *testing.T) {
	e := tokenEnv(t)
	admin := seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin-token")
	owner := seedUserToken(t, e.store, "tenant-1", RoleOwner, "db-owner-token")
	minted := decodeMinted(t, mintVia(t, e, owner, mintTokenRequest{
		Principal: "user", Role: RoleOwner, Label: "co-owner"}))

	wantError(t, e.do(t, "POST", "/api/v1/tokens/"+minted.TokenID+"/revoke", admin, nil), 403, codeForbidden)
	if rec := e.do(t, "POST", "/api/v1/tokens/"+minted.TokenID+"/revoke", owner, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner revoke status = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestTokenNeverReadBack is the PLAN.md "no role reads back API keys"
// criterion: after creation, no list response, revoke response, or error
// body carries a credential's plaintext or hash — for every role.
func TestTokenNeverReadBack(t *testing.T) {
	e := tokenEnv(t)
	roleToks := map[string]string{
		RoleViewer: seedUserToken(t, e.store, "tenant-1", RoleViewer, "db-viewer-token"),
		RoleTrader: seedUserToken(t, e.store, "tenant-1", RoleTrader, "db-trader-token"),
		RoleAdmin:  seedUserToken(t, e.store, "tenant-1", RoleAdmin, "db-admin-token"),
		RoleOwner:  seedUserToken(t, e.store, "tenant-1", RoleOwner, "db-owner-token"),
	}
	// Secrets that must never appear again: every seeded/minted plaintext
	// and every hash, agent token included.
	secrets := []string{}
	for _, plain := range roleToks {
		secrets = append(secrets, plain, hashToken(plain))
	}
	minted := decodeMinted(t, mintVia(t, e, roleToks[RoleAdmin], mintTokenRequest{
		Principal: "agent", StrategyID: strat1, Label: "agent"}))
	secrets = append(secrets, minted.Token, hashToken(minted.Token))

	assertNoSecret := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		body := rec.Body.String()
		if strings.Contains(body, "token_hash") {
			t.Errorf("%s: response carries a token_hash key: %q", name, body)
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s: response echoes a credential", name)
			}
		}
	}

	for role, tok := range roleToks {
		assertNoSecret("list as "+role, e.do(t, "GET", "/api/v1/tokens", tok, nil))
		assertNoSecret("mint as "+role, mintVia(t, e, tok, mintTokenRequest{
			Principal: "user", Role: RoleOwner, Label: "escalation-attempt"}))
	}
	assertNoSecret("list as env-admin", e.do(t, "GET", "/api/v1/tokens?tenant_id=tenant-1", adminTok, nil))
	assertNoSecret("revoke response",
		e.do(t, "POST", "/api/v1/tokens/"+minted.TokenID+"/revoke", roleToks[RoleAdmin], nil))
	assertNoSecret("revoked-token 401", e.do(t, "GET", "/api/v1/strategies", minted.Token, nil))

	// The admin list is non-empty and metadata-only (sanity that the
	// assertions above exercised real rows).
	rec := e.do(t, "GET", "/api/v1/tokens", roleToks[RoleAdmin], nil)
	var pg struct {
		Items []store.APIToken `json:"items"`
		Total int              `json:"total"`
	}
	decodeJSON(t, rec, &pg)
	if pg.Total < 5 || len(pg.Items) < 5 {
		t.Fatalf("token list = %+v, want the seeded tokens", pg)
	}
}
