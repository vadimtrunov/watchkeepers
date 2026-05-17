package k2k_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// TestMemoryRepository_SetCloseSummary_HappyPath pins that
// SetCloseSummary writes the supplied text onto an archived
// conversation and that a subsequent Get reflects the change.
func TestMemoryRepository_SetCloseSummary_HappyPath(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.SetCloseSummary(context.Background(), conv.ID, "first run summary"); err != nil {
		t.Fatalf("SetCloseSummary: %v", err)
	}
	got, err := r.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseSummary != "first run summary" {
		t.Errorf("CloseSummary = %q, want %q", got.CloseSummary, "first run summary")
	}
	// CloseReason is the lifecycle layer's column — must not have been
	// touched by SetCloseSummary.
	if got.CloseReason != "done" {
		t.Errorf("CloseReason = %q, want %q (SetCloseSummary must not touch CloseReason)", got.CloseReason, "done")
	}
}

func TestMemoryRepository_SetCloseSummary_UnknownIDSurfacesNotFound(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	err := r.SetCloseSummary(context.Background(), uuid.New(), "summary")
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("SetCloseSummary unknown id: err = %v, want ErrConversationNotFound", err)
	}
}

func TestMemoryRepository_SetCloseSummary_NilIDSurfacesNotFound(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	err := r.SetCloseSummary(context.Background(), uuid.Nil, "summary")
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("SetCloseSummary uuid.Nil: err = %v, want ErrConversationNotFound", err)
	}
}

func TestMemoryRepository_SetCloseSummary_OpenRowSurfacesNotArchived(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = r.SetCloseSummary(context.Background(), conv.ID, "summary on open row")
	if !errors.Is(err, k2k.ErrConversationNotArchived) {
		t.Errorf("SetCloseSummary on open row: err = %v, want ErrConversationNotArchived", err)
	}
}

func TestMemoryRepository_SetCloseSummary_CtxCancelledRefused(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.SetCloseSummary(ctx, conv.ID, "summary"); !errors.Is(err, context.Canceled) {
		t.Errorf("SetCloseSummary cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_SetCloseSummary_EmptySummaryAllowed(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.SetCloseSummary(context.Background(), conv.ID, ""); err != nil {
		t.Errorf("SetCloseSummary empty: err = %v, want nil", err)
	}
}

// TestMemoryRepository_SetCloseSummary_LastWriteWins pins overwrite
// semantics: a second SetCloseSummary against the same row overwrites
// the prior value. The peer-tool layer's idempotent-close path
// short-circuits BEFORE a second SetCloseSummary, but the storage
// layer's contract itself is last-write-wins (the peer layer is the
// sole writer and never re-writes).
func TestMemoryRepository_SetCloseSummary_LastWriteWins(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.SetCloseSummary(context.Background(), conv.ID, "first"); err != nil {
		t.Fatalf("SetCloseSummary first: %v", err)
	}
	if err := r.SetCloseSummary(context.Background(), conv.ID, "second"); err != nil {
		t.Fatalf("SetCloseSummary second: %v", err)
	}
	got, err := r.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseSummary != "second" {
		t.Errorf("CloseSummary = %q, want %q (last-write-wins at the storage layer)", got.CloseSummary, "second")
	}
}

// TestMemoryRepository_SetCloseSummary_ConcurrentSafe pins thread
// safety: 16 goroutines racing on the same archived row must all
// return nil and the final stored value must be one of the supplied
// summaries (not a corrupted concatenation).
func TestMemoryRepository_SetCloseSummary_ConcurrentSafe(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_ = i
			if err := r.SetCloseSummary(context.Background(), conv.ID, "concurrent"); err != nil {
				t.Errorf("SetCloseSummary goroutine: %v", err)
			}
		}()
	}
	wg.Wait()
	got, err := r.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CloseSummary != "concurrent" {
		t.Errorf("CloseSummary = %q, want %q", got.CloseSummary, "concurrent")
	}
}
