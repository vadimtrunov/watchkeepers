// Package archivestore wraps a [notebook.DB] snapshot lifecycle
// ([notebook.DB.Archive] / [notebook.DB.Import]) with a backend-agnostic
// blob-store interface and ships its first implementation, [LocalFS], that
// persists each snapshot as a gzipped tarball under
// `<root>/notebook/<agent_id>/<RFC3339>.tar.gz`.
//
// # Roadmap
//
// M2b.3.a — this package — covers only the interface surface and the
// LocalFS backend. The S3-compatible backend (`S3Compatible`), the minio
// testcontainer wiring, and the secrets shape that backend will consume
// land in M2b.3.b. Audit-log emission for `Put` URIs is deferred to
// M2b.7; CLI-driven import lives at M2b.6.
//
// # URI scheme
//
// Each backend defines its own URI scheme so callers can persist a single
// "archive pointer" string and route to the correct backend later. The
// LocalFS backend uses `file://<absolute-path>` URIs whose path component
// is always inside the configured root (the `Get` implementation rejects
// any path-traversal attempt with [ErrInvalidURI]). Future backends will
// add `s3://bucket/key` (M2b.3.b) and possibly `gs://...` shapes.
//
// # Tarball layout
//
// LocalFS wraps the SQLite snapshot bytes from [notebook.DB.Archive] in a
// gzip-compressed tar archive containing exactly one entry named
// `<agent_id>.sqlite` (mode `0o600`). On `Get` the wrapper transparently
// decompresses the gzip, opens the inner tar entry, and returns the body
// as an `io.ReadCloser` so callers can feed it straight into
// [notebook.DB.Import] without seeing the wrapper.
package archivestore

import (
	"context"
	"errors"
	"io"
	"regexp"
)

// ArchiveStore is the backend-agnostic blob-store interface used to persist
// per-agent SQLite snapshots produced by [notebook.DB.Archive] and to
// retrieve them for [notebook.DB.Import]. Implementations are expected to
// be safe for concurrent use across goroutines.
type ArchiveStore interface {
	// Put writes the snapshot under a backend-defined location keyed by
	// (agentID, time.Now().UTC()). Returns the resulting URI for audit
	// emission (M2b.7). The reader is consumed exactly once; callers
	// should not assume the implementation seeks or re-reads it.
	//
	// Returns [ErrInvalidAgentID] when agentID is not a canonical UUID,
	// in which case no filesystem touch occurs.
	Put(ctx context.Context, agentID string, snapshot io.Reader) (uri string, err error)

	// Get retrieves a snapshot by URI. The caller MUST Close() the
	// returned reader to release the underlying file handle and the
	// transparent gzip/tar wrappers. Returns [ErrNotFound] when the
	// backend lookup misses, or [ErrInvalidURI] when the URI's scheme,
	// shape, or path traversal disqualifies it.
	Get(ctx context.Context, uri string) (io.ReadCloser, error)

	// List returns snapshot URIs for a given agent, newest first. The
	// implementation chooses the ordering predicate (LocalFS sorts by
	// filename, which is a fixed-length RFC3339 timestamp and therefore
	// sorts lexicographically). Returns [ErrInvalidAgentID] for a
	// malformed agentID. An agent with no snapshots returns
	// (nil, nil).
	List(ctx context.Context, agentID string) ([]string, error)
}

// ErrNotFound is returned by [ArchiveStore.Get] when the URI is well-formed
// and points inside the configured backend root, but no object exists at
// that location. Callers can `errors.Is` against it to distinguish
// "missing" from "permission denied" / "I/O error".
var ErrNotFound = errors.New("archivestore: not found")

// ErrInvalidAgentID is returned by [ArchiveStore.Put] and
// [ArchiveStore.List] when the supplied agentID does not match the
// canonical RFC 4122 UUID shape (8-4-4-4-12 hex with dashes). No
// filesystem / network touch occurs before the validation runs.
var ErrInvalidAgentID = errors.New("archivestore: invalid agent id")

// ErrInvalidURI is returned by [ArchiveStore.Get] when the URI's scheme is
// not the one this backend handles (e.g. `s3://...` against [LocalFS]),
// the URI is otherwise malformed, or the resolved path escapes the
// configured backend root (path-traversal defence).
var ErrInvalidURI = errors.New("archivestore: invalid uri")

// uuidPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex with
// dashes). Mirrors the validator used by `core/pkg/notebook/path.go` and
// the Keep server's `core/internal/keep/server/handlers_write.go` so an
// agent that opens a notebook can also push it through the ArchiveStore
// without a second-shape divergence. Duplicated rather than imported to
// avoid pulling notebook (and therefore CGo SQLite) into every consumer
// of this package.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validateAgentID returns nil when agentID is a canonical UUID and
// [ErrInvalidAgentID] otherwise. Kept package-private; backends call it
// from their Put/List entry points before any I/O.
func validateAgentID(agentID string) error {
	if !uuidPattern.MatchString(agentID) {
		return ErrInvalidAgentID
	}
	return nil
}
