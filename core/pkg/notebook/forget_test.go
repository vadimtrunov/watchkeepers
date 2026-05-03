package notebook

import (
	"errors"
	"testing"
)

func TestForget_HappyPath(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id, err := db.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "to be forgotten",
		Embedding: makeEmbedding(60),
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	if err := db.Forget(ctx, id); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	for _, table := range []string{"entry", "entry_vec"} {
		var n int
		if err := db.sql.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE id = ?`, id,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s rows = %d, want 0 (sync contract violated)", table, n)
		}
	}
}

func TestForget_NotFound(t *testing.T) {
	db, ctx, _ := freshDB(t)

	const stray = "12345678-1234-1234-1234-123456789012"
	err := db.Forget(ctx, stray)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestForget_NotFound_RollsBack(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Seed a real row, then attempt Forget with a different (stray) id. The
	// forget call must roll back atomically: the existing row in entry_vec
	// must remain intact even though the stray id matched no rows in entry.
	id, err := db.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "keep me",
		Embedding: makeEmbedding(61),
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}

	const stray = "abcdef12-3456-7890-abcd-ef1234567890"
	if err := db.Forget(ctx, stray); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget(stray): %v, want ErrNotFound", err)
	}

	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM entry_vec WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("entry_vec rows for live id = %d, want 1 (rollback failed)", n)
	}
}

func TestForget_BadUUID(t *testing.T) {
	db, ctx, _ := freshDB(t)

	cases := []string{
		"",
		"not-a-uuid",
		"../../../etc/passwd",
		"12345678-1234-1234-1234-12345678901", // 11-hex tail
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			err := db.Forget(ctx, in)
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Forget(%q): err=%v, want ErrInvalidEntry", in, err)
			}
		})
	}
}
