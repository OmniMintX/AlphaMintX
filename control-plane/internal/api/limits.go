package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/riskgate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Runtime-changeable RiskLimits fields, the v1 whitelist
// (multi-tenant-rbac.md §Runtime limit changes). symbol_whitelist and
// require_stop_loss are NOT runtime-changeable in v1; accounting_quote is
// NEVER changeable.
const (
	fieldMaxOpenPositions            = "max_open_positions"
	fieldMaxOrdersPerMinute          = "max_orders_per_minute"
	fieldPerPositionNotionalCapQuote = "per_position_notional_cap_quote"
	fieldDailyLossLimitQuote         = "daily_loss_limit_quote"
	fieldMaxLossAtStopQuote          = "max_loss_at_stop_quote"
)

// limitsOverlay is one strategy's persisted overrides: nil fields fall
// through to the startup base.
type limitsOverlay struct {
	maxOpenPositions            *int
	maxOrdersPerMinute          *int
	perPositionNotionalCapQuote *decimal.Decimal
	dailyLossLimitQuote         *decimal.Decimal
	maxLossAtStopQuote          *decimal.Decimal
}

// LimitsProvider is the ONLY read path for effective limits
// (multi-tenant-rbac.md §Runtime limit changes): the risk-gate call site,
// the approval preflight daily-loss check, and any future consumer read it.
// Snapshots are immutable maps swapped atomically — a reader sees either
// the old or the new limits, never a torn set.
type LimitsProvider struct {
	base riskgate.RiskLimits
	mu   sync.Mutex // serializes writers; readers only Load the snapshot
	snap atomic.Pointer[map[string]limitsOverlay]
}

// newBaseLimitsProvider wraps the startup base with an empty overlay (test
// and replay wiring; serve mode hydrates with NewLimitsProvider).
func newBaseLimitsProvider(base riskgate.RiskLimits) *LimitsProvider {
	p := &LimitsProvider{base: base}
	m := map[string]limitsOverlay{}
	p.snap.Store(&m)
	return p
}

// NewLimitsProvider hydrates the effective-limits overlay from the
// persisted risk_limit_changes rows in rowid order — the normative replay
// order, last write wins — so the overlay ALWAYS beats the startup base,
// including after an env-config change plus restart.
func NewLimitsProvider(st *store.Store, base riskgate.RiskLimits) (*LimitsProvider, error) {
	p := newBaseLimitsProvider(base)
	rows, err := st.RiskLimitChanges()
	if err != nil {
		return nil, err
	}
	m := map[string]limitsOverlay{}
	for _, c := range rows {
		ov := m[c.StrategyID]
		if err := applyLimitChange(&ov, c.Field, c.NewValue); err != nil {
			return nil, fmt.Errorf("risk_limit_changes %s: %w", c.ChangeID, err)
		}
		m[c.StrategyID] = ov
	}
	p.snap.Store(&m)
	return p, nil
}

// Limits returns the effective RiskLimits for a strategy: the startup base
// overlaid with the latest persisted change per whitelisted field.
func (p *LimitsProvider) Limits(strategyID string) riskgate.RiskLimits {
	limits := p.base
	ov, ok := (*p.snap.Load())[strategyID]
	if !ok {
		return limits
	}
	if ov.maxOpenPositions != nil {
		limits.MaxOpenPositions = *ov.maxOpenPositions
	}
	if ov.maxOrdersPerMinute != nil {
		limits.MaxOrdersPerMinute = *ov.maxOrdersPerMinute
	}
	if ov.perPositionNotionalCapQuote != nil {
		limits.PerPositionNotionalCapQuote = *ov.perPositionNotionalCapQuote
	}
	if ov.dailyLossLimitQuote != nil {
		limits.DailyLossLimitQuote = *ov.dailyLossLimitQuote
	}
	if ov.maxLossAtStopQuote != nil {
		limits.MaxLossAtStopQuote = *ov.maxLossAtStopQuote
	}
	return limits
}

// apply installs pre-validated changes for a strategy by swapping in a new
// immutable snapshot (callers persist the audit rows FIRST).
func (p *LimitsProvider) apply(strategyID string, changes map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := *p.snap.Load()
	m := make(map[string]limitsOverlay, len(old)+1)
	for k, v := range old {
		m[k] = v
	}
	ov := m[strategyID]
	for field, value := range changes {
		if err := applyLimitChange(&ov, field, value); err != nil {
			return err
		}
	}
	m[strategyID] = ov
	p.snap.Store(&m)
	return nil
}

// applyLimitChange validates ONE whitelisted field/value pair — the same
// code path for the endpoint, the snapshot swap, and startup hydration —
// and sets it on the overlay. Decimal values must be in the strict
// ADR-0003 unsigned form (the contract regex: negatives and exponents are
// unrepresentable); ints carry the spec bounds.
func applyLimitChange(ov *limitsOverlay, field, value string) error {
	switch field {
	case fieldMaxOpenPositions:
		n, err := parseLimitInt(field, value, 0)
		if err != nil {
			return err
		}
		ov.maxOpenPositions = &n
	case fieldMaxOrdersPerMinute:
		n, err := parseLimitInt(field, value, 1)
		if err != nil {
			return err
		}
		ov.maxOrdersPerMinute = &n
	case fieldPerPositionNotionalCapQuote:
		return setLimitDecimal(field, value, &ov.perPositionNotionalCapQuote)
	case fieldDailyLossLimitQuote:
		return setLimitDecimal(field, value, &ov.dailyLossLimitQuote)
	case fieldMaxLossAtStopQuote:
		return setLimitDecimal(field, value, &ov.maxLossAtStopQuote)
	default:
		return fmt.Errorf("field %q is not runtime-changeable", field)
	}
	return nil
}

func parseLimitInt(field, value string, min int) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s %q: not an integer", field, value)
	}
	if n < min {
		return 0, fmt.Errorf("%s %d: must be >= %d", field, n, min)
	}
	return n, nil
}

func setLimitDecimal(field, value string, dst **decimal.Decimal) error {
	d, err := contract.ParseDecimal(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, value, err)
	}
	dd := d.Decimal()
	*dst = &dd
	return nil
}

// limitChangeRequest is the POST body: {"changes": {"<field>": <value>}}.
// Ints are JSON numbers; decimals are decimal strings (ADR-0003).
type limitChangeRequest struct {
	Changes map[string]json.RawMessage `json:"changes"`
}

// limitChangeResponse echoes the appended audit rows.
type limitChangeResponse struct {
	Changes []store.RiskLimitChange `json:"changes"`
}

// handlePostLimits is the runtime limit change (multi-tenant-rbac.md
// §Runtime limit changes): admin/owner own tenant, env-admin any tenant.
// The WHOLE request validates before anything happens (atomic reject); the
// audit rows land in one transaction BEFORE the in-memory snapshot swap.
func (s *Server) handlePostLimits(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(pr, strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	var req limitChangeRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if len(req.Changes) == 0 {
		writeError(w, http.StatusBadRequest, codeInvalidLimitChange, "changes must name at least one field")
		return
	}
	// Validate EVERY field before any effect: one bad entry rejects the
	// whole request (no partial apply).
	normalized := make(map[string]string, len(req.Changes))
	var scratch limitsOverlay
	for field, raw := range req.Changes {
		value, err := normalizeLimitValue(field, raw)
		if err == nil {
			err = applyLimitChange(&scratch, field, value)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, codeInvalidLimitChange, err.Error())
			return
		}
		normalized[field] = value
	}

	// One lock spans read-current + persist + apply: concurrent changes to
	// the same strategy serialize, so the audit old→new chain and the rowid
	// replay order always agree with the in-memory snapshot. The same lock
	// serializes gate evaluations (server.strategyLock), so a proposal never
	// interleaves between the audit append and the snapshot swap.
	lock := s.strategyLock(strategyID)
	lock.Lock()
	defer lock.Unlock()
	now, current := s.cfg.Now(), s.limits.Limits(strategyID)
	fields := make([]string, 0, len(normalized))
	for field := range normalized {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	changes := make([]store.RiskLimitChange, 0, len(fields))
	for _, field := range fields {
		old := currentLimitValue(current, field)
		changes = append(changes, store.RiskLimitChange{
			ChangeID:   uuid.NewString(),
			StrategyID: strategyID,
			Field:      field,
			OldValue:   &old,
			NewValue:   normalized[field],
			ActorID:    s.actorID(pr),
			ChangedAt:  formatTime(now),
		})
	}
	if err := s.cfg.Store.AppendRiskLimitChanges(changes); err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if err := s.limits.apply(strategyID, normalized); err != nil {
		s.writeInternal(w, r, err) // unreachable: values pre-validated above
		return
	}
	writeJSON(w, http.StatusOK, limitChangeResponse{Changes: changes})
}

// normalizeLimitValue converts the request's raw JSON value into the
// canonical persisted string: ints must be JSON integers, decimals must be
// JSON strings (validation proper happens in applyLimitChange).
func normalizeLimitValue(field string, raw json.RawMessage) (string, error) {
	switch field {
	case fieldMaxOpenPositions, fieldMaxOrdersPerMinute:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return "", fmt.Errorf("%s: must be a JSON integer", field)
		}
		return strconv.Itoa(n), nil
	case fieldPerPositionNotionalCapQuote, fieldDailyLossLimitQuote, fieldMaxLossAtStopQuote:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s: must be a decimal string (ADR-0003)", field)
		}
		return v, nil
	default:
		return "", fmt.Errorf("field %q is not runtime-changeable", field)
	}
}

// currentLimitValue renders the audited old value from the effective
// limits at change time.
func currentLimitValue(limits riskgate.RiskLimits, field string) string {
	switch field {
	case fieldMaxOpenPositions:
		return strconv.Itoa(limits.MaxOpenPositions)
	case fieldMaxOrdersPerMinute:
		return strconv.Itoa(limits.MaxOrdersPerMinute)
	case fieldPerPositionNotionalCapQuote:
		return limits.PerPositionNotionalCapQuote.String()
	case fieldDailyLossLimitQuote:
		return limits.DailyLossLimitQuote.String()
	default: // fieldMaxLossAtStopQuote: normalizeLimitValue pinned the set
		return limits.MaxLossAtStopQuote.String()
	}
}
