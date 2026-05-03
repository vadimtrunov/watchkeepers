# notebook — per-agent SQLite + sqlite-vec storage substrate

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/notebook`

This package owns the on-disk storage layer for an agent's private memory
("Notebook"). It opens (or creates) a per-agent SQLite file at
`$WATCHKEEPER_DATA/notebook/<agent_id>.sqlite`, applies
`PRAGMA journal_mode=WAL`, and ensures the schema exists. M2b.2.a adds the
in-process CRUD surface (`Remember` / `Recall` / `Forget` / `Stats`) on top of
the substrate. `Archive` / `Import` (snapshot lifecycle) and the audit-log
write to Keeper's Log remain out of scope and are deferred to M2b.2.b and
M2b.7 respectively.

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

## Out of scope (still deferred)

- `Archive` / `Import` snapshot lifecycle — see M2b.2.b.
- ArchiveStore for retired entries — see M2b.3.
- Audit-log integration with the Keep `keepers_log` — see M2b.7.
- Watchmaster `promote_to_keep` — see M2b.8.
