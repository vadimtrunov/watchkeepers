package notebook

import (
	"testing"
)

// assertStats fails the test if the supplied [Stats] does not match the
// scalar `wantTotal/wantActive/wantSuperseded` and the supplied per-category
// active counts. Categories not present in `wantByCategory` are asserted to
// be zero. Pulled out of [TestStats_CountsByCategory] to keep that test's
// cyclomatic complexity below the linter's ceiling.
func assertStats(t *testing.T, got Stats, wantTotal, wantActive, wantSuperseded int, wantByCategory map[string]int) {
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
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, supersederID, prefID,
	); err != nil {
		t.Fatalf("update superseded_by: %v", err)
	}

	got, err := db.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	assertStats(t, got, 5, 4, 1, map[string]int{
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
	assertStats(t, got, 0, 0, 0, map[string]int{})
}
