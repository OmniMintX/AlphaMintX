package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

func TestHealthUnauthenticated(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "GET", "/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	decodeJSON(t, rec, &body)
	if body["status"] != "ok" {
		t.Fatalf("body = %v", body)
	}
}

func TestAuthMatrix(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	cases := []struct {
		name       string
		method     string
		path       string
		token      string
		wantStatus int
		wantCode   string
	}{
		{"get no token", "GET", "/api/v1/strategies", "", 401, codeUnauthorized},
		{"get unknown token", "GET", "/api/v1/strategies", "nope", 401, codeUnauthorized},
		{"get operator token", "GET", "/api/v1/strategies", opTok, 403, codeForbidden},
		{"get agent token", "GET", "/api/v1/strategies", agent1Tok, 403, codeForbidden},
		{"approve read token", "POST", "/api/v1/strategies/" + strat1 + "/approvals", readTok, 403, codeForbidden},
		{"approve agent token", "POST", "/api/v1/strategies/" + strat1 + "/approvals", agent1Tok, 403, codeForbidden},
		{"approve no token", "POST", "/api/v1/strategies/" + strat1 + "/approvals", "", 401, codeUnauthorized},
		{"trace read token", "POST", "/api/v1/strategies/" + strat1 + "/traces", readTok, 403, codeForbidden},
		{"trace operator token", "POST", "/api/v1/strategies/" + strat1 + "/traces", opTok, 403, codeForbidden},
		{"trace out-of-scope token", "POST", "/api/v1/strategies/" + strat1 + "/traces", agent2Tok, 403, codeStrategyScopeMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, e.do(t, tc.method, tc.path, tc.token, nil), tc.wantStatus, tc.wantCode)
		})
	}
}

func TestListStrategies(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.do(t, "GET", "/api/v1/strategies", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var empty struct {
		Items []store.Strategy `json:"items"`
		Total int              `json:"total"`
		Page  int              `json:"page"`
		Limit int              `json:"limit"`
	}
	decodeJSON(t, rec, &empty)
	if empty.Items == nil || len(empty.Items) != 0 || empty.Total != 0 || empty.Page != 1 || empty.Limit != store.DefaultPageLimit {
		t.Fatalf("empty page = %+v", empty)
	}

	createStrategy(t, e.store, strat1, "paper")
	createStrategy(t, e.store, strat2, "live_l1")
	rec = e.do(t, "GET", "/api/v1/strategies?page=0&limit=1000", readTok, nil)
	var pg struct {
		Items []store.Strategy `json:"items"`
		Total int              `json:"total"`
		Page  int              `json:"page"`
		Limit int              `json:"limit"`
	}
	decodeJSON(t, rec, &pg)
	if len(pg.Items) != 2 || pg.Total != 2 || pg.Page != 1 || pg.Limit != store.MaxPageLimit {
		t.Fatalf("page = %+v", pg)
	}
}

func TestGetStrategy(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "live_l1")

	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+uid(9), readTok, nil), 404, codeUnknownStrategy)

	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var m map[string]json.RawMessage
	decodeJSON(t, rec, &m)
	wantKeys := []string{"created_at", "lifecycle_state", "name", "strategy_id", "tenant_id", "updated_at"}
	if got := sortedKeys(m); !slices.Equal(got, wantKeys) {
		t.Fatalf("keys = %v, want %v", got, wantKeys)
	}
}

func TestListRuns(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "paper")
	insertChain(t, e.store, 10, strat1, 0)
	insertChain(t, e.store, 20, strat1, 1)

	wantError(t, e.do(t, "GET", "/api/v1/strategies/"+uid(9)+"/runs", readTok, nil), 404, codeUnknownStrategy)

	rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/runs", readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var pg struct {
		Items []store.Run `json:"items"`
		Total int         `json:"total"`
	}
	decodeJSON(t, rec, &pg)
	if pg.Total != 2 || len(pg.Items) != 2 {
		t.Fatalf("page = %+v", pg)
	}
	if pg.Items[0].TickNumber != 1 || pg.Items[1].TickNumber != 0 {
		t.Fatalf("runs not tick DESC: %+v", pg.Items)
	}
	if pg.Items[0].CompletedAt != nil {
		t.Fatalf("completed_at = %v, want nil before trace", *pg.Items[0].CompletedAt)
	}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
