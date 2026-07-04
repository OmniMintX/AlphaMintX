package api

import (
	"encoding/json"
	"net/http"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// API error codes (SCREAMING_SNAKE, open set per SS-25). The spec-named
// codes reuse the contract constants where they exist.
const (
	codeUnauthorized          = "UNAUTHORIZED"
	codeForbidden             = "FORBIDDEN"
	codeRateLimited           = "RATE_LIMITED"
	codeBodyTooLarge          = "BODY_TOO_LARGE"
	codeUnknownStrategy       = "UNKNOWN_STRATEGY"
	codeUnknownRun            = "UNKNOWN_RUN"
	codeUnknownVerdict        = "UNKNOWN_VERDICT"
	codeNotPending            = "NOT_PENDING"
	codeAlreadyDecided        = "ALREADY_DECIDED"
	codeStrategyScopeMismatch = "STRATEGY_SCOPE_MISMATCH"
	codeSchemaInvalid         = contract.CodeSchemaInvalid
	codeIdempotencyConflict   = contract.CodeIdempotencyConflict
	codeRunTickConflict       = "RUN_TICK_CONFLICT"
	codeInternal              = "INTERNAL"
	// Multi-tenant RBAC codes (multi-tenant-rbac.md §Permission matrix).
	codeUnknownToken       = "UNKNOWN_TOKEN"
	codeUnknownTenant      = "UNKNOWN_TENANT"
	codeTenantExists       = "TENANT_EXISTS"
	codeInvalidTenantID    = "INVALID_TENANT_ID"
	codeInvalidRole        = "INVALID_ROLE"
	codeInvalidLimitChange = "INVALID_LIMIT_CHANGE"
	// codeSubmitFailed marks an approved decision whose OMS submission
	// failed: surfaced in the approval response and persisted to the
	// rejected_submissions audit surface.
	codeSubmitFailed = "SUBMIT_FAILED"
	// Billing and metering codes (billing-and-metering.md §Permission
	// matrix additions).
	codeInvalidMeteringRecord = "INVALID_METERING_RECORD"
	codeMeteringConflict      = "METERING_CONFLICT"
	codeInvalidPeriod         = "INVALID_PERIOD"
	codePeriodClosed          = "PERIOD_CLOSED"
	codePeriodOpen            = "PERIOD_OPEN"
	codeUnknownInvoice        = "UNKNOWN_INVOICE"
	codeUnknownReconciliation = "UNKNOWN_RECONCILIATION"
)

// Preflight reason codes (persistence-and-api.md §Approval preflight).
const (
	reasonKillSwitchActive     = contract.CodeKillSwitchActive
	reasonMarkPriceUnavailable = contract.CodeMarkPriceUnavailable
	reasonDailyLossLimitBreach = contract.CodeDailyLossLimitBreached
	reasonStrategyNotLive      = "STRATEGY_NOT_LIVE"
	// reasonSubmitterUnavailable blocks approvals in deployments with no
	// OMS Submitter wired: an approval that could never be submitted is
	// approved_but_blocked, never a false "submitted to OMS".
	reasonSubmitterUnavailable = "SUBMITTER_UNAVAILABLE"
)

// errorBody is the error response shape (web apiErrorBodySchema): 409 on an
// already-decided approval carries the recorded outcome.
type errorBody struct {
	Code     string          `json:"code"`
	Message  string          `json:"message"`
	Recorded *store.Approval `json:"recorded,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// writeInternal answers 500 without leaking internals; the cause is logged.
func (s *Server) writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Logf("api: %s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}
