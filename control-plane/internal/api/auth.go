package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Rate limit (spec Limits): per-token 60 req/min on every POST.
const (
	rateLimitBurst  = 60
	rateLimitPerSec = float64(rateLimitBurst) / 60
)

// Per-strategy proposal ingestion rate limit (docs/ARCHITECTURE.md: default
// 30/min); excess is 429 with NO persisted verdict.
const (
	proposalRateBurst  = 30
	proposalRatePerSec = float64(proposalRateBurst) / 60
)

// bearerToken extracts the Authorization bearer credential; ok=false when
// the header is absent or malformed. The value MUST never be logged.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

// tokenEqual is a constant-time comparison; an empty configured token never
// matches (a missing credential must not authorize anything).
func tokenEqual(configured, presented string) bool {
	return configured != "" &&
		subtle.ConstantTimeCompare([]byte(configured), []byte(presented)) == 1
}

// Principal classes (multi-tenant-rbac.md §Principals and roles): the three
// Phase 1 env classes, the NEW env-admin class, and the DB-token user class
// (DB agent tokens classify as classAgent with a tenant binding).
const (
	classRead     = "read"
	classOperator = "operator"
	classAgent    = "agent"
	classEnvAdmin = "env-admin"
	classUser     = "user"
)

// User roles, the fixed set (multi-tenant-rbac.md §Principals and roles).
const (
	RoleViewer = "viewer"
	RoleTrader = "trader"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

// roleRank orders viewer < trader < admin < owner (mint/revoke ceilings).
var roleRank = map[string]int{RoleViewer: 0, RoleTrader: 1, RoleAdmin: 2, RoleOwner: 3}

// principal is the resolved request identity. Env classes are
// platform-scoped (tenantID empty); DB tokens carry their tenant and their
// token_id audit identity. rateKey is the rate-limiter key: the plaintext
// for env tokens (Phase 1 behavior) and the token_hash for DB tokens, so
// plaintext is never held in long-lived maps (§Security rules).
type principal struct {
	class      string
	role       string // classUser only
	tenantID   string // DB tokens only
	strategyID string // classAgent: the token's strategy scope
	tokenID    string // DB tokens: the stable, non-secret audit identity
	rateKey    string
}

// tenantBound reports whether the principal is a tenant principal (DB
// token); env classes pass the root check for every tenant (§Principals).
func (p principal) tenantBound() bool { return p.tenantID != "" }

// actorID is the audit identity recorded in actor columns: token_id for DB
// principals, "env-admin" for the env admin, OperatorPrincipal otherwise
// (multi-tenant-rbac.md §Audit identity).
func (s *Server) actorID(pr principal) string {
	switch {
	case pr.tokenID != "":
		return pr.tokenID
	case pr.class == classEnvAdmin:
		return "env-admin"
	default:
		return s.cfg.OperatorPrincipal
	}
}

// hashToken is the DB-token lookup key: hex(SHA-256(plaintext)). The UNIQUE
// index matches whole fixed-length digests only — no prefix oracle, no
// length leak (§Token lifecycle, Lookup).
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// resolvePrincipal classifies a presented bearer. Env-token constant-time
// comparisons run FIRST and short-circuit the DB lookup (classification
// precedence, normative); the DB lookup observes revoked_at on EVERY
// request — a revoked token is unknown (401), never cached.
func (s *Server) resolvePrincipal(tok string) (principal, bool, error) {
	if tokenEqual(s.cfg.ReadToken, tok) {
		return principal{class: classRead, rateKey: tok}, true, nil
	}
	if tokenEqual(s.cfg.OperatorToken, tok) {
		return principal{class: classOperator, rateKey: tok}, true, nil
	}
	if tokenEqual(s.cfg.AdminToken, tok) {
		return principal{class: classEnvAdmin, rateKey: tok}, true, nil
	}
	for strategyID, agentTok := range s.cfg.AgentTokens {
		if tokenEqual(agentTok, tok) {
			return principal{class: classAgent, strategyID: strategyID, rateKey: tok}, true, nil
		}
	}
	hash := hashToken(tok)
	row, err := s.cfg.Store.TokenByHash(hash)
	if errors.Is(err, store.ErrNotFound) {
		return principal{}, false, nil
	}
	if err != nil {
		return principal{}, false, err
	}
	if row.RevokedAt != nil {
		return principal{}, false, nil
	}
	pr := principal{tenantID: row.TenantID, tokenID: row.TokenID, rateKey: hash}
	if row.Principal == "agent" {
		pr.class = classAgent
		if row.StrategyID != nil {
			pr.strategyID = *row.StrategyID
		}
	} else {
		pr.class = classUser
		if row.Role != nil {
			pr.role = *row.Role
		}
	}
	return pr, true, nil
}

// guard enforces one permission-matrix row in the normative order — auth
// (401), then role/class (403), then agent strategy scope (403
// STRATEGY_SCOPE_MISMATCH) — before the per-token POST rate limit and body
// cap. Tenant-scoped object resolution (404) is the handler's job, so an
// insufficient role never reveals whether an object exists.
func (s *Server) guard(perm RoutePermission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if perm.Public {
			next(w, r)
			return
		}
		tok, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing bearer token")
			return
		}
		pr, known, err := s.resolvePrincipal(tok)
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		if !known {
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "unknown token")
			return
		}
		if !perm.allows(pr) {
			writeError(w, http.StatusForbidden, codeForbidden, "token not valid for this endpoint")
			return
		}
		if pr.class == classAgent && pr.strategyID != r.PathValue("id") {
			writeError(w, http.StatusForbidden, codeStrategyScopeMismatch,
				"strategy outside the token scope")
			return
		}
		if r.Method != http.MethodGet {
			if !s.rl.allow(pr.rateKey) {
				writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded (60 req/min per token)")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next(w, r.WithContext(withPrincipal(r.Context(), pr)))
	}
}

// principalKey carries the resolved principal through the request context.
type principalKey struct{}

func withPrincipal(ctx context.Context, pr principal) context.Context {
	return context.WithValue(ctx, principalKey{}, pr)
}

// principalFrom returns the guard-resolved principal (zero value on the
// unauthenticated health route, which never reads it).
func principalFrom(r *http.Request) principal {
	pr, _ := r.Context().Value(principalKey{}).(principal)
	return pr
}

// rateLimiter is a keyed token bucket: capacity burst, refill perSec/s.
// The POST middleware keys it per token (60/min); proposal ingestion keys a
// second instance per strategy (30/min, docs/ARCHITECTURE.md).
type rateLimiter struct {
	now    func() time.Time
	burst  float64
	perSec float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(now func() time.Time, burst, perSec float64) *rateLimiter {
	return &rateLimiter{now: now, burst: burst, perSec: perSec, buckets: make(map[string]*bucket)}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
