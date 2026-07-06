package api

// Restore-gate API tests (docs/specs/deploy-and-survive.md §Test
// obligations, api side): gated proposals/approvals 503 RESTORE_GATE with
// nothing persisted and no proposal-limiter charge, the DS-8 ungated
// surface, the DS-5 ack flow (200 then proposals flow, re-ack 409,
// exactly one concurrent winner), the cleared-alert shape, and the DS-6
// status read.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/contract"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// stampRestored writes the DS-1 user_version stamp into the DB file at
// path so the NEXT store.Open engages the gate (DS-2).
func stampRestored(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(path))
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// restoredEnv is gatedEnv over a DB stamped as a restored artifact BEFORE
// Open, so the server boots with the restore gate engaged.
func restoredEnv(t *testing.T, mutate ...func(*Config)) *testEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-plane.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stampRestored(t, path)
	return gatedEnvAt(t, path, mutate...)
}

func restoreStatus(t *testing.T, e *testEnv, token string) bool {
	t.Helper()
	rec := e.do(t, "GET", "/api/v1/ops/restore", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/ops/restore = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var status restoreStatusResponse
	decodeJSON(t, rec, &status)
	return status.Engaged
}

// TestRestoreGateBlocksProposals: 503 RESTORE_GATE before anything else —
// no verdict, no run, no rate-limit charge, and the SAME 503 for an
// unknown strategy (no existence oracle). After the ack (200
// {"cleared": true}) a fresh proposal flows and is NOT 429 — 31 gated
// posts never charged the 30/min proposal limiter. Re-ack is 409.
func TestRestoreGateBlocksProposals(t *testing.T) {
	e := restoredEnv(t)
	createStrategy(t, e.store, strat1, "live_l3")
	putMark(e, "BTC/USDT", "64000")
	if !restoreStatus(t, e, readTok) {
		t.Fatal("status engaged = false, want true (DS-6)")
	}

	for i := 0; i < 31; i++ {
		wantError(t, e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
			store.ProposalSubmission{TickNumber: i, Proposal: openProposal(t, uid(100+i), strat1, uid(200+i))}),
			http.StatusServiceUnavailable, codeRestoreGate)
	}
	// strat2 exists as a token scope but not as a strategies row: the 503
	// is uniform across known/unknown strategies (DS-3).
	wantError(t, e.do(t, http.MethodPost, "/api/v1/strategies/"+strat2+"/proposals", agent2Tok,
		store.ProposalSubmission{TickNumber: 0, Proposal: openProposal(t, uid(90), strat2, uid(91))}),
		http.StatusServiceUnavailable, codeRestoreGate)
	// A MALFORMED body is the same 503, not 400: the gate precedes the
	// body read, so no rejected_submissions audit row is persisted either.
	wantError(t, e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/proposals", agent1Tok,
		[]byte(`{"bogus`)), http.StatusServiceUnavailable, codeRestoreGate)
	if rejected, err := e.store.RejectedSubmissions(strat1); err != nil || len(rejected) != 0 {
		t.Errorf("rejected_submissions = %d rows (%v), want 0 under the gate", len(rejected), err)
	}
	if _, total, err := e.store.ListRuns(strat1, 1, 10); err != nil || total != 0 {
		t.Errorf("runs total = %d (%v), want 0 persisted under the gate", total, err)
	}
	if _, err := e.store.GetVerdictByProposalID(uid(100)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("verdict for gated proposal: err = %v, want ErrNotFound", err)
	}

	rec := e.do(t, "POST", "/api/v1/ops/restore/ack", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var ack map[string]bool
	decodeJSON(t, rec, &ack)
	if !ack["cleared"] {
		t.Errorf("ack body = %q, want {\"cleared\": true}", rec.Body.String())
	}
	if restoreStatus(t, e, readTok) {
		t.Error("status engaged = true after ack, want false")
	}

	v, _ := postProposal(e, t, strat1, agent1Tok, 0, openProposal(t, uid(10), strat1, uid(12)))
	if v.Decision != contract.DecisionApprove {
		t.Fatalf("post-ack decision = %s (%v), want approve", v.Decision, v.Reasons)
	}
	wantError(t, e.do(t, "POST", "/api/v1/ops/restore/ack", adminTok, nil),
		http.StatusConflict, codeRestoreGateNotEngaged)
}

// TestRestoreGateBlocksApprovals: the gate check precedes body decode and
// every lookup — a well-formed AND a malformed body both 503; after the
// ack the handler reaches the normal flow (422 NOT_PENDING here).
func TestRestoreGateBlocksApprovals(t *testing.T) {
	e := restoredEnv(t)
	createStrategy(t, e.store, strat1, "live_l1")
	_, verdictID, runID := insertChain(t, e.store, 20, strat1, 0)
	body := map[string]any{"verdict_id": verdictID, "approved": true}

	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok, body),
		http.StatusServiceUnavailable, codeRestoreGate)
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok, []byte(`{"bogus`)),
		http.StatusServiceUnavailable, codeRestoreGate)
	if d, err := e.store.GetRunDetail(strat1, runID); err != nil || len(d.Approvals) != 0 {
		t.Errorf("approval rows = %d (%v), want 0 persisted under the gate", len(d.Approvals), err)
	}

	if rec := e.do(t, "POST", "/api/v1/ops/restore/ack", adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("ack = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	wantError(t, e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok, body),
		http.StatusUnprocessableEntity, codeNotPending)
}

// TestRestoreGateUngatedSurface pins DS-8: kill, kill/clear, lifecycle,
// heartbeat, traces, reads, and backups all proceed under the gate —
// safety and audit are never gated.
func TestRestoreGateUngatedSurface(t *testing.T) {
	e := restoredEnv(t, func(cfg *Config) { cfg.Backup = &fakeBackupEngine{} })
	createStrategy(t, e.store, strat1, "paper")
	_, _, runID := insertChain(t, e.store, 30, strat1, 0)

	if rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/heartbeat", agent1Tok, nil); rec.Code != http.StatusOK {
		t.Errorf("heartbeat = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/traces", agent1Tok,
		testTraceEnvelope(t, strat1, runID, nil)); rec.Code != http.StatusOK {
		t.Errorf("traces = %d (body %q), want 200 (OB-12a replay noise)", rec.Code, rec.Body.String())
	}
	rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/kill", adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("kill = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	var kill struct {
		KillEpoch int64 `json:"kill_epoch"`
	}
	decodeJSON(t, rec, &kill)
	if rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/kill/clear", adminTok,
		map[string]any{"reason": "restore drill", "observed_epoch": kill.KillEpoch}); rec.Code != http.StatusOK {
		t.Errorf("kill/clear = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodPost, "/api/v1/strategies/"+strat1+"/lifecycle", adminTok,
		map[string]any{"to": "paused", "reason": "operator pause under gate"}); rec.Code != http.StatusOK {
		t.Errorf("lifecycle = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "GET", "/api/v1/strategies", readTok, nil); rec.Code != http.StatusOK {
		t.Errorf("GET strategies = %d, want 200", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/v1/strategies/"+strat1+"/safety", readTok, nil); rec.Code != http.StatusOK {
		t.Errorf("GET safety = %d, want 200", rec.Code)
	}
	if rec := e.do(t, "GET", "/api/v1/ops/backups", adminTok, nil); rec.Code != http.StatusOK {
		t.Errorf("GET backups = %d, want 200", rec.Code)
	}
}

// TestRestoreAckClearedAlertShape: the DS-5 cleared row — kind
// restore_gate_cleared, strategy_id NULL, details {"actor_id":
// "env-admin"} — commits with the gate clear in one transaction.
func TestRestoreAckClearedAlertShape(t *testing.T) {
	e := restoredEnv(t)
	if rec := e.do(t, "POST", "/api/v1/ops/restore/ack", adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("ack = %d (body %q), want 200", rec.Code, rec.Body.String())
	}
	cleared := clearedAlerts(t, e.store)
	if len(cleared) != 1 {
		t.Fatalf("cleared alert rows = %d, want 1", len(cleared))
	}
	a := cleared[0]
	if a.StrategyID != nil || a.RefID != nil {
		t.Errorf("cleared alert scope = %v/%v, want NULL strategy_id and ref_id", a.StrategyID, a.RefID)
	}
	var details map[string]string
	if err := json.Unmarshal([]byte(a.DetailsJSON), &details); err != nil {
		t.Fatalf("details %q: %v", a.DetailsJSON, err)
	}
	if details["actor_id"] != "env-admin" {
		t.Errorf("details = %q, want actor_id env-admin (DS-7)", a.DetailsJSON)
	}
	if a.RecordedAt != formatTime(testNow) {
		t.Errorf("recorded_at = %q, want %q", a.RecordedAt, formatTime(testNow))
	}
}

// TestRestoreAckConcurrent: exactly one of two concurrent acks wins (200)
// and the loser is 409 — one cleared alert row ever commits (DS-5 CAS).
// The goroutines never touch *testing.T (t.Fatalf must only run on the
// test goroutine); codes travel back over the channel.
func TestRestoreAckConcurrent(t *testing.T) {
	e := restoredEnv(t)
	codeCh := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			req := httptest.NewRequest("POST", "/api/v1/ops/restore/ack", nil)
			req.Header.Set("Authorization", "Bearer "+adminTok)
			rec := httptest.NewRecorder()
			e.srv.ServeHTTP(rec, req)
			codeCh <- rec.Code
		}()
	}
	codes := []int{<-codeCh, <-codeCh}
	if !(codes[0] == http.StatusOK && codes[1] == http.StatusConflict) &&
		!(codes[0] == http.StatusConflict && codes[1] == http.StatusOK) {
		t.Fatalf("concurrent ack codes = %v, want exactly one 200 and one 409", codes)
	}
	if cleared := clearedAlerts(t, e.store); len(cleared) != 1 {
		t.Errorf("cleared alert rows = %d, want exactly 1", len(cleared))
	}
}

// TestRestoreAckNotEngaged: an ack on a never-gated deployment is 409
// (catches acks aimed at the wrong deployment) and status reads false.
func TestRestoreAckNotEngaged(t *testing.T) {
	e := newEnv(t, nil)
	wantError(t, e.do(t, "POST", "/api/v1/ops/restore/ack", adminTok, nil),
		http.StatusConflict, codeRestoreGateNotEngaged)
	if restoreStatus(t, e, readTok) {
		t.Error("status engaged = true on a never-gated deployment, want false")
	}
	if cleared := clearedAlerts(t, e.store); len(cleared) != 0 {
		t.Errorf("cleared alert rows = %d, want 0 (nothing written on 409)", len(cleared))
	}
}

// clearedAlerts lists the restore_gate_cleared safety_alerts rows.
func clearedAlerts(t *testing.T, s *store.Store) []store.SafetyAlert {
	t.Helper()
	rows, err := s.ListSafetyAlertsAfter(0, 100)
	if err != nil {
		t.Fatalf("ListSafetyAlertsAfter: %v", err)
	}
	var out []store.SafetyAlert
	for _, r := range rows {
		if r.Kind == "restore_gate_cleared" {
			out = append(out, r.SafetyAlert)
		}
	}
	return out
}
