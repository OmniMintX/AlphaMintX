package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

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
	// Live-OMS reconciliation codes (live-oms-and-reconciler.md §API
	// surface), value-consistent with the oms/live sentinel errors:
	// RECON_RUNNING is the 409 on POST .../oms/recon/run; the rest are
	// order-level — recorded on order status, rejected_submissions, and
	// oms_recon_events.details_json, never new HTTP shapes.
	codeReconRunning      = "RECON_RUNNING"
	codeReconcilePending  = "RECONCILE_PENDING"
	codeFilterUnavailable = "FILTER_UNAVAILABLE"
	codeFilterRejected    = "FILTER_REJECTED"
	codeExchangeRejected  = "EXCHANGE_REJECTED"
	// Safety-wiring code (safety-wiring.md §API surface): the platform
	// kill body must carry the explicit ack literal; missing/wrong is 400
	// and NO row is written.
	codePlatformKillAckRequired = "PLATFORM_KILL_ACK_REQUIRED"
	// Backup codes (ops-backup.md OB-6): the 409s follow the
	// RECON_RUNNING/TENANT_EXISTS precedent; the two 500s deliberately
	// bypass the uniform INTERNAL envelope and use the standard error
	// shape with these specific codes.
	codeBackupInProgress   = "BACKUP_IN_PROGRESS"
	codeBackupExists       = "BACKUP_EXISTS"
	codeBackupFailed       = "BACKUP_FAILED"
	codeBackupVerifyFailed = "BACKUP_VERIFY_FAILED"
	// Restore-gate codes (deploy-and-survive.md DS-3/DS-5): the 503
	// blocks new trading intent while a restored DB awaits the operator
	// ack; the 409 catches a lost ack race or an ack aimed at the wrong
	// deployment.
	codeRestoreGate           = "RESTORE_GATE"
	codeRestoreGateNotEngaged = "RESTORE_GATE_NOT_ENGAGED"
	// Strategy provisioning codes (strategy-provisioning.md SP-4/SP-4b):
	// both 409s follow the TENANT_EXISTS precedent — deterministic
	// conflicts for partner scripts, never silent duplicates.
	codeStrategyNameTaken    = "STRATEGY_NAME_TAKEN"
	codeStrategyLimitReached = "STRATEGY_LIMIT_REACHED"
	// Password-auth codes (multi-tenant-rbac.md §Password auth and web
	// sessions): every login failure — unknown email, wrong password,
	// disabled user — is the SAME 401 INVALID_CREDENTIALS (no account
	// enumeration); the two 409s follow the TENANT_EXISTS precedent.
	codeInvalidCredentials = "INVALID_CREDENTIALS"
	codeEmailExists        = "EMAIL_EXISTS"
	codeBootstrapComplete  = "BOOTSTRAP_COMPLETE"
	// Platform-secrets codes (platform-secrets.md §API): 503 when no
	// vault is wired (key file missing at startup is a startup failure,
	// nil Config.Vault is the test/replay wiring); 404 on the agent
	// llm-config read before the first set.
	codeVaultUnavailable = "VAULT_UNAVAILABLE"
	codeNotConfigured    = "NOT_CONFIGURED"
	// Market LLM analysis: 502 when the configured provider is
	// unreachable, answers non-200, or returns no completion content.
	codeLLMUpstream = "LLM_UPSTREAM"
	// Lifecycle and kill-clear codes (lifecycle-api.md §Error codes).
	codeInvalidLifecycleState    = "INVALID_LIFECYCLE_STATE"
	codeUseKillEndpoint          = "USE_KILL_ENDPOINT"
	codeIllegalTransition        = "ILLEGAL_TRANSITION"
	codePaperGateFailed          = "PAPER_GATE_FAILED"
	codeLifecycleConflict        = "LIFECYCLE_CONFLICT"
	codeNoActiveKill             = "NO_ACTIVE_KILL"
	codeClearConflict            = "CLEAR_CONFLICT"
	codePlatformClearAckRequired = "PLATFORM_CLEAR_ACK_REQUIRED"
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

// writeRateLimited answers 429 with the standard Retry-After header: the
// limiter's refill wait rounded UP to whole seconds, minimum 1.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration, message string) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, codeRateLimited, message)
}

// writeInternal answers 500 without leaking internals; the cause is logged.
func (s *Server) writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	s.cfg.Logf("api: %s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}
