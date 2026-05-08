package notebook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seed inserts an Entry with the given fields and returns the auto-generated
// id. Used by the Recall tests to set up known KNN configurations.
func seed(ctx context.Context, t *testing.T, db *DB, e Entry) string {
	t.Helper()
	id, err := db.Remember(ctx, e)
	if err != nil {
		t.Fatalf("seed Remember: %v", err)
	}
	return id
}

func TestRecall_TopK(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Five distinct one-hot vectors. The query is the e0 vector itself, so
	// e0 should rank first, then any other rows tie at distance 1.
	for i := 0; i < 5; i++ {
		seed(ctx, t, db, Entry{
			Category:  CategoryLesson,
			Content:   "row",
			Embedding: makeEmbedding(byte(i + 10)),
		})
	}

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(10),
		TopK:      3,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// First row must be the exact match (distance 0).
	if got[0].Distance != 0 {
		t.Fatalf("first distance = %f, want 0", got[0].Distance)
	}
	// Distances must be non-decreasing.
	for i := 1; i < len(got); i++ {
		if got[i].Distance < got[i-1].Distance {
			t.Fatalf("distances not ascending at %d: %v", i, got)
		}
	}
}

func TestRecall_CategoryFilter(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// 3 lessons + 2 observations, all sharing close vectors so all 5 are
	// within the topK. The category filter must drop the observations.
	for i := 0; i < 3; i++ {
		seed(ctx, t, db, Entry{
			Category:  CategoryLesson,
			Content:   "lesson",
			Embedding: makeEmbedding(byte(i + 20)),
		})
	}
	for i := 0; i < 2; i++ {
		seed(ctx, t, db, Entry{
			Category:  CategoryObservation,
			Content:   "obs",
			Embedding: makeEmbedding(byte(i + 30)),
		})
	}

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(20),
		TopK:      10,
		Category:  CategoryLesson,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 lessons", len(got))
	}
	for _, r := range got {
		if r.Category != CategoryLesson {
			t.Fatalf("got category %q in lessons-only result", r.Category)
		}
	}
}

func TestRecall_ActiveWindow(t *testing.T) {
	db, ctx, _ := freshDB(t)

	now := time.Now().UnixMilli()
	// Future row: active_after far ahead, must be filtered.
	futureID := seed(ctx, t, db, Entry{
		Category:    CategoryLesson,
		Content:     "future",
		Embedding:   makeEmbedding(40),
		ActiveAfter: now + int64(24*time.Hour/time.Millisecond),
	})
	// Always-active row.
	currentID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "current",
		Embedding: makeEmbedding(41),
	})

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(40),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range got {
		if r.ID == futureID {
			t.Fatalf("future-active row %q surfaced", r.ID)
		}
	}
	// And the current row must be there.
	var sawCurrent bool
	for _, r := range got {
		if r.ID == currentID {
			sawCurrent = true
		}
	}
	if !sawCurrent {
		t.Fatalf("current row %q missing from results: %+v", currentID, got)
	}
}

func TestRecall_SkipsSuperseded(t *testing.T) {
	db, ctx, _ := freshDB(t)

	oldID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "old",
		Embedding: makeEmbedding(50),
	})
	newID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "new",
		Embedding: makeEmbedding(51),
	})
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, newID, oldID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(50),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range got {
		if r.ID == oldID {
			t.Fatalf("superseded row %q surfaced", oldID)
		}
	}
}

// TestRecall_ExcludesNeedsReview pins AC2 of M5.6.a: a row that has been
// marked as needing review must NOT surface in [DB.Recall] results, alongside
// the existing superseded / cooling-off exclusions. The fixture seeds four
// rows whose embeddings cluster around the query vector — superseded,
// cooling-off, needs_review, and a fourth plain row — and asserts that only
// the plain row comes back.
func TestRecall_ExcludesNeedsReview(t *testing.T) {
	db, ctx, _ := freshDB(t)

	now := time.Now().UnixMilli()

	// Plain row that should be the only Recall hit.
	plainID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "plain",
		Embedding: makeEmbedding(80),
	})

	// Superseded row: even with a near-by embedding it must be excluded by
	// the existing `superseded_by IS NOT NULL` predicate.
	supersederID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "superseder",
		Embedding: makeEmbedding(90),
	})
	supersededID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "superseded",
		Embedding: makeEmbedding(81),
	})
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, supersederID, supersededID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	// Cooling-off row: active_after far in the future.
	coolingID := seed(ctx, t, db, Entry{
		Category:    CategoryLesson,
		Content:     "cooling",
		Embedding:   makeEmbedding(82),
		ActiveAfter: now + int64(24*time.Hour/time.Millisecond),
	})

	// Needs-review row: starts plain, gets flagged via MarkNeedsReview.
	needsReviewID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "needs review",
		Embedding: makeEmbedding(83),
	})
	if err := db.MarkNeedsReview(ctx, needsReviewID); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(80),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	for _, r := range got {
		if r.ID == supersededID {
			t.Errorf("superseded row %q surfaced", supersededID)
		}
		if r.ID == coolingID {
			t.Errorf("cooling-off row %q surfaced", coolingID)
		}
		if r.ID == needsReviewID {
			t.Errorf("needs-review row %q surfaced", needsReviewID)
		}
	}

	// The plain row must be present; the superseder is also active and may
	// or may not be in the topK depending on its distance — we don't assert
	// on it here.
	var sawPlain bool
	for _, r := range got {
		if r.ID == plainID {
			sawPlain = true
		}
	}
	if !sawPlain {
		t.Fatalf("plain row %q missing from results: %+v", plainID, got)
	}
}

// TestRecall_ClearNeedsReviewRestoresVisibility pins the inverse leg of
// AC2 + AC3: clearing the flag must restore Recall visibility for the same
// query that previously matched the entry.
func TestRecall_ClearNeedsReviewRestoresVisibility(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "round-trip",
		Embedding: makeEmbedding(84),
	})
	if err := db.MarkNeedsReview(ctx, id); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}

	got, err := db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(84),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Recall (flagged): %v", err)
	}
	for _, r := range got {
		if r.ID == id {
			t.Fatalf("flagged row %q surfaced", id)
		}
	}

	if err := db.ClearNeedsReview(ctx, id); err != nil {
		t.Fatalf("ClearNeedsReview: %v", err)
	}

	got, err = db.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(84),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("Recall (cleared): %v", err)
	}
	var sawCleared bool
	for _, r := range got {
		if r.ID == id {
			sawCleared = true
		}
	}
	if !sawCleared {
		t.Fatalf("cleared row %q missing from results: %+v", id, got)
	}
}

func TestRecall_InvalidQuery(t *testing.T) {
	db, ctx, _ := freshDB(t)

	cases := []struct {
		name string
		q    RecallQuery
	}{
		{
			name: "zero topK",
			q:    RecallQuery{Embedding: makeEmbedding(1), TopK: 0},
		},
		{
			name: "wrong embedding dim",
			q:    RecallQuery{Embedding: make([]float32, 8), TopK: 1},
		},
		{
			name: "bad category",
			q: RecallQuery{
				Embedding: makeEmbedding(1),
				TopK:      1,
				Category:  "fruit",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Recall(ctx, tc.q)
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("err=%v, want ErrInvalidEntry", err)
			}
		})
	}
}
