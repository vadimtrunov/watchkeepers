package notebook

import (
	"context"
	"testing"
	"time"
)

// TestRecallFilterCounts_3EntryFixture pins the canonical happy path: one
// plain row, one cooling-off row, and one needs_review row → counts (1, 1).
// The plain row is NOT counted by either predicate; only excluded rows are
// reported by the helper.
func TestRecallFilterCounts_3EntryFixture(t *testing.T) {
	db, ctx, _ := freshDB(t)

	now := time.Now().UnixMilli()

	// Plain row — must NOT increment either counter.
	seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "plain",
		Embedding: makeEmbedding(60),
	})

	// Cooling-off row: active_after far in the future.
	seed(ctx, t, db, Entry{
		Category:    CategoryLesson,
		Content:     "cooling",
		Embedding:   makeEmbedding(61),
		ActiveAfter: now + int64(24*time.Hour/time.Millisecond),
	})

	// Needs-review row: starts plain, gets flagged.
	needsReviewID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "needs review",
		Embedding: makeEmbedding(62),
	})
	if err := db.MarkNeedsReview(ctx, needsReviewID); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}

	coolingOff, needsReview, err := db.RecallFilterCounts(ctx, RecallQuery{
		Embedding: makeEmbedding(60),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("RecallFilterCounts: %v", err)
	}
	if coolingOff != 1 {
		t.Errorf("coolingOff = %d, want 1", coolingOff)
	}
	if needsReview != 1 {
		t.Errorf("needsReview = %d, want 1", needsReview)
	}
}

// TestRecallFilterCounts_EmptyDB pins the zero-rows path: an empty notebook
// yields (0, 0).
func TestRecallFilterCounts_EmptyDB(t *testing.T) {
	db, ctx, _ := freshDB(t)

	coolingOff, needsReview, err := db.RecallFilterCounts(ctx, RecallQuery{
		Embedding: makeEmbedding(1),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("RecallFilterCounts: %v", err)
	}
	if coolingOff != 0 || needsReview != 0 {
		t.Errorf("counts = (%d, %d), want (0, 0)", coolingOff, needsReview)
	}
}

// TestRecallFilterCounts_OnlyCoolingOff pins that exclusion classes are
// reported independently: only one cooling-off row → (1, 0).
func TestRecallFilterCounts_OnlyCoolingOff(t *testing.T) {
	db, ctx, _ := freshDB(t)

	now := time.Now().UnixMilli()

	seed(ctx, t, db, Entry{
		Category:    CategoryLesson,
		Content:     "cooling-only",
		Embedding:   makeEmbedding(70),
		ActiveAfter: now + int64(24*time.Hour/time.Millisecond),
	})

	coolingOff, needsReview, err := db.RecallFilterCounts(ctx, RecallQuery{
		Embedding: makeEmbedding(70),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("RecallFilterCounts: %v", err)
	}
	if coolingOff != 1 {
		t.Errorf("coolingOff = %d, want 1", coolingOff)
	}
	if needsReview != 0 {
		t.Errorf("needsReview = %d, want 0", needsReview)
	}
}

// TestRecallFilterCounts_SupersededNotCounted pins that rows excluded by
// the `superseded_by IS NOT NULL` predicate are NOT counted by either
// counter — superseded is a different exclusion class, not a cooling-off
// or needs_review match.
func TestRecallFilterCounts_SupersededNotCounted(t *testing.T) {
	db, ctx, _ := freshDB(t)

	supersederID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "superseder",
		Embedding: makeEmbedding(75),
	})
	supersededID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "superseded",
		Embedding: makeEmbedding(76),
	})
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, supersederID, supersededID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	coolingOff, needsReview, err := db.RecallFilterCounts(ctx, RecallQuery{
		Embedding: makeEmbedding(75),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("RecallFilterCounts: %v", err)
	}
	if coolingOff != 0 || needsReview != 0 {
		t.Errorf("counts = (%d, %d), want (0, 0); superseded must not be counted", coolingOff, needsReview)
	}
}

// TestRecallFilterCounts_CtxCancelled pins that a cancelled context is
// returned verbatim before any DB work.
func TestRecallFilterCounts_CtxCancelled(t *testing.T) {
	db, _, _ := freshDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := db.RecallFilterCounts(ctx, RecallQuery{
		Embedding: makeEmbedding(1),
		TopK:      10,
	})
	if err == nil {
		t.Fatal("RecallFilterCounts returned nil err on cancelled ctx")
	}
}
