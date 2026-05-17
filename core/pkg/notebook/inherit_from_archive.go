package notebook

// inherit_from_archive.go is the Phase 2 §M7.1.c thin wrapper that
// composes the already-merged [ImportFromArchive] primitive with a
// post-import [DB.Stats] read to return the imported-entry count.
// The wrapper exists so the M7.1.c `NotebookInheritStep` saga step
// can echo the count on its `notebook_inherited` audit payload
// without duplicating the fetch-open-import sequence inline.
//
// Why a separate helper instead of reusing [ImportFromArchive]
// directly: [ImportFromArchive] emits a `notebook_imported` audit
// row (the M2b.7 substrate observation) when a non-nil logger is
// supplied. The M7.1.c saga emits a SEPARATE `notebook_inherited`
// row (the saga-level inheritance observation) with a different
// payload shape, so it MUST pass nil to [ImportFromArchive] and
// own the audit emit at the step layer. The count read is the
// only additional work this wrapper does: re-open the DB after
// the import, call [DB.Stats], close. Re-opening is safe — the
// underlying SQLite file is closed by [ImportFromArchive] before
// this wrapper takes ownership.

import (
	"context"
	"fmt"
)

// InheritFromArchive is the M7.1.c saga-layer composition wrapper.
// It:
//
//  1. Calls [ImportFromArchive] with a nil logger — the saga step
//     owns the `notebook_inherited` audit emit, NOT this wrapper.
//     The substrate's `notebook_imported` row is intentionally
//     suppressed (the inheritance observation is the operator-
//     relevant signal).
//  2. Opens the per-agent notebook a second time and calls
//     [DB.Stats] to read the `TotalEntries` count, returning it to
//     the caller for inclusion on the saga's audit payload.
//  3. Closes the DB.
//
// Returns the count + nil on success, or `(0, err)` with a
// wrapped underlying error chain on any failure. Validation
// failures from [ImportFromArchive] (invalid agentID / empty URI
// / nil fetcher) surface as [ErrInvalidEntry] through the wrap.
//
// # Partial-failure contract
//
//   - Import failure → `(0, err)` matching [ImportFromArchive]'s
//     wrap chain (e.g. `errors.Is(err, ErrCorruptArchive)`,
//     `errors.Is(err, ErrTargetNotEmpty)`).
//   - Post-import Open failure → `(0, err)` wrapped
//     `fmt.Errorf("inherit count: open: %w", err)`. The import
//     itself succeeded; the inherited data IS in the DB. The
//     caller (saga step) returns an error so the saga rolls back,
//     and the M7.1.c compensator delegation (the downstream
//     [NotebookProvisionStep.Compensate] archives the file) runs
//     on the next saga.compensate walk.
//   - Stats failure → `(0, err)` wrapped
//     `fmt.Errorf("inherit count: stats: %w", err)`. Same
//     partial-failure contract.
//
// # Concurrency
//
// Safe for concurrent calls with DIFFERENT `agentID` values. NOT
// safe for concurrent calls with the SAME `agentID` (each call
// opens / closes the same on-disk SQLite file). Production wiring
// guarantees per-call exclusivity: a saga is the sole writer for
// its watchkeeperID-keyed notebook file during its run.
func InheritFromArchive(
	ctx context.Context,
	agentID string,
	archiveURI string,
	fetcher Fetcher,
) (int, error) {
	if err := ImportFromArchive(ctx, agentID, archiveURI, fetcher, nil); err != nil {
		return 0, err
	}

	db, err := Open(ctx, agentID)
	if err != nil {
		return 0, fmt.Errorf("inherit count: open: %w", err)
	}
	defer func() { _ = db.Close() }()

	stats, err := db.Stats(ctx)
	if err != nil {
		return 0, fmt.Errorf("inherit count: stats: %w", err)
	}
	return stats.TotalEntries, nil
}
