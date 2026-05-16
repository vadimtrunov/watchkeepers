# Project Lessons — M2b (Notebook library)

Patterns and decisions for the M2b milestone family of
`docs/ROADMAP-phase1.md` (per-agent embedded SQLite Notebook, ArchiveStore,
archive-on-retire, periodic backup, audit emission, promote-to-keep).

Appended by the `rdd` skill at Phase 7 when the closed TASK belongs to M2b.
Read by the `rdd` skill at the start of Phase 2 when the next TASK is in
the same milestone family.

See `docs/LESSONS.md` for the index across all milestones.

---

## 2026-05-16 — M2b verification bullet 216: gated 10k recall-latency benchmark with revised budget

**PR**: pending — to be opened in Phase 8 of `ship-roadmap-item`
**Merged**: pending — to be merged in Phase 8 of `ship-roadmap-item`

### Context

Closed the unverified M2b verification line "Recall latency stays sub-millisecond at 10k entries (benchmark gated)" — bullet 216 in `docs/ROADMAP-phase1.md` §M2b — and DoD-closure-plan item B1 from §10. The benchmark scaffolding (`core/pkg/notebook/recall_bench_test.go`) had already been committed in `9c872ef` behind a `//go:build benchmark` tag with a hard 1 ms p99 assertion test, but the make target it referenced (`make notebook-bench`) did not exist and the bench had never been run. First execution showed p50 ≈ 30 ms, p99 ≈ 37 ms on dev hardware — ~37× over the bullet's original ceiling. Investigation confirmed sqlite-vec currently ships only brute-force KNN via the `vec0` virtual table (HNSW on their roadmap, not released), so 1536-dim float32 dense vectors × 10k rows is architecturally bounded around 25–40 ms p50 on commodity CPUs. Operator decision: revise the bullet's budget to p99 < 100 ms (measured dev p99 ≈ 37 ms plus generous CI-runner headroom — the bench flaked near a tighter 50 ms ceiling) and file Phase 2 M7.5 to drive it back toward sub-millisecond via quantization / tiered retrieval / ANN backend swap.

### Pattern

**Benchmark scaffolding without execution is not "shipped"**: The original commit `9c872ef` added a complete bench + a p99-budget assertion test, but the file referenced a `make notebook-bench` target that did not exist and the bench was never run. Result: a budget assertion frozen at an aspirational value with no signal that it was unreachable. Pattern: a gated benchmark is only "done" when (a) the make target exists, (b) the bench has been executed end-to-end, and (c) the measured numbers appear in the commit message or lesson entry. Without those three, the benchmark is scaffolding, not verification.

**Aspirational latency budgets need backend-grounding before they ship to ROADMAP**: The "sub-millisecond at 10k entries" target was set without empirical measurement and without checking sqlite-vec's index capability (brute-force-only at `vec0`, no HNSW). The backend determines the achievable budget more than the code does. Pattern: a latency criterion in a ROADMAP must name the backend assumption — "p99 < 100 ms on sqlite-vec brute-force KNN at 1536-dim corpus" — so a future reader can tell whether tightening requires code work or a backend swap.

**Test name should encode intent, not the current threshold**: Renamed `TestRecallLatencyP99Under1ms` → `TestRecallLatencyP99WithinBudget`. The function asserts against a `recallP99Budget` constant; if the budget tightens (Phase 2 M7.5), the test name does not need to follow. Pattern: when the assertion uses a named constant, the test name should reference the constant's purpose, not its current value — generic survives change, specific decays.

**Gated bench files document the run path in their header doc-block**: The file's `//go:build benchmark` build tag means accidental `go test ./...` runs do not seed 10k rows or burn 40 s. A future engineer who opens the file must be able to run it without grepping for the build tag. The header doc-block includes both `make notebook-bench` and the raw `go test -tags=benchmark -bench=... -run=... ./...` command, so the file is self-documenting. Pattern: any test gated behind a build tag must include its run command in the top-of-file doc-block.

**Benchmark query vectors should be drawn independently from the shared seeded RNG, not copied from the corpus**: The bench seeds 10k entries from a PCG-seeded RNG stream, then draws the query embedding from the same stream as a separate fresh sample — sharing the stream gives reproducibility across runs and machines, and the independence (rather than reusing an entry vector verbatim) prevents the brute-force `vec0` scan from short-circuiting on a distance-0 top hit. Pattern: when designing a synthetic latency benchmark for a KNN backend, the query must come from the same distribution as the corpus (so the inner loop does real ranking work) but must NOT be one of the corpus rows (else the index short-circuits and you measure the wrong workload).

### References

- Files: `core/pkg/notebook/recall_bench_test.go` (budget + test rename + doc-block update), `Makefile` (new `notebook-bench` target).
- Docs: `docs/ROADMAP-phase1.md` §M2b verification bullet 216 → `[x]` with revised budget; §1.1 Status Dashboard M2b Notes column updated; §10 DoD Closure Plan B1 → `[x]`. `docs/ROADMAP-phase2.md` §M7 — added M7.5 (recall-latency optimization toward sub-ms) and matching verification bullet.
- Measured numbers (Apple M-series, VirtualApple @ 2.50 GHz, amd64, 1000 samples, TopK=10, EmbeddingDim=1536, corpus=10 000 entries): p50 = 30.45 ms, p99 = 36.52 ms, max = 100.10 ms; BenchmarkRecallAt10k ≈ 30.47 ms/op, 21 160 B/op, 227 allocs/op.

---

## 2026-05-03 — M2b.1: Notebook SQLite + sqlite-vec storage substrate

**PR**: [#19](https://github.com/vadimtrunov/watchkeepers/pull/19)
**Merged**: 2026-05-03 (squash commit `814e68c`)

### Context

Established the Notebook library's storage substrate using SQLite + sqlite-vec for vector embeddings. Three integration paths existed for SQLite + sqlite-vec in Go (mattn CGo, ncruces+wazero WASM, modernc pure-Go). After prototyping Option B (ncruces+wazero CGo-free), executor discovered a blocker: wazero v1.7.3 cannot enable `i32.atomic.store` instructions used in sqlite-vec's WASM bundle. Fallback to Option A (mattn/go-sqlite3 v1.14.44 + asg017/sqlite-vec-go-bindings/cgo v0.1.6) was clean and well-documented. Code-reviewer flagged a blocker on foreign-key enforcement and an important sync-contract documentation gap; fixer resolved both in one commit.

### Pattern

**Two-prong driver evaluation with documented fallback**: When adopting a new dependency with multiple integration options, encode the matrix in the TASK file with explicit reject criteria BEFORE writing code. Executor picked Option B per preference-driven rubric, hit a WASM-incompatibility wall, and fell back to Option A with confidence because the decision tree was already in place. Pattern: decision-matrix THEN evidence-driven pick THEN clean fallback.

**SQLite foreign-key enforcement OFF by default per connection**: `superseded_by TEXT NULL REFERENCES entry(id)` is silently a no-op unless the connection sets `PRAGMA foreign_keys=ON`. Mattn driver supports `_foreign_keys=on` DSN flag. Every new SQLite connection must (a) enable foreign-keys via DSN, (b) read it back via `PRAGMA foreign_keys` with fail-loud error if misnamed, and (c) carry a constraint-rejection negative test. Pattern: idempotent pragma readback mirrors M2.7.e.b's WAL pattern.

**Two-table sqlite-vec layout has explicit sync contract**: The vec0 virtual table and regular table joined on `id` is the canonical sqlite-vec pattern, but there is NO auto-cascade. INSERTs and DELETEs must be paired in the same transaction; UPDATE of join key is symmetric. Document the contract in package godoc + README so the next-layer API (M2b.2) doesn't get it wrong. Reviewer caught this on iter 1; fixer added the docs in `# Sync contract` godoc subsection + README mirror.

### References

- Files: `core/pkg/notebook/{db,path}.go` (+ `_test.go`), `core/pkg/notebook/README.md`, `go.mod` (added `mattn/go-sqlite3`, `asg017/sqlite-vec-go-bindings/cgo`)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.1. **M2b.1 substrate ready**; M2b.2 owns the `Remember`/`Recall`/`Forget`/`Archive`/`Import`/`Stats` public API and Recall-supporting indexes.

---

## 2026-05-03 — M2b.2.a: Notebook in-process CRUD (Remember/Recall/Forget/Stats)

**PR**: [#20](https://github.com/vadimtrunov/watchkeepers/pull/20)
**Merged**: 2026-05-03 (squash commit `6de9be1`)

### Context

Implemented the Notebook library's public CRUD API layer atop the M2b.1 substrate. Four endpoints (`Remember`, `Recall`, `Forget`, `Stats`) operationalize the sync contract between the `entry` and `entry_vec` tables. Phase 1 planner verdict was "too large"; decomposed M2b.2 into M2b.2.a (CRUD) and M2b.2.b (Archive/Import). M2b.2.a includes 18 tests passing under `-race`, schema migrations for partial indexes, and support for KNN recall with post-filtering. Code-reviewer converged at iteration 1 with zero blockers and zero importants (4 nits deferred to Follow-up).

### Pattern

**Sync-contract enforcement via single-tx wrapper**: M2b.1 documented the `entry`/`entry_vec` sync contract in godoc; M2b.2.a operationalized it — every public method that touches either table wraps the work in `BeginTx` + `defer tx.Rollback()` + explicit `tx.Commit()`. The `Remember` and `Forget` paths each touch both tables in one tx; rollback fires automatically on any error. Pattern: when a "sync contract" between two SQL artifacts is documented in the substrate, the layer above MUST wrap every mutation in a transaction so a partial failure doesn't leave the contract broken.

**sqlite-vec canonical query shape: `WHERE embedding MATCH ? AND k = ?`**: NOT `ORDER BY vec_distance_cosine(...) LIMIT ?`. The MATCH+k form uses the vec0 index; the ORDER BY+LIMIT form does a full scan. Tested via `TestRecall_TopK` against ≥5 rows. Always use the indexed form for Recall/KNN queries.

**Partial indexes for hot Recall predicates**: `CREATE INDEX entry_category_active ON entry(category) WHERE superseded_by IS NULL` and `entry_active_after ON entry(active_after) WHERE superseded_by IS NULL` are the canonical partial-index pattern for "active rows only" filters. Added via `CREATE INDEX IF NOT EXISTS` in the schema-init constant so existing M2b.1-era files transparently migrate on the next `Open` (verified by `TestSchema_IndexesAddedOnReopen`). Pattern reusable for any append-mostly table where most queries filter by a "is active" predicate.

**`COUNT(*) FILTER (WHERE ...)` requires SQLite 3.30+**: mattn driver bundles 3.46+ so this is fine. The pattern lets `Stats` compute totals + active + superseded in one query without a CASE WHEN dance.

**Pre-DB validation symmetric with substrate constraints**: `validate(*Entry)` checks `Category` ∈ enum (matches the DB CHECK constraint), `Content != ""` (matches NOT NULL), `len(Embedding) == 1536` (matches vec0 dim). All three rejections return `ErrInvalidEntry` synchronously before BeginTx, so a malformed Entry never opens a transaction.

### References

- Files: `core/pkg/notebook/{entry,errors,remember,recall,forget,stats}.go` + `_test.go`, `core/pkg/notebook/schema_test.go`, `core/pkg/notebook/db.go` (schema-init delta), `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.2 → M2b.2.a. **M2b.2.b owns** Archive/Import (snapshot lifecycle).

---

## 2026-05-03 — M2b.2.b: Notebook Archive + Import snapshot lifecycle

**PR**: [#21](https://github.com/vadimtrunov/watchkeepers/pull/21)
**Merged**: 2026-05-03 (squash commit `4013de0`)

### Context

Implemented the Notebook library's snapshot lifecycle: `Archive` exports the live `entry`/`entry_vec` tables to a standalone SQLite file via `VACUUM INTO`, and `Import` atomically replaces the live file with a spool-validated copy, maintaining single-writer isolation and exact embedding-byte preservation. Two distinct sync contexts (`archiveMu` for atomic reads, `importMu` per-receiver) and WAL/SHM sidecar cleanup ensure clean replacements. Reviewer iteration 1 surfaced one `important`: embedding-byte round-trip test coverage gap (AC5 explicitly required "embedding-bytes" match, but test used Recall ranking instead of direct `SELECT`).

### Pattern

**`VACUUM INTO` DDL escaping via `os.CreateTemp` + single-quote escape**: SQLite `VACUUM INTO 'path'` is DDL, not DML, so prepared-statement `?` placeholders do not apply. Safe pattern: (1) call `os.CreateTemp(dirPath, ".prefix-*")` to generate a path from a trusted system call; (2) validate it's absolute + NUL-free; (3) escape any single quotes via `'` → `''` (the SQL escape for string literals); (4) concatenate into the query string. Defense-in-depth: the path comes from a trusted source (CreateTemp) AND explicit quote-escaping guards against edge cases. Annotate the gosec G202 finding with a rationale comment citing this contract.

**Atomic file replacement requires same-FS rename**: Cross-device `os.Rename` fails on POSIX systems. For `Import` to atomically swap the live SQLite file, the spool-temp MUST be created in the SAME directory as the live file — use `os.CreateTemp(filepath.Dir(target), ".prefix-*")` instead of `os.TempDir()`. Using a temp-dir on a different filesystem breaks across mountpoint boundaries. Verified by `TestImport_AtomicRename`.

**WAL/SHM sidecar removal before rename**: SQLite WAL journal mode leaves `<file>-wal` and `<file>-shm` files next to the live DB. After a `Close()` (which checkpoints WAL via mattn driver) but BEFORE `os.Rename`, explicitly remove `-wal` and `-shm` so the new connection doesn't inherit stale journal pages. Belt-and-suspenders because Close should checkpoint, but the files may linger; verified by `TestImport_CleansSidecars`.

**`sync.Once` reset trick for re-openable resources**: M2b.1's `closeOne sync.Once` makes Close idempotent, but `Import` reopens the underlying `*sql.DB`. Pattern: under the receiver's `importMu` lock, swap the `*sql.DB` field, assign a fresh `sync.Once{}` to the close-once field, AND clear the cached `closeErr`. A subsequent `Close()` then runs once on the new connection. This is the canonical "re-init a once-guarded resource" Go pattern.

**Embedding-bytes round-trip requires direct `SELECT` + `bytes.Equal`**: Recall's KNN ranking is not the same as byte-for-byte embedding equality. To verify `Archive` → `Import` preserves embedding data exactly, the test must `SELECT entry_vec.embedding FROM entry_vec WHERE id = ?` and `bytes.Equal` against `vec.SerializeFloat32(seed.embedding)`. Without direct byte comparison, a vec0 truncation or zeroing bug would silently pass the KNN-ranking test. Pattern: when an AC names specific match criteria (ID, category, content, embedding-bytes), the test must assert each one explicitly, not just "Recall returns the row".

### References

- Files: `core/pkg/notebook/{archive,import,validate_archive}.go` + `_test.go`, `core/pkg/notebook/{db,errors,README.md}` (deltas)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.2 (now `[x]`). **M2b.3 owns ArchiveStore** — wraps Archive/Import with LocalFS + S3Compatible backends.

---

## 2026-05-03 — M2b.3.a: ArchiveStore interface + LocalFS implementation

**PR**: [#22](https://github.com/vadimtrunov/watchkeepers/pull/22)
**Merged**: 2026-05-03 (squash commit `ba28046`)

### Context

Introduced the `ArchiveStore` interface as an abstraction for backup-tarball storage, with a LocalFS implementation storing tarballs under a root directory. Phase 1 planner decomposed M2b.3 into M2b.3.a (interface + LocalFS) and M2b.3.b (S3Compatible). M2b.3.a includes parameterized contract tests (`runContractTests`) reusable by M2b.3.b, path-traversal defenses, and RFC3339 timestamp filenames. Executor delivered all 5 AC green in 1 commit; Phase 4 fixer iter 1 resolved 1 important (embedding-byte round-trip test coverage via external SQLite access).

### Pattern

**Stdlib `archive/tar` + `compress/gzip` cover backup-tarball needs without a new dep**: M2b.3.a wraps a `notebook.Archive` snapshot in a single-entry tarball at `<root>/notebook/<agentID>/<RFC3339>.tar.gz` using only stdlib. Pattern: spool the snapshot to a temp file in the same dir as the target tarball (cross-FS-rename safe per M2b.2.b LESSONS), `os.Stat` it for `tar.Header.Size`, then write tar+gzip in one streaming pass. No memory blowup, no third-party deps.

**Path-traversal defence via `filepath.Clean` + `strings.HasPrefix`**: For any `Get(uri)`-style API where the URI maps to a filesystem path under a known root, the canonical defence is: parse the scheme, strip prefix, `filepath.Abs` + `filepath.Clean` both the input AND the root, then verify `strings.HasPrefix(cleanedInput, cleanedRoot + filepath.Separator)`. Reject otherwise with a wrapped sentinel. Catches `file:///etc/passwd`, `file://<root>/../../etc/passwd`, and `s3://` schemes uniformly.

**Parameterised contract test suite for backend interfaces**: When introducing an interface that will have multiple implementations (here: `ArchiveStore` with `LocalFS` now and `S3Compatible` later), write the contract tests as `func runContractTests(t *testing.T, factory func(t *testing.T) ArchiveStore)`. Each implementation calls it with its own factory. Future M2b.3.b will reuse the suite verbatim with a minio-backed factory; no duplicated assertions.

**RFC3339 with hyphens-not-colons for filename-safe timestamps**: `time.Now().UTC().Format("2006-01-02T15-04-05Z")` produces fixed-length, lex-sortable, filename-portable timestamps. Colons are illegal in Windows filenames; the dash variant works on every filesystem.

**Cross-package vec0 access requires `sqlitevec.Auto()` registration**: Tests that open a SQLite file from outside the `notebook` package must register the sqlite-vec extension before the first connection. The notebook package does this via `vecOnce.Do(func() { sqlitevec.Auto() })` in `db.go`; external packages should call the same helper from `init()` so the global SQLite auto-extension table contains the entry by the time `sql.Open(..., "?mode=ro")` runs. Duplicate registrations are safe (sqlite-vec is idempotent).

### References

- Files: `core/pkg/archivestore/{archivestore,localfs}.go` + `_test.go`, `core/pkg/archivestore/contract_test.go`, `core/pkg/archivestore/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.3 → M2b.3.a. **M2b.3.b plugs** S3Compatible into the parameterised contract suite.

---

## 2026-05-03 — M2b.3.b: S3Compatible ArchiveStore via minio-go + testcontainers-go

**PR**: [#23](https://github.com/vadimtrunov/watchkeepers/pull/23)
**Merged**: 2026-05-03 (squash commit `fafb346`)

### Context

Implemented the S3Compatible backend for ArchiveStore using minio-go/v7 and testcontainers-go/modules/minio. Extracted tarball-streaming helpers (`writeTarballStream`, `openTarballStream`) from LocalFS into `internal_tar.go` so both backends share wire-compatible code. Phase 3 executor delivered 2 commits (refactor + S3 feat) with 7 files total; 17 S3-specific sub-tests passed against a real minio Docker container. Reviewer iteration 1 converged immediately (0 blocker, 0 important, 5 nits). PR squash-merged; cascade commit `b573a95` closed M2b.3 entirely.

### Pattern

**Tarball helper extraction is the right call on the second backend, not the first**: M2b.3.a kept tar/gzip helpers private to `localfs.go`. M2b.3.b refactored them into `internal_tar.go` (`writeTarballStream`, `openTarballStream`) so both LocalFS and S3Compatible call the same helpers, guaranteeing identical wire bytes. Pattern: premature extraction after a single implementation is YAGNI; on the second implementation it becomes necessary. Timing: refactor as its own commit (M2b.3.b iter 0) so reviews separate interface concerns from helper canonicalization.

**`testcontainers-go/modules/minio` + `sync.Once` singleton for integration tests**: Official testcontainers module is safer than hand-rolled GenericContainer. Pattern: `var (testMinioOnce sync.Once; testMinioContainer *minio.Container; testMinioErr error)` with a `sharedMinioContainer(t)` helper lazily initializing on first call. Per-test bucket isolation via UUID names prevents state leakage. Ryuk reaper terminates the container on process exit (no manual `Terminate`). Skip tests on Docker-unavailable via substring matcher (`"docker daemon"`, `"connection refused"`, `"Cannot connect"`) returning `t.Skip`; no build tags needed, single `go test ./...` invocation works everywhere.

**`minio.Client.GetObject()` → `obj.Stat()` pattern for `NoSuchKey` detection**: `GetObject` returns `*minio.Object` immediately; the actual fetch happens on first `Read`. To detect missing objects up-front (mapping to `ErrNotFound`), call `obj.Stat()` first and check `minio.ToErrorResponse(err).Code == "NoSuchKey"`. Without this, the error surfaces inside the tarball reader on first `Read`, where it's harder to map cleanly to a sentinel.

**Per-test bucket isolation without manual cleanup**: Contract tests create a fresh UUID-named bucket for each invocation. No explicit teardown needed — Ryuk reaper cleans up the whole container on exit, and bucket-creation latency (~10ms) is negligible against the container startup cost. Pattern is idempotent and test-friendly.

### References

- Files: `core/pkg/archivestore/{s3,internal_tar}.go` + `s3_test.go`, `core/pkg/archivestore/{localfs,README}.go` (refactored), `go.mod` (+ minio-go/v7 v7.1.0, testcontainers-go/modules/minio v0.42.0)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.3 (now `[x]`). **M2b.4 onward** (Archive on retire / Periodic backup / Import / Audit-log) plug into the stable ArchiveStore interface.

---

## 2026-05-03 — M2b.4: Notebook ArchiveOnRetire shutdown helper

**PR**: [#24](https://github.com/vadimtrunov/watchkeepers/pull/24)
**Merged**: 2026-05-03 (squash commit `6028b53`)

### Context

Implemented `notebook.ArchiveOnRetire` as a blocking orchestrator that chains Archive→Put→LogAppend during graceful shutdown. Three integration paths existed (literal `archivestore.ArchiveStore`, interface-based bridge, defer to caller). Planner decomposed to a library helper (this TASK) with harness-wiring deferred to M2b.4-successor. Phase 3 executor delivered 1 commit (+675 LOC, 8 tests). Phase 4 iter 1 surfaced 2 important items (real goroutine leak on producer-side unblock failure; test masking the leak via `io.Copy`-before-error). Fixer resolved both in 1 commit; Phase 4 iter 2 converged.

### Pattern

**`io.Pipe` Archive→Put bidirectional unblock required**: Streaming a producer into a consumer via `io.Pipe` works only if BOTH sides can unblock each other on failure. Producer side (Archive goroutine writing): if consumer aborts early, the next `pw.Write` blocks forever. Fix: after consumer's `Put(ctx, agentID, pr)` returns with non-nil error, call `pr.CloseWithError(putErr)` from the main goroutine BEFORE draining the producer error channel. Without this, real ArchiveStore impls (S3 auth failure, ECONNREFUSED before reading) leak the producer goroutine. Pattern: any `io.Pipe`-based streaming must terminate the producer explicitly on consumer failure, not just on context cancellation.

**Test fakes that drain before checking error mask producer-blocking bugs**: `fakeStore.Put` doing `io.Copy(&buf, src)` BEFORE checking injected error makes the test pass for the wrong reason — the pipe was fully drained, so the producer completed naturally. Real impls fail BEFORE reading. Pattern: when testing `(consumer, producer)` via `io.Pipe`, the fake consumer MUST fail without consuming. Add a `failBeforeRead bool` flag; test the early-fail path.

**Local interface to break import cycles**: M2b.4 was specced to take `archivestore.ArchiveStore`, but `archivestore/*_test.go` imports `notebook` for round-trips. Adopting `archivestore.ArchiveStore` in the consumer would cycle in the test build. Solution: define a local interface in the consumer (`notebook.Storer { Put(ctx, agentID, io.Reader) (string, error) }`). Go's structural typing lets concrete `archivestore` impls satisfy it without changes. Pattern: when a downstream package needs a type from an upstream package whose tests depend on the downstream, define a local interface and let structural typing bridge.

### References

- Files: `core/pkg/notebook/{archive_on_retire,archive_on_retire_test}.go`, `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.4. **Future M2b.4-successor** wires this helper into the actual harness once TS-vs-Go ambiguity is resolved.

---

## 2026-05-03 — M2b.5: Notebook PeriodicBackup helper

**PR**: [#25](https://github.com/vadimtrunov/watchkeepers/pull/25)
**Merged**: 2026-05-03 (squash commit `0c0bb82`)

### Context

Implemented `notebook.PeriodicBackup` as a best-effort periodic helper for archive-on-retire lifecycle. Uses `time.NewTicker` with `time.Duration` cadence (cron deferred to M3.3) and extracted a private `archiveAndAudit(ctx, db, agentID, store, logger, eventType)` helper from the original M2b.4 inline pipeline. Refactoring preserved the M2b.4 goroutine-leak fix via `pr.CloseWithError`. Phase 3 executor delivered 1 commit (+542 LOC, 7 tests passing under `-race`). Phase 4 iter 1 converged 0/0/0 (zero blocker/important/nit). Phase 6 CI green 9/9. Phase 7 PR squash-merged.

### Pattern

**Refactor-on-second-caller, not first**: M2b.4 shipped the Archive→Put→LogAppend pipeline inline in `ArchiveOnRetire`. M2b.5 needed the same pipeline with a different EventType — extracted a private `archiveAndAudit(ctx, db, agentID, store, logger, eventType)` helper. Pattern: when a second caller of the same logic appears, refactor THEN. Premature extraction on the first caller is YAGNI; on the second it is necessary. Same pattern successfully applied in M2b.3.b (tarball-streaming helpers extraction).

**`time.NewTicker` + `select { ctx.Done, ticker.C }` for periodic best-effort jobs**: Canonical Go pattern for "fire every N, exit on cancel". `time.NewTicker(cadence)` + `defer ticker.Stop()` to avoid goroutine leak. The `select` is two-way: `<-ctx.Done()` returns `ctx.Err()` cleanly; `<-ticker.C` runs the work. Per-tick failures DO NOT exit the loop — backups are best-effort; the next tick retries. The optional `onTick` callback is called synchronously (NOT spawned) — caller's responsibility to keep it fast. `time.Ticker` drops missed ticks rather than queueing, so a slow callback can cause skipped ticks but will not pile up.

**Polling deadline tests over fixed-sleep tests for time-driven loops**: `cadence=10ms` + `sleep(30ms)` is flaky on loaded CI (only 1 tick fires when the test expected ≥2). Replace with: `cadence=25ms` + poll until counter ≥ N OR deadline (e.g. 3s). The poll exits as soon as the assertion is met OR fails the test on timeout. Robust under jitter without slowing happy-path runs. Pattern reusable for any time-driven loop's tests.

**`flakyStore` pattern for fail-then-succeed test scenarios**: To test that a periodic loop survives transient failures, give the fake an internal counter that fails on odd-numbered Put calls and succeeds on even (or any other deterministic predicate). The test polls until BOTH at-least-one-error and at-least-one-success have been observed via the onTick callback. Avoids tying the assertion to a specific tick number.

### References

- Files: `core/pkg/notebook/{periodic_backup,archive_on_retire}.go` + `_test.go`, `core/pkg/notebook/{errors.go,README.md}`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.5. **Future**: cron-expression-driven scheduling lands in M3.3 (`robfig/cron`).

---

## 2026-05-04 — M2b.6: Notebook ImportFromArchive helper

**PR**: [#26](https://github.com/vadimtrunov/watchkeepers/pull/26)
**Merged**: 2026-05-04 (squash commit `534a5fe`)

### Context

Implemented `notebook.ImportFromArchive(ctx, db, agentID, fetcher)` as a library helper that streams an archive tarball into the live Notebook via a `Fetcher` interface abstraction. Complements M2b.4/M2b.5's `Storer` interface for archive orchestration. Phase 1 planner verdict: "fits" (~3–4 files, ≤ 1 day); executor (opus) delivered 1 commit (+714 LOC, 10 tests passing under `-race`). Phase 4 iter 1 converged 0/0/0. Phase 6 CI green 9/9. Phase 7 PR squash-merged (`534a5fe`); ROADMAP M2b.6 marked `[x]` (`0e88a44`).

### Pattern

**Single-method `Fetcher` interface complements `Storer` for cross-package consumption**: M2b.4/M2b.5 use `notebook.Storer { Put(...) }`; M2b.6 introduces `notebook.Fetcher { Get(...) }`. Both are one-method interfaces — Go's "accept interfaces, return structs" idiom. Concrete `archivestore.LocalFS` and `archivestore.S3Compatible` satisfy both via structural typing without explicit declaration. Pattern: when a downstream package needs DIFFERENT methods of an upstream package's type at different sites, define separate single-method interfaces in the downstream rather than extending one or accepting the wide concrete type.

**Defer LIFO ordering matters when one resource owns another**: `ImportFromArchive` does `defer rc.Close()` THEN `defer db.Close()`. Go runs defers in reverse, so `db.Close` runs FIRST when the function returns — that's correct because `db.Import(ctx, rc)` is a method on `db` that consumes `rc`; closing `rc` first while `db.Close` is still running could leave the database mid-flush with no source. Pattern: when X depends on Y for its operation, defer Y.Close BEFORE X.Close (so X.Close runs first via LIFO).

**Import-then-audit ordering with explicit data-presence test**: When orchestrating "side effect → audit emit", the data lands BEFORE the audit. If audit fails, the test must explicitly verify the data is still in place — not just that the function returned an error. M2b.6's `TestImportFromArchive_LogAppendFails` reopens the destination notebook AFTER the audit failure and runs `assertImportedSeed` for every seed; without this assertion, a regression where Import is also rolled back on audit failure would silently break the partial-failure contract.

**Cross-package `Fetcher` compile-time check via test-only import**: `var _ Fetcher = (*archivestore.LocalFS)(nil)` in `import_from_archive_test.go` confirms structural compatibility at build time. Production `notebook` code MUST NOT import `archivestore` (cycle: archivestore tests import notebook). But test files compile separately into a different binary — they CAN import archivestore. Pattern: when verifying interface compliance across packages with a one-way cycle, put the compile-time `var _ Iface = ...` in `_test.go`, not in production code.

### References

- Files: `core/pkg/notebook/import_from_archive.go` + `_test.go`, `core/pkg/notebook/README.md` (delta)
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.6. **Future**: `wk notebook import <wk> <archive>` CLI lands in M10.3 on top of this helper; auto-inheritance policy is Phase 2.

---

## 2026-05-04 — M2b.7: Notebook mutating ops emit correlated audit events

**PR**: [#27](https://github.com/vadimtrunov/watchkeepers/pull/27)
**Merged**: 2026-05-04 (squash commit `fd3caeb`)

### Context

Wired `notebook.Remember` and `notebook.Forget` to emit audit events via the Keep audit log (`keepers_log` table) when the mutation succeeds. Phase 1 planner verdict: "fits" (~5 files, ≤ 1 day); executor (opus) delivered 1 commit (+545 LOC, 8 new tests passing under `-race`; 76 total tests in package — 68 legacy preserved). Phase 4 iter 1 converged 0/0/0. Phase 6 CI green 9/9, 0 review threads. Phase 7 PR squash-merged (`fd3caeb`); ROADMAP M2b.7 marked `[x]` (`c76052b`).

### Pattern

**Functional options preserve backward compatibility for cross-cutting concerns**: `Open(ctx, agentID, opts ...DBOption)` adds variadic options WITHOUT breaking existing callers — `Open(ctx, agentID)` compiles and runs unchanged. `WithLogger(Logger)` attaches a logger that all mutating ops automatically use. Pattern: when adding a cross-cutting concern (audit, metrics, tracing) to an existing API, use functional options to ship the new feature WITHOUT breaking callsites. Nil-default behavior preserves the prior contract; opt-in for the new contract.

**Audit emit AFTER commit, never before — partial-failure shape (id, err)**: For `Remember`/`Forget`, the audit emit fires only after `tx.Commit()` returns nil — data is durable BEFORE the audit attempt. If `LogAppend` fails, return `(id, fmt.Errorf("audit emit: %w", err))` (Forget returns just `error` since there's no id). This mirrors M2b.4's `ArchiveOnRetire` contract and gives callers two pieces of information: the data IS in the DB (so don't retry the mutation) and the audit isn't (so retry just the audit emit). Pre-commit failures (validation, tx error, `ErrNotFound`) skip the audit block entirely — auditing rolled-back operations would be incorrect.

**Audit payload excludes PII and large fields**: `notebook_entry_remembered` carries only `agent_id`, `entry_id`, `category`, `created_at`. NOT `content`, NOT `embedding`, NOT `subject`. Audit log answers "what happened" not "what was stored" — the actual data is recoverable from the DB. Including 1536-float embeddings (~6 KiB each) or arbitrary user content would bloat the keepers_log table and create a PII surface. Tests carry explicit banned-field assertions to prevent regressions ("payload does NOT contain `content`").

### References

- Files: `core/pkg/notebook/{db,remember,forget}.go`, `core/pkg/notebook/mutation_audit_test.go` (new), `core/pkg/notebook/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.7. Keep is the single audit authority; Notebook has no audit surface of its own.

---

## 2026-05-04 — M2b.8: promote_to_keep helper for Watchmaster proposal flow

**PR**: [#28](https://github.com/vadimtrunov/watchkeepers/pull/28)
**Merged**: 2026-05-04 (squash commit `5bcaa80`)

### Context

Implemented `keep.PromoteToKeep(ctx, db, proposal)` as a read-only helper that emits a `notebook_promotion_proposed` audit event when a Watchmaster user proposes promoting a Notebook entry to Keep status. No side effects on the Keep database — the helper reads the entry, validates it meets Promote criteria (category, language, embedding present), and emits an audit event for orchestration downstream (M6.2 final-write). Phase 1 planner verdict: "fits" (~4–6 files, ≤ 1 day, deps M2b.1–M2b.7 closed); executor (opus) delivered 2 commits (+264 LOC in promote.go, +471 LOC in promote_test.go, +82 lines in README.md). Phase 4 iter 1 converged 0/0/5 nits. Phase 6 CI green 9/9, 0 blocking review threads. Phase 7 PR squash-merged (`5bcaa80`); ROADMAP M2b.8 marked `[x]` (`5c75bce`); entire M2b milestone now complete (`[x]`).

### Pattern

**Proposal struct shape mirrors upstream schema columns without importing upstream**: M2b.8 defines `keep.Proposal` with `AgentID`, `EntryID`, `Category`, `EmbeddingVector` — the same columns as `notebook.Entry` in `deploy/migrations/004_knowledge_chunk.sql`. Rather than importing `notebook.Entry` (which would create the same archivestore→notebook one-way-import cycle), the consumer (keep) mirrors the minimal shape it needs. Pattern: when a downstream package interfaces with an upstream schema, define a struct locally that captures only the columns you read or validate — avoid the upstream import; let the caller do the mapping if they own both.

**Two-stage promotion event taxonomy: proposal vs final-write**: M2b.8 emits `notebook_promotion_proposed` (proposal stage); M6.2 will emit `notebook_promoted_to_keep` (final-write stage). When a workflow has "request → approval → action" shape, give each stage its own event type. Pattern: audit consumers can build state machines without inferring intent from context ("did the proposal succeed? check if there's a final-write event yet") — each event type is a terminal fact at its stage.

**Embedding round-trip via binary.LittleEndian mirrors sqlitevec.SerializeFloat32**: The keep binding does not expose a public Deserialize helper. Consumer-side decoder reads 4-byte LE floats directly via `binary.LittleEndian.Uint32()` cast to `float32`. Doc-comment cites the encoding contract (`sqlitevec.SerializeFloat32` uses LE) so a future sqlite-vec major version bump triggers a visible test failure rather than silent corruption. Pattern: when a dependency's serialization contract is not explicitly public-API, doc-comment the consumer's decoding logic with a citation so the coupling is visible.

**Read-only audit emit (no transaction required)**: M2b.7 emit-after-tx-commit was about durability; M2b.8 emits-after-read because PromoteToKeep has no write side-effect. The return shape and error-handling remain identical: `(populated, fmt.Errorf("audit emit: %w", err))` vs `(fmt.Errorf("audit emit: %w", err))` per whether there's an id to return. Only the gate condition changes (post-Commit vs post-SELECT). Pattern: audit emit shape and error contract are the same regardless of write/read; only the pre-condition changes.

### References

- Files: `core/pkg/keep/promote.go`, `core/pkg/keep/promote_test.go`, `core/pkg/keep/README.md`
- Docs: `docs/ROADMAP-phase1.md` §M2b → M2b.8. **M2b complete**: all 8 leaves now `[x]`. Phase 1 Notebook surface is feature-complete.

---

## 2026-05-04 — M2b verification: gap-aware toggle PRs (Outcome C is fine)

**PR**: [#39](https://github.com/vadimtrunov/watchkeepers/pull/39)
**Merged**: 2026-05-04

### Pattern

**Partial coverage is normal in milestone audits — toggle what has evidence, leave gaps `[ ]` with rationale, recommend a dedicated PR for each gap**: when auditing milestone-level acceptance bullets, not every bullet will have matching test evidence. The right move is to toggle the covered ones and leave the rest `[ ]` with a documented rationale and a recommended-next-step. Do not bundle gap-filling work into the audit PR — it inflates scope and dilutes the audit's evidentiary purpose. Bullet 216 (sub-ms recall latency at 10k entries, benchmark gated) required a dedicated benchmark PR (~150 lines: seed + p99 latency assertion + build-tag gating + Makefile target) and was correctly left `[ ]`. _Update 2026-05-16_: the dedicated bench PR landed; bullet now reads "p99 < 100 ms" — the sub-ms aspiration was empirically unreachable on sqlite-vec brute-force `vec0` at 1536-dim, and the sub-ms goal moved to Phase 2 M7.5. See the top-of-file 2026-05-16 entry for full context. Reviewers verify both: that toggled bullets have evidence AND that untoggled bullets have a recommended-next-step. Audit PRs are about discipline, not maximum-toggle-count.

### References

- Docs: `docs/ROADMAP-phase1.md` §M2b (verification audit). Pattern applies to all future milestone toggle PRs.

---
