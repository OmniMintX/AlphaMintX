package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Reconciliation (docs/specs/billing-and-metering.md §Reconciliation): each
// run compares the invoice's client set (the close's rowid window) against
// the imported gateway export and appends a reconciliation_runs row plus
// its discrepancies in one transaction — append-only; re-running appends a
// fresh run (later imports can turn a FAIL into a PASS).

// Discrepancy classes (discrepancies.class CHECK constraint).
const (
	ClassOrphanClient        = "ORPHAN_CLIENT"
	ClassOrphanGateway       = "ORPHAN_GATEWAY"
	ClassEstimatedClient     = "ESTIMATED_CLIENT"
	ClassMismatchTokens      = "MISMATCH_TOKENS"
	ClassMismatchCost        = "MISMATCH_COST"
	ClassAttributionMismatch = "ATTRIBUTION_MISMATCH"
)

// ReconciliationRun mirrors the append-only reconciliation_runs table. The
// four cost sums partition the client set exactly (matched + orphan_client
// + estimated + unattributed == InvoiceTotalUSD, the arithmetic identity).
type ReconciliationRun struct {
	ReconID                   string `json:"recon_id"`
	TenantID                  string `json:"tenant_id"`
	Period                    string `json:"period"`
	InvoiceID                 string `json:"invoice_id"`
	Status                    string `json:"status"`
	MatchedCount              int    `json:"matched_count"`
	DiscrepancyCount          int    `json:"discrepancy_count"`
	MatchedClientCostUSD      string `json:"matched_client_cost_usd"`
	OrphanClientCostUSD       string `json:"orphan_client_cost_usd"`
	EstimatedClientCostUSD    string `json:"estimated_client_cost_usd"`
	UnattributedClientCostUSD string `json:"unattributed_client_cost_usd"`
	InvoiceTotalUSD           string `json:"invoice_total_usd"`
	RunAt                     string `json:"run_at"`
}

// Discrepancy mirrors the append-only discrepancies table.
type Discrepancy struct {
	DiscrepancyID string  `json:"discrepancy_id"`
	ReconID       string  `json:"recon_id"`
	Class         string  `json:"class"`
	RequestID     *string `json:"request_id"`
	StrategyID    *string `json:"strategy_id"`
	DetailsJSON   string  `json:"details_json"`
}

// Reconcile runs one reconciliation for the CLOSED (tenant, period):
// ErrNotFound for an unknown tenant, ErrPeriodOpen when no billing_periods
// row exists. Classification of every client-set row is the normative
// precedence — unattributed (NULL request_id), estimated, matched (equal
// strategy_id both sides), else orphan_client. The run PASSES iff every
// matched pair has exact token equality, the arithmetic identity holds
// exactly, the (strategy, usage day) aggregate check holds at ±0, and no
// ATTRIBUTION_MISMATCH exists; MISMATCH_COST and the orphan/estimated
// classes are enumerated, never failures.
func (s *Store) Reconcile(tenantID, period string, now time.Time) (ReconciliationRun, []Discrepancy, error) {
	periodStart, periodEnd, err := PeriodBounds(period)
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	defer rollback(tx)
	if err := requireTenant(tx, tenantID); err != nil {
		return ReconciliationRun{}, nil, err
	}
	var periodRowid, watermark int64
	err = tx.QueryRow(`SELECT rowid, watermark_rowid FROM billing_periods
		WHERE tenant_id = ? AND period = ?`, tenantID, period).Scan(&periodRowid, &watermark)
	if err == sql.ErrNoRows {
		return ReconciliationRun{}, nil, fmt.Errorf("period %s %s: %w", tenantID, period, ErrPeriodOpen)
	}
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	// The previous watermark as the close saw it: watermarks are
	// nondecreasing in close (billing_periods rowid) order under SQLite's
	// single writer, so the MAX over EARLIER closes reproduces the window.
	var prev int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(watermark_rowid), 0) FROM billing_periods
		WHERE tenant_id = ? AND rowid < ?`, tenantID, periodRowid).Scan(&prev); err != nil {
		return ReconciliationRun{}, nil, err
	}
	var invoiceID, invoiceTotal string
	if err := tx.QueryRow(`SELECT invoice_id, total_usd FROM invoices
		WHERE tenant_id = ? AND period = ?`, tenantID, period).Scan(&invoiceID, &invoiceTotal); err != nil {
		return ReconciliationRun{}, nil, err
	}
	costs, err := windowRows(tx, tenantID, prev, watermark)
	if err != nil {
		return ReconciliationRun{}, nil, err
	}

	run := ReconciliationRun{
		ReconID: uuid.NewString(), TenantID: tenantID, Period: period,
		InvoiceID: invoiceID, InvoiceTotalUSD: invoiceTotal, RunAt: formatTime(now),
	}
	discrepancies, sums, err := classifyClientSet(tx, &run, costs)
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	orphans, err := gatewayOrphans(tx, run.ReconID, tenantID, periodStart, periodEnd, sums.matchedIDs)
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	discrepancies = append(discrepancies, orphans...)

	total, err := decimal.NewFromString(invoiceTotal)
	if err != nil {
		return ReconciliationRun{}, nil, fmt.Errorf("invoice total_usd %q: %w", invoiceTotal, err)
	}
	identity := sums.matched.Add(sums.orphan).Add(sums.estimated).Add(sums.unattributed).Equal(total)
	run.Status = "pass"
	if sums.tokenMismatch || sums.attributionMismatch || !identity || !sums.aggregateOK() {
		run.Status = "fail"
	}
	run.MatchedClientCostUSD = sums.matched.String()
	run.OrphanClientCostUSD = sums.orphan.String()
	run.EstimatedClientCostUSD = sums.estimated.String()
	run.UnattributedClientCostUSD = sums.unattributed.String()
	run.DiscrepancyCount = len(discrepancies)

	if err := insertReconciliation(tx, run, discrepancies); err != nil {
		return ReconciliationRun{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ReconciliationRun{}, nil, err
	}
	return run, discrepancies, nil
}

// aggPair accumulates the (strategy_id, usage day) aggregate check over
// matched pairs: client token sums MUST equal gateway token sums at ±0.
type aggPair struct {
	clientIn, clientOut, gwIn, gwOut int
}

// reconSums carries the classification totals across the Reconcile helpers.
type reconSums struct {
	matched, orphan, estimated, unattributed decimal.Decimal
	matchedIDs                               map[string]bool
	tokenMismatch, attributionMismatch       bool
	aggs                                     map[[2]string]*aggPair
}

func (r *reconSums) aggregateOK() bool {
	for _, a := range r.aggs {
		if a.clientIn != a.gwIn || a.clientOut != a.gwOut {
			return false
		}
	}
	return true
}

// newDiscrepancy builds one discrepancies row; details marshal with sorted
// keys (encoding/json map order), so rows are deterministic.
func newDiscrepancy(reconID, class string, requestID, strategyID *string, details map[string]any) (Discrepancy, error) {
	b, err := json.Marshal(details)
	if err != nil {
		return Discrepancy{}, err
	}
	return Discrepancy{
		DiscrepancyID: uuid.NewString(), ReconID: reconID, Class: class,
		RequestID: requestID, StrategyID: strategyID, DetailsJSON: string(b),
	}, nil
}

// classifyClientSet applies the normative precedence to every client-set
// row (first match wins, every row in EXACTLY ONE class) and emits the
// client-side discrepancies in window (rowid) order.
func classifyClientSet(tx *sql.Tx, run *ReconciliationRun, costs []costRow) ([]Discrepancy, *reconSums, error) {
	sums := &reconSums{matchedIDs: map[string]bool{}, aggs: map[[2]string]*aggPair{}}
	var out []Discrepancy
	add := func(class string, requestID *string, strategyID string, details map[string]any) error {
		d, err := newDiscrepancy(run.ReconID, class, requestID, &strategyID, details)
		if err != nil {
			return err
		}
		out = append(out, d)
		return nil
	}
	for _, r := range costs {
		cost, err := decimal.NewFromString(r.CostUSD)
		if err != nil {
			return nil, nil, fmt.Errorf("cost_usd %q: %w", r.CostUSD, err)
		}
		// Rule 1: NULL request_id — unattributed (pre-upgrade, stub,
		// overflow-aggregate, conflict-nulled rows), identity only.
		if r.RequestID == nil {
			sums.unattributed = sums.unattributed.Add(cost)
			continue
		}
		// Rule 2: estimated — the gateway may hold real spend or nothing,
		// and estimated counts never promise equality.
		if r.IsEstimated {
			sums.estimated = sums.estimated.Add(cost)
			if err := add(ClassEstimatedClient, r.RequestID, r.StrategyID, map[string]any{
				"model": r.Model, "input_tokens": r.InTokens,
				"output_tokens": r.OutTokens, "cost_usd": r.CostUSD,
			}); err != nil {
				return nil, nil, err
			}
			continue
		}
		var gw MeteringRecord
		err = tx.QueryRow(`SELECT strategy_id, model, input_tokens, output_tokens, cost_usd
			FROM metering_records WHERE request_id = ?`, *r.RequestID).
			Scan(&gw.StrategyID, &gw.Model, &gw.InputTokens, &gw.OutputTokens, &gw.CostUSD)
		switch {
		case err == sql.ErrNoRows:
			// Rule 4: no gateway row at all.
			sums.orphan = sums.orphan.Add(cost)
			if err := add(ClassOrphanClient, r.RequestID, r.StrategyID, map[string]any{
				"model": r.Model, "input_tokens": r.InTokens,
				"output_tokens": r.OutTokens, "cost_usd": r.CostUSD,
			}); err != nil {
				return nil, nil, err
			}
		case err != nil:
			return nil, nil, err
		case gw.StrategyID != r.StrategyID:
			// Rule 4 with a failed strategy constraint: the row stays
			// orphan_client and ATTRIBUTION_MISMATCH surfaces the forgery.
			sums.orphan = sums.orphan.Add(cost)
			sums.attributionMismatch = true
			if err := add(ClassAttributionMismatch, r.RequestID, r.StrategyID, map[string]any{
				"client_strategy_id": r.StrategyID, "gateway_strategy_id": gw.StrategyID,
			}); err != nil {
				return nil, nil, err
			}
		default:
			// Rule 3: matched — token and cost checks on the pair.
			sums.matched = sums.matched.Add(cost)
			sums.matchedIDs[*r.RequestID] = true
			run.MatchedCount++
			agg, ok := sums.aggs[[2]string{r.StrategyID, r.UsageDay}]
			if !ok {
				agg = &aggPair{}
				sums.aggs[[2]string{r.StrategyID, r.UsageDay}] = agg
			}
			agg.clientIn += r.InTokens
			agg.clientOut += r.OutTokens
			agg.gwIn += gw.InputTokens
			agg.gwOut += gw.OutputTokens
			if r.InTokens != gw.InputTokens || r.OutTokens != gw.OutputTokens {
				sums.tokenMismatch = true
				if err := add(ClassMismatchTokens, r.RequestID, r.StrategyID, map[string]any{
					"client_input_tokens": r.InTokens, "client_output_tokens": r.OutTokens,
					"gateway_input_tokens": gw.InputTokens, "gateway_output_tokens": gw.OutputTokens,
				}); err != nil {
					return nil, nil, err
				}
			}
			gwCost, err := decimal.NewFromString(gw.CostUSD)
			if err != nil {
				return nil, nil, fmt.Errorf("gateway cost_usd %q: %w", gw.CostUSD, err)
			}
			if !cost.Equal(gwCost) {
				// EXPECTED on price-table drift: the client cost stays
				// billable; the discrepancy documents the drift.
				if err := add(ClassMismatchCost, r.RequestID, r.StrategyID, map[string]any{
					"client_cost_usd": r.CostUSD, "gateway_cost_usd": gw.CostUSD,
				}); err != nil {
					return nil, nil, err
				}
			}
		}
	}
	return out, sums, nil
}

// gatewayOrphans enumerates the tenant's metering_records with a metered_at
// UTC date inside the period that no client-set row matched (crashes before
// the trace POST, re-drive 409 drops, unparseable-body calls, zero-spend
// 429 rows, overflow-merged calls).
func gatewayOrphans(tx *sql.Tx, reconID, tenantID, periodStart, periodEnd string, matchedIDs map[string]bool) ([]Discrepancy, error) {
	rows, err := tx.Query(`SELECT m.request_id, m.strategy_id, m.model,
			m.input_tokens, m.output_tokens, m.cost_usd, m.metered_at
		FROM metering_records m JOIN strategies s ON s.strategy_id = m.strategy_id
		WHERE s.tenant_id = ? AND substr(m.metered_at, 1, 10) BETWEEN ? AND ?
		ORDER BY m.rowid`, tenantID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Discrepancy
	for rows.Next() {
		var m MeteringRecord
		if err := rows.Scan(&m.RequestID, &m.StrategyID, &m.Model,
			&m.InputTokens, &m.OutputTokens, &m.CostUSD, &m.MeteredAt); err != nil {
			return nil, err
		}
		if matchedIDs[m.RequestID] {
			continue
		}
		d, err := newDiscrepancy(reconID, ClassOrphanGateway, &m.RequestID, &m.StrategyID, map[string]any{
			"model": m.Model, "input_tokens": m.InputTokens, "output_tokens": m.OutputTokens,
			"cost_usd": m.CostUSD, "metered_at": m.MeteredAt,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// insertReconciliation appends the run row and its discrepancies.
func insertReconciliation(tx *sql.Tx, run ReconciliationRun, discrepancies []Discrepancy) error {
	if _, err := tx.Exec(`INSERT INTO reconciliation_runs
		(recon_id, tenant_id, period, invoice_id, status, matched_count, discrepancy_count,
		 matched_client_cost_usd, orphan_client_cost_usd, estimated_client_cost_usd,
		 unattributed_client_cost_usd, invoice_total_usd, run_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ReconID, run.TenantID, run.Period, run.InvoiceID, run.Status,
		run.MatchedCount, run.DiscrepancyCount, run.MatchedClientCostUSD,
		run.OrphanClientCostUSD, run.EstimatedClientCostUSD,
		run.UnattributedClientCostUSD, run.InvoiceTotalUSD, run.RunAt); err != nil {
		return err
	}
	for _, d := range discrepancies {
		if _, err := tx.Exec(`INSERT INTO discrepancies
			(discrepancy_id, recon_id, class, request_id, strategy_id, details_json)
			VALUES (?, ?, ?, ?, ?, ?)`,
			d.DiscrepancyID, d.ReconID, d.Class, d.RequestID, d.StrategyID, d.DetailsJSON); err != nil {
			return err
		}
	}
	return nil
}

const reconSelect = `SELECT recon_id, tenant_id, period, invoice_id, status,
	matched_count, discrepancy_count, matched_client_cost_usd, orphan_client_cost_usd,
	estimated_client_cost_usd, unattributed_client_cost_usd, invoice_total_usd, run_at
	FROM reconciliation_runs`

func scanReconciliation(row rowScanner) (ReconciliationRun, error) {
	var r ReconciliationRun
	err := row.Scan(&r.ReconID, &r.TenantID, &r.Period, &r.InvoiceID, &r.Status,
		&r.MatchedCount, &r.DiscrepancyCount, &r.MatchedClientCostUSD, &r.OrphanClientCostUSD,
		&r.EstimatedClientCostUSD, &r.UnattributedClientCostUSD, &r.InvoiceTotalUSD, &r.RunAt)
	return r, err
}

// ListReconciliations returns one page of reconciliation runs plus the
// tenant-scoped total; tenantID "" lists every tenant (platform classes).
func (s *Store) ListReconciliations(tenantID string, page, limit int) ([]ReconciliationRun, int, error) {
	page, limit = normalizePage(page, limit)
	where, args := "", []any{}
	if tenantID != "" {
		where, args = " WHERE tenant_id = ?", []any{tenantID}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reconciliation_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(reconSelect+where+` ORDER BY run_at, recon_id LIMIT ? OFFSET ?`,
		append(args, limit, (page-1)*limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ReconciliationRun
	for rows.Next() {
		r, err := scanReconciliation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// GetReconciliation returns one run with its discrepancies in stored order.
// tenantID "" resolves any tenant; a foreign or absent recon_id is
// ErrNotFound — indistinguishable from absence (no cross-tenant oracle).
func (s *Store) GetReconciliation(reconID, tenantID string) (ReconciliationRun, []Discrepancy, error) {
	where, args := ` WHERE recon_id = ?`, []any{reconID}
	if tenantID != "" {
		where, args = where+` AND tenant_id = ?`, append(args, tenantID)
	}
	r, err := scanReconciliation(s.db.QueryRow(reconSelect+where, args...))
	if err == sql.ErrNoRows {
		return ReconciliationRun{}, nil, fmt.Errorf("reconciliation %s: %w", reconID, ErrNotFound)
	}
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	rows, err := s.db.Query(`SELECT discrepancy_id, recon_id, class, request_id, strategy_id, details_json
		FROM discrepancies WHERE recon_id = ? ORDER BY rowid`, reconID)
	if err != nil {
		return ReconciliationRun{}, nil, err
	}
	defer rows.Close()
	var ds []Discrepancy
	for rows.Next() {
		var d Discrepancy
		var reqID, strategyID sql.NullString
		if err := rows.Scan(&d.DiscrepancyID, &d.ReconID, &d.Class, &reqID, &strategyID, &d.DetailsJSON); err != nil {
			return ReconciliationRun{}, nil, err
		}
		d.RequestID = nullable(reqID)
		d.StrategyID = nullable(strategyID)
		ds = append(ds, d)
	}
	return r, ds, rows.Err()
}
