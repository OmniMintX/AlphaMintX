package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
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

// tokenClass classifies a presented token: read, operator, or agent (with
// its strategy scope). known=false means 401.
func (s *Server) tokenClass(tok string) (class string, agentStrategy string, known bool) {
	if tokenEqual(s.cfg.ReadToken, tok) {
		return "read", "", true
	}
	if tokenEqual(s.cfg.OperatorToken, tok) {
		return "operator", "", true
	}
	for strategyID, agentTok := range s.cfg.AgentTokens {
		if tokenEqual(agentTok, tok) {
			return "agent", strategyID, true
		}
	}
	return "", "", false
}

// authorize resolves the request credential to a token class or writes the
// 401/403 itself (wantClass mismatch => 403: the token is real but its class
// must not use this endpoint — e.g. READ_TOKEN can never POST).
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, wantClass string) (token, agentStrategy string, ok bool) {
	tok, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing bearer token")
		return "", "", false
	}
	class, scope, known := s.tokenClass(tok)
	if !known {
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "unknown token")
		return "", "", false
	}
	if class != wantClass {
		writeError(w, http.StatusForbidden, codeForbidden, "token not valid for this endpoint")
		return "", "", false
	}
	return tok, scope, true
}

// readOnly guards GET endpoints: READ_TOKEN only.
func (s *Server) readOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := s.authorize(w, r, "read"); !ok {
			return
		}
		next(w, r)
	}
}

// operatorOnly guards POST .../approvals: OPERATOR_TOKEN + rate limit.
func (s *Server) operatorOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _, ok := s.authorize(w, r, "operator")
		if !ok {
			return
		}
		if !s.rl.allow(tok) {
			writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded (60 req/min per token)")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
}

// agentOnly guards POST .../traces: a per-strategy agent token whose scope
// matches the path {id} (else 403 STRATEGY_SCOPE_MISMATCH) + rate limit.
func (s *Server) agentOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, scope, ok := s.authorize(w, r, "agent")
		if !ok {
			return
		}
		if scope != r.PathValue("id") {
			writeError(w, http.StatusForbidden, codeStrategyScopeMismatch,
				"strategy outside the token scope")
			return
		}
		if !s.rl.allow(tok) {
			writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded (60 req/min per token)")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
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
