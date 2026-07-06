package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Password auth and web sessions (multi-tenant-rbac.md §Password auth and
// web sessions): bcrypt-hashed passwords, amxs_-prefixed session bearers
// (hash stored, plaintext returned exactly once), 7-day expiry, uniform
// 401 INVALID_CREDENTIALS on every login failure.
const (
	sessionTTL       = 7 * 24 * time.Hour
	minPasswordBytes = 8
	maxPasswordBytes = 72 // bcrypt's input limit
)

// invalidCredentialsMsg is the SINGLE login-failure message: unknown email,
// wrong password, and disabled user are indistinguishable on the wire.
const invalidCredentialsMsg = "invalid email or password"

// dummyPasswordHash equalizes login timing when the email is unknown: the
// handler compares against it so absent and present accounts cost the same.
var dummyPasswordHash, _ = bcrypt.GenerateFromPassword([]byte("amx-timing-equalizer"), bcrypt.DefaultCost)

// Login brute-force throttle: POST /api/v1/auth/login is Public, so the
// guard's per-token limiter never covers it. Each normalized email gets a
// bucket of loginFailBurst failed attempts refilling one per 30 s; ONLY
// failures charge it, and a successful login deletes the bucket.
const (
	loginFailBurst  = 5
	loginFailPerSec = float64(1) / 30
	// loginThrottleMax bounds the bucket map — emails are attacker-chosen,
	// so it must not grow without limit: fully-refilled buckets are pruned
	// on every access (a full bucket is indistinguishable from an absent
	// one, so it carries no information), and at the cap the entry closest
	// to full refill is evicted for the newcomer. Memory therefore never
	// exceeds loginThrottleMax live buckets.
	loginThrottleMax = 4096
)

// loginThrottle is the per-email failed-login token bucket: the same
// refill math and mutex discipline as rateLimiter, plus the prune/evict
// memory bound above. Unlike rateLimiter it separates the empty check
// (exhausted, uncharged) from the charge (fail), because only FAILED
// logins spend attempts.
type loginThrottle struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

func newLoginThrottle(now func() time.Time) *loginThrottle {
	return &loginThrottle{now: now, buckets: make(map[string]*bucket)}
}

// exhausted reports whether email's bucket is empty WITHOUT charging it,
// plus the time until it refills to one attempt (the Retry-After hint).
// It keys on the ATTEMPTED email whether or not an account exists, so the
// caller can reject before any user lookup or bcrypt work with identical
// behavior for known and unknown emails.
func (lt *loginThrottle) exhausted(email string) (bool, time.Duration) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	now := lt.now()
	lt.prune(now)
	b, ok := lt.buckets[email]
	if !ok {
		return false, 0
	}
	b.tokens = min(loginFailBurst, b.tokens+now.Sub(b.last).Seconds()*loginFailPerSec)
	b.last = now
	if b.tokens < 1 {
		return true, time.Duration((1 - b.tokens) / loginFailPerSec * float64(time.Second))
	}
	return false, 0
}

// fail charges one failed attempt against email, creating its bucket if
// absent and evicting at the cap.
func (lt *loginThrottle) fail(email string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	now := lt.now()
	lt.prune(now)
	b, ok := lt.buckets[email]
	if !ok {
		if len(lt.buckets) >= loginThrottleMax {
			lt.evict(now)
		}
		b = &bucket{tokens: loginFailBurst, last: now}
		lt.buckets[email] = b
	}
	b.tokens = min(loginFailBurst, b.tokens+now.Sub(b.last).Seconds()*loginFailPerSec)
	b.last = now
	if b.tokens > 0 {
		b.tokens--
	}
}

// reset deletes email's bucket on a successful login.
func (lt *loginThrottle) reset(email string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	delete(lt.buckets, email)
}

// prune drops every fully-refilled bucket (caller holds mu).
func (lt *loginThrottle) prune(now time.Time) {
	for k, b := range lt.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*loginFailPerSec >= loginFailBurst {
			delete(lt.buckets, k)
		}
	}
}

// evict removes the bucket closest to full refill — the one carrying the
// least throttle information — to admit a new email at the cap (caller
// holds mu).
func (lt *loginThrottle) evict(now time.Time) {
	var victim string
	best := math.Inf(-1)
	for k, b := range lt.buckets {
		if t := b.tokens + now.Sub(b.last).Seconds()*loginFailPerSec; t > best {
			best, victim = t, k
		}
	}
	delete(lt.buckets, victim)
}

// mintSessionPlaintext generates the amxs_ + 64-lowercase-hex session
// credential (32 CSPRNG bytes) and its storage hash. The plaintext leaves
// this package exactly once, in the login response; only the hash is
// persisted.
func mintSessionPlaintext() (plaintext, hash string, err error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", "", err
	}
	plaintext = "amxs_" + hex.EncodeToString(buf[:])
	return plaintext, hashToken(plaintext), nil
}

// authCredentials is the bootstrap/login body; signup embeds it.
type authCredentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// validateCredentials normalizes the email (trimmed, lowercased) and
// enforces the password policy (8..72 bytes); false means the 400 is
// already written. Login does NOT run this — its failures are uniform 401s.
func validateCredentials(w http.ResponseWriter, creds authCredentials) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(creds.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "email must contain '@'")
		return "", false
	}
	if len(creds.Password) < minPasswordBytes {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "password must be at least 8 bytes")
		return "", false
	}
	if len(creds.Password) > maxPasswordBytes {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "password must be at most 72 bytes")
		return "", false
	}
	return email, true
}

// sessionUser is the user view returned by the auth endpoints: identity and
// role only — never password_hash, never any credential material.
type sessionUser struct {
	UserID   string  `json:"user_id"`
	Email    string  `json:"email"`
	TenantID *string `json:"tenant_id"`
	Role     string  `json:"role"`
}

func sessionUserView(u store.User) sessionUser {
	return sessionUser{UserID: u.UserID, Email: u.Email, TenantID: u.TenantID, Role: u.Role}
}

// bootstrapResponse is the POST /api/v1/auth/bootstrap response.
type bootstrapResponse struct {
	User sessionUser `json:"user"`
}

// handleAuthBootstrap creates the FIRST platform_admin user — exactly once:
// any existing platform_admin (disabled included) is 409 BOOTSTRAP_COMPLETE,
// gated transactionally so concurrent bootstraps cannot race.
func (s *Server) handleAuthBootstrap(w http.ResponseWriter, r *http.Request) {
	var creds authCredentials
	if !decodeStrict(w, r, &creds) {
		return
	}
	email, ok := validateCredentials(w, creds)
	if !ok {
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(creds.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	u := store.User{
		UserID:    uuid.NewString(),
		Email:     email,
		Role:      RolePlatformAdmin,
		CreatedAt: formatTime(s.cfg.Now()),
	}
	err = s.cfg.Store.CreatePlatformAdmin(u, string(hash), uuid.NewString())
	switch {
	case errors.Is(err, store.ErrPlatformAdminExists):
		writeError(w, http.StatusConflict, codeBootstrapComplete, "platform admin already exists")
		return
	case errors.Is(err, store.ErrEmailExists):
		writeError(w, http.StatusConflict, codeEmailExists, "email already registered")
		return
	case err != nil:
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, bootstrapResponse{User: sessionUserView(u)})
}

// signupRequest is the POST /api/v1/auth/signup body: self-service tenant
// creation with its first owner user. The tenant_id is server-generated.
type signupRequest struct {
	TenantName string `json:"tenant_name"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

// signupResponse carries the created tenant and its owner user.
type signupResponse struct {
	Tenant store.Tenant `json:"tenant"`
	User   sessionUser  `json:"user"`
}

// newTenantID generates a self-service tenant id: 32 lowercase hex chars
// (a dash-less UUID), inside the normative ^[a-z0-9][a-z0-9_-]{0,31}$
// shape and never the reserved 'default'.
func newTenantID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

// handleAuthSignup creates a tenant AND its first owner user atomically
// (multi-tenant-rbac.md §Password auth): a taken email is 409 EMAIL_EXISTS
// and the tenant rolls back with it.
func (s *Server) handleAuthSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.TenantName == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "tenant_name is required")
		return
	}
	email, ok := validateCredentials(w, authCredentials{Email: req.Email, Password: req.Password})
	if !ok {
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	createdAt := formatTime(s.cfg.Now())
	for attempt := 0; attempt < 8; attempt++ {
		tenant := store.Tenant{TenantID: newTenantID(), Name: req.TenantName, CreatedAt: createdAt}
		u := store.User{
			UserID:    uuid.NewString(),
			TenantID:  &tenant.TenantID,
			Email:     email,
			Role:      RoleOwner,
			CreatedAt: createdAt,
		}
		err := s.cfg.Store.CreateTenantWithOwnerUser(tenant, u, string(hash), uuid.NewString())
		switch {
		case errors.Is(err, store.ErrTenantExists):
			continue // generated id collision: retry with a fresh one
		case errors.Is(err, store.ErrEmailExists):
			writeError(w, http.StatusConflict, codeEmailExists, "email already registered")
			return
		case err != nil:
			s.writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, signupResponse{Tenant: tenant, User: sessionUserView(u)})
		return
	}
	s.writeInternal(w, r, errors.New("signup: exhausted tenant-id collision retries"))
}

// loginResponse returns the session bearer — plaintext exactly once, only
// its hash is stored — plus its expiry and the authenticated user.
type loginResponse struct {
	Token     string      `json:"token"`
	ExpiresAt string      `json:"expires_at"`
	User      sessionUser `json:"user"`
}

// handleAuthLogin verifies email+password and mints a web session
// (multi-tenant-rbac.md §Password auth). EVERY failure — unknown email,
// wrong password, disabled user — is the same 401 INVALID_CREDENTIALS: no
// account enumeration, with a dummy bcrypt comparison equalizing the
// unknown-email timing.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var creds authCredentials
	if !decodeStrict(w, r, &creds) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(creds.Email))
	// Brute-force throttle, checked BEFORE any lookup or bcrypt work — but
	// only rejecting when already exhausted, keyed on the attempted email
	// regardless of existence (no enumeration signal).
	if blocked, retryAfter := s.loginRL.exhausted(email); blocked {
		writeRateLimited(w, retryAfter, "too many failed login attempts")
		return
	}
	u, passwordHash, err := s.cfg.Store.UserByEmail(email)
	if errors.Is(err, store.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(creds.Password))
		s.loginRL.fail(email)
		writeError(w, http.StatusUnauthorized, codeInvalidCredentials, invalidCredentialsMsg)
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(creds.Password)) != nil ||
		u.DisabledAt != nil {
		s.loginRL.fail(email)
		writeError(w, http.StatusUnauthorized, codeInvalidCredentials, invalidCredentialsMsg)
		return
	}
	s.loginRL.reset(email)
	now := s.cfg.Now()
	ws := store.WebSession{
		UserID:    u.UserID,
		CreatedAt: formatTime(now),
		ExpiresAt: formatTime(now.Add(sessionTTL)),
	}
	for attempt := 0; attempt < 8; attempt++ {
		plaintext, hash, err := mintSessionPlaintext()
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		ws.SessionID = uuid.NewString()
		err = s.cfg.Store.InsertWebSession(ws, hash, uuid.NewString())
		if errors.Is(err, store.ErrDuplicateTokenHash) {
			continue
		}
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, loginResponse{
			Token: plaintext, ExpiresAt: ws.ExpiresAt, User: sessionUserView(u),
		})
		return
	}
	s.writeInternal(w, r, errors.New("session mint: exhausted hash-collision retries"))
}

// logoutResponse acknowledges the revocation; revoked=false means a
// concurrent logout already revoked this session (idempotent).
type logoutResponse struct {
	Revoked bool `json:"revoked"`
}

// handleAuthLogout revokes the CALLING session (its single legal mutation)
// and appends the user_events 'logout' row; the bearer stops resolving on
// the next request.
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	revoked, err := s.cfg.Store.RevokeWebSession(pr.sessionID, formatTime(s.cfg.Now()), uuid.NewString())
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, logoutResponse{Revoked: revoked})
}

// meResponse is the GET /api/v1/auth/me shape: the session's user view
// plus the session identity (non-secret; never the token or its hash).
type meResponse struct {
	User      sessionUser `json:"user"`
	SessionID string      `json:"session_id"`
}

// handleAuthMe returns the authenticated session's user, straight from the
// guard-resolved principal.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	u := sessionUser{UserID: pr.userID, Email: pr.email, Role: pr.role}
	if pr.class == classEnvAdmin {
		u.Role = RolePlatformAdmin
	}
	if pr.tenantID != "" {
		u.TenantID = &pr.tenantID
	}
	writeJSON(w, http.StatusOK, meResponse{User: u, SessionID: pr.sessionID})
}
