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
		{
			name: "duplicate same payload different tick is a conflict",
			submissions: []ProposalSubmission{
				{TickNumber: 0, Proposal: base},
				{TickNumber: 1, Proposal: base},
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

func TestInsertProposalRunTickConflict(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	first := ProposalSubmission{TickNumber: 0, Proposal: testProposal(t, uid(10), uid(1), uid(12))}
	if _, err := s.InsertProposal(first, testNow); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// A different run must not claim the occupied (strategy_id, tick_number).
	_, err := s.InsertProposal(ProposalSubmission{TickNumber: 0, Proposal: testProposal(t, uid(20), uid(1), uid(22))}, testNow)
	if !errors.Is(err, ErrRunTickConflict) {
		t.Fatalf("different run, same tick: err = %v, want ErrRunTickConflict", err)
	}
	if _, err := s.GetProposalPayload(uid(20)); !errors.Is(err, ErrNotFound) {
		t.Errorf("conflicting proposal persisted: err = %v, want ErrNotFound", err)
	}

	// A fresh proposal reusing an existing run at a different tick
	// contradicts the run row.
	_, err = s.InsertProposal(ProposalSubmission{TickNumber: 5, Proposal: testProposal(t, uid(30), uid(1), uid(12))}, testNow)
	if !errors.Is(err, ErrRunTickConflict) {
		t.Errorf("same run, different tick: err = %v, want ErrRunTickConflict", err)
	}
}

func TestIsDuplicateProposal(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	base := testProposal(t, uid(10), uid(1), uid(12))
	sub := ProposalSubmission{TickNumber: 0, Proposal: base}

	if dup, err := s.IsDuplicateProposal(sub); err != nil || dup {
		t.Fatalf("fresh: dup=%v err=%v, want false, nil", dup, err)
	}
	if _, err := s.InsertProposal(sub, testNow); err != nil {
		t.Fatalf("InsertProposal: %v", err)
	}
	if dup, err := s.IsDuplicateProposal(sub); err != nil || !dup {
		t.Fatalf("verbatim retry: dup=%v err=%v, want true, nil", dup, err)
	}
	changed := testProposal(t, uid(10), uid(1), uid(12))
	changed.Reasoning = "a different payload for the same proposal_id"
	if _, err := s.IsDuplicateProposal(ProposalSubmission{TickNumber: 0, Proposal: changed}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different payload: err = %v, want ErrIdempotencyConflict", err)
	}
	if _, err := s.IsDuplicateProposal(ProposalSubmission{TickNumber: 3, Proposal: base}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("different tick: err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestApplySweepAtomicityAndSnapshot(t *testing.T) {
	s := openStore(t)
	createStrategy(t, s, uid(1))
	proposalID, _, _ := insertChain(t, s, 10, uid(1), 0)
	order := Order{
		OrderID: uid(30), ProposalID: &proposalID, Origin: "proposal", StrategyID: uid(1),
		Symbol: "BTC/USDT", Class: "ENTRY", Side: "buy", Type: "market", QtyBase: "0.1",
		Status: "open", SubmittedAt: formatTime(testNow),
	}
	position := Position{
		StrategyID: uid(1), Symbol: "BTC/USDT", QtyBase: "0.1", EntryPrice: "64000",
		FeesQuote: "3.2", RealizedPnLQuote: "-3.2", UpdatedAt: formatTime(testNow),
	}
	state := StrategyState{
		StrategyID: uid(1), EquityQuote: "9996.8", PeakEquityQuote: "10000",
		DailyRealizedPnLQuote: "-3.2", UTCDate: "2026-07-04", UpdatedAt: formatTime(testNow),
	}

	// A mid-batch failure rolls the WHOLE batch back: no torn sweep.
	failure := errors.New("mid-batch failure")
	err := s.ApplySweep(func(tx *SweepTx) error {
		if err := tx.InsertOrder(order); err != nil {
			return err
		}
		if err := tx.UpsertPosition(position); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("ApplySweep err = %v, want the callback failure", err)
	}
	snap, err := s.StrategySnapshot(uid(1))
	if err != nil {
		t.Fatalf("StrategySnapshot: %v", err)
	}
	if len(snap.Positions) != 0 || len(snap.OpenOrders) != 0 || snap.HasState {
		t.Fatalf("rolled-back sweep left rows: %+v", snap)
	}

	// The same batch commits atomically on success and the snapshot sees
	// all three surfaces together.
	err = s.ApplySweep(func(tx *SweepTx) error {
		if err := tx.InsertOrder(order); err != nil {
			return err
		}
		if err := tx.UpsertPosition(position); err != nil {
			return err
		}
		return tx.UpsertStrategyState(state)
	})
	if err != nil {
		t.Fatalf("ApplySweep: %v", err)
	}
	snap, err = s.StrategySnapshot(uid(1))
	if err != nil {
		t.Fatalf("StrategySnapshot: %v", err)
	}
	if len(snap.Positions) != 1 || len(snap.OpenOrders) != 1 || !snap.HasState {
		t.Fatalf("committed sweep snapshot = %+v, want 1 position, 1 open order, state", snap)
	}
	if snap.OpenOrders[0].OrderID != uid(30) || snap.Positions[0].QtyBase != "0.1" ||
		snap.State.EquityQuote != "9996.8" {
		t.Errorf("snapshot rows = %+v, want the committed batch verbatim", snap)
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
		"Close": true, "CreateStrategy": true, "CreateStrategyProvisioned": true,
		"InsertProposal": true, "InsertVerdict": true, "InsertTrace": true,
		"CreatePendingApproval": true, "ResolveApproval": true, "ExpirePendingApprovals": true,
		"InsertOrder": true, "InsertFill": true, "UpsertPosition": true,
		"AppendLifecycleTransition": true, "AppendRejectedSubmission": true, "AppendKillBreakerEvent": true,
		"ListStrategies": true, "GetStrategy": true, "ListRuns": true, "GetRunDetail": true,
		// Read-only helpers for the HTTP API (internal/api).
		"GetPendingApproval": true, "GetVerdictMeta": true, "MaxKillEpoch": true, "RunStrategy": true,
		"RejectedSubmissions": true,
		// Serve-mode runtime surface (internal/runstate, internal/omsbridge,
		// proposal ingestion). Orders mutate ONLY their FSM status/fill
		// columns; positions and strategy_state are mutable snapshots.
		"GetVerdictByProposalID": true, "GetProposalPayload": true,
		"ListOpenOrders": true, "ListPositions": true, "GetStrategyState": true,
		"CountRateVerdictsSince": true, "GlobalMaxKillEpoch": true,
		"UpsertStrategyState": true, "RecordOrderFill": true, "RecordOrderCancel": true,
		// IsDuplicateProposal is read-only (verbatim-retry detection);
		// ApplySweep groups the runtime writers above into one transaction
		// and StrategySnapshot is its consistent read twin — neither adds
		// an UPDATE/DELETE surface.
		"IsDuplicateProposal": true, "ApplySweep": true, "StrategySnapshot": true,
		// Multi-tenant RBAC surface (multi-tenant-rbac.md): tenants, DB
		// tokens (RevokeAPIToken is the api_tokens snapshot's single legal
		// mutation — revoked_at once), the tenant kill appender, the limit
		// audit appender/replayer, and tenant-scoped root reads.
		"CreateTenant": true, "GetTenant": true, "CreateTenantWithOwnerToken": true,
		"InsertAPIToken": true, "GetAPIToken": true, "TokenByHash": true,
		"ListAPITokens": true, "RevokeAPIToken": true, "CountUnrevokedOwnerTokens": true,
		// InsertOwnerRecoveryToken is the transactional zero-owner gate +
		// insert (no UPDATE/DELETE surface).
		"InsertOwnerRecoveryToken": true,
		"AppendTenantKill":         true, "AppendRiskLimitChanges": true, "RiskLimitChanges": true,
		"GetStrategyInTenant": true, "ListStrategiesByTenant": true,
		// Safety-wiring surface (safety-wiring.md §Store-surface amendment):
		// the strategy/platform kill appenders, the INSERT-only served-effect
		// marker and alert journal, the driver's lifecycle lock (transition
		// append + the strategies snapshot update, like
		// AppendLifecycleTransition), and the safety-engine reads.
		"AppendStrategyKill":       true,
		"AppendPlatformKill":       true,
		"AppendSafetyEffectDone":   true,
		"AppendSafetyAlert":        true,
		"AppendKillLifecycleLock":  true,
		"ListUnservedSafetyEvents": true,
		"ListSafetyAlerts":         true,
		"HasSafetyAlertToday":      true,
		"HasSafetyAlert":           true,
		// LatestStrategyKillEvent is the read-only WD-16 back-fill accessor
		// (docs/specs/watchdog.md §Wiring seams).
		"LatestStrategyKillEvent": true,
		// Operator-surface reads (docs/specs/operator-surface.md §Wiring
		// seams): the OS-10a single-snapshot status and the two paginated
		// alert feeds — read-only, no UPDATE/DELETE surface.
		"SafetyStatus":                   true,
		"ListSafetyAlertsByStrategyPage": true,
		"ListSafetyAlertsGlobalPage":     true,
		// Lifecycle-API surface (docs/specs/lifecycle-api.md §Store
		// surface): the CAS transition appender, the append-only SW-2
		// kill-clear appenders (kill rows are never mutated — a clear is
		// a new row, invariant 2), and the read-only lifecycle/paper-gate
		// accessors.
		"AppendLifecycleTransitionCAS": true,
		"AppendKillClearStrategy":      true,
		"AppendKillClearTenant":        true,
		"AppendKillClearPlatform":      true,
		"ActiveKill":                   true,
		"PaperWindowStart":             true,
		"PausedProvenance":             true,
		"ListPaperGateFills":           true,
		"SafetyEffectServed":           true,
		// Billing and metering surface (billing-and-metering.md): all six
		// tables are INSERT-only — imports, closes, and reconciliation
		// runs append; invoices and runs are read back, never mutated.
		"InsertMeteringRecords": true, "ClosePeriod": true, "Reconcile": true,
		"ListInvoices": true, "GetInvoice": true,
		"ListReconciliations": true, "GetReconciliation": true,
		// Live-OMS surface (live-oms-and-reconciler.md §Store-surface
		// amendment): INSERT-only writers for the intent journal, recon
		// audit, obligation timers, deferred fees, and venue epochs; the
		// enumerated Record* mutators (live columns mutate ONLY through
		// them); and the Reconciler/OMS read helpers.
		"InsertOrderIntent": true, "AppendOMSReconEvent": true,
		"InsertProtectiveObligation": true, "InsertPendingFillFee": true,
		"InsertVenueEpoch":  true,
		"RecordIntentClaim": true, "RecordIntentClaimRevoked": true,
		"RecordIntentAttempt": true, "RecordExchangeAck": true, "RecordOrderStatus": true,
		"RecordProtectiveSatisfied": true, "RecordFeeConverted": true,
		"ListPendingNewIntents": true, "ListOpenProtectiveObligations": true,
		"ListUnconvertedPendingFillFees": true, "CurrentVenueEpoch": true,
		"FillWatermark": true, "ListOMSReconEvents": true,
		// InsertJournaledOrder is the journal-before-send transaction
		// (orders + order_intents commit together); RecordVenueFill is the
		// one-transaction fill booking (deduped INSERT + fill bookkeeping +
		// monotone FSM + accounting, invariant 8). The rest are read-only.
		"InsertJournaledOrder": true, "RecordVenueFill": true,
		"GetLiveOrderByClientOrderID": true, "GetLiveOrderByExchangeOrderID": true,
		"ListNonTerminalLiveOrders": true, "GetOrderIntent": true, "ListFillsByOrder": true,
		"ListFilledProtectiveEntries": true, "GetLiveOrderForFill": true,
		"BreakerActiveToday": true,
		// Ops backup surface (ops-backup.md OB-2..OB-9): Backup performs
		// zero LOGICAL writes to the DB (OB-3; file-level snapshot +
		// retention in the backup dir) and ListBackups is a readdir —
		// neither adds an UPDATE/DELETE surface.
		"Backup": true, "ListBackups": true,
		// Alert notifier surface (alert-notifier.md AN-1a/AN-2/AN-7..9):
		// the three List*After reads and MaxAlertSourceRowid are read-only;
		// Seed/Upsert touch only alert_dispatch_state, which is a mutable
		// snapshot exempt like strategy_state (AN-9); the combined
		// recon-event+alert mutator is two INSERTs in one tx (AN-1a).
		"AlertDispatchWatermark": true, "SeedAlertDispatchWatermark": true,
		"UpsertAlertDispatchWatermark": true, "MaxAlertSourceRowid": true,
		"ListKillBreakerEventsAfter": true, "ListKillClearEventsAfter": true,
		"ListSafetyAlertsAfter": true, "AppendOMSReconEventWithAlert": true,
		// Restore gate (deploy-and-survive.md DS-2/DS-4/DS-5):
		// ClearRestoreGate's user_version write is a header flag, not a
		// row mutation — the append-only invariant holds.
		"RestoreGateEngaged": true, "RestoreGateUserVersion": true,
		"RestoreGateAlertPending": true, "ClearRestoreGate": true,
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
