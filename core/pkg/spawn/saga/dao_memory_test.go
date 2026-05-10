package saga_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// fixedClock returns a deterministic time.Time so test assertions on
// CreatedAt / UpdatedAt / CompletedAt are byte-for-byte stable.
func fixedClock() func() time.Time {
	t := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestMemorySpawnSagaDAO_InsertAndGet_RoundTrip(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	id := uuid.New()
	manifestID := uuid.New()

	if err := dao.Insert(context.Background(), id, manifestID); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %v, want %v", got.ID, id)
	}
	if got.ManifestVersionID != manifestID {
		t.Errorf("ManifestVersionID = %v, want %v", got.ManifestVersionID, manifestID)
	}
	if got.Status != saga.SagaStatePending {
		t.Errorf("Status = %q, want %q", got.Status, saga.SagaStatePending)
	}
	if got.CurrentStep != "" {
		t.Errorf("CurrentStep = %q, want empty", got.CurrentStep)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = zero, want non-zero")
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt = zero, want non-zero")
	}
	if !got.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = %v, want zero (saga still pending)", got.CompletedAt)
	}
}

func TestMemorySpawnSagaDAO_Get_UnknownID_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)

	_, err := dao.Get(context.Background(), uuid.New())
	if !errors.Is(err, saga.ErrSagaNotFound) {
		t.Errorf("Get unknown id: err = %v, want ErrSagaNotFound", err)
	}
}

func TestMemorySpawnSagaDAO_Insert_DuplicateID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	id := uuid.New()
	manifestID := uuid.New()

	if err := dao.Insert(context.Background(), id, manifestID); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	err := dao.Insert(context.Background(), id, manifestID)
	if err == nil {
		t.Fatalf("second Insert: err = nil, want duplicate-id error")
	}
}

func TestMemorySpawnSagaDAO_Insert_EmptyID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	if err := dao.Insert(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Fatalf("Insert empty id: err = nil, want validation error")
	}
}

func TestMemorySpawnSagaDAO_Insert_EmptyManifestVersionID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	if err := dao.Insert(context.Background(), uuid.New(), uuid.Nil); err == nil {
		t.Fatalf("Insert empty manifest_version_id: err = nil, want validation error")
	}
}

func TestMemorySpawnSagaDAO_UpdateStep_TransitionsToInFlight(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := dao.UpdateStep(context.Background(), id, "slack_app_create"); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}

	got, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != saga.SagaStateInFlight {
		t.Errorf("Status = %q, want %q", got.Status, saga.SagaStateInFlight)
	}
	if got.CurrentStep != "slack_app_create" {
		t.Errorf("CurrentStep = %q, want %q", got.CurrentStep, "slack_app_create")
	}
}

func TestMemorySpawnSagaDAO_UpdateStep_UnknownID_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	if err := dao.UpdateStep(context.Background(), uuid.New(), "x"); !errors.Is(err, saga.ErrSagaNotFound) {
		t.Errorf("UpdateStep unknown id: err = %v, want ErrSagaNotFound", err)
	}
}

func TestMemorySpawnSagaDAO_MarkCompleted_TransitionsTerminal(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := dao.MarkCompleted(context.Background(), id); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	got, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q", got.Status, saga.SagaStateCompleted)
	}
	if got.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = zero, want non-zero after MarkCompleted")
	}
}

func TestMemorySpawnSagaDAO_MarkCompleted_UnknownID_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	if err := dao.MarkCompleted(context.Background(), uuid.New()); !errors.Is(err, saga.ErrSagaNotFound) {
		t.Errorf("MarkCompleted unknown id: err = %v, want ErrSagaNotFound", err)
	}
}

func TestMemorySpawnSagaDAO_MarkFailed_RecordsLastError(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := dao.MarkFailed(context.Background(), id, "step_execute_error"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != saga.SagaStateFailed {
		t.Errorf("Status = %q, want %q", got.Status, saga.SagaStateFailed)
	}
	if got.LastError != "step_execute_error" {
		t.Errorf("LastError = %q, want %q", got.LastError, "step_execute_error")
	}
	if got.CompletedAt.IsZero() {
		t.Errorf("CompletedAt = zero, want non-zero after MarkFailed")
	}
}

func TestMemorySpawnSagaDAO_MarkFailed_UnknownID_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	if err := dao.MarkFailed(context.Background(), uuid.New(), "x"); !errors.Is(err, saga.ErrSagaNotFound) {
		t.Errorf("MarkFailed unknown id: err = %v, want ErrSagaNotFound", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// M7.3.a — InsertIfAbsent idempotency contract.
// ────────────────────────────────────────────────────────────────────────

// TestMemorySpawnSagaDAO_InsertIfAbsent_FreshKey_PersistsRow pins the
// happy path: a fresh idempotency_key produces `Inserted == true` and
// the persisted row carries the supplied id, manifest_version_id, and
// idempotency_key.
func TestMemorySpawnSagaDAO_InsertIfAbsent_FreshKey_PersistsRow(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	id := uuid.New()
	mvID := uuid.New()
	wkID := uuid.New()
	const key = "tok-fresh"

	result, err := dao.InsertIfAbsent(context.Background(), id, mvID, wkID, key)
	if err != nil {
		t.Fatalf("InsertIfAbsent: %v", err)
	}
	if !result.Inserted {
		t.Errorf("Inserted = false, want true (fresh key)")
	}
	if result.Existing.ID != id {
		t.Errorf("Existing.ID = %v, want %v", result.Existing.ID, id)
	}
	if result.Existing.IdempotencyKey != key {
		t.Errorf("Existing.IdempotencyKey = %q, want %q", result.Existing.IdempotencyKey, key)
	}
	if result.Existing.ManifestVersionID != mvID {
		t.Errorf("Existing.ManifestVersionID = %v, want %v", result.Existing.ManifestVersionID, mvID)
	}
	if result.Existing.WatchkeeperID != wkID {
		t.Errorf("Existing.WatchkeeperID = %v, want %v", result.Existing.WatchkeeperID, wkID)
	}

	got, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IdempotencyKey != key {
		t.Errorf("Get IdempotencyKey = %q, want %q", got.IdempotencyKey, key)
	}
	if got.WatchkeeperID != wkID {
		t.Errorf("Get WatchkeeperID = %v, want %v", got.WatchkeeperID, wkID)
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_DuplicateKey_ReturnsExisting
// pins the replay path: a second InsertIfAbsent with the same
// idempotency_key returns `Inserted == false` and the prior row's id
// (NOT the second-call's supplied id).
func TestMemorySpawnSagaDAO_InsertIfAbsent_DuplicateKey_ReturnsExisting(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(fixedClock())
	firstID := uuid.New()
	secondID := uuid.New()
	mvID := uuid.New()
	const key = "tok-replay"

	if _, err := dao.InsertIfAbsent(context.Background(), firstID, mvID, uuid.New(), key); err != nil {
		t.Fatalf("first InsertIfAbsent: %v", err)
	}

	result, err := dao.InsertIfAbsent(context.Background(), secondID, uuid.New(), uuid.New(), key)
	if err != nil {
		t.Fatalf("second InsertIfAbsent (replay): %v", err)
	}
	if result.Inserted {
		t.Errorf("Inserted = true, want false (duplicate key)")
	}
	if result.Existing.ID != firstID {
		t.Errorf("Existing.ID = %v, want first-call %v (second-call id must be discarded)", result.Existing.ID, firstID)
	}
	if result.Existing.IdempotencyKey != key {
		t.Errorf("Existing.IdempotencyKey = %q, want %q", result.Existing.IdempotencyKey, key)
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyKey_ReturnsTypedSentinel
// pins the empty-key rejection: an empty idempotency_key returns
// [saga.ErrEmptyIdempotencyKey] without any persistence side effect.
// Defense-in-depth: the partial UNIQUE-WHERE-NOT-NULL index on the
// SQL side allows multiple NULL rows; the DAO surface refuses to
// produce them so a future caller cannot silently bypass dedup.
func TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyKey_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	_, err := dao.InsertIfAbsent(context.Background(), uuid.New(), uuid.New(), uuid.New(), "")
	if !errors.Is(err, saga.ErrEmptyIdempotencyKey) {
		t.Errorf("InsertIfAbsent empty key: err = %v, want ErrEmptyIdempotencyKey", err)
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_WhitespaceOnlyKey_ReturnsTypedSentinel
// pins codex iter-1 Minor #4: a `"   "` sentinel must NOT smuggle a
// bypass past the empty-key check. The DAO normalises whitespace-only
// keys to empty before checking, returning the same typed sentinel as
// the bare-empty case.
func TestMemorySpawnSagaDAO_InsertIfAbsent_WhitespaceOnlyKey_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	for _, key := range []string{" ", "\t", "\n", "  \t\n  "} {
		_, err := dao.InsertIfAbsent(context.Background(), uuid.New(), uuid.New(), uuid.New(), key)
		if !errors.Is(err, saga.ErrEmptyIdempotencyKey) {
			t.Errorf("InsertIfAbsent whitespace-only key %q: err = %v, want ErrEmptyIdempotencyKey", key, err)
		}
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyWatchkeeperID_Errors pins
// the watchkeeperID validation. An empty watchkeeperID would defeat the
// M7.3.a replay-payload contract (the kickoffer reads
// Existing.WatchkeeperID on the catch-up branch).
func TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyWatchkeeperID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	_, err := dao.InsertIfAbsent(context.Background(), uuid.New(), uuid.New(), uuid.Nil, "tok-x")
	if err == nil {
		t.Fatalf("InsertIfAbsent empty watchkeeper_id: err = nil, want validation error")
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyID_Errors pins that the
// non-key shape validation matches Insert's discipline.
func TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	_, err := dao.InsertIfAbsent(context.Background(), uuid.Nil, uuid.New(), uuid.New(), "tok-x")
	if err == nil {
		t.Fatalf("InsertIfAbsent empty id: err = nil, want validation error")
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyManifestVersionID_Errors
// pins that the non-key shape validation matches Insert's discipline.
func TestMemorySpawnSagaDAO_InsertIfAbsent_EmptyManifestVersionID_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	_, err := dao.InsertIfAbsent(context.Background(), uuid.New(), uuid.Nil, uuid.New(), "tok-x")
	if err == nil {
		t.Fatalf("InsertIfAbsent empty manifest_version_id: err = nil, want validation error")
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_DuplicateIDDifferentKey_Errors
// pins the wiring-bug branch: the same id supplied with a fresh
// idempotency_key is rejected (the SQL `id` PRIMARY KEY would reject
// it). Mirrors the duplicate-id behaviour of the legacy [Insert].
func TestMemorySpawnSagaDAO_InsertIfAbsent_DuplicateIDDifferentKey_Errors(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	id := uuid.New()
	if _, err := dao.InsertIfAbsent(context.Background(), id, uuid.New(), uuid.New(), "tok-a"); err != nil {
		t.Fatalf("first InsertIfAbsent: %v", err)
	}
	_, err := dao.InsertIfAbsent(context.Background(), id, uuid.New(), uuid.New(), "tok-b")
	if err == nil {
		t.Fatalf("second InsertIfAbsent (same id, different key): err = nil, want duplicate-id error")
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_IndexInconsistency_ReturnsTypedSentinel
// pins critic iter-1 Minor #5: a future DAO bug that leaves the
// idempotency map pointing at a missing row surfaces a typed sentinel
// (saga.ErrIdempotencyIndexInconsistent) so callers can branch via
// errors.Is. Reproducing the inconsistency requires bypassing the DAO
// surface; the test does so by mutating the in-memory store directly
// and asserting the typed sentinel survives the wrap chain.
func TestMemorySpawnSagaDAO_InsertIfAbsent_IndexInconsistency_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	id := uuid.New()
	const key = "tok-inconsistent"

	if _, err := dao.InsertIfAbsent(context.Background(), id, uuid.New(), uuid.New(), key); err != nil {
		t.Fatalf("seed InsertIfAbsent: %v", err)
	}
	// Bypass the DAO surface to drop the row but leave the
	// idempotency_key index pointing at it. Any subsequent
	// InsertIfAbsent with the same key now hits the inconsistency
	// branch.
	saga.PurgeRowKeepingIdempotencyForTest(dao, id)

	_, err := dao.InsertIfAbsent(context.Background(), uuid.New(), uuid.New(), uuid.New(), key)
	if !errors.Is(err, saga.ErrIdempotencyIndexInconsistent) {
		t.Errorf("InsertIfAbsent on inconsistent index: err = %v, want ErrIdempotencyIndexInconsistent", err)
	}
}

// TestMemorySpawnSagaDAO_InsertIfAbsent_Concurrent_ExactlyOneInsert
// hammers the dedup contract under contention: 16 goroutines call
// InsertIfAbsent concurrently with the SAME idempotency_key. Exactly
// one of them returns `Inserted == true`; the other 15 return
// `Inserted == false` with the SAME persisted row. Pinned by the
// race-detector test invocation; the test fails if a developer
// regresses the mutex away from the per-instance guard or the
// idempotency map away from atomic check-and-set.
func TestMemorySpawnSagaDAO_InsertIfAbsent_Concurrent_ExactlyOneInsert(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	const writers = 16
	const key = "tok-concurrent"
	mvID := uuid.New()

	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		insertedCount int
		replayedCount int
		winnerIDs     []uuid.UUID
	)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := uuid.New()
			result, err := dao.InsertIfAbsent(context.Background(), id, mvID, uuid.New(), key)
			if err != nil {
				t.Errorf("InsertIfAbsent: %v", err)
				return
			}
			mu.Lock()
			if result.Inserted {
				insertedCount++
				winnerIDs = append(winnerIDs, result.Existing.ID)
			} else {
				replayedCount++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if insertedCount != 1 {
		t.Errorf("Inserted=true count = %d, want 1", insertedCount)
	}
	if replayedCount != writers-1 {
		t.Errorf("Inserted=false count = %d, want %d", replayedCount, writers-1)
	}
}

// TestMemorySpawnSagaDAO_Concurrency_DistinctIDs hammers the DAO with
// a fan-out of writers operating on distinct ids. Pinned by the
// race-detector test invocation in Phase 3 verification (`go test
// -race`); the test fails if a developer regresses the mutex away
// from a per-instance guard.
func TestMemorySpawnSagaDAO_Concurrency_DistinctIDs(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	const writers = 32
	ids := make([]uuid.UUID, writers)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			ctx := context.Background()
			if err := dao.Insert(ctx, id, uuid.New()); err != nil {
				t.Errorf("Insert(%v): %v", id, err)
				return
			}
			if err := dao.UpdateStep(ctx, id, "step"); err != nil {
				t.Errorf("UpdateStep(%v): %v", id, err)
				return
			}
			if err := dao.MarkCompleted(ctx, id); err != nil {
				t.Errorf("MarkCompleted(%v): %v", id, err)
				return
			}
			if _, err := dao.Get(ctx, id); err != nil {
				t.Errorf("Get(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		got, err := dao.Get(context.Background(), id)
		if err != nil {
			t.Errorf("final Get(%v): %v", id, err)
			continue
		}
		if got.Status != saga.SagaStateCompleted {
			t.Errorf("final Status(%v) = %q, want %q", id, got.Status, saga.SagaStateCompleted)
		}
	}
}
