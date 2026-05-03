package notebook

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Import replaces the live notebook file with the snapshot read from `src`.
// The sequence is:
//
//  1. Spool `src` to a temp file in the SAME directory as the live SQLite
//     file. Same-dir is mandatory because [os.Rename] cannot cross
//     filesystem boundaries on POSIX.
//  2. Validate the spool via [validateArchive] — magic header bytes plus
//     required tables (`entry`, `entry_vec`) and partial indexes
//     (`entry_category_active`, `entry_active_after`). Any failure wraps
//     [ErrCorruptArchive].
//  3. Refuse the import if the live `entry` table has any rows
//     ([ErrTargetNotEmpty]). M2b.2.b is strict: callers wanting to
//     overwrite must layer their own Archive + Forget-all + Import flow
//     (M2b.6's CLI may add a `--force` flag for that, see
//     [package godoc]).
//  4. Close the live `*sql.DB`, [os.Rename] the spool over the live path,
//     and re-open via [openAt] — the receiver's internal `*sql.DB` is
//     swapped in place so existing callers holding the same `*DB` see the
//     imported data on the next call.
//
// Cleanup: the spool file is removed on every error path (deferred
// [os.Remove]). The live file is only renamed AFTER validation has
// succeeded and AFTER the target-not-empty check, so a corrupt or
// non-empty-target Import never touches the on-disk live file.
//
// # Concurrency
//
// Import takes the receiver's `importMu` for its full duration. Callers
// MUST NOT invoke other [DB] methods on the same receiver concurrently
// with Import — Import closes the underlying `*sql.DB` and a Recall in
// flight would race the swap. After Import returns successfully the
// receiver is fully usable again on the new file.
//
// # ArchiveStore handoff
//
// Import reads from an [io.Reader] so the caller decides where the bytes
// come from. M2b.3 wraps Archive/Import with an `ArchiveStore` interface
// and LocalFS / S3 backends; this layer stays storage-agnostic.
func (d *DB) Import(ctx context.Context, src io.Reader) error {
	d.importMu.Lock()
	defer d.importMu.Unlock()

	if d.path == "" {
		return fmt.Errorf("notebook: import requires *DB constructed via Open")
	}

	// Spool to a hidden temp file in the SAME directory as the live SQLite
	// file. The leading dot prefix keeps `ls` listings tidy on the data
	// dir (which other agents on the same UID can list, even though they
	// can't read the contents thanks to the parent dir's 0o700 mode). The
	// `.sqlite` suffix is preserved for parity with the live file in case
	// an operator inspects the directory mid-import.
	dir := filepath.Dir(d.path)
	spool, err := os.CreateTemp(dir, ".notebook-import-*.sqlite")
	if err != nil {
		return fmt.Errorf("notebook: create import spool: %w", err)
	}
	spoolPath := spool.Name()
	// `removed` becomes true once the spool has been os.Rename-d into
	// place. Until then, the deferred Remove cleans it up on any error.
	var removed bool
	defer func() {
		if !removed {
			_ = os.Remove(spoolPath)
		}
	}()

	if _, err := io.Copy(spool, src); err != nil {
		_ = spool.Close()
		return fmt.Errorf("notebook: spool import: %w", err)
	}
	if err := spool.Close(); err != nil {
		return fmt.Errorf("notebook: close import spool: %w", err)
	}

	// Validate the spool BEFORE we touch the live file. validateArchive
	// already wraps its failures with ErrCorruptArchive.
	if err := validateArchive(ctx, spoolPath); err != nil {
		return err
	}

	// Refuse if the live DB still has entries. We query against the live
	// connection (which is still open at this point) rather than the
	// spool because the contract is "import refuses to drop live data".
	var n int
	if err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM entry`).Scan(&n); err != nil {
		return fmt.Errorf("notebook: count target entries: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("notebook: target has %d entries: %w", n, ErrTargetNotEmpty)
	}

	// Close the live connection so the rename can take its WAL/SHM
	// sidecars too. SQLite re-creates the sidecars on the next open.
	if err := d.sql.Close(); err != nil {
		return fmt.Errorf("notebook: close live db before import rename: %w", err)
	}

	// Best-effort cleanup of the WAL / SHM sidecars left by the previous
	// connection. SQLite tolerates their absence on the next open and
	// re-creates them on first write; leaving stale sidecars on disk would
	// be a subtle source of "imported data plus pre-import journal
	// fragments" weirdness.
	_ = os.Remove(d.path + "-wal")
	_ = os.Remove(d.path + "-shm")

	// Atomic rename — same directory guaranteed by os.CreateTemp(dir, ...)
	// above. After this call the spool file no longer exists at
	// spoolPath; flip `removed` so the deferred cleanup is a no-op.
	if err := os.Rename(spoolPath, d.path); err != nil {
		return fmt.Errorf("notebook: rename import spool to live path: %w", err)
	}
	removed = true

	// Re-open the live file. We re-use openAt so the WAL pragma and
	// schema-init run against the imported file (the schema-init is
	// idempotent — `CREATE ... IF NOT EXISTS` is a no-op when every
	// object already exists, which validateArchive just guaranteed).
	fresh, err := openAt(ctx, d.path)
	if err != nil {
		return fmt.Errorf("notebook: reopen after import: %w", err)
	}
	// Swap the underlying handle in place. Reset the closeOne so the
	// receiver's Close still works against the new connection. We hold
	// importMu so no other method can observe the half-swap.
	d.sql = fresh.sql
	// Reset close state so subsequent Close()s target the new handle.
	// `sync.Once` zero-value is the unfired state, so a fresh assignment
	// rearms it. Safe under importMu — no other goroutine is allowed to
	// touch the receiver during Import per the godoc concurrency contract.
	d.closeOne = sync.Once{}
	d.closeErr = nil
	return nil
}
