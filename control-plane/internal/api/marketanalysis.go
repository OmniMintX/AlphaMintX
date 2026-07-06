package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Market LLM analysis: POST /api/v1/market/llm-analysis runs ONE chat
// completion against the admin-saved LLM provider (the sealed "llm" secret)
// over an indicator snapshot the web client built. The web tier never holds
// the key — it stays in-process here; only the model's text answer crosses
// the boundary, and neither the key nor the upstream request is ever logged.

// Request bounds: the enumerated chart dimensions plus the snapshot cap.
const analysisSummaryMaxChars = 4000

var (
	analysisSymbolPattern = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)
	analysisMarkets       = []string{"spot", "futures"}
	analysisIntervals     = []string{"15m", "1h", "4h", "1d"}
	analysisLocales       = []string{"en", "vi"}
)

// marketAnalysisRequest is the POST /api/v1/market/llm-analysis body: the
// chart identity plus the plain-text indicator snapshot to analyze.
type marketAnalysisRequest struct {
	Symbol   string `json:"symbol"`
	Market   string `json:"market"`
	Interval string `json:"interval"`
	Locale   string `json:"locale"`
	Summary  string `json:"summary"`
}

// marketAnalysisResponse is the 200 body: the model's text and which model
// answered — never the provider config.
type marketAnalysisResponse struct {
	Text  string `json:"text"`
	Model string `json:"model"`
}

// Chat wire shapes: the OpenAI-compatible /v1/chat/completions subset this
// handler sends and reads.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// analysisPrompt embeds the system-style instruction in the single user
// message: analyst persona, locale, required structure, an explicit
// BUY/SELL/WAIT call with levels derived only from the snapshot, the
// not-financial-advice caveat, and the no-invented-levels rule.
func analysisPrompt(req marketAnalysisRequest) string {
	language := "English"
	action := "Recommendation: BUY, SELL, or WAIT"
	if req.Locale == "vi" {
		language = "Vietnamese"
		action = "Khuyến nghị: MUA, BÁN, or ĐỨNG NGOÀI"
	}
	return fmt.Sprintf("You are a crypto technical analyst. Analyze the indicator snapshot "+
		"below for %s (%s, %s timeframe). Respond in %s. Structure the analysis as: trend, "+
		"momentum, volatility, support/resistance hints from the data given, and a clear bias "+
		"conclusion (bullish/bearish/neutral). Then finish with a decisive final block:\n"+
		"1. %q — pick exactly one, in bold, with a confidence level (low/medium/high)\n"+
		"2. If BUY or SELL: an entry zone, a stop-loss, and a take-profit level, each derived "+
		"ONLY from levels present in the snapshot (last close, SMA/EMA values, Bollinger bands) "+
		"and each with a one-line justification\n"+
		"3. If WAIT: the specific condition that would flip the call (e.g. an RSI or MACD "+
		"threshold, a reclaim/loss of a moving average in the snapshot)\n"+
		"Include the caveat that this is not financial advice and the deterministic risk gate "+
		"governs all real orders. Do NOT invent price levels that are not derivable from the "+
		"snapshot.\n\nIndicator snapshot:\n%s",
		req.Symbol, req.Market, req.Interval, language, action, req.Summary)
}

// validateAnalysisRequest applies the pinned value rules; false means a 400
// SCHEMA_INVALID was written.
func validateAnalysisRequest(w http.ResponseWriter, req marketAnalysisRequest) bool {
	switch {
	case !analysisSymbolPattern.MatchString(req.Symbol):
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "symbol must match ^[A-Z0-9]{5,20}$")
	case !slices.Contains(analysisMarkets, req.Market):
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "market must be 'spot' or 'futures'")
	case !slices.Contains(analysisIntervals, req.Interval):
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "interval must be one of 15m, 1h, 4h, 1d")
	case !slices.Contains(analysisLocales, req.Locale):
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "locale must be 'en' or 'vi'")
	case req.Summary == "" || len(req.Summary) > analysisSummaryMaxChars:
		writeError(w, http.StatusBadRequest, codeSchemaInvalid, "summary must be 1..4000 characters")
	default:
		return true
	}
	return false
}

// handleMarketLLMAnalysis is POST /api/v1/market/llm-analysis (the standard
// strategy-data reader tier): 404 NOT_CONFIGURED before the first llm-secret
// set; any upstream failure — transport error, non-200, or a contentless
// completion — is 502 LLM_UPSTREAM with status only, never the upstream body.
func (s *Server) handleMarketLLMAnalysis(w http.ResponseWriter, r *http.Request) {
	if !s.vaultReady(w) {
		return
	}
	var req marketAnalysisRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !validateAnalysisRequest(w, req) {
		return
	}
	var cfg llmPayload
	found, err := s.openSecretPayload(secretKindLLM, &cfg)
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, codeNotConfigured, "llm secret is not configured")
		return
	}
	// Payloads sealed before the model/timeout fields existed lack them.
	model := cfg.DefaultModel
	if model == "" {
		model = llmDefaultModelDefault
	}
	timeout := cfg.TimeoutSeconds
	if timeout < llmTimeoutMin {
		timeout = llmTimeoutDefault
	}
	body, err := json.Marshal(chatCompletionRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: analysisPrompt(req)}},
	})
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	// OpenAI-convention base URLs often already end in /v1; avoid /v1/v1.
	base := strings.TrimRight(cfg.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.writeInternal(w, r, err)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(upstream)
	if err != nil {
		// The error may embed the request URL; the key never appears in it,
		// but the log still carries no upstream detail beyond the failure.
		s.cfg.Logf("api: market llm-analysis: llm provider unreachable")
		writeError(w, http.StatusBadGateway, codeLLMUpstream, "llm provider unreachable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.cfg.Logf("api: market llm-analysis: llm provider returned HTTP %d", resp.StatusCode)
		writeError(w, http.StatusBadGateway, codeLLMUpstream,
			fmt.Sprintf("llm provider returned HTTP %d", resp.StatusCode))
		return
	}
	var chat chatCompletionResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&chat); err != nil ||
		len(chat.Choices) == 0 || chat.Choices[0].Message.Content == "" {
		writeError(w, http.StatusBadGateway, codeLLMUpstream, "llm provider returned no completion content")
		return
	}
	writeJSON(w, http.StatusOK, marketAnalysisResponse{Text: chat.Choices[0].Message.Content, Model: model})
}
