package notebook

import (
	"context"
	"time"
)

// TickCallback is invoked by [PeriodicBackup] after each tick, exactly once
// per Archive→Put→LogAppend attempt. The `uri` and `err` parameters mirror
// the partial-failure contract of [ArchiveOnRetire]:
//
//   - ("", err)   — Archive→Put failed; nothing landed in the store.
//   - (uri, err)  — Put succeeded but the audit emit failed; the snapshot
//     is in the store and the caller could retry just the audit.
//   - (uri, nil)  — full success.
//
// The callback runs synchronously on the [PeriodicBackup] goroutine — the
// helper does NOT spawn a fresh goroutine per tick. A slow callback
// therefore delays the next ticker fire (Go's `time.Ticker` drops missed
// ticks rather than queueing). Keep callbacks fast (log + return); do
// async work via `go func() { ... }()` inside the callback if needed.
type TickCallback func(uri string, err error)

// PeriodicBackup runs the [ArchiveOnRetire] pipeline (Archive→Put→
// LogAppend) on every `cadence` interval, emitting `notebook_backed_up`
// audit events instead of `notebook_archived`. The function blocks until
// `ctx` is cancelled and returns the cancellation error
// (`context.Canceled` / `context.DeadlineExceeded`) on exit.
//
// # Pre-loop validation
//
// Before constructing the ticker the helper validates synchronously:
//
//   - `cadence <= 0` → returns [ErrInvalidCadence]; no ticker is started.
//   - `agentID` not a canonical UUID → returns [ErrInvalidEntry]; no
//     ticker is started.
//
// Both errors return immediately without touching `store` or `logger`.
//
// # Per-tick semantics
//
// Each tick calls the shared `archiveAndAudit` helper with
// `eventType = "notebook_backed_up"`. The `(uri, err)` tuple is forwarded
// to `onTick` (when non-nil) synchronously. **Per-tick failures do NOT
// kill the loop** — backups are best-effort: if a Put fails because the
// archivestore is briefly down, the next tick will retry. Operators
// observe failures via the callback (log / metric) without losing the
// scheduling thread.
//
// # Cadence vs cron
//
// `cadence` is a fixed `time.Duration` (e.g. `30 * time.Minute`). For
// cron-expression scheduling — `0 */30 * * * *`, `@daily`, etc. — see the
// `robfig/cron` integration scheduled for M3.3. The simpler fixed-cadence
// surface is sufficient for the per-agent shutdown-resilience use case
// M2b.5 owns.
//
// # Recommended call site
//
// Spawn `PeriodicBackup` in a dedicated goroutine that exits when the
// agent's main shutdown context is cancelled. Pair it with
// [ArchiveOnRetire] in a `defer` against the same shutdown context — the
// retire call covers graceful shutdown, the periodic loop covers crashes
// between graceful shutdowns.
//
//	go func() {
//	    err := notebook.PeriodicBackup(ctx, db, agentID, store, client,
//	        30*time.Minute, func(uri string, err error) {
//	            if err != nil { /* log; next tick will retry */ }
//	        })
//	    // err is ctx.Err() on shutdown.
//	    _ = err
//	}()
func PeriodicBackup(
	ctx context.Context,
	db *DB,
	agentID string,
	store Storer,
	logger Logger,
	cadence time.Duration,
	onTick TickCallback,
) error {
	if cadence <= 0 {
		return ErrInvalidCadence
	}
	if !uuidPattern.MatchString(agentID) {
		return ErrInvalidEntry
	}

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			uri, err := archiveAndAudit(ctx, db, agentID, store, logger, backupEventType)
			if onTick != nil {
				onTick(uri, err)
			}
			// Best-effort: do NOT exit on err. The next tick will retry.
		}
	}
}
