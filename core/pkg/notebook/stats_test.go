package notebook

import (
	"testing"
)

// assertStats fails the test if the supplied [Stats] does not match the
// scalar `wantTotal/wantActive/wantSuperseded/wantNeedsReview` and the
// supplied per-category active counts. Categories not present in
// `wantByCategory` are asserted to be zero. Pulled out of
// [TestStats_CountsByCategory] to keep that test's cyclomatic complexity
// below the linter's ceiling.
func assertStats(t *testing.T, got Stats, wantTotal, wantActive, wantSuperseded, wantNeedsReview int, wantByCategory map[string]int) {
	t.Helper()
	if got.TotalEntries != wantTotal {
		t.Errorf("TotalEntries = %d, want %d", got.TotalEntries, wantTotal)
	}
	if got.Active != wantActive {
		t.Errorf("Active = %d, want %d", got.Active, wantActive)
	}
	if got.Superseded != wantSuperseded {
		t.Errorf("Superseded = %d, want %d", got.Superseded, wantSuperseded)
	}
	if got.NeedsReview != wantNeedsReview {
		t.Errorf("NeedsReview = %d, want %d", got.NeedsReview, wantNeedsReview)
	}
	for cat := range categoryEnum {
		want := wantByCategory[cat]
		if got.ByCategory[cat] != want {
			t.Errorf("ByCategory[%s] = %d, want %d", cat, got.ByCategory[cat], want)
		}
	}
}

func TestStats_CountsByCategory(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Seed:
	//   2 active lessons   (initial loop)
	//   1 active observation
	//   1 active superseder lesson  (will mark the preference as superseded)
	//   1 superseded preference
	//
	// Expected counts:
	//   TotalEntries = 5
	//   Active       = 4 (3 lessons + 1 observation)
	//   Superseded   = 1
	//   ByCategory.active: lesson=3, observation=1, others=0.
	for i := 0; i < 2; i++ {
		if _, err := db.Remember(ctx, Entry{
			Category:  CategoryLesson,
			Content:   "lesson",
			Embedding: makeEmbedding(byte(i + 70)),
		}); err != nil {
			t.Fatalf("seed lesson: %v", err)
		}
	}
	if _, err := db.Remember(ctx, Entry{
		Category:  CategoryObservation,
		Content:   "obs",
		Embedding: makeEmbedding(72),
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	supersederID, err := db.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "newer lesson that supersedes a preference",
		Embedding: makeEmbedding(73),
	})
	if err != nil {
		t.Fatalf("seed superseder: %v", err)
	}
	prefID, err := db.Remember(ctx, Entry{
		Category:  CategoryPreference,
		Content:   "old preference",
		Embedding: makeEmbedding(74),
	})
	if err != nil {
		t.Fatalf("seed preference: %v", err)
	}
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, supersederID, prefID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	got, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	assertStats(t, got, 5, 4, 1, 0, map[string]int{
		CategoryLesson:      3,
		CategoryObservation: 1,
	})
}

func TestStats_Empty(t *testing.T) {
	db, ctx, _ := freshDB(t)

	got, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(got.ByCategory) != len(categoryEnum) {
		t.Fatalf("ByCategory size = %d, want %d", len(got.ByCategory), len(categoryEnum))
	}
	assertStats(t, got, 0, 0, 0, 0, map[string]int{})
}

// TestStats_NeedsReviewCounter pins AC4 + AC5 of M5.6.a: NeedsReview reports
// rows with `needs_review = 1 AND superseded_by IS NULL`, and the existing
// Active counter still includes review-flagged rows because the review flag
// is a pre-supersession Recall-visibility signal, not a lifecycle exit.
func TestStats_NeedsReviewCounter(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Seed two lessons; mark one as needing review.
	id1 := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "to flag",
		Embedding: makeEmbedding(110),
	})
	seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "untouched",
		Embedding: makeEmbedding(111),
	})

	if err := db.MarkNeedsReview(ctx, id1); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}

	got, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats (after Mark): %v", err)
	}
	// Active stays at 2 (AC5: review flag does NOT remove from Active);
	// NeedsReview = 1.
	assertStats(t, got, 2, 2, 0, 1, map[string]int{CategoryLesson: 2})

	// Clearing the flag drops NeedsReview back to 0 without touching Active.
	if err := db.ClearNeedsReview(ctx, id1); err != nil {
		t.Fatalf("ClearNeedsReview: %v", err)
	}
	got, err = db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats (after Clear): %v", err)
	}
	assertStats(t, got, 2, 2, 0, 0, map[string]int{CategoryLesson: 2})
}

// TestStats_NeedsReviewExcludesSuperseded pins the second half of AC4: a
// review-flagged row that is then superseded must drop out of NeedsReview
// (the per-AC4 filter says `needs_review = 1 AND superseded_by IS NULL`).
func TestStats_NeedsReviewExcludesSuperseded(t *testing.T) {
	db, ctx, _ := freshDB(t)

	flaggedID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "flagged then superseded",
		Embedding: makeEmbedding(112),
	})
	supersederID := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "superseder",
		Embedding: makeEmbedding(113),
	})
	if err := db.MarkNeedsReview(ctx, flaggedID); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}
	if _, err := db.sql.ExecContext(
		ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, supersederID, flaggedID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	got, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// 2 total, 1 active (the superseder), 1 superseded, 0 needs-review
	// (the flagged row is also superseded so it falls out of the
	// NeedsReview window).
	assertStats(t, got, 2, 1, 1, 0, map[string]int{CategoryLesson: 1})
}
