// Package api implements the control-plane HTTP API
// (docs/specs/persistence-and-api.md §HTTP API and
// docs/specs/multi-tenant-rbac.md): read endpoints for the web dashboard,
// the L1 approval endpoint, the agent-plane ingestion boundary, and the
// Phase 2 tenancy/RBAC surfaces (tenants, DB tokens, runtime limit changes,
// tenant kill). Contract payloads are returned verbatim from the store;
// auth is bearer tokens — platform env classes plus tenant-scoped DB tokens
// — and tokens are never logged.
package api

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/oms/live"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
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

// RuntimeStateSource hydrates the Risk Gate's RuntimeState for proposal
// ingestion; *runstate.Hydrator satisfies it.
type RuntimeStateSource interface {
	State(strategyID, lifecycleState, symbol string, now time.Time) (riskgate.RuntimeState, error)
}

// SafetyDriver is the live-OMS safety-effects seam (safety-wiring.md §API
// surface): DriveSafetyEffects re-drives unserved kill/breaker effects.
// Kill handlers invoke it in a detached goroutine AFTER the 200 response;
// errors are logged, never surfaced — the persisted row guarantees eventual
// effects via the reconcile cadence.
type SafetyDriver interface {
	DriveSafetyEffects(ctx context.Context) error
}

// ReconStatusProvider is the live-OMS reconciliation seam
// (live-oms-and-reconciler.md §API surface): Status is tenant-filtered by
// the principal's tenant scope ("" = platform view) and TriggerRun runs
// R1-R7 synchronously (ErrReconRunning when a run is in progress). The live
// OMS satisfies it; main.go wires it in live mode only.
type ReconStatusProvider interface {
	Status(tenantID string) (live.ReconStatus, error)
	TriggerRun(ctx context.Context, acceptVenueReset bool) error
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
	// Limits are the base RiskLimits every ingested proposal is evaluated
	// against; with RuntimeState it enables the proposal ingestion
	// endpoint (nil disables it: proposals cannot be gated without limits).
	Limits *riskgate.RiskLimits
	// RuntimeState hydrates the gate's runtime inputs at ingestion.
	RuntimeState RuntimeStateSource
	// LimitsProvider is the single read path for effective limits
	// (multi-tenant-rbac.md §Runtime limit changes); build it with
	// NewLimitsProvider so persisted overlay rows survive restarts. nil
	// with Limits set falls back to a provider over the bare base (no
	// persisted overlay — test/replay wiring).
	LimitsProvider *LimitsProvider
	// ReconStatus enables the two /api/v1/oms/recon routes; nil (paper
	// deployments) leaves them unregistered (404), preserving the
	// Phase 1/2 surface exactly (live-oms-and-reconciler.md §API surface).
	ReconStatus ReconStatusProvider
	// SafetyDriver is the OPTIONAL on-demand effects drive invoked by the
	// kill handlers after their row is acknowledged (safety-wiring.md
	// §API surface); nil in paper mode — no drive runs, the persisted row
	// alone gate-blocks.
	SafetyDriver SafetyDriver
	// Heartbeats receives heartbeat beats (watchdog.md §Wiring seams),
	// wired to the safety.Monitor in live mode; nil in paper mode — the
	// heartbeat handler accepts and discards (WD-3).
	Heartbeats HeartbeatSink

	// ReadToken authorizes GETs ONLY (web dashboard), every tenant.
	ReadToken string
	// OperatorToken authorizes POST .../approvals ONLY (Trader role).
	OperatorToken string
	// OperatorPrincipal is recorded as approvals.decided_by.
	OperatorPrincipal string
	// AgentTokens maps strategy_id -> bearer token; each token is valid only
	// for its strategy's two ingestion endpoints.
	AgentTokens map[string]string
	// AdminToken is the env-admin class (multi-tenant-rbac.md): tenant
	// management, token management, limits changes, tenant kill — any
	// tenant, no reads.
	AdminToken string

	// DailyLossBreached reports whether the strategy's daily-loss limit is
	// breached at now (derived at read time, Row rules); nil means the check
	// always passes (not wired in this deployment). The limit MUST come
	// from the LimitsProvider per strategy, never a startup capture.
	DailyLossBreached func(strategyID string, now time.Time) (bool, error)

	// Now defaults to time.Now; tests inject a fixed clock.
	Now func() time.Time
	// Logf defaults to log.Printf. MUST NOT be handed token values.
	Logf func(format string, args ...any)
}

// Server is the http.Handler for the control-plane API.
type Server struct {
	cfg    Config
	mux    *http.ServeMux
	limits *LimitsProvider   // effective-limits read path (nil: no gating)
	routes []RoutePermission // the registered subset of Permissions()
	rl     *rateLimiter      // per-token 60/min, every POST
	prl    *rateLimiter      // per-strategy 30/min, proposal ingestion only

	// gateMu serializes gate evaluations per strategy_id (risk-limits.md
	// "Gate evaluation order").
	gateMu     sync.Mutex
	strategyMu map[string]*sync.Mutex
}

// New builds the server; every route is registered FROM the permission
// matrix (multi-tenant-rbac.md §Test requirements) so no route can exist
// without a matrix entry.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	s := &Server{
		cfg:        cfg,
		mux:        http.NewServeMux(),
		limits:     cfg.LimitsProvider,
		rl:         newRateLimiter(cfg.Now, rateLimitBurst, rateLimitPerSec),
		prl:        newRateLimiter(cfg.Now, proposalRateBurst, proposalRatePerSec),
		strategyMu: make(map[string]*sync.Mutex),
	}
	if s.limits == nil && cfg.Limits != nil {
		s.limits = newBaseLimitsProvider(*cfg.Limits)
	}

	handlers := map[string]http.HandlerFunc{
		"GET /health":                                    s.handleHealth,
		"GET /api/v1/strategies":                         s.handleListStrategies,
		"GET /api/v1/strategies/{id}":                    s.handleGetStrategy,
		"GET /api/v1/strategies/{id}/runs":               s.handleListRuns,
		"GET /api/v1/strategies/{id}/runs/{run_id}":      s.handleGetRunDetail,
		"POST /api/v1/strategies/{id}/approvals":         s.handlePostApproval,
		"POST /api/v1/strategies/{id}/traces":            s.handlePostTrace,
		"POST /api/v1/strategies/{id}/proposals":         s.handlePostProposal,
		"POST /api/v1/strategies/{id}/heartbeat":         s.handleHeartbeat,
		"POST /api/v1/strategies/{id}/limits":            s.handlePostLimits,
		"POST /api/v1/strategies/{id}/kill":              s.handleStrategyKill,
		"POST /api/v1/tenants":                           s.handleCreateTenant,
		"POST /api/v1/tenants/{tenant_id}/kill":          s.handleTenantKill,
		"POST /api/v1/platform/kill":                     s.handlePlatformKill,
		"POST /api/v1/tokens":                            s.handleMintToken,
		"GET /api/v1/tokens":                             s.handleListTokens,
		"POST /api/v1/tokens/{token_id}/revoke":          s.handleRevokeToken,
		"POST /api/v1/billing/metering":                  s.handlePostMetering,
		"POST /api/v1/billing/periods/close":             s.handleClosePeriod,
		"POST /api/v1/billing/reconcile":                 s.handleReconcile,
		"GET /api/v1/billing/invoices":                   s.handleListInvoices,
		"GET /api/v1/billing/invoices/{invoice_id}":      s.handleGetInvoice,
		"GET /api/v1/billing/reconciliations":            s.handleListReconciliations,
		"GET /api/v1/billing/reconciliations/{recon_id}": s.handleGetReconciliation,
		"GET /api/v1/oms/recon/status":                   s.handleGetReconStatus,
		"POST /api/v1/oms/recon/run":                     s.handlePostReconRun,
	}
	for _, perm := range Permissions() {
		switch perm.Requires {
		case requiresIngestion:
			if s.limits == nil || cfg.RuntimeState == nil {
				continue
			}
		case requiresLimits:
			if s.limits == nil {
				continue
			}
		case requiresLiveOMS:
			if cfg.ReconStatus == nil {
				continue
			}
		}
		key := perm.Method + " " + perm.Path
		h, ok := handlers[key]
		if !ok {
			panic("api: permission matrix names an unknown route " + key)
		}
		s.routes = append(s.routes, perm)
		s.mux.HandleFunc(key, s.guard(perm, h))
	}
	return s
}

// strategyLock returns the per-strategy gate serialization lock.
func (s *Server) strategyLock(strategyID string) *sync.Mutex {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	if m, ok := s.strategyMu[strategyID]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.strategyMu[strategyID] = m
	return m
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
