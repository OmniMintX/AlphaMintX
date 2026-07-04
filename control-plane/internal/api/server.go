// Package api implements the Phase-1 control-plane HTTP API
// (docs/specs/persistence-and-api.md §HTTP API): read endpoints for the web
// dashboard, the L1 approval endpoint, and the agent-plane trace-ingestion
// boundary. Contract payloads are returned verbatim from the store; auth is
// static bearer tokens (read / operator / per-strategy agent) and tokens are
// never logged.
package api

import (
	"log"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// maxBodyBytes is the POST body cap (spec Limits: > 1 MiB => 413).
const maxBodyBytes = 1 << 20

// MarkSource provides the freshness-checked mark price for the approval
// preflight; *marketdata.Store satisfies it.
type MarkSource interface {
	Mark(symbol string, now time.Time) (decimal.Decimal, time.Time, bool)
}

// Submitter is the OMS seam: called at most once per verdict, on the single
// winning outcome=approved row (the OMS kill re-check still applies there).
type Submitter interface {
	SubmitApproved(meta store.VerdictMeta) error
}

// Config wires the server. Store is required; zero-value tokens disable
// their token class (they never match any request).
type Config struct {
	Store *store.Store
	// Marks feeds the preflight mark-freshness check; nil means no mark is
	// available (preflight blocks with MARK_PRICE_UNAVAILABLE).
	Marks MarkSource
	// Submitter receives the winning approved decision; nil means no OMS
	// submission is possible and the preflight blocks approvals with
	// SUBMITTER_UNAVAILABLE (approved_but_blocked, never a false
	// "submitted to OMS").
	Submitter Submitter

	// ReadToken authorizes GETs ONLY (web dashboard).
	ReadToken string
	// OperatorToken authorizes POST .../approvals ONLY (Trader role).
	OperatorToken string
	// OperatorPrincipal is recorded as approvals.decided_by.
	OperatorPrincipal string
	// AgentTokens maps strategy_id -> bearer token; each token is valid only
	// for its strategy's trace-ingestion endpoint.
	AgentTokens map[string]string

	// DailyLossBreached reports whether the strategy's daily-loss limit is
	// breached at now (derived at read time, Row rules); nil means the check
	// always passes (not wired in this deployment).
	DailyLossBreached func(strategyID string, now time.Time) (bool, error)

	// Now defaults to time.Now; tests inject a fixed clock.
	Now func() time.Time
	// Logf defaults to log.Printf. MUST NOT be handed token values.
	Logf func(format string, args ...any)
}

// Server is the http.Handler for the Phase-1 API.
type Server struct {
	cfg Config
	mux *http.ServeMux
	rl  *rateLimiter
}

// New builds the server and its routes.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux(), rl: newRateLimiter(cfg.Now)}

	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/strategies", s.readOnly(s.handleListStrategies))
	s.mux.HandleFunc("GET /api/v1/strategies/{id}", s.readOnly(s.handleGetStrategy))
	s.mux.HandleFunc("GET /api/v1/strategies/{id}/runs", s.readOnly(s.handleListRuns))
	s.mux.HandleFunc("GET /api/v1/strategies/{id}/runs/{run_id}", s.readOnly(s.handleGetRunDetail))
	s.mux.HandleFunc("POST /api/v1/strategies/{id}/approvals", s.operatorOnly(s.handlePostApproval))
	s.mux.HandleFunc("POST /api/v1/strategies/{id}/traces", s.agentOnly(s.handlePostTrace))
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// formatTime renders RFC 3339 UTC with Z suffix (store column convention).
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}
