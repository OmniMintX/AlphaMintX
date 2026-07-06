package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// analysisBody is a valid POST /api/v1/market/llm-analysis body; tests
// mutate single fields for the validation table.
func analysisBody() map[string]any {
	return map[string]any{
		"symbol": "BTCUSDT", "market": "spot", "interval": "1h",
		"locale": "en", "summary": "close 50000, RSI 55, MACD flat",
	}
}

// seedLLMSecret stores an llm secret pointing base_url at baseURL.
func seedLLMSecret(t *testing.T, e *testEnv, baseURL string) {
	t.Helper()
	rec := e.do(t, "POST", "/api/v1/platform/secrets/llm", adminTok, map[string]any{
		"base_url": baseURL, "api_key": testLLMKey,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("set llm = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestMarketAnalysisNotConfigured: before the first llm-secret set the
// route answers 404 NOT_CONFIGURED (the agent llm-config precedent).
func TestMarketAnalysisNotConfigured(t *testing.T) {
	e, _ := vaultEnv(t)
	wantError(t, e.do(t, "POST", "/api/v1/market/llm-analysis", readTok, analysisBody()),
		404, codeNotConfigured)
}

// TestMarketAnalysisValidation: the pinned value rules — symbol pattern,
// enumerated market/interval/locale, 1..4000-char summary — are each 400.
func TestMarketAnalysisValidation(t *testing.T) {
	e, _ := vaultEnv(t)
	tests := []struct {
		name, field string
		value       any
	}{
		{"bad symbol", "symbol", "btc"},
		{"bad market", "market", "margin"},
		{"bad interval", "interval", "5m"},
		{"bad locale", "locale", "fr"},
		{"empty summary", "summary", ""},
		{"oversized summary", "summary", strings.Repeat("x", analysisSummaryMaxChars+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := analysisBody()
			body[tc.field] = tc.value
			wantError(t, e.do(t, "POST", "/api/v1/market/llm-analysis", readTok, body),
				400, codeSchemaInvalid)
		})
	}
}

// TestMarketAnalysisHappyPath: the upstream sees the stored bearer and the
// default (analyst) model; the reader tier — env read token AND a DB viewer
// token — gets 200 with the completion text, the key never appears in the
// response, and no bearer is 401.
func TestMarketAnalysisHappyPath(t *testing.T) {
	e, _ := vaultEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testLLMKey {
			t.Errorf("upstream Authorization = %q, want the stored bearer", got)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		var req chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if req.Model != "gpt-4o-mini" {
			t.Errorf("upstream model = %q, want the default_model gpt-4o-mini", req.Model)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"role": "assistant", "content": "Trend: up. Bias: bullish.",
			}}},
		})
	}))
	defer upstream.Close()
	seedLLMSecret(t, e, upstream.URL)

	createTenant(t, e.store, "tenant-1")
	viewer := seedUserToken(t, e.store, "tenant-1", RoleViewer, "db-viewer-token")
	for _, tok := range []string{readTok, viewer} {
		rec := e.do(t, "POST", "/api/v1/market/llm-analysis", tok, analysisBody())
		if rec.Code != http.StatusOK {
			t.Fatalf("analysis = %d (body %q), want 200", rec.Code, rec.Body.String())
		}
		var got marketAnalysisResponse
		decodeJSON(t, rec, &got)
		if got.Text != "Trend: up. Bias: bullish." || got.Model != "gpt-4o-mini" {
			t.Fatalf("analysis = %+v, want the completion text and model", got)
		}
		if strings.Contains(rec.Body.String(), testLLMKey) {
			t.Error("analysis response leaks the llm api_key")
		}
	}
	wantError(t, e.do(t, "POST", "/api/v1/market/llm-analysis", "", analysisBody()),
		401, codeUnauthorized)
}

// TestMarketAnalysisUpstreamErrors: a non-200 upstream is 502 LLM_UPSTREAM
// with the status only (the body is never echoed), and so is a completion
// without content.
func TestMarketAnalysisUpstreamErrors(t *testing.T) {
	e, _ := vaultEnv(t)
	status, body := http.StatusInternalServerError, `{"error":"upstream-detail-must-not-echo"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()
	seedLLMSecret(t, e, upstream.URL)

	rec := e.do(t, "POST", "/api/v1/market/llm-analysis", readTok, analysisBody())
	wantError(t, rec, 502, codeLLMUpstream)
	if !strings.Contains(rec.Body.String(), "500") || strings.Contains(rec.Body.String(), "upstream-detail") {
		t.Fatalf("502 body = %q, want the status without the upstream body", rec.Body.String())
	}

	status, body = http.StatusOK, `{"choices":[]}`
	wantError(t, e.do(t, "POST", "/api/v1/market/llm-analysis", readTok, analysisBody()),
		502, codeLLMUpstream)
}

// TestMarketAnalysisBaseURLV1Suffix: OpenAI-convention base URLs already end
// in /v1; the handler must not double the version segment (/v1/v1).
func TestMarketAnalysisBaseURLV1Suffix(t *testing.T) {
	e, _ := vaultEnv(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"role": "assistant", "content": "ok",
			}}},
		})
	}))
	defer upstream.Close()
	seedLLMSecret(t, e, upstream.URL+"/v1")

	rec := e.do(t, "POST", "/api/v1/market/llm-analysis", readTok, analysisBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("analysis = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
}
