package notebook

import (
	"errors"
	"strings"
	"testing"
)

// makeEmbedding returns a deterministic 1536-dim float32 vector seeded by the
// caller-supplied byte. Distinct seeds produce non-parallel vectors so
// sqlite-vec's KNN ordering in TestRecall_TopK is well-defined.
func makeEmbedding(seed byte) []float32 {
	v := make([]float32, EmbeddingDim)
	v[int(seed)%EmbeddingDim] = 1
	return v
}

func TestRemember_HappyPath(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id, err := db.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "do not paste secrets into chat",
		Embedding: makeEmbedding(1),
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}
	if !uuidPattern.MatchString(id) {
		t.Fatalf("returned id %q is not a canonical UUID", id)
	}

	var (
		gotID       string
		gotCategory string
		gotContent  string
		gotCreated  int64
	)
	if err := db.sql.QueryRowContext(ctx,
		`SELECT id, category, content, created_at FROM entry WHERE id = ?`, id,
	).Scan(&gotID, &gotCategory, &gotContent, &gotCreated); err != nil {
		t.Fatalf("entry select: %v", err)
	}
	if gotID != id || gotCategory != CategoryLesson || gotContent != "do not paste secrets into chat" {
		t.Fatalf("entry mismatch: id=%q cat=%q content=%q", gotID, gotCategory, gotContent)
	}
	if gotCreated == 0 {
		t.Fatal("created_at was not auto-populated")
	}

	var vecCount int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM entry_vec WHERE id = ?`, id,
	).Scan(&vecCount); err != nil {
		t.Fatalf("entry_vec count: %v", err)
	}
	if vecCount != 1 {
		t.Fatalf("entry_vec rows = %d, want 1 (sync contract violated)", vecCount)
	}
}

func TestRemember_PreservesCallerID(t *testing.T) {
	db, ctx, _ := freshDB(t)

	const wantID = "11111111-2222-3333-4444-555555555555"
	gotID, err := db.Remember(ctx, Entry{
		ID:        wantID,
		Category:  CategoryLesson,
		Content:   "preserved id",
		Embedding: makeEmbedding(2),
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if gotID != wantID {
		t.Fatalf("returned id = %q, want %q", gotID, wantID)
	}
}

func TestRemember_InvalidShape(t *testing.T) {
	db, ctx, _ := freshDB(t)

	cases := []struct {
		name string
		e    Entry
	}{
		{
			name: "empty content",
			e: Entry{
				Category:  CategoryLesson,
				Embedding: makeEmbedding(1),
			},
		},
		{
			name: "wrong embedding dim",
			e: Entry{
				Category:  CategoryLesson,
				Content:   "x",
				Embedding: make([]float32, 8),
			},
		},
		{
			name: "bad category",
			e: Entry{
				Category:  "bananas",
				Content:   "x",
				Embedding: makeEmbedding(1),
			},
		},
		{
			name: "missing embedding",
			e: Entry{
				Category: CategoryLesson,
				Content:  "x",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := db.Remember(ctx, tc.e)
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("err=%v, want ErrInvalidEntry", err)
			}
			if id != "" {
				t.Fatalf("returned id %q on invalid entry", id)
			}
		})
	}

	// Confirm the DB is still empty — none of the rejected calls should have
	// written anything.
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("entry rows = %d, want 0 after invalid Remembers", n)
	}
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM entry_vec`).Scan(&n); err != nil {
		t.Fatalf("vec count: %v", err)
	}
	if n != 0 {
		t.Fatalf("entry_vec rows = %d, want 0 after invalid Remembers", n)
	}
}

func TestRemember_DuplicateIDFails(t *testing.T) {
	db, ctx, _ := freshDB(t)

	const id = "22222222-3333-4444-5555-666666666666"
	e := Entry{
		ID:        id,
		Category:  CategoryLesson,
		Content:   "first",
		Embedding: makeEmbedding(3),
	}
	if _, err := db.Remember(ctx, e); err != nil {
		t.Fatalf("first Remember: %v", err)
	}
	_, err := db.Remember(ctx, e)
	if err == nil {
		t.Fatal("second Remember with same id succeeded; PRIMARY KEY not enforced")
	}
	// Sanity check that the failure was a constraint error rather than a
	// sentinel.
	if !strings.Contains(strings.ToLower(err.Error()), "unique") &&
		!strings.Contains(strings.ToLower(err.Error()), "primary key") {
		t.Fatalf("expected uniqueness error, got: %v", err)
	}
}
