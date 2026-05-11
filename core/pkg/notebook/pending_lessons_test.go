package notebook

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pendingLessonsEmbedding returns a unit vector pinned at index 0 so
// every pending-lesson row carries a valid embedding for the
// [DB.Remember] insert path. The PendingLessons query never reads the
// embedding column (the digest is a static filter, not a KNN), so the
// concrete vector contents are irrelevant — we just need the shape to
// pass [validate].
func pendingLessonsEmbedding() []float32 {
	v := make([]float32, EmbeddingDim)
	v[0] = 1
	return v
}

// seedCoolingLesson inserts one lesson entry with the given activeAfter (unix
// ms). Returns the assigned id so tests can correlate matches.
func seedCoolingLesson(t *testing.T, db *DB, subject, content string, activeAfter int64) string {
	t.Helper()
	id, err := db.Remember(context.Background(), Entry{
		Category:    CategoryLesson,
		Subject:     subject,
		Content:     content,
		ActiveAfter: activeAfter,
		Embedding:   pendingLessonsEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed lesson %q: %v", subject, err)
	}
	return id
}

func TestPendingLessons_RejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	for _, n := range []int{0, -1, -100} {
		if _, err := db.PendingLessons(context.Background(), time.Now(), n); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("limit=%d: err = %v, want ErrInvalidEntry", n, err)
		}
	}
}

func TestPendingLessons_ReturnsEmptyOnNoMatches(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	got, err := db.PendingLessons(context.Background(), time.Now(), 10)
	if err != nil {
		t.Fatalf("PendingLessons on empty DB: %v", err)
	}
	if got == nil {
		t.Fatalf("PendingLessons returned nil; want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("PendingLessons returned %d rows; want 0", len(got))
	}
}

func TestPendingLessons_SelectsCoolingOffOnly(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	pastID := seedCoolingLesson(t, db, "past: already active", "X", now.UnixMilli()-1)
	nowID := seedCoolingLesson(t, db, "now: exactly now", "X", now.UnixMilli())
	futureID := seedCoolingLesson(t, db, "future: cooling off", "X", now.UnixMilli()+1)

	rows, err := db.PendingLessons(context.Background(), now, 100)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, r := range rows {
		gotIDs[r.ID] = true
	}
	if gotIDs[pastID] {
		t.Errorf("PendingLessons returned past-active id %q; want only cooling-off rows", pastID)
	}
	if gotIDs[nowID] {
		t.Errorf("PendingLessons returned active_after==now id %q; the SQL is strict > now", nowID)
	}
	if !gotIDs[futureID] {
		t.Errorf("PendingLessons MISSING cooling-off id %q", futureID)
	}
}

func TestPendingLessons_FiltersOutNonLessonCategories(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	lessonID, err := db.Remember(context.Background(), Entry{
		Category: CategoryLesson, Subject: "lesson", Content: "Y",
		ActiveAfter: future, Embedding: pendingLessonsEmbedding(),
	})
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	if _, err := db.Remember(context.Background(), Entry{
		Category: CategoryPendingTask, Subject: "pending task", Content: "Y",
		ActiveAfter: future, Embedding: pendingLessonsEmbedding(),
	}); err != nil {
		t.Fatalf("seed pending_task: %v", err)
	}
	if _, err := db.Remember(context.Background(), Entry{
		Category: CategoryPreference, Subject: "pref", Content: "Y",
		ActiveAfter: future, Embedding: pendingLessonsEmbedding(),
	}); err != nil {
		t.Fatalf("seed preference: %v", err)
	}

	rows, err := db.PendingLessons(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows; want exactly 1 (lesson category only)", len(rows))
	}
	if rows[0].ID != lessonID {
		t.Errorf("got id %q, want %q", rows[0].ID, lessonID)
	}
}

func TestPendingLessons_OrdersByAscendingActiveAfter(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	farID := seedCoolingLesson(t, db, "subject-far", "X", now.UnixMilli()+int64(20*time.Hour/time.Millisecond))
	nearID := seedCoolingLesson(t, db, "subject-near", "X", now.UnixMilli()+int64(1*time.Hour/time.Millisecond))
	midID := seedCoolingLesson(t, db, "subject-mid", "X", now.UnixMilli()+int64(10*time.Hour/time.Millisecond))

	rows, err := db.PendingLessons(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows; want 3", len(rows))
	}
	if rows[0].ID != nearID || rows[1].ID != midID || rows[2].ID != farID {
		t.Errorf("order = [%s, %s, %s]; want [%s (near), %s (mid), %s (far)]",
			rows[0].ID, rows[1].ID, rows[2].ID, nearID, midID, farID)
	}
}

func TestPendingLessons_ClampsLimit(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	for i := 0; i < 5; i++ {
		seedCoolingLesson(t, db, "subj", "X", now.UnixMilli()+int64(i+1)*int64(time.Hour/time.Millisecond))
	}

	rows, err := db.PendingLessons(context.Background(), now, 3)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows; want 3 (limit)", len(rows))
	}
}

func TestPendingLessons_ExcludesSuperseded(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	supersededID := seedCoolingLesson(t, db, "old", "X", future)
	newID := seedCoolingLesson(t, db, "new", "X", future)

	// Mark supersededID superseded_by newID via the FlagSuperseded
	// helper if present, OR a direct UPDATE for the test.
	if _, err := db.sql.ExecContext(
		context.Background(),
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, newID, supersededID,
	); err != nil {
		t.Fatalf("UPDATE superseded_by: %v", err)
	}

	rows, err := db.PendingLessons(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows; want 1", len(rows))
	}
	if rows[0].ID != newID {
		t.Errorf("got id %q; want %q (superseded should be excluded)", rows[0].ID, newID)
	}
}

func TestPendingLessons_ExcludesNeedsReview(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	future := now.UnixMilli() + int64(24*time.Hour/time.Millisecond)

	flaggedID := seedCoolingLesson(t, db, "flagged", "X", future)
	cleanID := seedCoolingLesson(t, db, "clean", "X", future)

	if _, err := db.sql.ExecContext(
		context.Background(),
		`UPDATE entry SET needs_review = 1 WHERE id = ?`, flaggedID,
	); err != nil {
		t.Fatalf("UPDATE needs_review: %v", err)
	}

	rows, err := db.PendingLessons(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows; want 1", len(rows))
	}
	if rows[0].ID != cleanID {
		t.Errorf("got id %q; want %q (needs_review should be excluded)", rows[0].ID, cleanID)
	}
}

func TestPendingLessons_ProjectsDigestColumnsOnly(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	activeAfter := now.UnixMilli() + int64(12*time.Hour/time.Millisecond)
	id := seedCoolingLesson(t, db, "subj-A", "body-A", activeAfter)

	rows, err := db.PendingLessons(context.Background(), now, 1)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows; want 1", len(rows))
	}
	got := rows[0]
	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.Subject != "subj-A" {
		t.Errorf("Subject = %q, want %q", got.Subject, "subj-A")
	}
	if got.ActiveAfter != activeAfter {
		t.Errorf("ActiveAfter = %d, want %d", got.ActiveAfter, activeAfter)
	}
	if got.CreatedAt == 0 {
		t.Errorf("CreatedAt = 0; want non-zero")
	}
}

// TestPendingLessons_SecondarySortByIDAscOnTiedActiveAfter pins the
// `ORDER BY active_after ASC, id ASC` secondary key (iter-1 critic
// gap): two lessons with identical active_after must surface in id
// ASC order so the digest's "most-imminent first" contract is
// deterministic when the cooling-off window is identical.
func TestPendingLessons_SecondarySortByIDAscOnTiedActiveAfter(t *testing.T) {
	t.Parallel()
	db, ctx, _ := freshDB(t)

	now := time.UnixMilli(1_700_000_000_000)
	tied := now.UnixMilli() + int64(12*time.Hour/time.Millisecond)

	// Use explicit UUIDs so we control which id sorts first.
	idLow := "01900000-0000-7000-8000-000000000001"
	idHigh := "01900000-0000-7000-8000-000000000099"
	for _, id := range []string{idHigh, idLow} { // insert out of order
		if _, err := db.Remember(ctx, Entry{
			ID:          id,
			Category:    CategoryLesson,
			Subject:     "tied",
			Content:     "Y",
			ActiveAfter: tied,
			Embedding:   pendingLessonsEmbedding(),
		}); err != nil {
			t.Fatalf("seed %q: %v", id, err)
		}
	}
	rows, err := db.PendingLessons(ctx, now, 10)
	if err != nil {
		t.Fatalf("PendingLessons: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows; want 2", len(rows))
	}
	if rows[0].ID != idLow || rows[1].ID != idHigh {
		t.Errorf("order = [%s, %s]; want [%s, %s] (id ASC on tied active_after)",
			rows[0].ID, rows[1].ID, idLow, idHigh)
	}
}
