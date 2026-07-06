package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// authBody is the bootstrap/login request shape.
func authBody(email, password string) map[string]string {
	return map[string]string{"email": email, "password": password}
}

// authUserView mirrors the sessionUser wire shape.
type authUserView struct {
	UserID   string  `json:"user_id"`
	Email    string  `json:"email"`
	TenantID *string `json:"tenant_id"`
	Role     string  `json:"role"`
}

// login performs a login expected to succeed and returns the minted
// session bearer, its expiry, and the user view.
func login(t *testing.T, e *testEnv, email, password string) (token, expiresAt string, user authUserView) {
	t.Helper()
	rec := e.do(t, "POST", "/api/v1/auth/login", "", authBody(email, password))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token     string       `json:"token"`
		ExpiresAt string       `json:"expires_at"`
		User      authUserView `json:"user"`
	}
	decodeJSON(t, rec, &resp)
	return resp.Token, resp.ExpiresAt, resp.User
}

// TestAuthBootstrapLoginFlow: bootstrap creates the FIRST platform_admin
// exactly once (409 BOOTSTRAP_COMPLETE thereafter); its session bearer is
// amxs_-prefixed, resolves on /me, and carries the env-admin surface
// (POST /api/v1/tenants) including the strategy-data reads
// (multi-tenant-rbac.md §Principal mapping: the session is the platform
// operator's only credential). No response ever contains password or
// hash material.
func TestAuthBootstrapLoginFlow(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "POST", "/api/v1/auth/bootstrap", "", authBody(" Root@Example.com ", "super-secret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d (body %q)", rec.Code, rec.Body.String())
	}
	var boot struct {
		User authUserView `json:"user"`
	}
	decodeJSON(t, rec, &boot)
	if boot.User.Role != RolePlatformAdmin || boot.User.TenantID != nil || boot.User.Email != "root@example.com" {
		t.Fatalf("bootstrap user = %+v, want normalized platform_admin with nil tenant", boot.User)
	}
	for _, leak := range []string{"password", "hash"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("bootstrap body contains %q: %q", leak, rec.Body.String())
		}
	}
	wantError(t, e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("other@example.com", "super-secret")), 409, codeBootstrapComplete)
	wantError(t, e.do(t, "POST", "/api/v1/auth/login", "",
		authBody("root@example.com", "wrong-password")), 401, codeInvalidCredentials)

	tok, _, _ := login(t, e, "root@example.com", "super-secret")
	if !strings.HasPrefix(tok, "amxs_") || len(tok) != len("amxs_")+64 {
		t.Fatalf("session token shape = %q, want amxs_ + 64 hex", tok)
	}
	rec = e.do(t, "GET", "/api/v1/auth/me", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d (body %q)", rec.Code, rec.Body.String())
	}
	var me struct {
		User      authUserView `json:"user"`
		SessionID string       `json:"session_id"`
	}
	decodeJSON(t, rec, &me)
	if me.User.Email != "root@example.com" || me.User.Role != RolePlatformAdmin || me.SessionID == "" {
		t.Fatalf("me = %+v, want the platform_admin user with a session id", me)
	}
	rec = e.do(t, "POST", "/api/v1/tenants", tok, map[string]string{"tenant_id": "acme", "name": "Acme"})
	if rec.Code != http.StatusOK {
		t.Fatalf("platform_admin session POST /api/v1/tenants = %d (body %q)", rec.Code, rec.Body.String())
	}
	rec = e.do(t, "GET", "/api/v1/strategies", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform_admin session GET /api/v1/strategies = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestAuthSignupLoginFlow: signup atomically creates tenant + owner user;
// the owner session gets the tenant user surface (own-tenant reads, no
// env-admin routes); API and env tokens never pass the session-only rows;
// logout revokes once and the bearer stops resolving.
func TestAuthSignupLoginFlow(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "POST", "/api/v1/auth/signup", "", map[string]string{
		"tenant_name": "Acme", "email": "Owner@Acme.com", "password": "hunter2-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("signup = %d (body %q)", rec.Code, rec.Body.String())
	}
	var su struct {
		Tenant struct {
			TenantID string `json:"tenant_id"`
			Name     string `json:"name"`
		} `json:"tenant"`
		User authUserView `json:"user"`
	}
	decodeJSON(t, rec, &su)
	if len(su.Tenant.TenantID) != 32 || su.Tenant.Name != "Acme" {
		t.Fatalf("signup tenant = %+v, want a 32-hex generated id", su.Tenant)
	}
	if su.User.Role != RoleOwner || su.User.TenantID == nil || *su.User.TenantID != su.Tenant.TenantID ||
		su.User.Email != "owner@acme.com" {
		t.Fatalf("signup user = %+v, want the tenant's owner", su.User)
	}
	wantError(t, e.do(t, "POST", "/api/v1/auth/signup", "", map[string]string{
		"tenant_name": "Other", "email": "owner@acme.com", "password": "hunter2-secret"}),
		409, codeEmailExists)

	tok, _, _ := login(t, e, "owner@acme.com", "hunter2-secret")
	wantError(t, e.do(t, "GET", "/api/v1/auth/me", readTok, nil), 403, codeForbidden)
	wantError(t, e.do(t, "GET", "/api/v1/auth/me", adminTok, nil), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/auth/logout", adminTok, nil), 403, codeForbidden)
	if rec := e.do(t, "GET", "/api/v1/strategies", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner session GET /api/v1/strategies = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "POST", "/api/v1/tenants", tok,
		map[string]string{"tenant_id": "acme2", "name": "Acme2"}), 403, codeForbidden)

	rec = e.do(t, "POST", "/api/v1/auth/logout", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout = %d (body %q)", rec.Code, rec.Body.String())
	}
	var lo struct {
		Revoked bool `json:"revoked"`
	}
	decodeJSON(t, rec, &lo)
	if !lo.Revoked {
		t.Fatal("logout revoked = false, want true")
	}
	wantError(t, e.do(t, "GET", "/api/v1/auth/me", tok, nil), 401, codeUnauthorized)
	wantError(t, e.do(t, "POST", "/api/v1/auth/logout", tok, nil), 401, codeUnauthorized)
}

// TestAuthLoginFailuresUniform: unknown email, wrong password, and a
// disabled user answer the IDENTICAL 401 INVALID_CREDENTIALS body (no
// account enumeration), and disabling also kills existing sessions on
// their next request.
func TestAuthLoginFailuresUniform(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "POST", "/api/v1/auth/signup", "", map[string]string{
		"tenant_name": "Acme", "email": "owner@acme.com", "password": "hunter2-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("signup = %d (body %q)", rec.Code, rec.Body.String())
	}
	var su struct {
		User authUserView `json:"user"`
	}
	decodeJSON(t, rec, &su)
	tok, _, _ := login(t, e, "owner@acme.com", "hunter2-secret")

	unknown := e.do(t, "POST", "/api/v1/auth/login", "", authBody("nobody@acme.com", "hunter2-secret"))
	wrong := e.do(t, "POST", "/api/v1/auth/login", "", authBody("owner@acme.com", "wrong-password"))
	if err := e.store.DisableUser(su.User.UserID, formatTime(testNow)); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	disabled := e.do(t, "POST", "/api/v1/auth/login", "", authBody("owner@acme.com", "hunter2-secret"))
	wantError(t, unknown, 401, codeInvalidCredentials)
	wantError(t, wrong, 401, codeInvalidCredentials)
	wantError(t, disabled, 401, codeInvalidCredentials)
	if unknown.Body.String() != wrong.Body.String() || wrong.Body.String() != disabled.Body.String() {
		t.Errorf("login failure bodies differ: %q / %q / %q",
			unknown.Body.String(), wrong.Body.String(), disabled.Body.String())
	}
	wantError(t, e.do(t, "GET", "/api/v1/auth/me", tok, nil), 401, codeUnauthorized)
}

// TestAuthSessionExpiry: a session is valid strictly before its 7-day
// expiry and unknown (401) at and after it — observed on every request
// against the server clock.
func TestAuthSessionExpiry(t *testing.T) {
	now := testNow
	e := newEnv(t, func(cfg *Config) { cfg.Now = func() time.Time { return now } })
	if rec := e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("root@example.com", "super-secret")); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap = %d (body %q)", rec.Code, rec.Body.String())
	}
	tok, expiresAt, _ := login(t, e, "root@example.com", "super-secret")
	if want := formatTime(testNow.Add(sessionTTL)); expiresAt != want {
		t.Errorf("expires_at = %q, want %q", expiresAt, want)
	}
	now = testNow.Add(sessionTTL - time.Second)
	if rec := e.do(t, "GET", "/api/v1/auth/me", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("me just before expiry = %d (body %q)", rec.Code, rec.Body.String())
	}
	now = testNow.Add(sessionTTL)
	wantError(t, e.do(t, "GET", "/api/v1/auth/me", tok, nil), 401, codeUnauthorized)
}

// TestAuthPasswordPolicy: the 8..72-byte password policy and email shape
// bind bootstrap and signup with 400 SCHEMA_INVALID — and a policy 400
// persists nothing (bootstrap stays available).
func TestAuthPasswordPolicy(t *testing.T) {
	e := newEnv(t, nil)
	wantError(t, e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("root@example.com", "short7c")), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("root@example.com", strings.Repeat("x", 73))), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("not-an-email", "super-secret")), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/auth/signup", "", map[string]string{
		"tenant_name": "", "email": "o@a.com", "password": "super-secret"}), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/auth/signup", "", map[string]string{
		"tenant_name": "Acme", "email": "o@a.com", "password": "short7c"}), 400, codeSchemaInvalid)
	if rec := e.do(t, "POST", "/api/v1/auth/bootstrap", "",
		authBody("root@example.com", "super-secret")); rec.Code != http.StatusOK {
		t.Fatalf("bootstrap after policy 400s = %d (body %q)", rec.Code, rec.Body.String())
	}
}
