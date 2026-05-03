# notebook — per-agent SQLite + sqlite-vec storage substrate

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/notebook`

This package owns the on-disk storage layer for an agent's private memory
("Notebook"). It opens (or creates) a per-agent SQLite file at
`$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`, applies
`PRAGMA journal_mode=WAL`, and ensures the schema exists. M2b.2.a adds the
in-process CRUD surface (`Remember` / `Recall` / `Forget` / `Stats`) on top of
the substrate; M2b.2.b layers the snapshot lifecycle (`Archive` / `Import`)
on top of that. The audit-log write to Keeper's Log remains out of scope
(deferred to M2b.7).

## Public API

The four exported methods on `*DB` (M2b.2.a) cover the in-process CRUD
surface that the harness will call from M2b.4 onwards:

| Method                                               | Purpose                                                                                                                                                         | AC  |
| ---------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | --- |
| `Remember(ctx, e Entry) (string, error)`             | Insert into `entry` and `entry_vec` in one transaction; auto-generates a UUID v7 when `Entry.ID` is empty and defaults `CreatedAt` to `time.Now().UnixMilli()`. | AC1 |
| `Recall(ctx, q RecallQuery) ([]RecallResult, error)` | Cosine KNN against `entry_vec` joined back to `entry`; filters out superseded rows and rows whose `active_after` is in the future. Optional category filter.    | AC2 |
| `Forget(ctx, id string) error`                       | Atomic delete from both tables. Returns `ErrNotFound` when the id is well-formed but unknown.                                                                   | AC3 |
| `Stats(ctx) (Stats, error)`                          | Aggregate counts: total / active / superseded, plus `ByCategory` over active entries.                                                                           | AC4 |

Sentinel errors live in `errors.go`:

- `ErrInvalidEntry` — bad shape (empty content, wrong embedding dim, bad
  category, non-canonical UUID).
- `ErrNotFound` — `Forget` called against a missing id.
- `ErrCorruptArchive` — `Import` was given a payload that fails the
  SQLite-header / required-schema check.
- `ErrTargetNotEmpty` — `Import` was called against a live DB that still
  has at least one row in `entry`.

## Snapshot lifecycle

M2b.2.b adds the snapshot half of the substrate on top of the M2b.2.a CRUD
surface:

| Method                             | Purpose                                                                                                                                                                                                                                                                                                                                                                                                                                              | AC      |
| ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------- |
| `Archive(ctx, w io.Writer) error`  | Run SQLite's native `VACUUM INTO <tempfile>` against a temp file under `os.TempDir()`, then stream the bytes to `w`. Read-only with respect to the live `*DB` — concurrent reads/writes during Archive are safe. An empty agent still produces a valid snapshot (the schema rides along).                                                                                                                                                            | AC1     |
| `Import(ctx, src io.Reader) error` | Spool `src` to a hidden `.notebook-import-*.sqlite` file in the SAME directory as `agentDBPath(...)` (so `os.Rename` does not cross filesystems on POSIX), validate via the SQLite magic header + the required `entry`/`entry_vec` tables and the two partial indexes, refuse on a non-empty target, then close + rename + reopen. The receiver's internal `*sql.DB` is swapped in place so existing callers see the imported data on the next call. | AC2/AC4 |

`Import` is **strict**: when the live `entry` table has any rows it returns
`ErrTargetNotEmpty` without touching the on-disk file. Callers wanting an
overwrite-with-archive flow should layer `Archive` + Forget-all + `Import`
themselves; M2b.6's CLI may add a `--force` flag for this, but the substrate
itself never drops live data.

`Import` validation failures wrap `ErrCorruptArchive`. The validator
checks the SQLite file-format magic header (`"SQLite format 3\x00"`) plus
the presence of every object the package emits during `openAt`: the
`entry` table, the `entry_vec` virtual table (which appears in
`sqlite_schema` with `type = 'table'`), and the two partial indexes
(`entry_category_active`, `entry_active_after`). A snapshot taken from an
older binary that pre-dates the partial indexes is therefore rejected as
corrupt rather than transparently re-created — callers should re-archive
under the current binary before importing.

### Concurrency contract

`Import` takes a per-receiver `sync.Mutex` for the duration of the call
and closes + reopens the underlying `*sql.DB` in the middle. Callers MUST
NOT invoke other `*DB` methods concurrently with an Import on the same
receiver: a Recall in flight would race the connection swap. After Import
returns successfully the receiver is fully usable on the new file.

### ArchiveStore handoff

Both methods deliberately speak in `io.Reader` / `io.Writer` so the
package stays storage-agnostic. M2b.3 wraps Archive/Import with an
`ArchiveStore` interface and LocalFS / S3 backends; this package will
provide the bytes either direction without knowing where they end up.

Two partial indexes back the hot read paths and are added by the idempotent
schema-init (AC5):

- `entry_category_active ON entry(category) WHERE superseded_by IS NULL`
- `entry_active_after ON entry(active_after) WHERE superseded_by IS NULL`

The indexes use `CREATE INDEX IF NOT EXISTS` so an existing M2b.1 file
upgrades transparently the first time it is opened by an M2b.2.a binary;
the migration is exercised by `TestSchema_IndexesAddedOnReopen`.

## Driver decision

This package uses **Option A** from the M2b.1 task brief: the CGo driver
[`github.com/mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3) v1.14.44
paired with
[`github.com/asg017/sqlite-vec-go-bindings/cgo`](https://github.com/asg017/sqlite-vec)
v0.1.6, which compiles `sqlite-vec` into the binary and registers it as a
SQLite auto-extension via `sqlite_vec.Auto()` before the first connection is
opened.

**Option B** (the CGo-free combination of `ncruces/go-sqlite3` + the
`sqlite-vec-go-bindings/ncruces` WASM bundle) was prototyped first because it
avoids CGo entirely. The bundle shipped in v0.1.6 / v0.1.7-alpha.2 of the
ncruces sub-binding uses WebAssembly threads/atomic instructions
(`i32.atomic.store`) that the wazero runtime version pinned by the
asg017 binding (v1.7.3) does not enable. The WASM module fails to compile
inside the Go runtime, so `Open` cannot reach `vec_version()`. The trade-off
of Option A is that consumers must build with `CGO_ENABLED=1` and a working
C toolchain; we accept it because every consumer of this package is the
Notebook tool inside a watchkeeper agent process which already links other
CGo deps.

If a future asg017 release ships a WASM bundle compatible with a
threads-aware wazero (or ncruces upgrades wazero past the threads gap), this
package can swap to Option B without touching the public API.

## File location and `WATCHKEEPER_DATA`

`agentDBPath(agentID)` resolves to `<data>/notebook/<agent_id>.sqlite`,
where `<data>` is:

- `$WATCHKEEPER_DATA` when set (e.g. operators pointing the agent at a
  user-specific data directory or a tmpfs);
- `$HOME/.local/share/watchkeepers` otherwise (XDG-style default for
  Linux/macOS).

Notebook directories are created with mode `0o700` (`os.MkdirAll` +
explicit `os.Chmod` to defeat a permissive umask), so only the owning user
can read another agent's notebook on a shared host.

The `<agent_id>` segment must be a canonical RFC 4122 UUID (8-4-4-4-12 hex
with dashes). The validator mirrors the regex used by the Keep server's
`handlers_write.go` so an agent that legitimately calls Keep can also open
its notebook with the same id.

## Schema

The `entry` table holds 12 columns specified by ROADMAP §M2b.1: `id`
(PK / UUID v7), `category` (CHECK constraint over the five fixed values),
`subject`, `content`, `created_at`, `last_used_at`, `relevance_score`,
`superseded_by` (self-FK), `evidence_log_ref`, `tool_version`, and
`active_after` (default 0). All `INTEGER` timestamps are Unix epoch
**milliseconds** to match the rest of the watchkeepers stack.

Embeddings live in a sibling `entry_vec` virtual table built with
`vec0(id TEXT PRIMARY KEY, embedding float[1536])`. The 1536 dimension
mirrors the Keep server's `knowledgeChunkEmbeddingDim` so an entry promoted
from Notebook to Keep keeps the same vector shape. Two tables joined by
`id` is the standard sqlite-vec layout: it keeps `vec0`'s vector-only column
space separate from the regular SQL columns, so common queries that don't
touch the embedding don't have to read it.

### Sync contract

The `entry` and `entry_vec` tables must be kept in lock-step by all
callers. The `vec0` virtual table does **not** auto-cascade deletes or
updates from `entry`. Concretely: every INSERT into `entry` must be paired
with an INSERT into `entry_vec(id, embedding)` in the same transaction, and
every DELETE from `entry` (Forget / Archive) must also DELETE from
`entry_vec` by id. M2b.2 owns the transactional Insert / Delete wrappers
that enforce this contract. See the `# Sync contract` section in the
[package godoc](db.go) for the full specification.

## Per-agent isolation

Each agent has exactly one notebook file. The directory mode `0o700` is the
only filesystem-level guard: there is no row-level security or per-agent
encryption-at-rest in M2b.1. Operators concerned about cross-agent
information leaks on a shared host should rely on standard Unix user
isolation (separate UIDs per agent process) — running multiple agents under
the same UID lets each read every other notebook in the same data dir.

## ArchiveOnRetire

M2b.4 ships a harness-language-neutral library helper that orchestrates the
three already-merged primitives — `notebook.*DB.Archive`,
`archivestore.ArchiveStore.Put`, `keepclient.Client.LogAppend` — into one
shutdown-time call. It streams the live notebook through `Archive` →
`Put` via `io.Pipe` (no on-disk intermediate) and emits a
`notebook_archived` event to Keeper's Log on success. See the godoc on
[`ArchiveOnRetire`](archive_on_retire.go) for the full sequence and the
partial-failure return-shape contract.

```go
client := keepclient.NewClient(...)
store, _ := archivestore.NewLocalFS(archivestore.WithRoot("/var/lib/watchkeepers"))
defer func() {
    uri, err := notebook.ArchiveOnRetire(shutdownCtx, db, agentID, store, client)
    if err != nil {
        // log; URI may still be set if only the audit emit failed —
        // caller can retry just the LogAppend with the same payload.
    }
}()
```

The helper builds in NO retry logic. Callers distinguish three outcomes by
inspecting the `(uri, err)` tuple:

- `("", err)` — Archive→Put failure, snapshot never landed; retry the
  whole call while the agent process is still alive.
- `(uri, err)` — Put succeeded, audit emit failed; the snapshot is in the
  store and the caller can retry just the audit with the same payload.
- `(uri, nil)` — full success.

Wiring this helper INTO any specific harness (Go binary, CLI shim, or TS
shellout) is deferred to a follow-up; M2b.4 ships only the library
function.

## PeriodicBackup

M2b.5 layers a periodic-backup helper on top of the same Archive→Put→
LogAppend pipeline `ArchiveOnRetire` uses. Where retire fires once at
graceful shutdown, `PeriodicBackup` blocks in a ticker loop and emits
`notebook_backed_up` audit events on every cadence interval. It exists so
agents that crash between graceful shutdowns still leave a recent
snapshot in the archivestore — the retire path covers clean exits, the
periodic loop covers everything else.

```go
go func() {
    err := notebook.PeriodicBackup(ctx, db, agentID, store, client, 30*time.Minute, func(uri string, err error) {
        if err != nil { /* log; next tick will retry */ }
    })
    // err is ctx.Err() on shutdown.
    _ = err
}()
```

Contract:

- `cadence` is a fixed `time.Duration`. Cron-expression scheduling
  (`@daily`, `0 */30 * * * *`, …) is **not** part of M2b.5 — see M3.3 for
  the `robfig/cron` integration. A non-positive cadence returns the new
  `ErrInvalidCadence` sentinel synchronously without starting a goroutine.
- Per-tick failures are best-effort: `Archive`, `Put`, or `LogAppend`
  errors are surfaced via the optional `onTick` callback but do **not**
  exit the loop. The next tick still fires. If a transient archivestore
  outage drops three ticks in a row the fourth still tries.
- `onTick` is called **synchronously** on the loop goroutine (no
  per-tick `go func()`). A slow callback delays the next ticker fire —
  Go's `time.Ticker` drops missed ticks rather than queueing. Keep
  callbacks fast (log + return); spawn a goroutine inside the callback
  if you need async work.
- The loop exits only when `ctx` is cancelled, returning the
  cancellation error (`context.Canceled` / `context.DeadlineExceeded`).
  An in-flight tick is allowed to finish current step (the ctx
  cancellation propagates into `Archive` / `Put` / `LogAppend`, all of
  which honour ctx).

The shared Archive→Put→LogAppend body lives in a private
`archiveAndAudit(ctx, db, agentID, store, logger, eventType)` so
`ArchiveOnRetire` and `PeriodicBackup` agree on the partial-failure
contract by construction. The only difference between the two callers is
the `event_type` written to keepers_log: `"notebook_archived"` vs
`"notebook_backed_up"`. Downstream subscribers distinguish the two via
that column.

## Out of scope (still deferred)

- Watchmaster `promote_to_keep` — see M2b.8.
