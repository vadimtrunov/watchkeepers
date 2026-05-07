package notebook

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestMarkNeedsReview_HappyPath pins the AC3 happy path: MarkNeedsReview
// flips the column to 1, ClearNeedsReview flips it back to 0, both observed
// directly in the underlying row.
func TestMarkNeedsReview_HappyPath(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "flip me",
		Embedding: makeEmbedding(100),
	})

	if err := db.MarkNeedsReview(ctx, id); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}
	if got := readNeedsReview(ctx, t, db, id); got != 1 {
		t.Fatalf("after Mark: needs_review = %d, want 1", got)
	}

	if err := db.ClearNeedsReview(ctx, id); err != nil {
		t.Fatalf("ClearNeedsReview: %v", err)
	}
	if got := readNeedsReview(ctx, t, db, id); got != 0 {
		t.Fatalf("after Clear: needs_review = %d, want 0", got)
	}
}

// TestMarkNeedsReview_Idempotent pins AC6: re-flipping an already-flagged
// (or already-clear) row must not error and must leave the column value
// unchanged. SQLite's UPDATE matches by id, so RowsAffected stays 1 even
// when the new value equals the old — neither Mark nor Clear should
// surface an error in that case.
func TestMarkNeedsReview_Idempotent(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seed(ctx, t, db, Entry{
		Category:  CategoryLesson,
		Content:   "idempotent",
		Embedding: makeEmbedding(101),
	})

	for i := 0; i < 2; i++ {
		if err := db.MarkNeedsReview(ctx, id); err != nil {
			t.Fatalf("MarkNeedsReview iter %d: %v", i, err)
		}
		if got := readNeedsReview(ctx, t, db, id); got != 1 {
			t.Fatalf("iter %d Mark: needs_review = %d, want 1", i, got)
		}
	}
	for i := 0; i < 2; i++ {
		if err := db.ClearNeedsReview(ctx, id); err != nil {
			t.Fatalf("ClearNeedsReview iter %d: %v", i, err)
		}
		if got := readNeedsReview(ctx, t, db, id); got != 0 {
			t.Fatalf("iter %d Clear: needs_review = %d, want 0", i, got)
		}
	}
}

// TestMarkNeedsReview_NotFound pins AC7: an unknown entry id surfaces as
// ErrNotFound for both Mark and Clear; the underlying table sees no
// mutation since the WHERE never matched.
func TestMarkNeedsReview_NotFound(t *testing.T) {
	db, ctx, _ := freshDB(t)

	const stray = "12345678-1234-1234-1234-123456789012"
	if err := db.MarkNeedsReview(ctx, stray); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Mark(stray): err=%v, want ErrNotFound", err)
	}
	if err := db.ClearNeedsReview(ctx, stray); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Clear(stray): err=%v, want ErrNotFound", err)
	}
}

// TestMarkNeedsReview_InvalidEntry pins AC8: an empty entry id surfaces as
// ErrInvalidEntry without touching the database.
func TestMarkNeedsReview_InvalidEntry(t *testing.T) {
	db, ctx, _ := freshDB(t)

	if err := db.MarkNeedsReview(ctx, ""); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("Mark(\"\"): err=%v, want ErrInvalidEntry", err)
	}
	if err := db.ClearNeedsReview(ctx, ""); !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("Clear(\"\"): err=%v, want ErrInvalidEntry", err)
	}
}

// TestMarkNeedsReview_CrossAgentIsolation pins AC9: agent A's MarkNeedsReview
// against agent B's entry id must surface as ErrNotFound. The per-agent
// SQLite file boundary is the underlying enforcement mechanism — the test
// pins the contract at the API level so future migrations cannot regress
// it without flipping this assertion red.
func TestMarkNeedsReview_CrossAgentIsolation(t *testing.T) {
	ctx := context.Background()

	dirA := t.TempDir()
	dirB := t.TempDir()
	dbA, err := openAt(ctx, filepath.Join(dirA, "agentA.sqlite"))
	if err != nil {
		t.Fatalf("openAt A: %v", err)
	}
	t.Cleanup(func() { _ = dbA.Close() })
	dbB, err := openAt(ctx, filepath.Join(dirB, "agentB.sqlite"))
	if err != nil {
		t.Fatalf("openAt B: %v", err)
	}
	t.Cleanup(func() { _ = dbB.Close() })

	// Seed an entry only into agent B's notebook.
	idB, err := dbB.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "owned by B",
		Embedding: makeEmbedding(102),
	})
	if err != nil {
		t.Fatalf("Remember B: %v", err)
	}

	// Agent A asking to flip B's id must not find it.
	if err := dbA.MarkNeedsReview(ctx, idB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A.Mark(B-id): err=%v, want ErrNotFound", err)
	}
	if err := dbA.ClearNeedsReview(ctx, idB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A.Clear(B-id): err=%v, want ErrNotFound", err)
	}

	// And B's row must remain unflagged — A's failed call cannot have
	// reached across the file boundary.
	if got := readNeedsReview(ctx, t, dbB, idB); got != 0 {
		t.Fatalf("B-row needs_review = %d after A's failed Mark, want 0", got)
	}
}

// readNeedsReview returns the raw integer value of the `needs_review`
// column for a given entry id. Used as a low-level assertion helper rather
// than going through Recall (which already filters needs_review = 1 rows
// out and would therefore mask the very thing the Mark/Clear tests are
// pinning).
func readNeedsReview(ctx context.Context, t *testing.T, db *DB, id string) int {
	t.Helper()
	var v int
	if err := db.sql.QueryRowContext(
		ctx,
		`SELECT needs_review FROM entry WHERE id = ?`, id,
	).Scan(&v); err != nil {
		t.Fatalf("read needs_review: %v", err)
	}
	return v
}
