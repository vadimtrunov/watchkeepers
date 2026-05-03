package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImport_RejectsNonSQLite — AC5: a non-SQLite blob (16+ bytes so the
// header check actually runs) must be rejected with ErrCorruptArchive.
func TestImport_RejectsNonSQLite(t *testing.T) {
	dst, ctx, _ := freshDB(t)

	junk := bytes.NewReader([]byte("not a sqlite file padded to 16+ bytes\n"))
	err := dst.Import(ctx, junk)
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("Import(junk): err=%v, want ErrCorruptArchive", err)
	}
}

// TestImport_RejectsMissingTable — build a SQLite file with just `entry`
// and no `entry_vec`; Import must reject it as a corrupt archive.
func TestImport_RejectsMissingTable(t *testing.T) {
	dst, ctx, _ := freshDB(t)

	bad := buildBadArchive(ctx, t, badArchiveOptions{
		// Only the entry table — no entry_vec, no indexes.
		createEntry: true,
	})
	err := dst.Import(ctx, bytes.NewReader(bad))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("Import(missing table): err=%v, want ErrCorruptArchive", err)
	}
}

// TestImport_RejectsMissingIndex — build a SQLite file with both tables
// but neither partial index; Import must reject it.
func TestImport_RejectsMissingIndex(t *testing.T) {
	dst, ctx, _ := freshDB(t)

	bad := buildBadArchive(ctx, t, badArchiveOptions{
		createEntry:    true,
		createEntryVec: true,
	})
	err := dst.Import(ctx, bytes.NewReader(bad))
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("Import(missing index): err=%v, want ErrCorruptArchive", err)
	}
}

// TestImport_RejectsNonEmptyTarget — Remember 1 entry into the live DB,
// then Import a valid snapshot. Must return ErrTargetNotEmpty AND leave
// the live DB usable (the existing entry must still Recall).
func TestImport_RejectsNonEmptyTarget(t *testing.T) {
	dst, ctx, _ := freshDB(t)

	const liveID = "55555555-5555-5555-5555-555555555555"
	if _, err := dst.Remember(ctx, Entry{
		ID:        liveID,
		Category:  CategoryLesson,
		Content:   "live row",
		Embedding: makeEmbedding(11),
	}); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	// Source the snapshot from a separate, equally valid notebook.
	src, _, _ := freshDBAt(t, filepath.Join(t.TempDir(), "src.sqlite"))
	if _, err := src.Remember(ctx, Entry{
		Category:  CategoryLesson,
		Content:   "src row",
		Embedding: makeEmbedding(12),
	}); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	var snap bytes.Buffer
	if err := src.Archive(ctx, &snap); err != nil {
		t.Fatalf("Archive src: %v", err)
	}

	err := dst.Import(ctx, &snap)
	if !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("Import(non-empty target): err=%v, want ErrTargetNotEmpty", err)
	}

	// Live DB must still be usable post-rejection.
	results, recallErr := dst.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(11),
		TopK:      5,
	})
	if recallErr != nil {
		t.Fatalf("post-rejection Recall: %v", recallErr)
	}
	if len(results) != 1 || results[0].ID != liveID {
		t.Fatalf("post-rejection Recall = %+v, want exactly the live row", results)
	}
}

// TestImport_TempFileCleanup — induce a validation failure (junk reader)
// and confirm the spool file under the agent dir was deleted. The agent
// dir should contain only the live `.sqlite` file (plus any WAL/SHM
// sidecars), no leftover `.notebook-import-*` spool.
func TestImport_TempFileCleanup(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "agent.sqlite")
	dst, ctx, _ := freshDBAt(t, livePath)

	junk := bytes.NewReader([]byte("definitely not a sqlite file just plain text"))
	if err := dst.Import(ctx, junk); !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("Import(junk): err=%v, want ErrCorruptArchive", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".notebook-import-") {
			t.Fatalf("spool file %q not cleaned up after failed Import", name)
		}
	}
}

// TestImport_AtomicRename — exercise the AC5 concurrency contract: after
// Import returns successfully, the receiver is fully usable on the new
// file. We Archive an entry into a buffer, Import it into a fresh empty
// DB, then call Recall — it must surface the imported entry.
func TestImport_AtomicRename(t *testing.T) {
	src, ctx, _ := freshDB(t)
	const sentinelID = "66666666-6666-6666-6666-666666666666"
	if _, err := src.Remember(ctx, Entry{
		ID:        sentinelID,
		Category:  CategoryLesson,
		Content:   "post-import recallable",
		Embedding: makeEmbedding(13),
	}); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	var snap bytes.Buffer
	if err := src.Archive(ctx, &snap); err != nil {
		t.Fatalf("Archive src: %v", err)
	}

	dst, _, _ := freshDBAt(t, filepath.Join(t.TempDir(), "dst.sqlite"))
	if err := dst.Import(ctx, &snap); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// The receiver must speak to the new file. Recall the imported row.
	results, err := dst.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(13),
		TopK:      3,
	})
	if err != nil {
		t.Fatalf("post-Import Recall: %v", err)
	}
	if len(results) != 1 || results[0].ID != sentinelID {
		t.Fatalf("post-Import Recall = %+v, want exactly the sentinel row", results)
	}

	// Close should still work post-swap (AC: receiver remains valid).
	if err := dst.Close(); err != nil {
		t.Fatalf("post-Import Close: %v", err)
	}
}

// badArchiveOptions controls which schema objects buildBadArchive lays
// down on the throw-away SQLite file. All-false produces a valid file
// with the magic header but no schema objects we care about.
type badArchiveOptions struct {
	createEntry    bool
	createEntryVec bool
}

// buildBadArchive constructs a SQLite file at a temp path with only the
// schema objects requested by `opts`, returning the file's bytes for use
// as an Import source. The file is guaranteed to start with a valid
// SQLite magic header (so the header check passes and the schema check is
// the failing gate).
func buildBadArchive(ctx context.Context, t *testing.T, opts badArchiveOptions) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), "bad.sqlite")
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", path)
	dbh, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open bad archive: %v", err)
	}
	dbh.SetMaxOpenConns(1)
	if err := dbh.PingContext(ctx); err != nil {
		t.Fatalf("ping bad archive: %v", err)
	}

	if opts.createEntry {
		if _, err := dbh.ExecContext(ctx, `
			CREATE TABLE entry (
				id TEXT PRIMARY KEY,
				category TEXT NOT NULL,
				content TEXT NOT NULL,
				created_at INTEGER NOT NULL
			)
		`); err != nil {
			t.Fatalf("create bad entry: %v", err)
		}
	}
	if opts.createEntryVec {
		if _, err := dbh.ExecContext(ctx, `
			CREATE VIRTUAL TABLE entry_vec USING vec0(
				id TEXT PRIMARY KEY,
				embedding float[1536]
			)
		`); err != nil {
			t.Fatalf("create bad entry_vec: %v", err)
		}
	}
	if err := dbh.Close(); err != nil {
		t.Fatalf("close bad archive: %v", err)
	}

	// Read the file back as bytes so the caller can hand it to
	// bytes.NewReader (the Import path expects an io.Reader).
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reopen bad archive: %v", err)
	}
	defer func() { _ = f.Close() }()
	out, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read bad archive: %v", err)
	}
	return out
}
