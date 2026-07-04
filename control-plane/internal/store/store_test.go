package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOpenAppliesSchemaIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		createStrategy(t, s, uid(100+i))
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if _, total, err := s.ListStrategies(1, 10); err != nil || total != 2 {
		t.Fatalf("ListStrategies after reopen: total=%d err=%v, want 2, nil", total, err)
	}
}

func TestConnectionPragmas(t *testing.T) {
	s := openStore(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busy < 5000 {
		t.Errorf("busy_timeout = %d ms, want >= 5000", busy)
	}
}

func TestInsertProposalIdempotency(t *testing.T) {
	strategyID := uid(1)
	base := testProposal(t, uid(10), strategyID, uid(12))
	changed := testProposal(t, uid(10), strategyID, uid(12))
	changed.Reasoning = "a different payload for the same proposal_id"

	tests := []struct {
		name         string
		submissions  []ProposalSubmission
		wantInserted []bool
		wantErr      error
	}{
		{
			name: "duplicate same payload is a no-op",
			submissions: []ProposalSubmission{
				{TickNumber: 0, Proposal: base},
				{TickNumber: 0, Proposal: base},
			},
			wantInserted: []bool{true, false},
		},
		{
			name: "duplicate different payload is a conflict",
			submissions: []ProposalSubmission{
				{TickNumber: 0, Proposal: base},
				{TickNumber: 0, Proposal: changed},
			},
			wantInserted: []bool{true, false},
			wantErr:      ErrIdempotencyConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			createStrategy(t, s, strategyID)
			var lastErr error
			for i, sub := range tc.submissions {
				inserted, err := s.InsertProposal(sub, testNow)
				lastErr = err
				if i < len(tc.submissions)-1 && err != nil {
					t.Fatalf("submission %d: %v", i, err)
				}
				if err == nil && inserted != tc.wantInserted[i] {
					t.Errorf("submission %d inserted = %v, want %v", i, inserted, tc.wantInserted[i])
				}
			}
			if !errors.Is(lastErr, tc.wantErr) && lastErr != tc.wantErr {
				t.Errorf("final err = %v, want %v", lastErr, tc.wantErr)
			}
		})
	}
}

func TestInsertVerdictUniquePerProposal(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	proposalID, _, _ := insertChain(t, s, 10, uid(1), 0)

	if inserted, err := s.InsertVerdict(testVerdict(t, uid(11), proposalID)); err != nil || inserted {
		t.Errorf("identical verdict retry: inserted=%v err=%v, want false, nil", inserted, err)
	}
	if _, err := s.InsertVerdict(testVerdict(t, uid(19), proposalID)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("second verdict for proposal: err = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.InsertVerdict(testVerdict(t, uid(20), uid(21))); !errors.Is(err, ErrNotFound) {
		t.Errorf("verdict for unknown proposal: err = %v, want ErrNotFound", err)
	}
}

func TestAppendLifecycleTransitionAdvancesSnapshot(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	err := s.AppendLifecycleTransition(LifecycleTransition{
		TransitionID: uid(50), StrategyID: uid(1), FromState: "paper", ToState: "live_l1",
		ActorID: "admin-1", ActorRole: "admin", Reason: "promotion", RecordedAt: "2026-07-04T13:00:00Z",
	})
	if err != nil {
		t.Fatalf("AppendLifecycleTransition: %v", err)
	}
	st, err := s.GetStrategy(uid(1))
	if err != nil {
		t.Fatalf("GetStrategy: %v", err)
	}
	if st.LifecycleState != "live_l1" || st.UpdatedAt != "2026-07-04T13:00:00Z" {
		t.Errorf("strategy snapshot = %+v, want lifecycle_state live_l1 at 13:00", st)
	}
}

// TestStoreSurfaceIsAppendOnly pins the exported Store surface: append-only
// tables (invariant 7) must have no exported UPDATE/DELETE mutators, ever.
func TestStoreSurfaceIsAppendOnly(t *testing.T) {
	allowed := map[string]bool{
		"Close": true, "CreateStrategy": true,
		"InsertProposal": true, "InsertVerdict": true, "InsertTrace": true,
		"CreatePendingApproval": true, "ResolveApproval": true, "ExpirePendingApprovals": true,
		"InsertOrder": true, "InsertFill": true, "UpsertPosition": true,
		"AppendLifecycleTransition": true, "AppendRejectedSubmission": true, "AppendKillBreakerEvent": true,
		"ListStrategies": true, "GetStrategy": true, "ListRuns": true, "GetRunDetail": true,
		// Read-only helpers for the HTTP API (internal/api).
		"GetPendingApproval": true, "GetVerdictMeta": true, "MaxKillEpoch": true, "RunStrategy": true,
		"RejectedSubmissions": true,
	}
	tp := reflect.TypeOf(&Store{})
	for i := 0; i < tp.NumMethod(); i++ {
		name := tp.Method(i).Name
		if !allowed[name] {
			t.Errorf("unexpected exported Store method %s (append-only surface is pinned)", name)
		}
		if strings.HasPrefix(name, "Update") || strings.HasPrefix(name, "Delete") {
			t.Errorf("mutator method %s violates the append-only invariant", name)
		}
	}
}
