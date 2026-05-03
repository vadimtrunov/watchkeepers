package notebook

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// roundTripSeed describes one entry to seed-then-round-trip in
// [TestArchive_RoundTrip]. Held as a struct so the per-seed assertion
// helper has a single argument shape.
type roundTripSeed struct {
	id        string
	category  string
	content   string
	embedding []float32
}

// roundTripSeeds is the canonical 3-row fixture for [TestArchive_RoundTrip]:
// three different categories with three orthogonal embeddings so each
// Recall returns exactly one row.
var roundTripSeeds = []roundTripSeed{
	{
		id:        "11111111-1111-1111-1111-111111111111",
		category:  CategoryLesson,
		content:   "lesson row",
		embedding: makeEmbedding(1),
	},
	{
		id:        "22222222-2222-2222-2222-222222222222",
		category:  CategoryPreference,
		content:   "preference row",
		embedding: makeEmbedding(2),
	},
	{
		id:        "33333333-3333-3333-3333-333333333333",
		category:  CategoryObservation,
		content:   "observation row",
		embedding: makeEmbedding(3),
	},
}

// assertRoundTrip queries `dst` for the row described by `s` and fails
// the test if id/content do not match. Extracted from the test body to
// keep TestArchive_RoundTrip's cyclomatic complexity under the gocyclo
// threshold.
func assertRoundTrip(ctx context.Context, t *testing.T, dst *DB, s roundTripSeed) {
	t.Helper()
	results, err := dst.Recall(ctx, RecallQuery{
		Embedding: s.embedding,
		TopK:      3,
		Category:  s.category,
	})
	if err != nil {
		t.Fatalf("Recall(%s): %v", s.category, err)
	}
	if len(results) != 1 {
		t.Fatalf("Recall(%s) returned %d rows, want 1", s.category, len(results))
	}
	if results[0].ID != s.id {
		t.Fatalf("Recall(%s) id = %q, want %q", s.category, results[0].ID, s.id)
	}
	if results[0].Content != s.content {
		t.Fatalf("Recall(%s) content = %q, want %q", s.category, results[0].Content, s.content)
	}
}

// TestArchive_RoundTrip exercises AC1+AC2+AC5 in one shot: write three
// entries with different categories and embeddings, Archive to a buffer,
// Import the buffer into a brand-new agent path, and Recall against the
// fresh agent. All three rows must come back with the same id/content/
// category and the embedding bytes must round-trip through `entry_vec`.
func TestArchive_RoundTrip(t *testing.T) {
	src, ctx, _ := freshDB(t)

	for _, s := range roundTripSeeds {
		if _, err := src.Remember(ctx, Entry{
			ID:        s.id,
			Category:  s.category,
			Content:   s.content,
			Embedding: s.embedding,
		}); err != nil {
			t.Fatalf("seed Remember %s: %v", s.id, err)
		}
	}

	var buf bytes.Buffer
	if err := src.Archive(ctx, &buf); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// Cheap sanity: the buffer must start with the SQLite magic header.
	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatal("Archive output missing SQLite magic header")
	}

	// Build a fresh, empty target DB at a different path and import.
	dst, _, _ := freshDBAt(t, filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err := dst.Import(ctx, &buf); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Recall every category we seeded; the row id and content must match.
	for _, s := range roundTripSeeds {
		assertRoundTrip(ctx, t, dst, s)
	}

	// Stats over the imported file must report 3 active entries.
	st, err := dst.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.TotalEntries != 3 || st.Active != 3 || st.Superseded != 0 {
		t.Fatalf("Stats = %+v, want Total=3 Active=3 Superseded=0", st)
	}
}

// TestArchive_EmptyAgent — fresh agent, no Remember calls. Archive must
// still produce a valid SQLite file (the schema rides along) and Import
// into a second fresh agent must succeed with Stats reporting zero rows.
func TestArchive_EmptyAgent(t *testing.T) {
	src, ctx, _ := freshDB(t)

	var buf bytes.Buffer
	if err := src.Archive(ctx, &buf); err != nil {
		t.Fatalf("Archive empty: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("SQLite format 3\x00")) {
		t.Fatal("empty Archive output missing SQLite magic header")
	}

	dst, _, _ := freshDBAt(t, filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err := dst.Import(ctx, &buf); err != nil {
		t.Fatalf("Import empty: %v", err)
	}
	st, err := dst.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.TotalEntries != 0 || st.Active != 0 || st.Superseded != 0 {
		t.Fatalf("Stats = %+v, want all zero on empty import", st)
	}
}

// TestArchive_NoMutationToLiveDB — AC5 row "Archive does not mutate the
// live DB". Remember 2 entries, Archive, then Recall — the live DB must
// still surface the 2 entries (i.e. Archive's temp-file dance did not
// truncate, vacuum-replace, or otherwise touch the user-visible file).
func TestArchive_NoMutationToLiveDB(t *testing.T) {
	src, ctx, _ := freshDB(t)
	for i, content := range []string{"row-1", "row-2"} {
		if _, err := src.Remember(ctx, Entry{
			Category:  CategoryLesson,
			Content:   content,
			Embedding: makeEmbedding(byte(i + 7)),
		}); err != nil {
			t.Fatalf("seed Remember %d: %v", i, err)
		}
	}

	var buf bytes.Buffer
	if err := src.Archive(ctx, &buf); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// Re-Recall against the LIVE handle. The query embedding doesn't have
	// to match anything in particular — we only care that two rows still
	// exist post-Archive.
	results, err := src.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(7),
		TopK:      10,
	})
	if err != nil {
		t.Fatalf("post-Archive Recall: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("post-Archive Recall len = %d, want 2 (Archive must not mutate live DB)", len(results))
	}
}

// freshDBAt is a sibling of [freshDB] that lets the caller supply the
// target path — used by Archive/Import tests so they can create a second
// agent file in a known location.
func freshDBAt(t *testing.T, path string) (*DB, context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := openAt(ctx, path)
	if err != nil {
		t.Fatalf("openAt(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx, path
}
