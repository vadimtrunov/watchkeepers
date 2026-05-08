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
