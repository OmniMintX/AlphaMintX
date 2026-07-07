package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/papergate"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// Arena (Phase 28): two READ-ONLY surfaces over the paper-gate replay —
// the per-strategy equity curve + stats and the global leaderboard ranked
// by return_pct. Both are O(window fills) per strategy, so like the
// paper-gate GET they charge the per-token 60/min bucket themselves.

// defaultMaxPoints is the equity-curve downsampling default (?max_points).
const defaultMaxPoints = 500

var hundred = decimal.NewFromInt(100)

// equityPointJSON is one curve sample; equity is a signed decimal string.
type equityPointJSON struct {
	TS     string `json:"ts"`
	Equity string `json:"equity"`
}

// performanceStatsJSON renders papergate.CurveStats per ADR-0003:
// decimals as strings, profit_factor null when gross loss is zero,
// last_fill_at null when the window has no fills.
type performanceStatsJSON struct {
	RealizedPnL    string  `json:"realized_pnl"`
	ReturnPct      string  `json:"return_pct"`
	MaxDrawdownPct string  `json:"max_drawdown_pct"`
	ClosedTrades   int     `json:"closed_trades"`
	Wins           int     `json:"wins"`
	Losses         int     `json:"losses"`
	WinRatePct     string  `json:"win_rate_pct"`
	ProfitFactor   *string `json:"profit_factor"`
	FeesPaid       string  `json:"fees_paid"`
	LastFillAt     *string `json:"last_fill_at"`
}

// performanceResponse is the GET .../performance body (web
// strategyPerformanceSchema): window_started_at and model are null when
// absent; equity_curve is [] never null.
type performanceResponse struct {
	StrategyID      string               `json:"strategy_id"`
	WindowStartedAt *string              `json:"window_started_at"`
	EvaluatedAt     string               `json:"evaluated_at"`
	Seed            string               `json:"seed"`
	Model           *string              `json:"model"`
	EquityCurve     []equityPointJSON    `json:"equity_curve"`
	Stats           performanceStatsJSON `json:"stats"`
}

// leaderboardItem is one ranked row (web leaderboardItemSchema).
type leaderboardItem struct {
	Rank           int     `json:"rank"`
	StrategyID     string  `json:"strategy_id"`
	Name           string  `json:"name"`
	TenantID       string  `json:"tenant_id"`
	LifecycleState string  `json:"lifecycle_state"`
	Model          *string `json:"model"`
	Seed           string  `json:"seed"`
	Equity         string  `json:"equity"`
	RealizedPnL    string  `json:"realized_pnl"`
	ReturnPct      string  `json:"return_pct"`
	MaxDrawdownPct string  `json:"max_drawdown_pct"`
	ClosedTrades   int     `json:"closed_trades"`
	WinRatePct     string  `json:"win_rate_pct"`
	ProfitFactor   *string `json:"profit_factor"`
	LastFillAt     *string `json:"last_fill_at"`
}

// leaderboardResponse is the GET /api/v1/arena/leaderboard body; items is
// [] never null.
type leaderboardResponse struct {
	EvaluatedAt string            `json:"evaluated_at"`
	Items       []leaderboardItem `json:"items"`
}

// arenaReplay runs the paper-window replay curve for one strategy from
// persisted rows only (the LC-16 window + LC-18 fill join, papergate
// math). windowOK=false means the gate window fails closed: an empty
// curve and zero stats — never an error.
func (s *Server) arenaReplay(strategyID string) (curve []papergate.CurvePoint, stats papergate.CurveStats, windowStart string, windowOK bool, err error) {
	seed := s.cfg.AllocatedCapitalQuote
	startStr, ok, err := s.cfg.Store.PaperWindowStart(strategyID)
	if err != nil {
		return nil, papergate.CurveStats{}, "", false, err
	}
	if !ok {
		_, stats = papergate.ReplayCurve(nil, seed)
		return nil, stats, "", false, nil
	}
	rows, err := s.cfg.Store.ListPaperGateFills(strategyID, startStr)
	if err != nil {
		return nil, papergate.CurveStats{}, "", false, err
	}
	fills := make([]papergate.Fill, 0, len(rows))
	for _, row := range rows {
		f := papergate.Fill{Symbol: row.Symbol, Side: row.Side, ReduceOnly: row.ReduceOnly, FillTS: row.FillTS}
		if f.QtyBase, err = decimal.NewFromString(row.QtyBase); err != nil {
			return nil, papergate.CurveStats{}, "", false, fmt.Errorf("fills.qty_base %q: %w", row.QtyBase, err)
		}
		if f.FillPrice, err = decimal.NewFromString(row.FillPrice); err != nil {
			return nil, papergate.CurveStats{}, "", false, fmt.Errorf("fills.fill_price %q: %w", row.FillPrice, err)
		}
		if f.FeeQuote, err = decimal.NewFromString(row.FeeQuote); err != nil {
			return nil, papergate.CurveStats{}, "", false, fmt.Errorf("fills.fee_quote %q: %w", row.FeeQuote, err)
		}
		fills = append(fills, f)
	}
	curve, stats = papergate.ReplayCurve(fills, seed)
	return curve, stats, startStr, true, nil
}

// returnPct is realized_pnl / seed x 100; zero when seed <= 0 (the same
// fail-closed edge as the gate's drawdown — never a division by zero).
func returnPct(realizedPnL, seed decimal.Decimal) decimal.Decimal {
	if seed.Sign() <= 0 {
		return decimal.Zero
	}
	return realizedPnL.Div(seed).Mul(hundred)
}

// renderStats converts papergate.CurveStats to the JSON view (ADR-0003
// decimal strings; nulls for the absent profit factor and last fill).
func renderStats(stats papergate.CurveStats, seed decimal.Decimal) performanceStatsJSON {
	out := performanceStatsJSON{
		RealizedPnL:    stats.RealizedPnL.String(),
		ReturnPct:      returnPct(stats.RealizedPnL, seed).String(),
		MaxDrawdownPct: stats.MaxDrawdownPct.String(),
		ClosedTrades:   stats.ClosedTrades,
		Wins:           stats.Wins,
		Losses:         stats.Losses,
		WinRatePct:     stats.WinRatePct.String(),
		FeesPaid:       stats.FeesPaid.String(),
	}
	if stats.ProfitFactor != nil {
		pf := stats.ProfitFactor.String()
		out.ProfitFactor = &pf
	}
	if stats.LastFillAt != "" {
		last := stats.LastFillAt
		out.LastFillAt = &last
	}
	return out
}

// downsample thins the curve to at most maxPoints samples, ALWAYS keeping
// the first and last points; intermediates are picked evenly spaced.
func downsample(points []equityPointJSON, maxPoints int) []equityPointJSON {
	if maxPoints < 2 {
		maxPoints = 2
	}
	if len(points) <= maxPoints {
		return points
	}
	out := make([]equityPointJSON, 0, maxPoints)
	last := len(points) - 1
	for i := 0; i < maxPoints-1; i++ {
		out = append(out, points[i*last/(maxPoints-1)])
	}
	return append(out, points[last])
}

// handleGetPerformance is GET /api/v1/strategies/{id}/performance: the
// paper-window equity curve (?max_points, default 500) plus stats. The
// replay is O(window fills), so like the paper-gate GET it charges the
// per-token 60/min bucket itself (the guard charges POSTs only).
func (s *Server) handleGetPerformance(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	if ok, retryAfter := s.rl.allow(pr.rateKey); !ok {
		writeRateLimited(w, retryAfter, "rate limit exceeded (60 req/min per token)")
		return
	}
	strategyID := r.PathValue("id")
	if _, err := s.rootStrategy(pr, strategyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, codeUnknownStrategy, "unknown strategy")
			return
		}
		s.writeInternal(w, r, err)
		return
	}
	maxPoints := defaultMaxPoints
	if v := r.URL.Query().Get("max_points"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxPoints = n
		}
	}
	curve, stats, windowStart, windowOK, err := s.arenaReplay(strategyID)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	seed := s.cfg.AllocatedCapitalQuote
	resp := performanceResponse{
		StrategyID:  strategyID,
		EvaluatedAt: formatTime(s.cfg.Now()),
		Seed:        seed.String(),
		EquityCurve: []equityPointJSON{},
		Stats:       renderStats(stats, seed),
	}
	if windowOK {
		resp.WindowStartedAt = &windowStart
		// The curve is anchored at the window start with the seed, then
		// one post-fill sample per fill, downsampled to max_points with
		// the anchor and the newest sample always kept.
		points := make([]equityPointJSON, 0, len(curve)+1)
		points = append(points, equityPointJSON{TS: windowStart, Equity: seed.String()})
		for _, p := range curve {
			points = append(points, equityPointJSON{TS: p.TS, Equity: p.Equity.String()})
		}
		resp.EquityCurve = downsample(points, maxPoints)
	}
	model, ok, err := s.cfg.Store.LatestTraderModel(strategyID)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if ok {
		resp.Model = &model
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetLeaderboard is GET /api/v1/arena/leaderboard: every visible
// strategy replayed and ranked by return_pct desc (ties: realized_pnl
// desc, then strategy_id asc). Tenant principals see their own tenant
// only (§Lists: no foreign rows); env classes see the platform. The scan
// is O(strategies x window fills) — the same 60/min self-charge.
func (s *Server) handleGetLeaderboard(w http.ResponseWriter, r *http.Request) {
	pr := principalFrom(r)
	if ok, retryAfter := s.rl.allow(pr.rateKey); !ok {
		writeRateLimited(w, retryAfter, "rate limit exceeded (60 req/min per token)")
		return
	}
	strategies, err := s.allStrategies(pr)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	models, err := s.cfg.Store.LatestTraderModels()
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	seed := s.cfg.AllocatedCapitalQuote
	type ranked struct {
		item      leaderboardItem
		returnPct decimal.Decimal
		realized  decimal.Decimal
	}
	items := make([]ranked, 0, len(strategies))
	for _, st := range strategies {
		_, stats, _, _, err := s.arenaReplay(st.StrategyID)
		if err != nil {
			s.writeInternal(w, r, err)
			return
		}
		js := renderStats(stats, seed)
		it := leaderboardItem{
			StrategyID:     st.StrategyID,
			Name:           st.Name,
			TenantID:       st.TenantID,
			LifecycleState: st.LifecycleState,
			Seed:           seed.String(),
			Equity:         seed.Add(stats.RealizedPnL).String(),
			RealizedPnL:    js.RealizedPnL,
			ReturnPct:      js.ReturnPct,
			MaxDrawdownPct: js.MaxDrawdownPct,
			ClosedTrades:   js.ClosedTrades,
			WinRatePct:     js.WinRatePct,
			ProfitFactor:   js.ProfitFactor,
			LastFillAt:     js.LastFillAt,
		}
		if m, ok := models[st.StrategyID]; ok {
			it.Model = &m
		}
		items = append(items, ranked{item: it, returnPct: returnPct(stats.RealizedPnL, seed), realized: stats.RealizedPnL})
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].returnPct.Equal(items[j].returnPct) {
			return items[i].returnPct.GreaterThan(items[j].returnPct)
		}
		if !items[i].realized.Equal(items[j].realized) {
			return items[i].realized.GreaterThan(items[j].realized)
		}
		return items[i].item.StrategyID < items[j].item.StrategyID
	})
	out := make([]leaderboardItem, len(items))
	for i, it := range items {
		it.item.Rank = i + 1
		out[i] = it.item
	}
	writeJSON(w, http.StatusOK, leaderboardResponse{
		EvaluatedAt: formatTime(s.cfg.Now()),
		Items:       out,
	})
}

// allStrategies pages through the tenant-scoped strategy list to the end
// (the leaderboard ranks the full set, not one page).
func (s *Server) allStrategies(pr principal) ([]store.Strategy, error) {
	var out []store.Strategy
	for pageNum := 1; ; pageNum++ {
		var (
			items []store.Strategy
			total int
			err   error
		)
		if pr.tenantBound() {
			items, total, err = s.cfg.Store.ListStrategiesByTenant(pr.tenantID, pageNum, store.MaxPageLimit)
		} else {
			items, total, err = s.cfg.Store.ListStrategies(pageNum, store.MaxPageLimit)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if len(items) == 0 || len(out) >= total {
			return out, nil
		}
	}
}
