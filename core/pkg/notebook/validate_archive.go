package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"

	// Side-effect import already pulled in by db.go but listed here so this
	// file compiles in isolation if db.go is ever moved.
	_ "github.com/mattn/go-sqlite3"
)

// sqliteHeader is the magic 16-byte prefix every SQLite database file begins
// with. Spec: https://www.sqlite.org/fileformat.html#magic_header_string.
const sqliteHeader = "SQLite format 3\x00"

// requiredArchiveTables and requiredArchiveIndexes are the schema objects
// validateArchive insists on. They mirror the schema constants in [db.go]:
//
//   - `entry` — regular table holding the 12 user columns.
//   - `entry_vec` — `vec0` virtual table; sqlite-vec virtual tables show up
//     in `sqlite_schema` with `type = 'table'`, NOT `'virtual'`.
//   - `entry_category_active` / `entry_active_after` — partial indexes
//     introduced by M2b.2.a; their absence on an imported file would reset
//     a notebook to the M2b.1 (un-indexed) shape, which we treat as
//     corruption rather than transparently re-creating them.
var (
	requiredArchiveTables  = []string{"entry", "entry_vec"}
	requiredArchiveIndexes = []string{"entry_category_active", "entry_active_after"}
)

// validateArchive opens the file at `path` read-only and confirms it looks
// like a notebook snapshot:
//
//  1. The first 16 bytes of the file equal [sqliteHeader].
//  2. `sqlite_schema` contains every name in [requiredArchiveTables] with
//     `type = 'table'` and every name in [requiredArchiveIndexes] with
//     `type = 'index'`.
//
// Any failure returns an error wrapping [ErrCorruptArchive] so callers can
// `errors.Is(err, ErrCorruptArchive)`. The function holds no state; it
// closes the validation `*sql.DB` before returning so the caller can rename
// the file on macOS without hitting a stale-file-handle error.
func validateArchive(ctx context.Context, path string) error {
	if err := checkSQLiteHeader(path); err != nil {
		return err
	}
	return checkArchiveSchema(ctx, path)
}

// checkSQLiteHeader reads the first 16 bytes of `path` and compares them to
// [sqliteHeader]. Files shorter than 16 bytes count as corrupt.
func checkSQLiteHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("notebook: open archive for header check: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, buf); err != nil {
		// Includes io.ErrUnexpectedEOF (truncated file) and io.EOF
		// (zero-byte file). Both are corruption from our perspective.
		return fmt.Errorf("notebook: read archive header: %w: %w", err, ErrCorruptArchive)
	}
	if string(buf) != sqliteHeader {
		return fmt.Errorf("notebook: archive header mismatch: %w", ErrCorruptArchive)
	}
	return nil
}

// checkArchiveSchema opens the file at `path` via a fresh `*sql.DB` in
// read-only mode and asserts every entry in [requiredArchiveTables] /
// [requiredArchiveIndexes] is present.
func checkArchiveSchema(ctx context.Context, path string) error {
	// `?mode=ro&_foreign_keys=on` keeps the validation step strictly
	// read-only — we never want validation to mutate the spool file even
	// accidentally. We do NOT enable WAL here because read-only mode
	// makes journal_mode irrelevant and a read-only WAL upgrade would fail
	// on a file that does not yet have a `-wal` sidecar.
	dsn := fmt.Sprintf("file:%s?mode=ro&_foreign_keys=on", path)
	roDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("notebook: open archive ro: %w: %w", err, ErrCorruptArchive)
	}
	// Close the validation handle BEFORE we return so a subsequent
	// os.Rename on macOS does not race a still-open file descriptor.
	defer func() { _ = roDB.Close() }()
	roDB.SetMaxOpenConns(1)

	if err := roDB.PingContext(ctx); err != nil {
		return fmt.Errorf("notebook: ping archive ro: %w: %w", err, ErrCorruptArchive)
	}

	rows, err := roDB.QueryContext(ctx, `
		SELECT name, type
		FROM sqlite_schema
		WHERE type IN ('table', 'index')
	`)
	if err != nil {
		return fmt.Errorf("notebook: query archive schema: %w: %w", err, ErrCorruptArchive)
	}
	defer rows.Close()

	tables := make(map[string]struct{})
	indexes := make(map[string]struct{})
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return fmt.Errorf("notebook: scan archive schema: %w: %w", err, ErrCorruptArchive)
		}
		switch kind {
		case "table":
			tables[name] = struct{}{}
		case "index":
			indexes[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("notebook: iterate archive schema: %w: %w", err, ErrCorruptArchive)
	}

	for _, name := range requiredArchiveTables {
		if _, ok := tables[name]; !ok {
			return fmt.Errorf("notebook: archive missing table %q: %w", name, ErrCorruptArchive)
		}
	}
	for _, name := range requiredArchiveIndexes {
		if _, ok := indexes[name]; !ok {
			return fmt.Errorf("notebook: archive missing index %q: %w", name, ErrCorruptArchive)
		}
	}
	return nil
}
