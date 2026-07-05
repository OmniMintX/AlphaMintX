package live

import (
	"encoding/json"
	"errors"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// ReconStatus is the GET /api/v1/oms/recon/status payload (spec §API
// surface). Env classes (read/env-admin) receive the full account-level
// view; tenant principals receive only the restricted subset plus their own
// strategies' pending-intent and orphan counts (multi-tenant-rbac.md
// isolation rule) — the env-only fields stay omitted.
type ReconStatus struct {
	Mode       string    `json:"mode"`
	VenueEnv   string    `json:"venue_env"`
	Reconciled bool      `json:"reconciled"`
	LastRun    *ReconRun `json:"last_run"`
	// PendingIntents counts pending_new intents: global for env classes,
	// the tenant's own strategies only for tenant principals.
	PendingIntents int `json:"pending_intents"`
	// Orphans (tenant view only) counts the last run's orphan_canceled
	// events attributed to the tenant's strategies.
	Orphans *int `json:"orphans,omitempty"`
	// Watermarks and VenueEpoch are account-level detail (env-class only).
	Watermarks []Watermark `json:"watermarks,omitempty"`
	VenueEpoch *int64      `json:"venue_epoch,omitempty"`
}

// ReconRun summarizes the latest reconcile run, derived from the persisted
// run_started/run_completed/run_failed brackets (restart-safe). Tenant
// principals see only Status and CompletedAt.
type ReconRun struct {
	RunID       string       `json:"run_id,omitempty"`
	StartedAt   string       `json:"started_at,omitempty"`
	CompletedAt string       `json:"completed_at,omitempty"`
	Status      string       `json:"status"` // running|completed|failed|incomplete
	Counters    *RunCounters `json:"counters,omitempty"`
}

// Watermark is one (symbol, venue_epoch, exchange_trade_id) R5 watermark.
type Watermark struct {
	Symbol          string `json:"symbol"`
	VenueEpoch      int64  `json:"venue_epoch"`
	ExchangeTradeID int64  `json:"exchange_trade_id"`
}

// Status is the api.ReconStatusProvider read seam. tenantID "" is the
// platform (env-class) view: full last_run, global pending_intents,
// watermarks, and the venue epoch. A non-empty tenantID restricts the
// payload to the tenant subset with counts over that tenant's strategies
// only (a foreign strategy is indistinguishable from absence).
func (o *OMS) Status(tenantID string) (ReconStatus, error) {
	o.mu.Lock()
	reconciled := o.reconciled && !o.resetPending
	epoch := o.venueEpoch.VenueEpoch
	running := o.running
	o.mu.Unlock()

	last, err := o.lastRun(running)
	if err != nil {
		return ReconStatus{}, err
	}
	intents, err := o.st.ListPendingNewIntents()
	if err != nil {
		return ReconStatus{}, err
	}
	st := ReconStatus{Mode: "live", VenueEnv: o.venueEnv, Reconciled: reconciled, LastRun: last}
	if tenantID == "" {
		st.PendingIntents = len(intents)
		st.VenueEpoch = &epoch
		for _, sym := range o.symbols {
			wm, ok, err := o.st.FillWatermark(epoch, o.venueOf[sym])
			if err != nil {
				return ReconStatus{}, err
			}
			if ok {
				st.Watermarks = append(st.Watermarks,
					Watermark{Symbol: sym, VenueEpoch: epoch, ExchangeTradeID: wm})
			}
		}
		return st, nil
	}
	if last != nil {
		st.LastRun = &ReconRun{Status: last.Status, CompletedAt: last.CompletedAt}
	}
	inTenant := make(map[string]bool)
	owns := func(strategyID string) (bool, error) {
		if v, ok := inTenant[strategyID]; ok {
			return v, nil
		}
		strat, err := o.st.GetStrategy(strategyID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		inTenant[strategyID] = err == nil && strat.TenantID == tenantID
		return inTenant[strategyID], nil
	}
	for _, intent := range intents {
		ok, err := owns(intent.StrategyID)
		if err != nil {
			return ReconStatus{}, err
		}
		if ok {
			st.PendingIntents++
		}
	}
	orphans := 0
	if last != nil && last.RunID != "" {
		events, err := o.st.ListOMSReconEvents(store.OMSReconEventFilter{
			RunID: last.RunID, Kind: "orphan_canceled",
		})
		if err != nil {
			return ReconStatus{}, err
		}
		for _, ev := range events {
			if ev.StrategyID == nil {
				continue
			}
			ok, err := owns(*ev.StrategyID)
			if err != nil {
				return ReconStatus{}, err
			}
			if ok {
				orphans++
			}
		}
	}
	st.Orphans = &orphans
	return st, nil
}

// lastRun derives the newest run's summary from the audit trail: the latest
// run_started row plus its completion bracket; counters parse from the
// run_completed details_json.
func (o *OMS) lastRun(running bool) (*ReconRun, error) {
	starts, err := o.st.ListOMSReconEvents(store.OMSReconEventFilter{Kind: "run_started"})
	if err != nil || len(starts) == 0 {
		return nil, err
	}
	newest := starts[len(starts)-1]
	if newest.RunID == nil {
		return nil, nil
	}
	run := &ReconRun{RunID: *newest.RunID, StartedAt: newest.RecordedAt, Status: "running"}
	events, err := o.st.ListOMSReconEvents(store.OMSReconEventFilter{RunID: run.RunID})
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		switch ev.Kind {
		case "run_completed":
			run.Status, run.CompletedAt = "completed", ev.RecordedAt
			var c RunCounters
			if json.Unmarshal([]byte(ev.DetailsJSON), &c) == nil {
				run.Counters = &c
			}
		case "run_failed":
			run.Status, run.CompletedAt = "failed", ev.RecordedAt
		}
	}
	if run.Status == "running" && !running {
		run.Status = "incomplete" // a crash interrupted the bracket
	}
	return run, nil
}
