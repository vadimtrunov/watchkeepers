# archivestore — backend-agnostic snapshot store for `notebook.DB`

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/archivestore`

This package wraps the snapshot lifecycle of
[`core/pkg/notebook`](../notebook) — `Archive` produces SQLite bytes;
`Import` consumes them — with a backend-agnostic blob-store interface so
operators can swap the on-disk store for a remote object store without
touching agent code. M2b.3.a shipped the interface surface and the
`LocalFS` backend; M2b.3.b adds `S3Compatible` (any
S3-compatible endpoint via [minio-go](https://github.com/minio/minio-go))
plus a singleton-per-process MinIO testcontainer for the integration
suite.

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
The S3-compatible backend uses `s3://<bucket>/<key>`:

- `s3://bucket/key` — `S3Compatible`, M2b.3.b.

A scheme-routing dispatcher can be layered on top, but until that lands
each deployment chooses one backend at startup and rejects every other
scheme via `ErrInvalidURI`.

## `S3Compatible` layout

`S3Compatible` writes each snapshot at object key
`notebook/<agent_id>/<timestamp>.tar.gz` inside the configured bucket.
The on-the-wire format (single-entry gzipped tar with one
`<agent_id>.sqlite` member, mode `0o600`) is **byte-identical** to the
LocalFS layout, produced by the shared `internal_tar.go` helper. The
returned URI is `s3://<bucket>/notebook/<agent_id>/<timestamp>.tar.gz`.

```go
store, err := archivestore.NewS3Compatible(
    archivestore.S3Config{
        Endpoint:  "s3.us-east-1.amazonaws.com",
        AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
        SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
        Bucket:    "watchkeepers-snapshots",
        Region:    "us-east-1",
        Secure:    true,
    },
    // Optional: auto-create the bucket if it does not exist.
    archivestore.WithCreateBucket(false),
)
```

`Endpoint`, `AccessKey`, `SecretKey`, and `Bucket` are mandatory; the
constructor returns a non-nil error when any is empty. `Region` is
optional (AWS S3 wants the right value for SigV4; MinIO and R2 accept
`""`). `Secure` toggles HTTPS; flip it off only for local MinIO test
endpoints.

`WithCreateBucket(true)` lets the constructor `MakeBucket` when the
configured bucket does not exist. Default is `false`: a missing bucket
returns `ErrInvalidURI` so production deployments fail fast instead of
silently provisioning storage.

### Endpoint format

| Provider      | Endpoint shape                                    | Notes                             |
| ------------- | ------------------------------------------------- | --------------------------------- |
| AWS S3        | `s3.<region>.amazonaws.com`                       | `Region` must match.              |
| Cloudflare R2 | `<account_id>.r2.cloudflarestorage.com`           | `Region` can be `""` or `"auto"`. |
| Wasabi        | `s3.<region>.wasabisys.com`                       | Region required.                  |
| MinIO         | `host:port` (commonly `localhost:9000` for tests) | `Secure: false` for HTTP.         |
| SeaweedFS     | `<host>:<port>` of the S3 gateway                 | Vendor-specific quirks may apply. |
| Garage        | `<host>:<port>` of the gateway                    | Vendor-specific quirks may apply. |

Tested against MinIO `RELEASE.2024-08-29T01-40-52Z` via a
testcontainers-go singleton; the contract suite, the auto-create test,
and the `Notebook → Archive → Put → Get → Import` round-trip all
exercise the same wire format.

### Path-traversal & cross-bucket defence

`Get` rejects every URI whose bucket portion does not equal the
configured one (`s3://other-bucket/...`) with `ErrInvalidURI` before
any network call. Keys must sit under the `notebook/` prefix and must
not contain `..` segments — both checks happen pre-network so a
malicious URI cannot probe arbitrary objects.

### Secrets

The `S3Config` shape exposes credentials as plain struct fields
intentionally — the cross-backend secrets interface (`Secrets.Get`,
provider-aware lookup) lands in M9. Callers wire credentials from
environment variables, `~/.aws/credentials`, or whatever the deployment
uses today; the M9 follow-up will swap in a getter callback without
breaking the rest of the API.

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
over `func(t *testing.T) ArchiveStore`. Every backend plugs in via a
`TestBackend_Contract` wrapper:

- `TestLocalFS_Contract` — `localfs_test.go` (always runs; no Docker).
- `TestS3Compatible_Contract` — `s3_test.go` (skips when Docker is
  unavailable; otherwise runs against a singleton MinIO testcontainer).

Both backends pass the same six contract cases:

1. `PutGetRoundTrip`
2. `ListNewestFirst`
3. `PutRejectsBadAgentID`
4. `GetRejectsTraversal`
5. `GetRejectsUnknownURI`
6. `GetRejectsNonFileScheme`

Backend-specific edge cases (mode bits, timestamp regex, traversal
escapes that depend on POSIX semantics, MinIO bucket auto-create) live
alongside the `TestBackend_Contract` invocation in the per-backend
`_test.go` file. `S3Compatible` adds `BucketAutoCreate`,
`RejectsMissingBucket`, `GetWrongBucket`, and a `Notebook →
Archive → Put → Get → Import` round-trip with embedding-bytes
byte-equality.

### Running tests offline

The S3 lane uses [`testcontainers-go`](https://golang.testcontainers.org)
to spin up MinIO. When the local Docker daemon is unreachable, every
S3 test calls `t.Skip` rather than failing — `go test
./core/pkg/archivestore/...` stays green on offline laptops. CI lanes
that need the S3 coverage run on Docker-enabled runners; see
`.github/workflows/ci.yml`.

## Out of scope (deferred)

- Cross-backend secrets interface — M9 (replaces `S3Config`'s plain
  struct fields with a getter callback).
- Audit-log emission of returned URIs — M2b.7.
- CLI-driven import flow — M2b.6.
- Periodic / scheduled backup cadence — M2b.5.
