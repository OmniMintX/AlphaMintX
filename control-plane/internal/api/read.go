package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// page is the pagination envelope: {items, total, page, limit} (page
// 1-based, limit default 20, max 100). Items is always a JSON array.
type page[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Limit int `json:"limit"`
}

func newPage[T any](items []T, total, pageNum, limit int) page[T] {
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items, Total: total, Page: pageNum, Limit: limit}
}

// pageParams parses ?page&limit and applies the store's normalization so
// the echoed values match what was actually queried.
func pageParams(r *http.Request) (int, int) {
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if pageNum < 1 {
		pageNum = 1
	}
	if limit < 1 {
		limit = store.DefaultPageLimit
	}
	if limit > store.MaxPageLimit {
		limit = store.MaxPageLimit
	}
	return pageNum, limit
}

// rootStrategy is the tenant-scoped root resolution of multi-tenant-rbac.md
// §Tenancy rules: tenant principals resolve within their own tenant — a
// foreign-tenant strategy is indistinguishable from absence — while
// platform env classes resolve every tenant. tenantID is threaded from the
// authenticated principal, never from request input.
func (s *Server) rootStrategy(pr principal, strategyID string) (store.Strategy, error) {
	if pr.tenantBound() {
		return s.cfg.Store.GetStrategyInTenant(strategyID, pr.tenantID)
	}
	return s.cfg.Store.GetStrategy(strategyID)
}

func (s *Server) handleListStrategies(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	pageNum, limit := pageParams(r)
	var (
		items []store.Strategy
		total int
		err   error
	)
	if pr.tenantBound() {
		items, total, err = s.cfg.Store.ListStrategiesByTenant(pr.tenantID, pageNum, limit)
	} else {
		items, total, err = s.cfg.Store.ListStrategies(pageNum, limit)
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

func (s *Server) handleGetStrategy(w http.ResponseWriter, r *http.Request) {
	st, err := s.rootStrategy(principalFrom(r), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(principalFrom(r), strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	pageNum, limit := pageParams(r)
	items, total, err := s.cfg.Store.ListRuns(strategyID, pageNum, limit)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newPage(items, total, pageNum, limit))
}

// runDetailResponse is store.RunDetail plus pending_approval, with arrays
// always non-null and the trace's schema_version stripped (web
// runDetailSchema is the shape authority for this endpoint).
type runDetailResponse struct {
	Run             store.Run              `json:"run"`
	Proposal        json.RawMessage        `json:"proposal"`
	Verdict         json.RawMessage        `json:"verdict"`
	Trace           json.RawMessage        `json:"trace"`
	Orders          []store.Order          `json:"orders"`
	Fills           []store.Fill           `json:"fills"`
	Approvals       []store.Approval       `json:"approvals"`
	PendingApproval *store.PendingApproval `json:"pending_approval"`
}

func (s *Server) handleGetRunDetail(w http.ResponseWriter, r *http.Request) {
	// Tenant root check first: a foreign-tenant strategy answers exactly
	// like a nonexistent run (the same 404 the missing pair below yields).
	// The sub-reads inside GetRunDetail key on the checked (strategy, run).
	if pr := principalFrom(r); pr.tenantBound() {
		if _, err := s.cfg.Store.GetStrategyInTenant(r.PathValue("id"), pr.tenantID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, codeUnknownRun, "unknown run")
				return
			}
			s.writeInternal(w, r, err)
			return
		}
	}
	d, err := s.cfg.Store.GetRunDetail(r.PathValue("id"), r.PathValue("run_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, codeUnknownRun, "unknown run")
		return
	}
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	resp := runDetailResponse{
		Run:       d.Run,
		Proposal:  d.Proposal,
		Verdict:   d.Verdict,
		Orders:    d.Orders,
		Fills:     d.Fills,
		Approvals: d.Approvals,
	}
	if resp.Trace, err = stripSchemaVersion(d.Trace); err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if resp.Orders == nil {
		resp.Orders = []store.Order{}
	}
	if resp.Fills == nil {
		resp.Fills = []store.Fill{}
	}
	if resp.Approvals == nil {
		resp.Approvals = []store.Approval{}
	}
	// A pending item is shown only until its approvals row supersedes it.
	if d.Verdict != nil && len(resp.Approvals) == 0 {
		var v struct {
			VerdictID string `json:"verdict_id"`
		}
		if err := json.Unmarshal(d.Verdict, &v); err != nil {
			s.writeInternal(w, r, err)
			return
		}
		if p, ok, err := s.cfg.Store.GetPendingApproval(v.VerdictID); err != nil {
			s.writeInternal(w, r, err)
			return
		} else if ok {
			resp.PendingApproval = &p
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// stripSchemaVersion removes the envelope's schema_version key (the run
// detail's embedded trace omits it, web agentTraceSchema); field values are
// preserved verbatim. A nil payload stays nil (marshals as JSON null).
func stripSchemaVersion(payload json.RawMessage) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	delete(fields, "schema_version")
	return json.Marshal(fields)
}
