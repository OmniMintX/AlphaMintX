package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
)

// klinesPageLimit is the per-request row cap (spot allows 1000, futures
// 1500; the smaller bound is used for both so pagination is venue-agnostic).
const klinesPageLimit = 1000

// Kline is one closed candle as fetched from Binance. Prices and volume are
// the venue's VERBATIM decimal strings (ADR-0003: never round-tripped
// through float64); OpenTime is the candle open in ms since the Unix epoch.
type Kline struct {
	OpenTime int64
	Open     string
	High     string
	Low      string
	Close    string
	Volume   string
}

// FetchKlines pages the Binance klines REST endpoint (spot GET
// /api/v3/klines, futures GET /fapi/v1/klines) for the canonical symbol over
// [startMS, endMS] (both bounds on candle open_time, inclusive, ms epoch),
// returning candles in ascending open_time order. cfg follows BinanceConfig
// conventions: Market is required, RESTURL/HTTPClient default to production
// values when zero (RESTURL is the CONTROLPLANE_BINANCE_REST_URL override
// seam, market-data.md §Endpoint overrides). Price strings are validated as
// decimals but preserved verbatim.
func FetchKlines(ctx context.Context, cfg BinanceConfig, symbol, interval string, startMS, endMS int64) ([]Kline, error) {
	path := "/api/v3/klines"
	switch cfg.Market {
	case MarketSpot:
		if cfg.RESTURL == "" {
			cfg.RESTURL = spotRESTURL
		}
	case MarketFutures:
		path = "/fapi/v1/klines"
		if cfg.RESTURL == "" {
			cfg.RESTURL = futuresRESTURL
		}
	default:
		return nil, fmt.Errorf("marketdata: unknown market %q", cfg.Market)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if startMS > endMS {
		return nil, fmt.Errorf("marketdata: klines window start %d > end %d", startMS, endMS)
	}
	venue, err := ToBinance(symbol)
	if err != nil {
		return nil, err
	}

	var out []Kline
	cursor := startMS
	for {
		page, err := fetchKlinesPage(ctx, cfg.HTTPClient, cfg.RESTURL+path, venue, interval, cursor, endMS)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return out, nil
		}
		for _, k := range page {
			if k.OpenTime < cursor {
				return nil, fmt.Errorf("marketdata: klines %s %s: open_time %d out of order (cursor %d)", symbol, interval, k.OpenTime, cursor)
			}
			if k.OpenTime > endMS {
				return out, nil
			}
			out = append(out, k)
		}
		last := page[len(page)-1].OpenTime
		if len(page) < klinesPageLimit || last >= endMS {
			return out, nil
		}
		cursor = last + 1
	}
}

// fetchKlinesPage performs one paginated request and parses the mixed-type
// row arrays ([open_time, open, high, low, close, volume, close_time, ...]),
// keeping the price/volume strings verbatim.
func fetchKlinesPage(ctx context.Context, client *http.Client, endpoint, venue, interval string, startMS, endMS int64) ([]Kline, error) {
	q := url.Values{}
	q.Set("symbol", venue)
	q.Set("interval", interval)
	q.Set("startTime", strconv.FormatInt(startMS, 10))
	q.Set("endTime", strconv.FormatInt(endMS, 10))
	q.Set("limit", strconv.Itoa(klinesPageLimit))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("marketdata: klines %s %s: status %d", venue, interval, resp.StatusCode)
	}
	var rows [][]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("marketdata: klines %s %s: %w", venue, interval, err)
	}
	out := make([]Kline, 0, len(rows))
	for i, row := range rows {
		if len(row) < 6 {
			return nil, fmt.Errorf("marketdata: klines %s %s row %d: %d fields (want >= 6)", venue, interval, i, len(row))
		}
		var k Kline
		if err := json.Unmarshal(row[0], &k.OpenTime); err != nil {
			return nil, fmt.Errorf("marketdata: klines %s %s row %d open_time: %w", venue, interval, i, err)
		}
		fields := []*string{&k.Open, &k.High, &k.Low, &k.Close, &k.Volume}
		names := []string{"open", "high", "low", "close", "volume"}
		for j, dst := range fields {
			if err := json.Unmarshal(row[j+1], dst); err != nil {
				return nil, fmt.Errorf("marketdata: klines %s %s row %d %s: %w", venue, interval, i, names[j], err)
			}
			// Strict ADR-0003 decimal form (contract regex), matching the
			// dataset validators on both planes.
			if _, err := contract.ParseDecimal(*dst); err != nil {
				return nil, fmt.Errorf("marketdata: klines %s %s row %d %s: %w", venue, interval, i, names[j], err)
			}
		}
		out = append(out, k)
	}
	return out, nil
}
