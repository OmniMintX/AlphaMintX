package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/store"
)

// seedApproval sets up a live_l1 strategy with a pending verdict and a fresh
// mark, ready for a passing preflight.
func seedApproval(t *testing.T, e *testEnv, state string) (proposalID, verdictID string) {
	t.Helper()
	createStrategy(t, e.store, strat1, state)
	proposalID, verdictID, _ = insertChain(t, e.store, 10, strat1, 0)
	if err := e.store.CreatePendingApproval(verdictID, strat1, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}
	e.marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("64000"), TS: testNow})
	return proposalID, verdictID
}

func postApproval(t *testing.T, e *testEnv, strategyID, verdictID string, approved bool) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, "POST", "/api/v1/strategies/"+strategyID+"/approvals", opTok,
		approvalRequest{VerdictID: verdictID, Approved: approved})
}

func TestApprovalApproved(t *testing.T) {
	e := newEnv(t, nil)
	proposalID, verdictID := seedApproval(t, e, "live_l1")

	rec := postApproval(t, e, strat1, verdictID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var a store.Approval
	decodeJSON(t, rec, &a)
	if a.Outcome != store.OutcomeApproved || a.VerdictID != verdictID || a.ProposalID != proposalID {
		t.Fatalf("approval = %+v", a)
	}
	if a.DecidedBy != "trader-1" || a.TimeoutSeconds != 600 || a.DecidedAt != formatTime(testNow) {
		t.Fatalf("approval = %+v", a)
	}
	if a.PreflightReasons != nil {
		t.Fatalf("preflight_reasons = %v, want nil", a.PreflightReasons)
	}
	if e.sub.count() != 1 || e.sub.calls[0].VerdictID != verdictID {
		t.Fatalf("submitter calls = %+v", e.sub.calls)
	}
	var raw map[string]any
	decodeJSON(t, rec, &raw)
	if raw["submitted"] != true {
		t.Fatalf("submitted = %v, want true", raw["submitted"])
	}
	if _, ok := raw["submit_error_code"]; ok {
		t.Fatalf("submit_error_code present on a successful submission: %v", raw)
	}

	// Second decision: 409 with the recorded outcome; no second submit.
	rec = postApproval(t, e, strat1, verdictID, false)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	var body errorBody
	decodeJSON(t, rec, &body)
	if body.Code != codeAlreadyDecided || body.Recorded == nil || body.Recorded.Outcome != store.OutcomeApproved {
		t.Fatalf("conflict body = %+v", body)
	}
	if e.sub.count() != 1 {
		t.Fatalf("submitter called %d times, want 1", e.sub.count())
	}
}

func TestApprovalRejected(t *testing.T) {
	e := newEnv(t, nil)
	_, verdictID := seedApproval(t, e, "live_l1")

	rec := postApproval(t, e, strat1, verdictID, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var a store.Approval
	decodeJSON(t, rec, &a)
	if a.Outcome != store.OutcomeRejected || a.PreflightReasons != nil {
		t.Fatalf("approval = %+v", a)
	}
	if e.sub.count() != 0 {
		t.Fatal("rejected decision must not submit")
	}
}

// TestApprovalSubmitFailure: an OMS submission failure after the approval
// row is committed is surfaced ({submitted:false, submit_error_code}) and
// persisted as a SUBMIT_FAILED rejected_submissions row — the audit trail
// never claims an execution that did not happen.
func TestApprovalSubmitFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.sub.err = errors.New("KILL_SWITCH_ACTIVE: kill-epoch stale at submission")
	_, verdictID := seedApproval(t, e, "live_l1")

	rec := postApproval(t, e, strat1, verdictID, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		store.Approval
		Submitted       *bool  `json:"submitted"`
		SubmitErrorCode string `json:"submit_error_code"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Outcome != store.OutcomeApproved {
		t.Fatalf("outcome = %q, want approved (the decision itself is recorded)", resp.Outcome)
	}
	if resp.Submitted == nil || *resp.Submitted {
		t.Fatalf("submitted = %v, want false", resp.Submitted)
	}
	if resp.SubmitErrorCode != codeSubmitFailed {
		t.Fatalf("submit_error_code = %q, want %q", resp.SubmitErrorCode, codeSubmitFailed)
	}
	rows, err := e.store.RejectedSubmissions(strat1)
	if err != nil {
		t.Fatalf("RejectedSubmissions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rejected_submissions rows = %d, want 1", len(rows))
	}
	if !strings.HasPrefix(rows[0].Reason, codeSubmitFailed) || !strings.Contains(rows[0].Reason, "kill-epoch stale") {
		t.Errorf("rejection reason = %q, want SUBMIT_FAILED with the OMS error", rows[0].Reason)
	}
	if !strings.Contains(rows[0].PayloadJSON, verdictID) {
		t.Errorf("rejection payload %q does not reference verdict %s", rows[0].PayloadJSON, verdictID)
	}
}

func TestApprovalErrors(t *testing.T) {
	e := newEnv(t, nil)
	createStrategy(t, e.store, strat1, "live_l1")
	createStrategy(t, e.store, strat2, "live_l1")
	// strat1 verdict WITHOUT a pending row; strat2 verdict with one.
	_, verdict1, _ := insertChain(t, e.store, 10, strat1, 0)
	_, verdict2, _ := insertChain(t, e.store, 20, strat2, 0)
	if err := e.store.CreatePendingApproval(verdict2, strat2, testNow, 600); err != nil {
		t.Fatalf("CreatePendingApproval: %v", err)
	}

	wantError(t, postApproval(t, e, strat1, uid(99), true), 404, codeUnknownVerdict)
	// verdict -> proposal -> strategy match is REQUIRED.
	wantError(t, postApproval(t, e, strat1, verdict2, true), 404, codeUnknownVerdict)
	wantError(t, postApproval(t, e, strat1, verdict1, true), 422, codeNotPending)

	badBody := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok, []byte(`{"verdict_id":1}`))
	wantError(t, badBody, 400, codeSchemaInvalid)
}

func TestApprovalPreflightBlocked(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		mutate     func(*Config)
		seed       func(t *testing.T, e *testEnv)
		wantReason string
	}{
		{
			name:       "mark unavailable",
			state:      "live_l1",
			wantReason: reasonMarkPriceUnavailable,
			seed: func(t *testing.T, e *testEnv) {
				// Stale mark: older than the 60 s max age at decision time.
				e.marks.Put(marketdata.Tick{
					Symbol: "BTC/USDT",
					Mark:   decimal.RequireFromString("64000"),
					TS:     testNow.Add(-2 * time.Minute),
				})
			},
		},
		{
			name:       "strategy not live",
			state:      "paper",
			wantReason: reasonStrategyNotLive,
		},
		{
			name:       "kill epoch moved",
			state:      "live_l1",
			wantReason: reasonKillSwitchActive,
			seed: func(t *testing.T, e *testEnv) {
				epoch := int64(1)
				err := e.store.AppendKillBreakerEvent(store.KillBreakerEvent{
					EventID: uid(80), Kind: "kill", Scope: "strategy", StrategyID: &strat1,
					KillEpoch: &epoch, ActorID: "admin-1", RecordedAt: formatTime(testNow),
				})
				if err != nil {
					t.Fatalf("AppendKillBreakerEvent: %v", err)
				}
			},
		},
		{
			name:       "daily loss breached",
			state:      "live_l1",
			wantReason: reasonDailyLossLimitBreach,
			mutate: func(cfg *Config) {
				cfg.DailyLossBreached = func(string, time.Time) (bool, error) { return true, nil }
			},
		},
		{
			// No Submitter wired: approving must not record a false
			// "submitted to OMS" execution.
			name:       "submitter unavailable",
			state:      "live_l1",
			wantReason: reasonSubmitterUnavailable,
			mutate:     func(cfg *Config) { cfg.Submitter = nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.mutate)
			createStrategy(t, e.store, strat1, tc.state)
			_, verdictID, _ := insertChain(t, e.store, 10, strat1, 0)
			if err := e.store.CreatePendingApproval(verdictID, strat1, testNow, 600); err != nil {
				t.Fatalf("CreatePendingApproval: %v", err)
			}
			if tc.name != "mark unavailable" {
				e.marks.Put(marketdata.Tick{Symbol: "BTC/USDT", Mark: decimal.RequireFromString("64000"), TS: testNow})
			}
			if tc.seed != nil {
				tc.seed(t, e)
			}

			rec := postApproval(t, e, strat1, verdictID, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
			}
			var a store.Approval
			decodeJSON(t, rec, &a)
			if a.Outcome != store.OutcomeApprovedButBlocked {
				t.Fatalf("outcome = %q, want approved_but_blocked", a.Outcome)
			}
			if !slices.Contains(a.PreflightReasons, tc.wantReason) {
				t.Fatalf("preflight_reasons = %v, want %q", a.PreflightReasons, tc.wantReason)
			}
			if e.sub.count() != 0 {
				t.Fatal("blocked approval must not submit")
			}
		})
	}
}

func TestApprovalRateLimit(t *testing.T) {
	e := newEnv(t, nil)
	body := approvalRequest{VerdictID: uid(99), Approved: false}
	for i := 0; i < 70; i++ {
		rec := e.do(t, "POST", "/api/v1/strategies/"+strat1+"/approvals", opTok, body)
		want := http.StatusNotFound
		if i >= 60 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("request %d: status = %d, want %d", i, rec.Code, want)
		}
		if want == http.StatusTooManyRequests {
			wantRetryAfter(t, rec)
		}
	}
}
