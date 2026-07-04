package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Billing and metering persistence (docs/specs/billing-and-metering.md):
// metering_records, billing_periods, invoices, invoice_lines,
// reconciliation_runs, and discrepancies are ALL append-only (invariant 7)
// — INSERT methods only, no mutators, ever. Money is decimal-as-string
// (ADR-0003): computed OUTPUTS are shopspring-normalized, stored INPUTS
// keep their original strings verbatim.

// Invoice line entry types (invoice_lines.entry_type CHECK constraint).
// credit_note is pinned but DEFERRED (v1.1): v1 emits no credit notes.
const (
	EntryTypeUsage      = "usage"
	EntryTypeCarryOver  = "carry_over"
	EntryTypeCreditNote = "credit_note"
)

// MeteringRecord mirrors the append-only metering_records table: one
// gateway spend-log row, strategy RESOLVED by the caller (directly or via
// alias_map) — the record's tenant derives from strategies.tenant_id,
// never from import input.
type MeteringRecord struct {
	RecordID     string `json:"record_id"`
	Source       string `json:"source"`
	RequestID    string `json:"request_id"`
	StrategyID   string `json:"strategy_id"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	CostUSD      string `json:"cost_usd"`
	MeteredAt    string `json:"metered_at"`
	ImportedAt   string `json:"imported_at"`
}

// BillingPeriod mirrors the append-only billing_periods table; absence of a
// row means the (tenant, period) is open, its insertion IS the close.
type BillingPeriod struct {
	PeriodID       string `json:"period_id"`
	TenantID       string `json:"tenant_id"`
	Period         string `json:"period"`
	PeriodStart    string `json:"period_start"`
	PeriodEnd      string `json:"period_end"`
	Status         string `json:"status"`
	ClosedAt       string `json:"closed_at"`
	WatermarkRowid int64  `json:"watermark_rowid"`
}

// Invoice mirrors the append-only invoices table (immutable once written;
// corrections are future credit_note lines, never edits).
type Invoice struct {
	InvoiceID   string `json:"invoice_id"`
	TenantID    string `json:"tenant_id"`
	Period      string `json:"period"`
	TotalUSD    string `json:"total_usd"`
	LineCount   int    `json:"line_count"`
	GeneratedAt string `json:"generated_at"`
}

// InvoiceLine mirrors the append-only invoice_lines table. OriginalPeriod
// is non-nil iff EntryType == carry_over.
type InvoiceLine struct {
	LineID         string  `json:"line_id"`
	InvoiceID      string  `json:"invoice_id"`
	StrategyID     string  `json:"strategy_id"`
	Model          string  `json:"model"`
	EntryType      string  `json:"entry_type"`
	OriginalPeriod *string `json:"original_period"`
	InputTokens    int     `json:"input_tokens"`
	OutputTokens   int     `json:"output_tokens"`
	AmountUSD      string  `json:"amount_usd"`
}

// PeriodBounds returns the inclusive first/last UTC dates of a YYYY-MM
// billing period (a UTC calendar month per tenant).
func PeriodBounds(period string) (string, string, error) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return "", "", fmt.Errorf("invalid period %q: %w", period, err)
	}
	start := t.UTC()
	end := start.AddDate(0, 1, -1)
	return start.Format("2006-01-02"), end.Format("2006-01-02"), nil
}

// InsertMeteringRecords imports one gateway-export batch atomically
// (billing-and-metering.md §Metering ingest). Idempotent per record: an
// existing request_id with identical content (cost_usd by DECIMAL VALUE,
// the other fields by byte equality; source/imported_at excluded) is a
// no-op; different content returns ErrMeteringConflict and NOTHING of the
// batch persists. The caller validates records and resolves strategies;
// the store assigns record_id and imported_at. Record validation (decimal
// shape, non-negative tokens, timestamps) is the API handler's
// responsibility — the store trusts its caller.
func (s *Store) InsertMeteringRecords(source string, records []MeteringRecord, now time.Time) (imported, skipped int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer rollback(tx)
	importedAt := formatTime(now)
	for _, rec := range records {
		var stored MeteringRecord
		err := tx.QueryRow(`SELECT strategy_id, model, input_tokens, output_tokens, cost_usd, metered_at
			FROM metering_records WHERE request_id = ?`, rec.RequestID).
			Scan(&stored.StrategyID, &stored.Model, &stored.InputTokens, &stored.OutputTokens,
				&stored.CostUSD, &stored.MeteredAt)
		switch {
		case err == nil:
			same, err := meteringContentEqual(stored, rec)
			if err != nil {
				return 0, 0, err
			}
			if !same {
				return 0, 0, fmt.Errorf("request %s: %w", rec.RequestID, ErrMeteringConflict)
			}
			skipped++
			continue
		case err != sql.ErrNoRows:
			return 0, 0, err
		}
		if _, err := tx.Exec(`INSERT INTO metering_records
			(record_id, source, request_id, strategy_id, model, input_tokens, output_tokens, cost_usd, metered_at, imported_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), source, rec.RequestID, rec.StrategyID, rec.Model,
			rec.InputTokens, rec.OutputTokens, rec.CostUSD, rec.MeteredAt, importedAt); err != nil {
			return 0, 0, err
		}
		imported++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return imported, skipped, nil
}

// meteringContentEqual pins "identical" for idempotent re-imports:
// cost_usd compares by decimal value ("0.50" == "0.5000"); strategy_id,
// model, token counts, and metered_at compare byte-for-byte.
func meteringContentEqual(stored, rec MeteringRecord) (bool, error) {
	storedCost, err := decimal.NewFromString(stored.CostUSD)
	if err != nil {
		return false, fmt.Errorf("stored cost_usd %q: %w", stored.CostUSD, err)
	}
	recCost, err := decimal.NewFromString(rec.CostUSD)
	if err != nil {
		return false, fmt.Errorf("cost_usd %q: %w", rec.CostUSD, err)
	}
	return stored.StrategyID == rec.StrategyID && stored.Model == rec.Model &&
		stored.InputTokens == rec.InputTokens && stored.OutputTokens == rec.OutputTokens &&
		stored.MeteredAt == rec.MeteredAt && storedCost.Equal(recCost), nil
}

// costRow is one billable model_costs row inside a close's rowid window,
// carrying the reconciliation columns too so ClosePeriod and Reconcile
// share the same window query.
type costRow struct {
	RequestID   *string
	IsEstimated bool
	StrategyID  string
	Model       string
	InTokens    int
	OutTokens   int
	CostUSD     string
	UsageDay    string // UTC date of the run's agent_traces.started_at
}

// windowRows reads the tenant's billable model_costs rows in the rowid
// window (prev, watermark] — the SOLE billing partition (billing spec
// §Billing). The usage day is the run's started_at UTC date (the ledger's
// attribution, llm-routing §4), never recorded_at. Ordered by rowid so
// downstream output is deterministic.
func windowRows(tx *sql.Tx, tenantID string, prev, watermark int64) ([]costRow, error) {
	rows, err := tx.Query(`SELECT mc.request_id, mc.is_estimated, mc.strategy_id, mc.model,
			mc.input_tokens, mc.output_tokens, mc.cost_usd, substr(t.started_at, 1, 10)
		FROM model_costs mc
		JOIN agent_traces t ON t.run_id = mc.run_id
		JOIN strategies s ON s.strategy_id = mc.strategy_id
		WHERE s.tenant_id = ? AND mc.rowid > ? AND mc.rowid <= ?
		ORDER BY mc.rowid`, tenantID, prev, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []costRow
	for rows.Next() {
		var r costRow
		var reqID sql.NullString
		if err := rows.Scan(&reqID, &r.IsEstimated, &r.StrategyID, &r.Model,
			&r.InTokens, &r.OutTokens, &r.CostUSD, &r.UsageDay); err != nil {
			return nil, err
		}
		r.RequestID = nullable(reqID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// requireTenant answers ErrNotFound for an absent tenant inside a billing
// transaction (404 UNKNOWN_TENANT at the API).
func requireTenant(tx *sql.Tx, tenantID string) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tenants WHERE tenant_id = ?`, tenantID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("tenant %s: %w", tenantID, ErrNotFound)
	}
	return nil
}

// ClosePeriod closes the (tenant, period) UTC-month billing period: it
// inserts the billing_periods row, the invoice, and all invoice_lines in
// ONE transaction (billing spec §Billing, §Invoices). ErrNotFound for an
// unknown tenant; ErrPeriodClosed when already closed. The billable set is
// the rowid window (prev tenant watermark, MAX(model_costs.rowid)] — every
// row billed exactly once, whatever calendar order the closes run in. The
// API validates the period shape and the period_end < today precondition.
func (s *Store) ClosePeriod(tenantID, period string, now time.Time) (Invoice, []InvoiceLine, error) {
	periodStart, periodEnd, err := PeriodBounds(period)
	if err != nil {
		return Invoice{}, nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Invoice{}, nil, err
	}
	defer rollback(tx)
	if err := requireTenant(tx, tenantID); err != nil {
		return Invoice{}, nil, err
	}
	var closed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM billing_periods
		WHERE tenant_id = ? AND period = ?`, tenantID, period).Scan(&closed); err != nil {
		return Invoice{}, nil, err
	}
	if closed > 0 {
		return Invoice{}, nil, fmt.Errorf("period %s %s: %w", tenantID, period, ErrPeriodClosed)
	}
	var watermark, prev int64
	// Implicit rowid as window key: control.db is never VACUUMed — VACUUM
	// could renumber rowids and corrupt stored windows (billing spec §Billing).
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM model_costs`).Scan(&watermark); err != nil {
		return Invoice{}, nil, err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(watermark_rowid), 0) FROM billing_periods
		WHERE tenant_id = ?`, tenantID).Scan(&prev); err != nil {
		return Invoice{}, nil, err
	}
	costs, err := windowRows(tx, tenantID, prev, watermark)
	if err != nil {
		return Invoice{}, nil, err
	}
	invoice, lines, err := buildInvoice(tenantID, period, costs, formatTime(now))
	if err != nil {
		return Invoice{}, nil, err
	}
	if _, err := tx.Exec(`INSERT INTO billing_periods
		(period_id, tenant_id, period, period_start, period_end, status, closed_at, watermark_rowid)
		VALUES (?, ?, ?, ?, ?, 'closed', ?, ?)`,
		uuid.NewString(), tenantID, period, periodStart, periodEnd, formatTime(now), watermark); err != nil {
		return Invoice{}, nil, err
	}
	if _, err := tx.Exec(`INSERT INTO invoices
		(invoice_id, tenant_id, period, total_usd, line_count, generated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		invoice.InvoiceID, invoice.TenantID, invoice.Period, invoice.TotalUSD,
		invoice.LineCount, invoice.GeneratedAt); err != nil {
		return Invoice{}, nil, err
	}
	for _, l := range lines {
		if _, err := tx.Exec(`INSERT INTO invoice_lines
			(line_id, invoice_id, strategy_id, model, entry_type, original_period, input_tokens, output_tokens, amount_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.LineID, l.InvoiceID, l.StrategyID, l.Model, l.EntryType, l.OriginalPeriod,
			l.InputTokens, l.OutputTokens, l.AmountUSD); err != nil {
			return Invoice{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Invoice{}, nil, err
	}
	return invoice, lines, nil
}

// buildInvoice aggregates the billable rows into deterministic invoice
// lines: one line per (strategy_id, model, entry_type[, original_period]),
// entry_type = usage iff the row's usage day ∈ the period, else carry_over
// labeled with the usage day's own period. Order: entry_type (usage <
// carry_over < credit_note), then strategy_id, model, original_period, all
// ascending; line_id = "{invoice_id}#{n}" (0-based). Closing the same DB
// state twice would emit byte-identical lines. NO ROUNDING: amounts and the
// total are exact shopspring sums, serialized normalized ("0" for zero).
func buildInvoice(tenantID, period string, costs []costRow, generatedAt string) (Invoice, []InvoiceLine, error) {
	type lineKey struct {
		strategyID, model, entryType, originalPeriod string
	}
	type lineSum struct {
		inTokens, outTokens int
		amount              decimal.Decimal
	}
	sums := map[lineKey]*lineSum{}
	for _, r := range costs {
		key := lineKey{r.StrategyID, r.Model, EntryTypeUsage, ""}
		if usagePeriod := r.UsageDay[:7]; usagePeriod != period {
			key.entryType, key.originalPeriod = EntryTypeCarryOver, usagePeriod
		}
		cost, err := decimal.NewFromString(r.CostUSD)
		if err != nil {
			return Invoice{}, nil, fmt.Errorf("cost_usd %q: %w", r.CostUSD, err)
		}
		sum, ok := sums[key]
		if !ok {
			sum = &lineSum{}
			sums[key] = sum
		}
		sum.inTokens += r.InTokens
		sum.outTokens += r.OutTokens
		sum.amount = sum.amount.Add(cost)
	}
	keys := make([]lineKey, 0, len(sums))
	for k := range sums {
		keys = append(keys, k)
	}
	rank := map[string]int{EntryTypeUsage: 0, EntryTypeCarryOver: 1, EntryTypeCreditNote: 2}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if rank[a.entryType] != rank[b.entryType] {
			return rank[a.entryType] < rank[b.entryType]
		}
		if a.strategyID != b.strategyID {
			return a.strategyID < b.strategyID
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.originalPeriod < b.originalPeriod
	})
	// invoice_id is deterministic from the natural key; UNIQUE
	// (tenant_id, period) makes a second invoice unrepresentable.
	invoiceID := "inv-" + tenantID + "-" + period
	total := decimal.Zero
	lines := make([]InvoiceLine, 0, len(keys))
	for n, k := range keys {
		sum := sums[k]
		line := InvoiceLine{
			LineID: fmt.Sprintf("%s#%d", invoiceID, n), InvoiceID: invoiceID,
			StrategyID: k.strategyID, Model: k.model, EntryType: k.entryType,
			InputTokens: sum.inTokens, OutputTokens: sum.outTokens,
			AmountUSD: sum.amount.String(),
		}
		if k.entryType == EntryTypeCarryOver {
			op := k.originalPeriod
			line.OriginalPeriod = &op
		}
		lines = append(lines, line)
		total = total.Add(sum.amount)
	}
	return Invoice{InvoiceID: invoiceID, TenantID: tenantID, Period: period,
		TotalUSD: total.String(), LineCount: len(lines), GeneratedAt: generatedAt}, lines, nil
}

const invoiceSelect = `SELECT invoice_id, tenant_id, period, total_usd, line_count, generated_at FROM invoices`

// ListInvoices returns one page of invoices plus the tenant-scoped total.
// tenantID "" lists every tenant (platform env classes, multi-tenant-rbac.md
// §Principals); tenant principals always pass their own tenant.
func (s *Store) ListInvoices(tenantID string, page, limit int) ([]Invoice, int, error) {
	page, limit = normalizePage(page, limit)
	where, args := "", []any{}
	if tenantID != "" {
		where, args = " WHERE tenant_id = ?", []any{tenantID}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM invoices`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(invoiceSelect+where+` ORDER BY tenant_id, period LIMIT ? OFFSET ?`,
		append(args, limit, (page-1)*limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.InvoiceID, &inv.TenantID, &inv.Period,
			&inv.TotalUSD, &inv.LineCount, &inv.GeneratedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, inv)
	}
	return out, total, rows.Err()
}

// GetInvoice returns one invoice with its lines in stored (deterministic)
// order. tenantID "" resolves any tenant; a foreign or absent invoice_id is
// ErrNotFound — indistinguishable from absence (no cross-tenant oracle).
func (s *Store) GetInvoice(invoiceID, tenantID string) (Invoice, []InvoiceLine, error) {
	where, args := ` WHERE invoice_id = ?`, []any{invoiceID}
	if tenantID != "" {
		where, args = where+` AND tenant_id = ?`, append(args, tenantID)
	}
	var inv Invoice
	err := s.db.QueryRow(invoiceSelect+where, args...).Scan(&inv.InvoiceID, &inv.TenantID,
		&inv.Period, &inv.TotalUSD, &inv.LineCount, &inv.GeneratedAt)
	if err == sql.ErrNoRows {
		return Invoice{}, nil, fmt.Errorf("invoice %s: %w", invoiceID, ErrNotFound)
	}
	if err != nil {
		return Invoice{}, nil, err
	}
	rows, err := s.db.Query(`SELECT line_id, invoice_id, strategy_id, model, entry_type,
			original_period, input_tokens, output_tokens, amount_usd
		FROM invoice_lines WHERE invoice_id = ? ORDER BY rowid`, invoiceID)
	if err != nil {
		return Invoice{}, nil, err
	}
	defer rows.Close()
	var lines []InvoiceLine
	for rows.Next() {
		var l InvoiceLine
		var op sql.NullString
		if err := rows.Scan(&l.LineID, &l.InvoiceID, &l.StrategyID, &l.Model, &l.EntryType,
			&op, &l.InputTokens, &l.OutputTokens, &l.AmountUSD); err != nil {
			return Invoice{}, nil, err
		}
		l.OriginalPeriod = nullable(op)
		lines = append(lines, l)
	}
	return inv, lines, rows.Err()
}
