package notebook

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Archive produces a self-contained snapshot of the agent's notebook file
// and streams it to `w`. The implementation runs SQLite's native `VACUUM
// INTO <tempfile>` against a fresh temp file under [os.TempDir], then copies
// the bytes out via [io.Copy]. The temp file is removed before returning
// (defer [os.Remove]) regardless of error.
//
// Archive is read-only with respect to the live `*DB`: it does not close,
// reopen, or otherwise mutate the underlying [database/sql.DB]. Concurrent
// reads/writes on the same `*DB` are safe for the duration of an Archive
// call. An empty agent (no entries) still produces a valid snapshot — only
// the schema (the `entry` table, the `entry_vec` virtual table, and the two
// partial indexes) carries over.
//
// Returns the first non-nil error from temp-file creation, the VACUUM INTO
// exec, the temp-file open, or the [io.Copy] streaming step.
//
// # ArchiveStore handoff
//
// `Archive` writes to an [io.Writer] so the caller decides where the bytes
// land. M2b.3 wraps Archive/Import with an `ArchiveStore` interface and
// LocalFS / S3 backends; this layer stays storage-agnostic.
func (d *DB) Archive(ctx context.Context, w io.Writer) error {
	// Create the temp file, close the OS handle (we just need a unique
	// path), then unlink it: VACUUM INTO refuses to write to an existing
	// file. The deferred Remove is a defence in depth — VACUUM INTO
	// recreates the file at the given path, and we want it gone whether
	// VACUUM, the open, or the io.Copy fails.
	tmpFile, err := os.CreateTemp("", "notebook-archive-*.sqlite")
	if err != nil {
		return fmt.Errorf("notebook: create archive temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	if cerr := tmpFile.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("notebook: close archive temp: %w", cerr)
	}
	if rerr := os.Remove(tmpPath); rerr != nil {
		return fmt.Errorf("notebook: prepare archive temp: %w", rerr)
	}
	defer func() { _ = os.Remove(tmpPath) }()

	// VACUUM INTO is a SQLite DDL statement and does NOT accept `?`
	// placeholders. We have to string-interpolate the path. The path is
	// safe to interpolate because:
	//   1. We just created it ourselves via os.CreateTemp.
	//   2. os.CreateTemp returns an absolute path under os.TempDir, neither
	//      of which contains user input.
	// Defensively, we still escape any single quote by doubling it (SQLite
	// single-quoted-literal escape) and reject paths whose escaped form
	// would still contain a NUL byte.
	if !filepath.IsAbs(tmpPath) {
		return fmt.Errorf("notebook: archive temp path %q is not absolute", tmpPath)
	}
	if strings.ContainsRune(tmpPath, 0) {
		return fmt.Errorf("notebook: archive temp path contains NUL")
	}
	quoted := "'" + strings.ReplaceAll(tmpPath, "'", "''") + "'"
	// SQLite's VACUUM INTO is a DDL statement and refuses `?` parameters,
	// so we have to interpolate the path. The path is locally-generated
	// (os.CreateTemp returns an absolute path under os.TempDir) and we
	// double any single quotes above; the gosec warning is the price of
	// the API and not a real injection vector.
	// #nosec G202
	if _, err := d.sql.ExecContext(ctx, "VACUUM INTO "+quoted); err != nil {
		return fmt.Errorf("notebook: vacuum into archive: %w", err)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return fmt.Errorf("notebook: open archive temp: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("notebook: stream archive: %w", err)
	}
	return nil
}
