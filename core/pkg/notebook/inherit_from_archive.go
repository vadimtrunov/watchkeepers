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
	"log/slog"
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
// wrapped underlying error chain on the IMPORT-failure paths.
// Validation failures from [ImportFromArchive] (invalid agentID /
// empty URI / nil fetcher) surface as [ErrInvalidEntry] through
// the wrap.
//
// # Partial-failure contract
//
//   - Import failure → `(0, err)` matching [ImportFromArchive]'s
//     wrap chain (e.g. `errors.Is(err, ErrCorruptArchive)`,
//     `errors.Is(err, ErrTargetNotEmpty)`). The destination DB
//     is untouched on this branch.
//   - Post-import Open / Stats failure → `(0, nil)` AFTER the
//     import has succeeded. The inherited data IS in the DB;
//     only the count read failed. Rolling the saga back on a
//     count-read miss would discard a successful inheritance and
//     trigger a downstream rollback walk over a step that has no
//     compensator yet — the alternative is far worse than emitting
//     a `notebook_inherited` row with `entries_imported=0`. An
//     operator can re-read the count via a follow-up Stats call;
//     the audit row's `entries_imported=0` is the documented
//     "count unknown / sentinel" value. Iter-1 codex+critic P1
//     fix.
//
// The post-import-failure path logs the underlying error via
// [log/slog] so an operator can correlate a count-zero audit row
// to a notebook-side incident.
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

	// Post-import path: the import succeeded and the inherited
	// data is in the DB. Open/Stats failures here are degraded to
	// `(0, nil)` so a transient sqlite OS-level error or a brief
	// FD-table pressure does NOT undo a successful inheritance.
	// The slog.Warn record carries the underlying error chain.
	db, err := Open(ctx, agentID)
	if err != nil {
		slog.WarnContext(
			ctx,
			"notebook: inherit count: post-import Open failed; count degraded to 0",
			"agent_id", agentID,
			"err_class", "inherit_count_open_failed",
			"err", err.Error(),
		)
		return 0, nil
	}
	defer func() { _ = db.Close() }()

	stats, err := db.Stats(ctx)
	if err != nil {
		slog.WarnContext(
			ctx,
			"notebook: inherit count: post-import Stats failed; count degraded to 0",
			"agent_id", agentID,
			"err_class", "inherit_count_stats_failed",
			"err", err.Error(),
		)
		return 0, nil
	}
	return stats.TotalEntries, nil
}
