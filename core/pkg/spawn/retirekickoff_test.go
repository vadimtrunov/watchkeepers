package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Helpers — test fixtures shared with spawnkickoff_test.go reused via
// the same `spawn_test` package; only retire-specific helpers are
// declared here.
// ────────────────────────────────────────────────────────────────────────

// testRetireClaim is the canonical claim forwarded into every retire
// kickoff test. The kickoffer itself does NOT validate the
// AuthorityMatrix in M7.2.a — the matrix entry is purely a sentinel
// the downstream M7.2.c MarkRetired step's gate will consult. Tests
// here that read `claim.AuthorityMatrix["retire_watchkeeper"]` only
// pin the `WithSpawnContext` forwarding contract; they do NOT pin
// any kickoffer-side gate that does not yet exist. Once M7.2.c lands,
// the matrix entry becomes load-bearing.
func testRetireClaim() saga.SpawnClaim {
	return saga.SpawnClaim{
		OrganizationID: "org-test",
		AgentID:        "agent-watchmaster",
		AuthorityMatrix: map[string]string{
			"retire_watchkeeper": "lead_approval",
		},
	}
}

// retireKickoffWithDefaults invokes [spawn.RetireKickoffer.Kickoff]
// with a fresh watchkeeperID and the canonical [testRetireClaim].
// Lets per-branch tests stay focused on their specific assertions
// without re-stating the per-call values on every line. Mirrors
// [kickoffWithDefaults] for the spawn family.
func retireKickoffWithDefaults(
	ctx context.Context,
	k *spawn.RetireKickoffer,
	sagaID, manifestVersionID uuid.UUID,
	approvalToken string,
) error {
	return k.Kickoff(ctx, sagaID, manifestVersionID, uuid.New(), testRetireClaim(), approvalToken)
}

// newRetireKickoffer composes a real saga.Runner backed by `dao` plus a
// real *keeperslog.Writer, and returns a kickoffer wired to all three.
// Hoisted so the per-branch tests stay scannable.
func newRetireKickoffer(t *testing.T, dao saga.SpawnSagaDAO, keep *fakeLocalKeepClient, agentID string) *spawn.RetireKickoffer {
	t.Helper()
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	return spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: agentID,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewRetireKickoffer_NilLogger_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRetireKickoffer with nil Logger did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})
	_ = spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{DAO: dao, Runner: runner, AgentID: "bot"})
}

func TestNewRetireKickoffer_NilDAO_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRetireKickoffer with nil DAO did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	_ = spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{Logger: writer, Runner: runner, AgentID: "bot"})
}

func TestNewRetireKickoffer_NilRunner_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRetireKickoffer with nil Runner did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	_ = spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{Logger: keeperslog.New(keep), DAO: dao, AgentID: "bot"})
}

func TestNewRetireKickoffer_EmptyAgentID_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRetireKickoffer with empty AgentID did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	_ = spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{Logger: writer, DAO: dao, Runner: runner})
}

// TestNewRetireKickoffer_StepsDefensiveCopy pins the M7.1.c.c lesson
// (defensive deep copy on reference-typed step config): a
// post-construction mutation of the caller's Steps slice MUST NOT
// affect the saga's run. The kickoffer snapshots Steps at
// construction time. Without the snapshot, a caller that re-uses
// their original slice could swap a step in mid-saga.
func TestNewRetireKickoffer_StepsDefensiveCopy(t *testing.T) {
	t.Parallel()

	rec := newRecordingStepLog()
	tick := &atomic.Int32{}
	mu := &sync.Mutex{}
	step1 := &recordingSagaStep{name: "step_one", mu: mu, seq: tick, rec: rec}
	step2 := &recordingSagaStep{name: "step_two", mu: mu, seq: tick, rec: rec}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})

	callerSlice := []saga.Step{step1, step2}
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
		Steps:   callerSlice,
	})

	// Mutate the caller's slice AFTER construction. A non-defensive
	// copy would surface this mutation in the next saga run.
	swapped := &recordingSagaStep{name: "swapped_step", mu: mu, seq: tick, rec: rec}
	callerSlice[0] = swapped

	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testRetireClaim(), "tok-defcopy"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	got := rec.recorded()
	if len(got) != 2 {
		t.Fatalf("recorded steps = %d, want 2", len(got))
	}
	if got[0].stepName != "step_one" {
		t.Errorf("step[0] name = %q, want %q (caller post-mutation must NOT bleed into saga run)", got[0].stepName, "step_one")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Happy path — zero-step.
// ────────────────────────────────────────────────────────────────────────

// TestRetireKickoff_HappyPath_EmitsTwoEventsInOrder pins the zero-step
// audit chain: 1× `retire_approved_for_watchkeeper` + 1×
// `saga_completed`, in that order. Mirrors the M7.1.b spawn-kickoff
// happy path; the saga.Runner is the same (M7.1.a) so the saga-side
// of the chain is identical.
func TestRetireKickoff_HappyPath_EmitsTwoEventsInOrder(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot-watchmaster")

	sagaID := uuid.New()
	mvID := uuid.New()
	token := strings.Repeat("ZZ", 16)

	if err := retireKickoffWithDefaults(context.Background(), k, sagaID, mvID, token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	row, err := dao.Get(context.Background(), sagaID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q", row.Status, saga.SagaStateCompleted)
	}
	if row.ManifestVersionID != mvID {
		t.Errorf("ManifestVersionID = %q, want %q", row.ManifestVersionID, mvID)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Insert-before-Run ordering — same shape as M7.1.b.
// ────────────────────────────────────────────────────────────────────────

func TestRetireKickoff_InsertPrecedesRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertIfAbsentSeq) != 1 {
		t.Fatalf("InsertIfAbsent call count = %d, want 1", len(rec.insertIfAbsentSeq))
	}
	if len(rec.getSeq) != 1 {
		t.Fatalf("Get call count = %d, want 1 (saga.Runner.Run resolves the row first)", len(rec.getSeq))
	}
	if rec.insertIfAbsentSeq[0] >= rec.getSeq[0] {
		t.Errorf("InsertIfAbsent.seq=%d, Get.seq=%d — InsertIfAbsent MUST precede Run (Get is the runner's first action)",
			rec.insertIfAbsentSeq[0], rec.getSeq[0])
	}
	if len(rec.insertSeq) != 0 {
		t.Errorf("Insert (legacy) call count = %d, want 0 (M7.3.a routes through InsertIfAbsent)", len(rec.insertSeq))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative paths — append + insert error propagation.
// ────────────────────────────────────────────────────────────────────────

func TestRetireKickoff_AppendError_StopsBeforeRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	appendErr := errors.New("keep client offline")
	keep := &fakeLocalKeepClient{errToReturn: appendErr}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test")
	if err == nil {
		t.Fatal("Kickoff returned nil, want non-nil error")
	}
	if !errors.Is(err, appendErr) {
		t.Errorf("errors.Is(err, appendErr) = false; want wrapped chain (got %v)", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertIfAbsentSeq) != 1 {
		t.Errorf("InsertIfAbsent calls = %d, want 1 (decides insert-vs-replay before Append)", len(rec.insertIfAbsentSeq))
	}
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after audit failure)", len(rec.getSeq))
	}
}

func TestRetireKickoff_InsertIfAbsentError_NoAuditRow(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	rec.insertIfAbsentErr = errors.New("postgres unreachable")
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test")
	if err == nil {
		t.Fatal("Kickoff returned nil, want non-nil error")
	}
	if !errors.Is(err, rec.insertIfAbsentErr) {
		t.Errorf("errors.Is(err, insertIfAbsentErr) = false; want wrapped chain (got %v)", err)
	}

	if got := keep.eventTypes(); len(got) != 0 {
		t.Errorf("event_type chain = %v, want empty (transient DAO error must not leak partial audit)", got)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after InsertIfAbsent failure)", len(rec.getSeq))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Fail-fast: uuid.Nil arguments rejected BEFORE Append/Insert.
// ────────────────────────────────────────────────────────────────────────

func TestRetireKickoff_FailsFastOnNilArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		sagaID            uuid.UUID
		manifestVersionID uuid.UUID
		watchkeeperID     uuid.UUID
		approvalToken     string
	}{
		{
			name:              "nil sagaID",
			sagaID:            uuid.Nil,
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.New(),
			approvalToken:     "tok-fail-fast",
		},
		{
			name:              "nil manifestVersionID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.Nil,
			watchkeeperID:     uuid.New(),
			approvalToken:     "tok-fail-fast",
		},
		{
			name:              "nil watchkeeperID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.Nil,
			approvalToken:     "tok-fail-fast",
		},
		{
			// M7.3.a: empty approvalToken would silently bypass the
			// idempotency dedup at the DAO layer. Mirrors the spawn-side
			// fail-fast pin.
			name:              "empty approvalToken",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.New(),
			approvalToken:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecordingDAO()
			keep := &fakeLocalKeepClient{}
			writer := keeperslog.New(keep)
			runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
			k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
				Logger:  writer,
				DAO:     rec,
				Runner:  runner,
				AgentID: "bot",
			})

			err := k.Kickoff(context.Background(), tc.sagaID, tc.manifestVersionID, tc.watchkeeperID, testRetireClaim(), tc.approvalToken)
			if err == nil {
				t.Fatalf("Kickoff: err = nil, want wrapped ErrInvalidKickoffArgs")
			}
			if !errors.Is(err, spawn.ErrInvalidKickoffArgs) {
				t.Errorf("errors.Is(err, ErrInvalidKickoffArgs) = false; got %v", err)
			}

			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keep rows = %d, want 0 (fail-fast must precede audit)", len(got))
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.insertIfAbsentSeq) != 0 {
				t.Errorf("InsertIfAbsent calls = %d, want 0 (fail-fast must precede persistence)", len(rec.insertIfAbsentSeq))
			}
			if len(rec.getSeq) != 0 {
				t.Errorf("Get calls = %d, want 0 (Run must not fire on fail-fast)", len(rec.getSeq))
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// M7.3.a — idempotency replay flow (retire-side mirror of the
// spawn-side replay tests).
// ────────────────────────────────────────────────────────────────────────

func TestRetireKickoff_ReplayedCall_EmitsReplayedEvent_NoSecondRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot-watchmaster",
	})

	firstSagaID := uuid.New()
	secondSagaID := uuid.New()
	mvID := uuid.New()
	//nolint:gosec // G101: synthetic test idempotency-key constant, not a real credential.
	const token = "tok-retire-replay-dedup"

	if err := retireKickoffWithDefaults(context.Background(), k, firstSagaID, mvID, token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	if err := retireKickoffWithDefaults(context.Background(), k, secondSagaID, mvID, token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaCompleted,
		spawn.EventTypeRetireApprovalReplayedForWatchkeeper,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertIfAbsentSeq) != 2 {
		t.Errorf("InsertIfAbsent calls = %d, want 2 (one per Kickoff)", len(rec.insertIfAbsentSeq))
	}
	if !equalStrings(rec.insertIfAbsentKeys, []string{token, token}) {
		t.Errorf("InsertIfAbsent idempotency_keys = %v, want both %q", rec.insertIfAbsentKeys, token)
	}
	if len(rec.getSeq) != 1 {
		t.Errorf("Get calls = %d, want 1 (Runner.Run fired only on 1st Kickoff)", len(rec.getSeq))
	}
}

func TestRetireKickoff_ReplayedCall_PayloadShape(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
	})

	firstSagaID := uuid.New()
	mvID := uuid.New()
	token := strings.Repeat("YY", 16)

	if err := retireKickoffWithDefaults(context.Background(), k, firstSagaID, mvID, token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), mvID, token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	replayRow := findEventRow(t, keep, spawn.EventTypeRetireApprovalReplayedForWatchkeeper)
	data := decodePayloadData(t, replayRow.Payload)

	assertReplayedPayloadKeys(t, data, []string{
		"saga_id", "manifest_version_id", "watchkeeper_id",
		"approval_token_prefix", "agent_id", "previous_status",
	})
	assertReplayedPayloadPII(t, replayRow.Payload, token)

	if got := data["previous_status"]; got != string(saga.SagaStateCompleted) {
		t.Errorf("previous_status = %v, want %q", got, saga.SagaStateCompleted)
	}
	if got := data["saga_id"]; got != firstSagaID.String() {
		t.Errorf("saga_id = %v, want first-call %q", got, firstSagaID)
	}
}

// TestRetireKickoff_ReplayedPayload_UsesExistingIDs pins codex iter-1
// Major (retire-side mirror of the spawn-side test): the replayed
// payload sources `manifest_version_id` and `watchkeeper_id` from
// the existing saga row, NOT the second-call's discarded args.
func TestRetireKickoff_ReplayedPayload_UsesExistingIDs(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
	})

	firstSagaID := uuid.New()
	firstMvID := uuid.New()
	firstWkID := uuid.New()
	const token = "tok-retire-existing-ids"

	if err := k.Kickoff(context.Background(), firstSagaID, firstMvID, firstWkID, testRetireClaim(), token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	secondSagaID := uuid.New()
	secondMvID := uuid.New()
	secondWkID := uuid.New()
	if err := k.Kickoff(context.Background(), secondSagaID, secondMvID, secondWkID, testRetireClaim(), token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	rows := keep.recorded()
	var replayRow *keepclient.LogAppendRequest
	for i := range rows {
		if rows[i].EventType == spawn.EventTypeRetireApprovalReplayedForWatchkeeper {
			replayRow = &rows[i]
			break
		}
	}
	if replayRow == nil {
		t.Fatalf("no replay row in %v", keep.eventTypes())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(replayRow.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}

	if got := data["saga_id"]; got != firstSagaID.String() {
		t.Errorf("saga_id = %v, want FIRST-call %q (not second-call %q)", got, firstSagaID, secondSagaID)
	}
	if got := data["manifest_version_id"]; got != firstMvID.String() {
		t.Errorf("manifest_version_id = %v, want FIRST-call %q (not second-call %q)", got, firstMvID, secondMvID)
	}
	if got := data["watchkeeper_id"]; got != firstWkID.String() {
		t.Errorf("watchkeeper_id = %v, want FIRST-call %q (not second-call %q)", got, firstWkID, secondWkID)
	}
}

// TestRetireKickoff_PendingStatus_CatchUpResumesSaga pins codex iter-1
// Critical (retire-side mirror): a pending saga left by a partial
// first-call resumes on the next Kickoff via the catch-up branch.
func TestRetireKickoff_PendingStatus_CatchUpResumesSaga(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})

	firstSagaID := uuid.New()
	firstMvID := uuid.New()
	firstWkID := uuid.New()
	const token = "tok-retire-catch-up"
	if _, err := dao.InsertIfAbsent(context.Background(), firstSagaID, firstMvID, firstWkID, token); err != nil {
		t.Fatalf("seed InsertIfAbsent: %v", err)
	}
	if got, _ := dao.Get(context.Background(), firstSagaID); got.Status != saga.SagaStatePending {
		t.Fatalf("seeded saga status = %q, want %q", got.Status, saga.SagaStatePending)
	}

	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
	})
	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), token); err != nil {
		t.Fatalf("retry Kickoff (catch-up): %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v (catch-up emits original audit chain)", got, wantEvents)
	}

	got, err := dao.Get(context.Background(), firstSagaID)
	if err != nil {
		t.Fatalf("Get firstSagaID: %v", err)
	}
	if got.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q (catch-up resumes Run)", got.Status, saga.SagaStateCompleted)
	}
	if got.WatchkeeperID != firstWkID {
		t.Errorf("Persisted WatchkeeperID = %v, want first-call %v", got.WatchkeeperID, firstWkID)
	}
}

func TestEventTypeRetireApprovalReplayedForWatchkeeper_NoPrefixCollision(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeRetireApprovalReplayedForWatchkeeper
	const want = "retire_approval_replayed_for_watchkeeper"
	if et != want {
		t.Errorf("event_type = %q, want exact %q", et, want)
	}
	if strings.HasPrefix(et, "llm_turn_cost") {
		t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
	}
	if strings.HasPrefix(et, "saga_") {
		t.Errorf("event_type %q has forbidden saga_ prefix (M7.1.a)", et)
	}
	if strings.HasPrefix(et, "manifest_") {
		t.Errorf("event_type %q has forbidden manifest_ prefix (M7.1.b spawn-family)", et)
	}
	if strings.HasPrefix(et, "watchmaster_retire_watchkeeper") {
		t.Errorf("event_type %q has forbidden watchmaster_retire_watchkeeper prefix (M6.2.c synchronous tool)", et)
	}
	if et == spawn.EventTypeRetireApprovedForWatchkeeper {
		t.Errorf("event_type %q must differ from EventTypeRetireApprovedForWatchkeeper", et)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security — token-prefix discipline + closed-set payload (M2b.7).
// ────────────────────────────────────────────────────────────────────────

// parseRetireKickoffPayload extracts the `data` envelope from the
// first recorded keep row and returns it as a map. Hoisted so the
// payload-shape + payload-PII tests stay focused on assertions
// rather than JSON parsing boilerplate (and so each test stays
// under the gocyclo=15 ceiling enforced by the project's
// golangci-lint config).
func parseRetireKickoffPayload(t *testing.T, keep *fakeLocalKeepClient) (keepclient.LogAppendRequest, map[string]any) {
	t.Helper()
	rows := keep.recorded()
	if len(rows) == 0 {
		t.Fatalf("recorded rows = 0; expected the audit chain")
	}
	row := rows[0]
	if row.EventType != spawn.EventTypeRetireApprovedForWatchkeeper {
		t.Fatalf("row[0].EventType = %q, want %q", row.EventType, spawn.EventTypeRetireApprovedForWatchkeeper)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}
	return row, data
}

// TestRetireKickoff_PayloadClosedSet pins the closed-set keys of the
// `retire_approved_for_watchkeeper` payload: exactly
// {manifest_version_id, watchkeeper_id, approval_token_prefix,
// agent_id} — no extras, no missing.
func TestRetireKickoff_PayloadClosedSet(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot-watchmaster")

	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testRetireClaim(), "tok-closed-set"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	_, data := parseRetireKickoffPayload(t, keep)

	allowed := map[string]bool{
		"manifest_version_id":   true,
		"watchkeeper_id":        true,
		"approval_token_prefix": true,
		"agent_id":              true,
	}
	if len(data) != len(allowed) {
		t.Errorf("payload key count = %d, want %d (keys=%v)", len(data), len(allowed), data)
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("payload contains forbidden key %q (allowed: manifest_version_id, watchkeeper_id, approval_token_prefix, agent_id)", k)
		}
	}
	for k := range allowed {
		if _, ok := data[k]; !ok {
			t.Errorf("payload missing required key %q", k)
		}
	}
}

// TestRetireKickoff_PayloadValues pins the data values pass through
// verbatim: watchkeeper_id, manifest_version_id, agent_id all match
// the per-call inputs.
func TestRetireKickoff_PayloadValues(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	const agentID = "bot-watchmaster"
	k := newRetireKickoffer(t, dao, keep, agentID)

	mvID := uuid.New()
	wkID := uuid.New()
	if err := k.Kickoff(context.Background(), uuid.New(), mvID, wkID, testRetireClaim(), "tok-values"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	_, data := parseRetireKickoffPayload(t, keep)
	if got, _ := data["watchkeeper_id"].(string); got != wkID.String() {
		t.Errorf("watchkeeper_id = %q, want %q", got, wkID.String())
	}
	if got, _ := data["manifest_version_id"].(string); got != mvID.String() {
		t.Errorf("manifest_version_id = %q, want %q", got, mvID.String())
	}
	if got, _ := data["agent_id"].(string); got != agentID {
		t.Errorf("agent_id = %q, want %q", got, agentID)
	}
}

// TestRetireKickoff_PayloadPIIRedaction pins the M2b.7 PII discipline:
// the full approval_token MUST NOT appear in the raw payload bytes;
// the `approval_token` (full) and `error` JSON keys MUST be absent.
func TestRetireKickoff_PayloadPIIRedaction(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot-watchmaster")

	token := strings.Repeat("ZZ", 16)
	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testRetireClaim(), token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	row, _ := parseRetireKickoffPayload(t, keep)
	rawPayload := string(row.Payload)
	if strings.Contains(rawPayload, token) {
		t.Errorf("payload leaked full approval_token: %s", rawPayload)
	}
	if strings.Contains(rawPayload, `"approval_token"`) {
		t.Errorf("payload contains forbidden `approval_token` key: %s", rawPayload)
	}
	if strings.Contains(rawPayload, `"error"`) {
		t.Errorf("payload contains forbidden `error` key: %s", rawPayload)
	}
}

// TestRetireKickoff_EmptyApprovalToken_RejectedAtFailFast pins the
// M7.3.a inversion of the M7.2.a "kickoffer does not validate
// approvalToken" contract. The token now serves as the
// idempotency_key for [saga.SpawnSagaDAO.InsertIfAbsent]; an empty
// key would silently bypass the partial UNIQUE-WHERE-NOT-NULL dedup
// index, so the kickoffer rejects empty tokens at fail-fast (BEFORE
// audit / state-write side effects). The "bare tok- prefix payload
// shape" the M7.2.a iter-1 lesson documented is now structurally
// unreachable on the production path.
func TestRetireKickoff_EmptyApprovalToken_RejectedAtFailFast(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot")

	err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "")
	if err == nil {
		t.Fatal("Kickoff returned nil, want wrapped ErrInvalidKickoffArgs")
	}
	if !errors.Is(err, spawn.ErrInvalidKickoffArgs) {
		t.Errorf("errors.Is(err, ErrInvalidKickoffArgs) = false; got %v", err)
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keep rows = %d, want 0 (fail-fast must precede audit)", len(got))
	}
}

// TestRetireKickoff_ZeroValueKickoffer_Panics pins the doc-block claim
// "the zero value is NOT usable" by exercising it directly. A
// `RetireKickoffer{}` has nil logger / DAO / runner; calling Kickoff
// against it MUST panic rather than returning a misleading error.
// Without this test, the doc-block could silently rot if a future
// maintainer e.g. adds a defensive nil-check inside Kickoff that
// silently no-ops instead of panicking.
func TestRetireKickoff_ZeroValueKickoffer_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("zero-value RetireKickoffer{}.Kickoff did not panic")
		}
	}()
	var k spawn.RetireKickoffer
	_ = k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testRetireClaim(), "tok-zero")
}

func TestRetireKickoff_TokenPrefixDiscipline(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot")

	fullToken := strings.Repeat("ZZ", 16)
	const wantPrefix = "tok-ZZZZZZ"

	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), fullToken); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	row := keep.recorded()[0]
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}
	got, _ := data["approval_token_prefix"].(string)
	if got != wantPrefix {
		t.Errorf("approval_token_prefix = %q, want %q", got, wantPrefix)
	}
	if got == fullToken {
		t.Errorf("approval_token_prefix equals full token — token-prefix discipline failed")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Event-type vocabulary discipline.
// ────────────────────────────────────────────────────────────────────────

// TestEventTypeRetireApprovedForWatchkeeper_NoPrefixCollision pins the
// closed-set vocabulary: the kickoff event_type must equal exactly
// `retire_approved_for_watchkeeper` AND must NOT collide with the
// `manifest_*` (M7.1.b), `saga_*` (M7.1.a), `llm_turn_cost_*` (M6.3.e),
// `notebook_*` (M2b), or `watchmaster_retire_watchkeeper_*` (M6.2.c)
// families.
func TestEventTypeRetireApprovedForWatchkeeper_NoPrefixCollision(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeRetireApprovedForWatchkeeper
	const want = "retire_approved_for_watchkeeper"
	if et != want {
		t.Errorf("event_type = %q, want exact %q", et, want)
	}
	if strings.HasPrefix(et, "manifest_") {
		t.Errorf("event_type %q has forbidden manifest_ prefix (M7.1.b)", et)
	}
	if strings.HasPrefix(et, "saga_") {
		t.Errorf("event_type %q has forbidden saga_ prefix (M7.1.a)", et)
	}
	if strings.HasPrefix(et, "llm_turn_cost") {
		t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
	}
	if strings.HasPrefix(et, "notebook_") {
		t.Errorf("event_type %q has forbidden notebook_ prefix (M2b)", et)
	}
	if strings.HasPrefix(et, "watchmaster_retire_watchkeeper_") {
		t.Errorf("event_type %q collides with the M6.2.c synchronous retire tool family", et)
	}
}

// TestRetireKickoff_DoesNotEmitSpawnFamilyEvents pins the vocabulary
// boundary in the OTHER direction: the retire kickoff MUST NOT emit
// any `manifest_approved_for_spawn` or any other `manifest_*` event.
// Defense-in-depth for a future maintainer who edits the constant
// and accidentally aliases retire onto the spawn family.
func TestRetireKickoff_DoesNotEmitSpawnFamilyEvents(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newRetireKickoffer(t, dao, keep, "bot")

	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	for _, et := range keep.eventTypes() {
		if et == spawn.EventTypeManifestApprovedForSpawn {
			t.Errorf("retire kickoff emitted spawn-family event %q", et)
		}
		if strings.HasPrefix(et, "manifest_") {
			t.Errorf("retire kickoff emitted forbidden manifest_-prefixed event %q", et)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// SpawnContext seeded on ctx; multi-step happy path.
// ────────────────────────────────────────────────────────────────────────

// TestRetireKickoff_RegisteredSteps_RunInOrderWithSeededContext mirrors
// the M7.1.c.c contract for the retire kickoff: the kickoffer hands
// [saga.Runner.Run] its registered step list; each step sees a
// [saga.SpawnContext] seeded with the per-call manifest_version id,
// watchkeeperID (=AgentID), and Watchmaster claim. M7.2.b
// NotebookArchive + M7.2.c MarkRetired will lean on this contract.
func TestRetireKickoff_RegisteredSteps_RunInOrderWithSeededContext(t *testing.T) {
	t.Parallel()

	rec := newRecordingStepLog()
	tick := &atomic.Int32{}
	mu := &sync.Mutex{}
	step1 := &recordingSagaStep{name: "notebook_archive", mu: mu, seq: tick, rec: rec}
	step2 := &recordingSagaStep{name: "mark_retired", mu: mu, seq: tick, rec: rec}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
		Steps:   []saga.Step{step1, step2},
	})

	sagaID := uuid.New()
	mvID := uuid.New()
	wkID := uuid.New()
	claim := testRetireClaim()
	const token = "tok-retirestep"

	if err := k.Kickoff(context.Background(), sagaID, mvID, wkID, claim, token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	entries := rec.recorded()
	if len(entries) != 2 {
		t.Fatalf("recorded step entries = %d, want 2", len(entries))
	}

	wantNames := []string{"notebook_archive", "mark_retired"}
	for i, want := range wantNames {
		if entries[i].stepName != want {
			t.Errorf("step #%d name = %q, want %q", i, entries[i].stepName, want)
		}
	}
	if entries[0].tickOrder >= entries[1].tickOrder {
		t.Errorf("step tick order = %v, want strictly ascending", []int32{entries[0].tickOrder, entries[1].tickOrder})
	}

	for _, entry := range entries {
		if !entry.hadContext {
			t.Errorf("%s: SpawnContextFromContext(ctx) ok = false; want true", entry.stepName)
			continue
		}
		if entry.spawnContext.ManifestVersionID != mvID {
			t.Errorf("%s: SpawnContext.ManifestVersionID = %q, want %q",
				entry.stepName, entry.spawnContext.ManifestVersionID, mvID)
		}
		if entry.spawnContext.AgentID != wkID {
			t.Errorf("%s: SpawnContext.AgentID = %q, want %q (watchkeeperID)",
				entry.stepName, entry.spawnContext.AgentID, wkID)
		}
		if entry.spawnContext.Claim.OrganizationID != claim.OrganizationID {
			t.Errorf("%s: SpawnContext.Claim.OrganizationID = %q, want %q",
				entry.stepName, entry.spawnContext.Claim.OrganizationID, claim.OrganizationID)
		}
		if entry.spawnContext.Claim.AuthorityMatrix["retire_watchkeeper"] != "lead_approval" {
			t.Errorf("%s: SpawnContext.Claim.AuthorityMatrix[retire_watchkeeper] = %q, want lead_approval",
				entry.stepName, entry.spawnContext.Claim.AuthorityMatrix["retire_watchkeeper"])
		}
		// Pin that the spawn-only OAuthCode field is left zero on
		// the retire flow (M7.2.a iter-1: a step author who copy-
		// pastes from M7.1.c.b.b OAuthInstallStep would otherwise
		// silently read an empty string from a field they should
		// not be consulting for retire).
		if entry.spawnContext.OAuthCode != "" {
			t.Errorf("%s: SpawnContext.OAuthCode = %q, want \"\" (retire flow does not seed OAuthCode)",
				entry.stepName, entry.spawnContext.OAuthCode)
		}
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

func TestRetireKickoff_NilSteps_RetainsZeroStepBehaviour(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
	})

	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-nil"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// TestRetireKickoff_StepFailure_AuditsSagaFailed pins that a registered
// step returning an error surfaces as `saga_failed` on the audit chain
// AND the kickoffer returns the wrapped error.
func TestRetireKickoff_StepFailure_AuditsSagaFailed(t *testing.T) {
	t.Parallel()

	stepErr := errors.New("simulated retire-step failure")
	failing := &errSagaStep{name: "boom_retire", err: stepErr}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
		Steps:   []saga.Step{failing},
	})

	err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testRetireClaim(), "tok-fail")
	if err == nil {
		t.Fatal("Kickoff returned nil, want wrapped step error")
	}
	if !errors.Is(err, stepErr) {
		t.Errorf("errors.Is(err, stepErr) = false; got %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaFailed,
		// M7.3.b: the saga runner emits a `saga_compensated`
		// summary row on every failure path (zero per-step
		// rows here because the failing step is a non-Compensator
		// stub). The retire kickoffer does NOT emit a per-saga
		// rejection event in M7.3.b — that surface is spawn-side
		// only per the roadmap; a future M7.4-or-equivalent
		// retire-rollback emit is out of scope for M7.3.b.
		saga.EventTypeSagaCompensated,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Context propagation pin via recording fake (M7.1.e lesson #6).
// ────────────────────────────────────────────────────────────────────────

// TestRetireKickoff_CtxForwardedVerbatim pins that the kickoffer
// forwards the caller's `ctx` to the [keeperslog.Writer.Append] seam
// VERBATIM, rather than substituting `context.Background()` /
// `context.WithoutCancel(ctx)` / a fresh ctx.
//
// Recording shape (M7.1.e lesson #6 held forward): the fake
// keep-client records the per-call ctx; the test cancels the parent
// AFTER the synchronous Kickoff returns; if the kickoffer forwarded
// `ctx` verbatim, the recorded ctx is the same value (or a child) and
// observes the cancellation via `ctx.Err() == context.Canceled`. A
// future regression that strips ctx (`Append(context.Background(),
// ...)`) would leave the recorded ctx un-cancellable and fail the
// assertion.
func TestRetireKickoff_CtxForwardedVerbatim(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
	})

	ctx, cancel := context.WithCancel(context.Background())

	if err := retireKickoffWithDefaults(ctx, k, uuid.New(), uuid.New(), "tok-ctx"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	// Cancel the parent ctx AFTER the synchronous Kickoff returns; if
	// the kickoffer forwarded ctx verbatim, the recorded ctx now
	// observes the cancellation.
	cancel()

	ctxs := keep.recordedCtxs()
	if len(ctxs) == 0 {
		t.Fatalf("recordedCtxs = 0; expected at least one keep.LogAppend call")
	}
	if !errors.Is(ctxs[0].Err(), context.Canceled) {
		t.Errorf("recordedCtxs[0].Err() = %v, want context.Canceled (kickoffer must forward ctx verbatim through the keeperslog.Append seam)", ctxs[0].Err())
	}
}

// TestRetireKickoff_PreCancelledCtx_StillCompletes documents the
// EXISTING contract of a pre-cancelled ctx against the in-memory
// DAO: the kickoffer does NOT short-circuit on `ctx.Err()`, so the
// audit row IS emitted and the saga reaches the completed state.
// This matches the M7.1.b SpawnKickoffer behaviour (neither kickoffer
// adds a `ctx.Err()` gate at the top) and the in-memory DAO (which
// also does not honour ctx). The test pins the contract so a future
// maintainer who edits the kickoffer to add a `ctx.Err()` gate WILL
// fail the test, surfacing the contract change for review.
//
// Production-side note: the keep client's HTTP layer + the future
// Postgres saga DAO do honour ctx, so a pre-cancelled ctx in
// production short-circuits at the first network/DB call. The unit
// test against in-memory fakes captures the LOCAL behaviour only.
func TestRetireKickoff_PreCancelledCtx_StillCompletes(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := retireKickoffWithDefaults(ctx, k, uuid.New(), uuid.New(), "tok-pre-cancel"); err != nil {
		t.Fatalf("Kickoff: %v (in-memory fakes do not honour ctx; expect nil)", err)
	}
	wantEvents := []string{
		spawn.EventTypeRetireApprovedForWatchkeeper,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Errorf("event_type chain = %v, want %v (in-memory contract: no ctx short-circuit)", got, wantEvents)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency stress — 16 goroutines, race detector verifies safety.
// ────────────────────────────────────────────────────────────────────────

// TestRetireKickoff_ConcurrentKickoffs pins the kickoffer's
// concurrency contract (mirrors the M7.1.c.c 16-goroutine pattern):
// 16 goroutines invoke Kickoff with distinct saga ids AND distinct
// approval_tokens (each kickoff is a unique upstream retire request,
// not 16 replays of the same one) on a shared kickoffer. Race
// detector verifies no shared-state corruption; the audit chain
// count must equal 16 × (1 retire_approved + 1 saga_completed) =
// 32 events. Concurrent replays of the SAME token are exercised in
// the M7.3.a-introduced replay-flow test instead — a kickoff under
// distinct tokens MUST still produce 16 independent sagas.
func TestRetireKickoff_ConcurrentKickoffs(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
	})

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		token := "tok-concurrent-" + uuid.New().String()
		go func(tok string) {
			defer wg.Done()
			if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), tok); err != nil {
				errCh <- err
			}
		}(token)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Kickoff: %v", err)
	}

	rows := keep.recorded()
	if len(rows) != goroutines*2 {
		t.Errorf("audit row count = %d, want %d (16 × (retire_approved + saga_completed))",
			len(rows), goroutines*2)
	}

	// Every retire_approved row must be matched by a saga_completed
	// row downstream — the per-saga event chain stays coherent under
	// concurrent kickoffs.
	var retireCount, completedCount int
	for _, r := range rows {
		switch r.EventType {
		case spawn.EventTypeRetireApprovedForWatchkeeper:
			retireCount++
		case saga.EventTypeSagaCompleted:
			completedCount++
		}
	}
	if retireCount != goroutines || completedCount != goroutines {
		t.Errorf("retire_approved=%d, saga_completed=%d; want %d / %d",
			retireCount, completedCount, goroutines, goroutines)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Source-grep AC — pin the lean-seam claim by reading retirekickoff.go
// off disk and asserting forbidden imports / shortcuts are absent.
// ────────────────────────────────────────────────────────────────────────

// TestRetireKickoff_LeanSeamSourceGrepAC reads retirekickoff.go and
// asserts the file does NOT take a shortcut by importing substrate
// packages directly (notebook, archivestore, runtime, messenger).
// The kickoffer's contract is "compose the saga; substrate concerns
// belong to the registered steps". A future maintainer who imports
// `notebook` here to call `ArchiveOnRetire` directly (skipping the
// step layer) is the regression this AC pins. Mirrors the M7.1.d /
// M7.1.e source-grep pattern.
func TestRetireKickoff_LeanSeamSourceGrepAC(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve test file path")
	}
	dir := filepath.Dir(thisFile)
	target := filepath.Join(dir, "retirekickoff.go")
	src, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}

	forbidden := []string{
		"core/pkg/notebook",
		"core/pkg/archivestore",
		"core/pkg/runtime",
		"core/pkg/messenger",
		// M7.2.a iter-1 additions: pin against the most likely
		// future regression vectors the lesson narrative implicitly
		// forbids — the synchronous M6.2.c retire substrate
		// (harnessrpc), the watchkeeper-status flip layer that
		// M7.2.c will eventually bridge through (lifecycle), and
		// the M7.1.c.b.a crypto primitive that has no business
		// inside a kickoffer (secrets).
		"core/pkg/harnessrpc",
		"core/pkg/lifecycle",
		"core/pkg/secrets",
		"WatchkeeperSlackAppCredsDAO",
	}
	for _, sub := range forbidden {
		if containsOutsideComments(string(src), sub) {
			t.Errorf("retirekickoff.go contains forbidden substring %q (substrate concerns belong to saga steps, not the kickoffer)", sub)
		}
	}
}

// containsOutsideComments returns true when `needle` appears in `src`
// on a line that is NOT a `//`-prefixed comment line. Whitespace-only
// prefixes are tolerated (Go test fixtures indent comments).
func containsOutsideComments(src, needle string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// ────────────────────────────────────────────────────────────────────────
// M7.2.b RetireResult seeding pin — every retire saga's per-call ctx
// MUST carry a fresh, non-nil [saga.RetireResult] pointer when the
// kickoffer's registered steps execute. This is what makes the M7.2.b
// NotebookArchive step's URI publish reach the M7.2.c MarkRetired step.
// ────────────────────────────────────────────────────────────────────────

// retireResultProbeStep is a hand-rolled [saga.Step] that captures the
// [saga.RetireResult] pointer (and its presence boolean) it observes
// off the per-call ctx. The pointer is recorded — NOT a snapshot of
// its fields — so a downstream test can mutate via the captured
// pointer and re-read to confirm pointer-identity-stability across
// step boundaries (mirrors the M7.2.b NotebookArchive→MarkRetired
// inter-step write-then-read protocol).
type retireResultProbeStep struct {
	name       string
	mu         sync.Mutex
	captured   *saga.RetireResult
	hadResult  bool
	executed   bool
	mutateURI  string // when non-empty, Execute writes this to captured.ArchiveURI
	executions atomic.Int32
}

func (s *retireResultProbeStep) Name() string { return s.name }

func (s *retireResultProbeStep) Execute(ctx context.Context) error {
	s.executions.Add(1)
	r, ok := saga.RetireResultFromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = r
	s.hadResult = ok
	s.executed = true
	if s.mutateURI != "" && r != nil {
		r.ArchiveURI = s.mutateURI
	}
	return nil
}

// TestRetireKickoff_SeedsFreshRetireResultOnCtx pins the M7.2.b
// kickoffer-side seeding contract: every Kickoff stamps a fresh,
// non-nil [saga.RetireResult] onto the per-call ctx before
// dispatching the registered step list. Without this seeding, the
// M7.2.b NotebookArchive step has nowhere to publish its archive_uri
// for the M7.2.c MarkRetired step to consume; the step would
// fail-closed with [spawn.ErrMissingRetireResult].
func TestRetireKickoff_SeedsFreshRetireResultOnCtx(t *testing.T) {
	t.Parallel()

	probe := &retireResultProbeStep{name: "retire_result_probe"}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
		Steps:   []saga.Step{probe},
	})

	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-seed"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !probe.executed {
		t.Fatal("probe step did not execute")
	}
	if !probe.hadResult {
		t.Errorf("RetireResultFromContext(ctx) ok = false; want true (kickoffer must seed RetireResult before Run)")
	}
	if probe.captured == nil {
		t.Errorf("captured RetireResult = nil; want non-nil (kickoffer must stamp a fresh &RetireResult{} per Kickoff)")
	}
	if probe.captured != nil && probe.captured.ArchiveURI != "" {
		t.Errorf("captured.ArchiveURI = %q; want \"\" (the seeded outbox must be a fresh zero-value, not carry stale data)",
			probe.captured.ArchiveURI)
	}
}

// TestRetireKickoff_SeedsDistinctRetireResultPerKickoff pins
// per-call isolation: two Kickoffs against the same kickoffer must
// observe DISTINCT [saga.RetireResult] pointers. A regression that
// stamps a single shared pointer at construction time would leak
// state across concurrent retire sagas — Kickoff-N's archive_uri
// would land on Kickoff-(N-1)'s outbox.
func TestRetireKickoff_SeedsDistinctRetireResultPerKickoff(t *testing.T) {
	t.Parallel()

	probe := &retireResultProbeStep{name: "retire_result_isolation_probe"}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
		Steps:   []saga.Step{probe},
	})

	// Kickoff #1: probe captures an outbox + writes a sentinel URI.
	probe.mutateURI = "file:///k1.tar.gz"
	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-k1"); err != nil {
		t.Fatalf("Kickoff #1: %v", err)
	}
	probe.mu.Lock()
	first := probe.captured
	probe.mu.Unlock()
	if first == nil {
		t.Fatal("Kickoff #1 captured nil RetireResult")
	}

	// Kickoff #2: probe captures a SECOND outbox; pointer identity
	// MUST differ from the first.
	probe.mutateURI = "file:///k2.tar.gz"
	if err := retireKickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-k2"); err != nil {
		t.Fatalf("Kickoff #2: %v", err)
	}
	probe.mu.Lock()
	second := probe.captured
	probe.mu.Unlock()
	if second == nil {
		t.Fatal("Kickoff #2 captured nil RetireResult")
	}
	if first == second {
		t.Errorf("Kickoff #1 and Kickoff #2 captured the same *RetireResult (%p) — kickoffer must seed a fresh outbox per call to isolate concurrent sagas", first)
	}
	// First outbox carries Kickoff #1's URI; second carries Kickoff #2's.
	if first.ArchiveURI != "file:///k1.tar.gz" {
		t.Errorf("first.ArchiveURI = %q, want %q (Kickoff #2 leaked a write back into Kickoff #1's outbox)",
			first.ArchiveURI, "file:///k1.tar.gz")
	}
	if second.ArchiveURI != "file:///k2.tar.gz" {
		t.Errorf("second.ArchiveURI = %q, want %q", second.ArchiveURI, "file:///k2.tar.gz")
	}
}
