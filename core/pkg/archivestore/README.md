# archivestore — backend-agnostic snapshot store for `notebook.DB`

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/archivestore`

This package wraps the snapshot lifecycle of
[`core/pkg/notebook`](../notebook) — `Archive` produces SQLite bytes;
`Import` consumes them — with a backend-agnostic blob-store interface so
operators can swap the on-disk store for a remote object store without
touching agent code. M2b.3.a (this milestone) ships only the interface
surface and the first implementation: `LocalFS`. The S3-compatible
backend, the minio testcontainer wiring, and the secrets shape that
backend will consume land in M2b.3.b.

## Public API

```go
type ArchiveStore interface {
    Put(ctx context.Context, agentID string, snapshot io.Reader) (uri string, err error)
    Get(ctx context.Context, uri string) (io.ReadCloser, error)
    List(ctx context.Context, agentID string) ([]string, error)
}
```

| Method | Purpose                                                                                                                                                                      | Returned errors                |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| `Put`  | Write the snapshot under a backend-defined location keyed by `(agentID, time.Now().UTC())`. Returns the URI for audit emission (M2b.7). The reader is consumed exactly once. | `ErrInvalidAgentID`            |
| `Get`  | Open a snapshot by URI. The returned `io.ReadCloser` streams the inner SQLite bytes — gzip and tar wrappers are transparent. Caller MUST `Close()`.                          | `ErrNotFound`, `ErrInvalidURI` |
| `List` | Enumerate snapshot URIs for `agentID`, newest first.                                                                                                                         | `ErrInvalidAgentID`            |

Sentinel errors live in `archivestore.go`:

- `ErrNotFound` — `Get` URI is well-formed and inside the backend root,
  but no object exists at that location.
- `ErrInvalidAgentID` — `Put` / `List` agentID is not a canonical
  RFC 4122 UUID. No filesystem touch occurs before the validation.
- `ErrInvalidURI` — `Get` URI scheme is wrong (e.g. `s3://` against
  `LocalFS`), the URI is malformed, or the resolved path escapes the
  backend root (path-traversal defence).

## `LocalFS` layout

`LocalFS` lays each snapshot out as
`<root>/notebook/<agent_id>/<timestamp>.tar.gz`, where `<timestamp>` is
`time.Now().UTC().Format("2006-01-02T15-04-05Z")`. The format omits
colons (illegal in Windows filenames), is fixed-length, and sorts
lexicographically — `List` reverses the lex order to return newest
first.

Each tarball contains exactly one entry named `<agent_id>.sqlite`
(mode `0o600`) carrying the bytes from `notebook.DB.Archive`. On `Get`
the wrapper transparently decompresses the gzip, opens the inner tar
entry, and returns the body as an `io.ReadCloser` so callers can feed
the result straight into `notebook.DB.Import`.

Directory tree on disk:

```text
<root>/
  notebook/
    <agent_id_a>/
      2026-05-03T12-00-00Z.tar.gz
      2026-05-03T13-30-00Z.tar.gz
    <agent_id_b>/
      ...
```

Every directory level lands at mode `0o700` — snapshots can carry the
agent's private memory bytes verbatim, so only the owner reads. Mirrors
the directory-mode discipline `core/pkg/notebook/path.go` applies to the
notebook substrate.

## URI scheme

`LocalFS` produces and accepts `file://<absolute-path>` URIs. The path
is always inside the configured root; `Get` rejects path-traversal and
non-`file://` schemes with `ErrInvalidURI` before any filesystem read.
Future backends introduce their own schemes:

- `s3://bucket/key` — `S3Compatible`, M2b.3.b.

A future router (M2b.3.b or later) can dispatch on the scheme prefix to
the right backend, but until then a single deployment chooses one
backend at startup and rejects every other scheme.

## Construction

```go
store, err := archivestore.NewLocalFS(
    archivestore.WithRoot("/var/lib/watchkeepers/archives"),
)
```

`WithRoot` is mandatory; omitting it returns a non-nil error rather
than silently defaulting to the working directory. `WithClock` accepts
a clock callback for testing — the default is `time.Now`.

## Path-traversal defence

`Get` parses the `file://` URI, resolves it via `filepath.Abs`, and
verifies the cleaned absolute path is either equal to the root or is a
descendant of `root + filepath.Separator`. Sibling paths whose prefix
accidentally matches without a separator (`/foo/archives` vs
`/foo/archives_evil`) and `..`-laden paths whose resolved form escapes
the root are both rejected.

## Contract test suite

`contract_test.go` defines `runContractTests(t, factory)`, parameterised
over `func(t *testing.T) ArchiveStore`. The LocalFS backend plugs in via
`TestLocalFS_Contract` in `localfs_test.go`. The future S3Compatible
backend (M2b.3.b) calls the same `runContractTests` with a
minio-backed factory so every backend is held to the same six contract
cases:

1. `PutGetRoundTrip`
2. `ListNewestFirst`
3. `PutRejectsBadAgentID`
4. `GetRejectsTraversal`
5. `GetRejectsUnknownURI`
6. `GetRejectsNonFileScheme`

Backend-specific edge cases (mode bits, timestamp regex, traversal
escapes that depend on POSIX semantics) live alongside the
`TestBackend_Contract` invocation in the per-backend `_test.go` file.

## Out of scope (deferred)

- `S3Compatible` backend + minio testcontainer — M2b.3.b.
- Secrets interface for cloud-backend credentials — M9.
- Audit-log emission of returned URIs — M2b.7.
- CLI-driven import flow — M2b.6.
