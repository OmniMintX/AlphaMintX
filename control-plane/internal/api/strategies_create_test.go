package api

import (
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// TestCreateStrategyTenantResolution pins SP-3: owner/admin create in
// their own tenant (foreign body tenant_id 403, no lookup), env-admin
// needs an EXISTING body tenant (missing 400, unknown 404 UNKNOWN_TENANT).
func TestCreateStrategyTenantResolution(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	owner := seedUserToken(t, e.store, "tenant-a", RoleOwner, "db-a-owner")
	admin := seedUserToken(t, e.store, "tenant-a", RoleAdmin, "db-a-admin")

	for i, tok := range []string{owner, admin} {
		rec := e.do(t, "POST", "/api/v1/strategies", tok,
			createStrategyRequest{Name: "own-" + string(rune('a'+i))})
		if rec.Code != http.StatusOK {
			t.Fatalf("own-tenant create #%d = %d (body %q)", i, rec.Code, rec.Body.String())
		}
		var st store.Strategy
		decodeJSON(t, rec, &st)
		if st.TenantID != "tenant-a" || st.LifecycleState != "draft" || st.StrategyID == "" {
			t.Fatalf("created = %+v, want tenant-a draft with id", st)
		}
		if got, err := e.store.GetStrategy(st.StrategyID); err != nil || got.Name != st.Name {
			t.Fatalf("persisted read-back = %+v (err %v)", got, err)
		}
	}
	wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
		createStrategyRequest{TenantID: "tenant-b", Name: "x"}), 403, codeForbidden)
	wantError(t, e.do(t, "POST", "/api/v1/strategies", adminTok,
		createStrategyRequest{Name: "x"}), 400, codeInvalidTenantID)
	wantError(t, e.do(t, "POST", "/api/v1/strategies", adminTok,
		createStrategyRequest{TenantID: "nope", Name: "x"}), 404, codeUnknownTenant)
	rec := e.do(t, "POST", "/api/v1/strategies", adminTok,
		createStrategyRequest{TenantID: "tenant-a", Name: "via-env-admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("env-admin create = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestCreateStrategyStateAndBody pins SP-2: draft default, paper allowed
// (bootstrap row via the store, asserted in the store tests), every other
// state 400; name content/length rules; unknown fields 400.
func TestCreateStrategyStateAndBody(t *testing.T) {
	e := newEnv(t, nil)
	createTenant(t, e.store, "tenant-a")
	owner := seedUserToken(t, e.store, "tenant-a", RoleOwner, "db-a-owner")

	rec := e.do(t, "POST", "/api/v1/strategies", owner,
		createStrategyRequest{Name: "paper-one", LifecycleState: "paper"})
	if rec.Code != http.StatusOK {
		t.Fatalf("paper create = %d (body %q)", rec.Code, rec.Body.String())
	}
	var st store.Strategy
	decodeJSON(t, rec, &st)
	if st.LifecycleState != "paper" {
		t.Fatalf("lifecycle_state = %q, want paper", st.LifecycleState)
	}
	for _, bad := range []string{"live_l1", "live_l2", "live_l3", "killed", "paused", "bogus"} {
		wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
			createStrategyRequest{Name: "s-" + bad, LifecycleState: bad}), 400, codeSchemaInvalid)
	}
	for _, badName := range []string{"", "   ", strings.Repeat("x", 129),
		"line\nbreak", "bidi\u202Espoof", "iso\u2066late", "ctl\x7f", "rep\uFFFDlaced"} {
		wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
			createStrategyRequest{Name: badName}), 400, codeSchemaInvalid)
	}
	// Invalid UTF-8 and lone surrogates: encoding/json COERCES both to
	// U+FFFD without error, so the 400 comes from the replacement-char
	// rule — raw bytes on the wire prove the whole path.
	wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
		[]byte("{\"name\":\"a\xffb\"}")), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
		[]byte(`{"name":"a\ud800b"}`)), 400, codeSchemaInvalid)
	wantError(t, e.do(t, "POST", "/api/v1/strategies", owner,
		[]byte(`{"name":"x","strategy_id":"self-chosen"}`)), 400, codeSchemaInvalid)
	// None of the rejected bodies persisted a row: only paper-one so far.
	if _, total, err := e.store.ListStrategies(1, 50); err != nil || total != 1 {
		t.Fatalf("rows after 400s = %d (err %v), want 1", total, err)
	}
	// Trimming applies BEFORE the length bound: 128 content bytes padded
	// with whitespace is fine.
	rec = e.do(t, "POST", "/api/v1/strategies", owner,
		createStrategyRequest{Name: "  " + strings.Repeat("y", 128) + "  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("padded 128-byte name = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestCreateStrategyAttributionAndNoLeak pins SP-4a end-to-end: the API
// path persists actorID(pr) — the DB token_id for user principals,
// "env-admin" for the env class — and created_by leaks into NO response
// (the create 200 and the reads are shaped by store.Strategy, which has
// no such field).
func TestCreateStrategyAttributionAndNoLeak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane.db")
	e := newEnvAt(t, path, nil)
	createTenant(t, e.store, "tenant-a")
	owner := seedUserToken(t, e.store, "tenant-a", RoleOwner, "db-a-owner")

	rec := e.do(t, "POST", "/api/v1/strategies", owner, createStrategyRequest{Name: "mine"})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner create = %d (body %q)", rec.Code, rec.Body.String())
	}
	var created map[string]any
	decodeJSON(t, rec, &created)
	if _, leaked := created["created_by"]; leaked {
		t.Fatal("create response exposes created_by (audit-only, SP-4a)")
	}
	sid, _ := created["strategy_id"].(string)
	rec = e.do(t, "POST", "/api/v1/strategies", adminTok,
		createStrategyRequest{TenantID: "tenant-a", Name: "envs"})
	if rec.Code != http.StatusOK {
		t.Fatalf("env-admin create = %d (body %q)", rec.Code, rec.Body.String())
	}

	rec = e.do(t, "GET", "/api/v1/strategies/"+sid, readTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-back = %d (body %q)", rec.Code, rec.Body.String())
	}
	var read map[string]any
	decodeJSON(t, rec, &read)
	if _, leaked := read["created_by"]; leaked {
		t.Fatal("GET strategy exposes created_by (audit-only, SP-4a)")
	}

	// Audit values, read through a second connection (WAL, read-only).
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open audit connection: %v", err)
	}
	defer db.Close()
	var tokenID string
	if err := db.QueryRow(`SELECT token_id FROM api_tokens WHERE label = 'owner@tenant-a'`).Scan(&tokenID); err != nil {
		t.Fatalf("read token_id: %v", err)
	}
	for _, c := range []struct{ name, want string }{{"mine", tokenID}, {"envs", "env-admin"}} {
		var by string
		if err := db.QueryRow(`SELECT created_by FROM strategies WHERE name = ?`, c.name).Scan(&by); err != nil {
			t.Fatalf("read created_by(%s): %v", c.name, err)
		}
		if by != c.want {
			t.Errorf("created_by(%s) = %q, want %q", c.name, by, c.want)
		}
	}
}

// TestCreateStrategyNameConflictAndQuota pins SP-4/SP-4b over HTTP: 409
// STRATEGY_NAME_TAKEN on a same-tenant duplicate (same name in another
// tenant is fine), 409 STRATEGY_LIMIT_REACHED at the cap, and concurrent
// duplicate POSTs yield exactly one 200 and one 409.
func TestCreateStrategyNameConflictAndQuota(t *testing.T) {
	e := newEnv(t, func(cfg *Config) { cfg.MaxStrategiesPerTenant = 3 })
	createTenant(t, e.store, "tenant-a")
	createTenant(t, e.store, "tenant-b")
	ownerA := seedUserToken(t, e.store, "tenant-a", RoleOwner, "db-a-owner")
	ownerB := seedUserToken(t, e.store, "tenant-b", RoleOwner, "db-b-owner")

	if rec := e.do(t, "POST", "/api/v1/strategies", ownerA,
		createStrategyRequest{Name: "alpha"}); rec.Code != http.StatusOK {
		t.Fatalf("first = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "POST", "/api/v1/strategies", ownerA,
		createStrategyRequest{Name: " alpha "}), 409, codeStrategyNameTaken)
	if rec := e.do(t, "POST", "/api/v1/strategies", ownerB,
		createStrategyRequest{Name: "alpha"}); rec.Code != http.StatusOK {
		t.Fatalf("other tenant = %d (body %q)", rec.Code, rec.Body.String())
	}

	// Concurrent duplicates: exactly one 200, one 409 (single-connection
	// invariant, SP-4).
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = e.do(t, "POST", "/api/v1/strategies", ownerA,
				createStrategyRequest{Name: "raced"}).Code
		}(i)
	}
	wg.Wait()
	if !(codes[0] == 200 && codes[1] == 409) && !(codes[0] == 409 && codes[1] == 200) {
		t.Fatalf("concurrent duplicate codes = %v, want one 200 and one 409", codes)
	}

	// tenant-a now holds alpha + raced + one more = cap 3; the next is 409.
	if rec := e.do(t, "POST", "/api/v1/strategies", ownerA,
		createStrategyRequest{Name: "third"}); rec.Code != http.StatusOK {
		t.Fatalf("third = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "POST", "/api/v1/strategies", ownerA,
		createStrategyRequest{Name: "fourth"}), 409, codeStrategyLimitReached)
	// The other tenant is unaffected by tenant-a's cap.
	if rec := e.do(t, "POST", "/api/v1/strategies", ownerB,
		createStrategyRequest{Name: "beta"}); rec.Code != http.StatusOK {
		t.Fatalf("tenant-b under cap = %d (body %q)", rec.Code, rec.Body.String())
	}
}

// TestCreateStrategyNotRestoreGated pins SP-6: with the DS-2 gate engaged
// creation still works (it is not trading intent) while proposals stay 503.
func TestCreateStrategyNotRestoreGated(t *testing.T) {
	e := restoredEnv(t)
	if !restoreStatus(t, e, readTok) {
		t.Fatal("gate engaged = false, want true")
	}
	rec := e.do(t, "POST", "/api/v1/strategies", adminTok,
		createStrategyRequest{TenantID: "default", Name: "born-under-gate"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create under gate = %d (body %q)", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: openProposal(t, uid(100), strat1, uid(200))}),
		http.StatusServiceUnavailable, codeRestoreGate)
	if !restoreStatus(t, e, readTok) {
		t.Fatal("gate engaged after create = false, want still true")
	}
}
