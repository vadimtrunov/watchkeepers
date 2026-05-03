// Package notebook owns the per-agent SQLite + sqlite-vec embedded storage
// substrate for an agent's private memory ("Notebook"). Each agent gets its
// own SQLite file under `$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`
// (defaulting `WATCHKEEPER_DATA` to `~/.local/share/watchkeepers` on
// Linux/macOS), with the directory created at mode 0o700 so only the owner
// reads.
//
// # Driver decision
//
// This package uses Option A: the CGo driver
// [github.com/mattn/go-sqlite3] paired with
// [github.com/asg017/sqlite-vec-go-bindings/cgo] (sqlite-vec compiled in via
// CGo + auto-extension registration). Option B (the CGo-free
// [github.com/ncruces/go-sqlite3] + sqlite-vec WASM bundle from
// `asg017/sqlite-vec-go-bindings/ncruces`) was prototyped first because it
// avoids CGo entirely; it failed to load because the sqlite-vec WASM build
// shipped in `v0.1.6` uses WebAssembly threads/atomic instructions that the
// pinned wazero runtime (v1.7.3, the version asg017's `ncruces` binding
// depends on) does not support, manifesting as
// `i32.atomic.store invalid as feature "" is disabled` at module compile.
// We therefore fall back to Option A. Trade-off: every consumer of this
// package must build with `CGO_ENABLED=1` and a working C toolchain.
//
// # Schema layout
//
// The `entry` table holds the 12 columns specified by ROADMAP §M2b.1
// (id, category, subject, content, created_at, last_used_at,
// relevance_score, superseded_by, evidence_log_ref, tool_version,
// active_after — and the implicit `id` PK is the 12th). Embeddings live in a
// sibling `entry_vec` virtual table (`vec0`) keyed on the same `id`. Two
// tables joined by id is the standard sqlite-vec pattern: it keeps
// `vec0`'s vector-only column space separate from the regular SQL columns
// so common queries that don't touch the embedding don't have to read it.
//
// # Sync contract
//
// The substrate exposes two tables that callers MUST keep in lock-step:
//
//   - INSERT into entry MUST be paired with INSERT into entry_vec(id, embedding)
//     in the same transaction.
//   - DELETE from entry (Forget / Archive) MUST also DELETE from entry_vec
//     by id. The vec0 virtual table does NOT auto-cascade.
//   - UPDATE of entry.id (rare) requires symmetric UPDATE of entry_vec.id.
//
// The substrate does not enforce this — M2b.2 owns transactional Insert /
// Delete that wraps both tables.
//
// # Out of scope
//
// The Remember / Recall / Forget / Archive / Import / Stats public API
// surface lands in M2b.2 on top of this substrate. M2b.1 ships only [Open],
// [DB.Close], and the package-private path helper.
package notebook

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	// Side-effect import: registers sqlite-vec as a SQLite auto-extension so
	// every connection opened via [database/sql] has the `vec_*` functions
	// and the `vec0` virtual-table module available. Must precede the
	// `mattn/go-sqlite3` driver registration so the auto-extension is in
	// place before the first connection is opened.
	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	// Side-effect import: registers the "sqlite3" driver name with
	// [database/sql] so [sql.Open]("sqlite3", ...) below resolves.
	_ "github.com/mattn/go-sqlite3"
)

// schemaSQL is the idempotent schema definition. Every column constraint is
// enforced server-side (no client-only checks) so M2b.2's Go API can rely on
// the database to reject malformed writes.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS entry (
  id TEXT PRIMARY KEY,
  category TEXT NOT NULL CHECK (category IN ('lesson', 'preference', 'observation', 'pending_task', 'relationship_note')),
  subject TEXT NULL,
  content TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NULL,
  relevance_score REAL NULL,
  superseded_by TEXT NULL REFERENCES entry(id),
  evidence_log_ref TEXT NULL,
  tool_version TEXT NULL,
  active_after INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE IF NOT EXISTS entry_vec USING vec0(
  id TEXT PRIMARY KEY,
  embedding float[1536]
);

-- Partial indexes added by M2b.2.a. The IF NOT EXISTS clauses make the
-- schema-init idempotent across upgrades: an existing M2b.1 file picks up
-- the indexes the first time it is opened by an M2b.2.a binary.
CREATE INDEX IF NOT EXISTS entry_category_active
  ON entry(category) WHERE superseded_by IS NULL;
CREATE INDEX IF NOT EXISTS entry_active_after
  ON entry(active_after) WHERE superseded_by IS NULL;
`

// EmbeddingDim is the embedding dimension used by the `entry_vec` virtual
// table. Mirrors the Keep server's `knowledgeChunkEmbeddingDim`
// (1536) so an entry promoted from Notebook to Keep keeps the same vector
// shape end-to-end.
const EmbeddingDim = 1536

// vecOnce ensures the sqlite-vec auto-extension is registered exactly once
// per process even if [Open] is called concurrently from multiple
// goroutines. [sqlitevec.Auto] mutates SQLite's process-global
// auto-extension table, which is not safe to invoke concurrently and is a
// no-op-but-error after the first call.
var vecOnce sync.Once

// DB is a thin wrapper around the underlying [database/sql] handle for a
// single agent's notebook file. The zero value is not usable; construct via
// [Open]. [DB.Close] is idempotent — repeated calls return nil and do not
// touch the underlying handle twice.
type DB struct {
	sql      *sql.DB
	closeOne sync.Once
	closeErr error
}

// Open opens (or creates) the SQLite file backing the given agent's
// notebook, applies `PRAGMA journal_mode=WAL`, and runs the schema init
// idempotently. The returned [*DB] is safe for concurrent use because the
// underlying [database/sql.DB] is.
//
// `agentID` must be a canonical UUID; otherwise [ErrInvalidAgentID] is
// returned without any filesystem touch.
func Open(ctx context.Context, agentID string) (*DB, error) {
	path, err := agentDBPath(agentID)
	if err != nil {
		return nil, err
	}
	return openAt(ctx, path)
}

// openAt is the test-friendly seam used by [Open] and the unit tests; it
// skips agent-id validation and the path resolver so tests can point at a
// `t.TempDir()` file directly.
func openAt(ctx context.Context, path string) (*DB, error) {
	// Register sqlite-vec as a SQLite auto-extension on first call. Must
	// happen before sql.Open opens the first connection. sql.Open itself is
	// lazy, but the subsequent PingContext/Exec calls below would race with
	// a concurrent Open from another goroutine without this once-guard.
	vecOnce.Do(func() { sqlitevec.Auto() })

	// `_journal_mode=WAL` is honoured by mattn/go-sqlite3 as an open-string
	// pragma; we additionally re-issue the PRAGMA below so we can read it
	// back and surface a clear error on mis-configuration. `_busy_timeout`
	// avoids `SQLITE_BUSY` under contention with no measurable cost.
	// `_foreign_keys=on` enables FK enforcement per connection (SQLite's
	// default is OFF); we verify it stuck with a PRAGMA readback below
	// because mattn silently drops misnamed open-string flags.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on", path)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("notebook: open %q: %w", path, err)
	}

	// Restrict the pool to a single writer connection. SQLite serialises
	// writes and a deeper pool offers no throughput win for the notebook's
	// per-agent workload; a fixed connection also ensures the WAL pragma we
	// set in DSN holds across the lifetime of *DB.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: ping %q: %w", path, err)
	}

	// Confirm WAL is in effect. SQLite silently downgrades to the previous
	// mode if WAL cannot be enabled (e.g. read-only filesystem); fail loudly
	// rather than produce silent data-loss surprises.
	var mode string
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: set journal_mode=WAL on %q: %w", path, err)
	}
	if mode != "wal" {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: journal_mode=WAL ignored on %q (got %q)", path, mode)
	}

	// Confirm foreign-key enforcement is active. mattn/go-sqlite3 silently
	// ignores misnamed open-string flags, so we read the pragma back rather
	// than trusting the DSN was accepted.
	var fkOn int
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: read foreign_keys pragma on %q: %w", path, err)
	}
	if fkOn != 1 {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: foreign_keys pragma did not stick on %q (got %d, want 1)", path, fkOn)
	}

	if _, err := sqlDB.ExecContext(ctx, schemaSQL); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("notebook: schema init on %q: %w", path, err)
	}

	return &DB{sql: sqlDB}, nil
}

// Close closes the underlying [database/sql.DB]. Safe to call multiple times
// — only the first call performs the close; subsequent calls return nil.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	d.closeOne.Do(func() {
		if d.sql != nil {
			d.closeErr = d.sql.Close()
		}
	})
	return d.closeErr
}
