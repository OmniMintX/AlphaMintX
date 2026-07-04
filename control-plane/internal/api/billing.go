package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// periodPattern is the YYYY-MM billing-period shape (UTC calendar month).
var periodPattern = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)

// meteringRecordRequest is one gateway spend-log row of the import body.
// Exactly one of strategy_id / api_key_alias resolves the strategy.
type meteringRecordRequest struct {
	RequestID    string `json:"request_id"`
	StrategyID   string `json:"strategy_id,omitempty"`
	APIKeyAlias  string `json:"api_key_alias,omitempty"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CostUSD      string `json:"cost_usd"`
	MeteredAt    string `json:"metered_at"`
}

// meteringRequest is the POST /api/v1/billing/metering body (env-admin
// ONLY: the export file is a deployer artifact, no tenant holds it).
type meteringRequest struct {
	Source   string                  `json:"source"`
	AliasMap map[string]string       `json:"alias_map,omitempty"`
	Records  []meteringRecordRequest `json:"records"`
}

// meteringResponse acknowledges one atomic import batch.
type meteringResponse struct {
	Source   string `json:"source"`
	Imported int    `json:"imported"`
	Skipped  int    `json:"skipped"`
}

// handlePostMetering imports a mintrouter spend-log export batch
// (billing-and-metering.md §Metering ingest). The batch is atomic: ANY
// invalid record 400-rejects the whole POST (nothing persisted); a
// request_id disagreeing with its stored content 409-rejects it likewise.
// Chunked imports are safe by per-record idempotency.
func (s *Server) handlePostMetering(w http.ResponseWriter, r *http.Request) {
	var req meteringRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "source is required")
		return
	}
	records := make([]store.MeteringRecord, 0, len(req.Records))
	for i, rec := range req.Records {
		resolved, msg, err := s.resolveMeteringRecord(rec, req.AliasMap)
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		if msg != "" {
			writeError(w, http.StatusBadRequest, codeInvalidMeteringRecord,
				fmt.Sprintf("records[%d]: %s", i, msg))
			return
		}
		records = append(records, resolved)
	}
	imported, skipped, err := s.cfg.Store.InsertMeteringRecords(req.Source, records, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrMeteringConflict):
		writeError(w, http.StatusConflict, codeMeteringConflict,
			"a request_id is already imported with different content")
	case err != nil:
		s.writeInternal(w, r, err)
	default:
		writeJSON(w, http.StatusOK, meteringResponse{Source: req.Source, Imported: imported, Skipped: skipped})
	}
}

// resolveMeteringRecord validates one import record and resolves its
// strategy (directly or via alias_map); the record's tenant derives from
// strategies.tenant_id, never from the body. "" msg means valid; a non-nil
// err is a store failure (500, never a record error). Records without
// request_id are REJECTED: a row that cannot ever join is a defective
// export, and minting a synthetic id would fabricate coverage.
func (s *Server) resolveMeteringRecord(rec meteringRecordRequest, aliasMap map[string]string) (store.MeteringRecord, string, error) {
	if !uuidPattern.MatchString(rec.RequestID) {
		return store.MeteringRecord{}, "request_id is required and must be a lowercase UUID", nil
	}
	strategyID := rec.StrategyID
	if strategyID == "" {
		if rec.APIKeyAlias == "" {
			return store.MeteringRecord{}, "one of strategy_id or api_key_alias is required", nil
		}
		mapped, ok := aliasMap[rec.APIKeyAlias]
		if !ok {
			return store.MeteringRecord{}, fmt.Sprintf("api_key_alias %q is not in alias_map", rec.APIKeyAlias), nil
		}
		strategyID = mapped
	}
	if _, err := s.cfg.Store.GetStrategy(strategyID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return store.MeteringRecord{}, "", err
		}
		return store.MeteringRecord{}, fmt.Sprintf("strategy %q does not exist", strategyID), nil
	}
	if rec.Model == "" {
		return store.MeteringRecord{}, "model is required", nil
	}
	if rec.InputTokens < 0 || rec.OutputTokens < 0 {
		return store.MeteringRecord{}, "negative token count", nil
	}
	if _, err := contract.ParseDecimal(rec.CostUSD); err != nil {
		return store.MeteringRecord{}, fmt.Sprintf("cost_usd %q is not a decimal string", rec.CostUSD), nil
	}
	if _, err := contract.ParseUTCTime(rec.MeteredAt); err != nil {
		return store.MeteringRecord{}, fmt.Sprintf("metered_at %q is not an RFC 3339 UTC timestamp", rec.MeteredAt), nil
	}
	return store.MeteringRecord{
		RequestID: rec.RequestID, StrategyID: strategyID, Model: rec.Model,
		InputTokens: rec.InputTokens, OutputTokens: rec.OutputTokens,
		CostUSD: rec.CostUSD, MeteredAt: rec.MeteredAt,
	}, "", nil
}

// periodRequest is the close and reconcile body: {tenant_id, period}.
type periodRequest struct {
	TenantID string `json:"tenant_id"`
	Period   string `json:"period"`
}

// closePeriodResponse carries the generated invoice with its lines.
type closePeriodResponse struct {
	Invoice store.Invoice       `json:"invoice"`
	Lines   []store.InvoiceLine `json:"lines"`
}

// handleClosePeriod closes a (tenant, period) UTC month and generates its
// invoice in one transaction (billing-and-metering.md §Billing): a running
// month cannot close (400 INVALID_PERIOD), a second close answers 409
// PERIOD_CLOSED, and an unknown tenant 404 (env-admin only, no oracle).
func (s *Server) handleClosePeriod(w http.ResponseWriter, r *http.Request) {
	var req periodRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !periodPattern.MatchString(req.Period) {
		writeError(w, http.StatusBadRequest, codeInvalidPeriod, "period must be YYYY-MM")
		return
	}
	now := s.cfg.Now()
	_, periodEnd, err := store.PeriodBounds(req.Period)
	if err != nil {
		writeError(w, http.StatusBadRequest, codeInvalidPeriod, "period must be YYYY-MM")
		return
	}
	if periodEnd >= now.UTC().Format("2006-01-02") {
		writeError(w, http.StatusBadRequest, codeInvalidPeriod, "a running month cannot close")
		return
	}
	invoice, lines, err := s.cfg.Store.ClosePeriod(req.TenantID, req.Period, now)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeUnknownTenant, "unknown tenant")
	case errors.Is(err, store.ErrPeriodClosed):
		writeError(w, http.StatusConflict, codePeriodClosed, "period is already closed")
	case err != nil:
		s.writeInternal(w, r, err)
	default:
		if lines == nil {
			lines = []store.InvoiceLine{}
		}
		writeJSON(w, http.StatusOK, closePeriodResponse{Invoice: invoice, Lines: lines})
	}
}

// reconcileResponse carries the appended run with its discrepancies.
type reconcileResponse struct {
	Run           store.ReconciliationRun `json:"run"`
	Discrepancies []store.Discrepancy     `json:"discrepancies"`
}

// handleReconcile runs one reconciliation for a CLOSED period
// (billing-and-metering.md §Reconciliation): reconcile-before-close is 409
// PERIOD_OPEN — the invoice is the comparison target.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var req periodRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !periodPattern.MatchString(req.Period) {
		writeError(w, http.StatusBadRequest, codeInvalidPeriod, "period must be YYYY-MM")
		return
	}
	run, discrepancies, err := s.cfg.Store.Reconcile(req.TenantID, req.Period, s.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, codeUnknownTenant, "unknown tenant")
	case errors.Is(err, store.ErrPeriodOpen):
		writeError(w, http.StatusConflict, codePeriodOpen, "period is not closed")
	case err != nil:
		s.writeInternal(w, r, err)
	default:
		if discrepancies == nil {
			discrepancies = []store.Discrepancy{}
		}
		writeJSON(w, http.StatusOK, reconcileResponse{Run: run, Discrepancies: discrepancies})
	}
}

// listTenant scopes billing lists: tenant principals see ONLY their own
// tenant; the platform read and env-admin classes ("" tenant) see every
// tenant, exactly as the read class reads every tenant's runs.
func listTenant(pr principal) string { return pr.tenantID }

// handleListInvoices lists invoices, tenant-scoped for tenant principals.
func (s *Server) handleListInvoices(w http.ResponseWriter, r *http.Request) {
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListInvoices(listTenant(principalFrom(r)), pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

// invoiceResponse is the invoice detail shape: the invoice plus its lines.
type invoiceResponse struct {
	Invoice store.Invoice       `json:"invoice"`
	Lines   []store.InvoiceLine `json:"lines"`
}

// handleGetInvoice returns one invoice with lines; a foreign or absent
// invoice_id is the SAME 404 (no cross-tenant existence oracle).
func (s *Server) handleGetInvoice(w http.ResponseWriter, r *http.Request) {
	invoice, lines, err := s.cfg.Store.GetInvoice(r.PathValue("invoice_id"), listTenant(principalFrom(r)))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownInvoice, "unknown invoice")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if lines == nil {
		lines = []store.InvoiceLine{}
	}
	writeJSON(w, http.StatusOK, invoiceResponse{Invoice: invoice, Lines: lines})
}

// handleListReconciliations lists reconciliation runs, tenant-scoped for
// tenant principals.
func (s *Server) handleListReconciliations(w http.ResponseWriter, r *http.Request) {
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListReconciliations(listTenant(principalFrom(r)), pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

// handleGetReconciliation returns one run with its discrepancies; a foreign
// or absent recon_id is the SAME 404 (no cross-tenant existence oracle).
func (s *Server) handleGetReconciliation(w http.ResponseWriter, r *http.Request) {
	run, discrepancies, err := s.cfg.Store.GetReconciliation(r.PathValue("recon_id"), listTenant(principalFrom(r)))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownReconciliation, "unknown reconciliation")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if discrepancies == nil {
		discrepancies = []store.Discrepancy{}
	}
	writeJSON(w, http.StatusOK, reconcileResponse{Run: run, Discrepancies: discrepancies})
}
