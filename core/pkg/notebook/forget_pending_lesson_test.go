package notebook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// forgetPLEmbedding mirrors pendingLessonsEmbedding for the
// forget-pending-lesson tests' seed inserts.
func forgetPLEmbedding() []float32 {
	v := make([]float32, EmbeddingDim)
	v[0] = 1
	return v
}

func TestForgetPendingLesson_RejectsNonCanonicalUUID(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	for _, id := range []string{
		"",
		"not-a-uuid",
		"01900000-0000-7000-8000-XXXXXXXXXXXX",
		"01900000",
	} {
		if err := db.ForgetPendingLesson(context.Background(), id, time.Now()); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("id=%q: err = %v, want ErrInvalidEntry", id, err)
		}
	}
}

func TestForgetPendingLesson_NotFoundOnUnknownID(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	err := db.ForgetPendingLesson(
		context.Background(),
		"01900000-0000-7000-8000-000000000999", time.Now(),
	)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestForgetPendingLesson_DeletesCoolingOffLesson(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	id, err := db.Remember(ctx, Entry{
		Category:    CategoryLesson,
		Subject:     "cooling",
		Content:     "Y",
		ActiveAfter: future,
		Embedding:   forgetPLEmbedding(),
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := db.ForgetPendingLesson(ctx, id, now); err != nil {
		t.Fatalf("ForgetPendingLesson: %v", err)
	}

	// Row must be gone from both tables.
	var n int
	if err := db.sql.QueryRowContext(
		ctx,
		`SELECT count(*) FROM entry WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count entry: %v", err)
	}
	if n != 0 {
		t.Errorf("entry row count = %d, want 0", n)
	}
	if err := db.sql.QueryRowContext(
		ctx,
		`SELECT count(*) FROM entry_vec WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count entry_vec: %v", err)
	}
	if n != 0 {
		t.Errorf("entry_vec row count = %d, want 0", n)
	}
}

func TestForgetPendingLesson_RefusesNonLessonCategory(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	for _, cat := range []string{
		CategoryPendingTask,
		CategoryPreference,
		CategoryObservation,
		CategoryRelationshipNote,
	} {
		id, err := db.Remember(ctx, Entry{
			Category:    cat,
			Subject:     "x",
			Content:     "Y",
			ActiveAfter: future,
			Embedding:   forgetPLEmbedding(),
		})
		if err != nil {
			t.Fatalf("seed %q: %v", cat, err)
		}
		if err := db.ForgetPendingLesson(ctx, id, now); !errors.Is(err, ErrNotPendingLesson) {
			t.Errorf("category=%q: err = %v, want ErrNotPendingLesson", cat, err)
		}
		// And the row MUST still exist.
		var n int
		_ = db.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry WHERE id = ?`, id).Scan(&n)
		if n != 1 {
			t.Errorf("category=%q: entry deleted despite refusal", cat)
		}
	}
}

func TestForgetPendingLesson_RefusesActiveLesson(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)

	// active_after in the PAST → lesson is already active, not cooling-off.
	id, err := db.Remember(ctx, Entry{
		Category:    CategoryLesson,
		Subject:     "active",
		Content:     "Y",
		ActiveAfter: now.UnixMilli() - 1,
		Embedding:   forgetPLEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ForgetPendingLesson(ctx, id, now); !errors.Is(err, ErrNotPendingLesson) {
		t.Errorf("err = %v, want ErrNotPendingLesson", err)
	}
}

func TestForgetPendingLesson_RefusesActiveAfterEqualNow(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)

	// Boundary: active_after == now means the lesson has JUST become
	// Recall-eligible — should NOT match the cooling-off scope.
	id, err := db.Remember(ctx, Entry{
		Category:    CategoryLesson,
		Subject:     "boundary",
		Content:     "Y",
		ActiveAfter: now.UnixMilli(),
		Embedding:   forgetPLEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ForgetPendingLesson(ctx, id, now); !errors.Is(err, ErrNotPendingLesson) {
		t.Errorf("err = %v, want ErrNotPendingLesson", err)
	}
}

func TestForgetPendingLesson_RefusesSuperseded(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	oldID, err := db.Remember(ctx, Entry{
		Category: CategoryLesson, Subject: "old", Content: "Y",
		ActiveAfter: future, Embedding: forgetPLEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed old: %v", err)
	}
	newID, err := db.Remember(ctx, Entry{
		Category: CategoryLesson, Subject: "new", Content: "Y",
		ActiveAfter: future, Embedding: forgetPLEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed new: %v", err)
	}
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, newID, oldID,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if err := db.ForgetPendingLesson(ctx, oldID, now); !errors.Is(err, ErrNotPendingLesson) {
		t.Errorf("err = %v, want ErrNotPendingLesson (superseded)", err)
	}
}
