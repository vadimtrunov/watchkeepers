package archivestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// runContractTests is the parameterised contract suite every
// [ArchiveStore] backend must pass. Backends supply a `factory` callback
// that returns a fresh, isolated store per sub-test (e.g. one whose root
// lives under `t.TempDir()` for [LocalFS]; or a minio-backed instance for
// the future S3Compatible backend in M2b.3.b).
//
// The suite covers the round-trip happy path, list ordering, agentID
// validation, path-traversal defence, scheme validation, and the
// unknown-URI error mapping. Backend-specific edge cases live in the
// per-backend `_test.go` file alongside this one.
func runContractTests(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	t.Run("PutGetRoundTrip", func(t *testing.T) { testPutGetRoundTrip(t, factory) })
	t.Run("ListNewestFirst", func(t *testing.T) { testListNewestFirst(t, factory) })
	t.Run("PutRejectsBadAgentID", func(t *testing.T) { testPutRejectsBadAgentID(t, factory) })
	t.Run("GetRejectsTraversal", func(t *testing.T) { testGetRejectsTraversal(t, factory) })
	t.Run("GetRejectsUnknownURI", func(t *testing.T) { testGetRejectsUnknownURI(t, factory) })
	t.Run("GetRejectsNonFileScheme", func(t *testing.T) { testGetRejectsNonFileScheme(t, factory) })
}

// validAgentID is the canonical fixture id used across the contract
// tests. Hard-coded rather than uuid.New() so failures are reproducible
// even when the suite is run with `-shuffle=on`.
const validAgentID = "11111111-2222-3333-4444-555555555555"

// testPutGetRoundTrip verifies the simplest happy path: bytes written
// via Put come back byte-for-byte from Get, with the inner gzip/tar
// transparent to the caller.
func testPutGetRoundTrip(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	want := []byte("snapshot-bytes-" + strings.Repeat("x", 64))
	uri, err := store.Put(ctx, validAgentID, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if uri == "" {
		t.Fatal("Put returned empty URI")
	}

	rc, err := store.Get(ctx, uri)
	if err != nil {
		t.Fatalf("Get(%q): %v", uri, err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-tripped bytes differ: got %d bytes, want %d bytes", len(got), len(want))
	}
}

// testListNewestFirst writes three snapshots and verifies they come
// back from List in newest-first order. Backends use a fixed-length
// timestamp filename (LocalFS) or a similar lex-sortable key so a
// reverse-lex sort matches reverse-chronological order.
func testListNewestFirst(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	// Stamp three Puts; the factory MAY plug in a deterministic clock so
	// the file names are distinct even on fast machines (LocalFS test
	// helper does this). The contract suite itself only requires the
	// returned URIs differ and come back in newest-first order.
	uris := make([]string, 3)
	for i := 0; i < 3; i++ {
		uri, err := store.Put(ctx, validAgentID, bytes.NewReader([]byte{byte(i)}))
		if err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
		uris[i] = uri
	}

	listed, err := store.List(ctx, validAgentID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("List returned %d URIs, want 3 (uris=%v)", len(listed), listed)
	}
	// Newest first: the last uri Put'd should be at index 0.
	if listed[0] != uris[2] {
		t.Fatalf("List[0] = %q, want newest = %q", listed[0], uris[2])
	}
	if listed[2] != uris[0] {
		t.Fatalf("List[2] = %q, want oldest = %q", listed[2], uris[0])
	}
}

// testPutRejectsBadAgentID verifies the canonical-UUID guard. No
// filesystem touch should occur — the per-agent dir must not be
// created when validation fails.
func testPutRejectsBadAgentID(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	_, err := store.Put(ctx, "not-a-uuid", bytes.NewReader([]byte("payload")))
	if !errors.Is(err, ErrInvalidAgentID) {
		t.Fatalf("Put(bad agentID) error = %v, want ErrInvalidAgentID", err)
	}

	// List should also refuse a bad agentID with the same sentinel.
	_, lerr := store.List(ctx, "not-a-uuid")
	if !errors.Is(lerr, ErrInvalidAgentID) {
		t.Fatalf("List(bad agentID) error = %v, want ErrInvalidAgentID", lerr)
	}
}

// testGetRejectsTraversal asserts the path-traversal defence. A URI
// pointing at `/etc/passwd` (or its `..`-laden equivalent under the
// configured root) must be rejected with [ErrInvalidURI] before any
// filesystem read.
func testGetRejectsTraversal(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	for _, uri := range []string{
		"file:///etc/passwd",
		"file:///",
	} {
		_, err := store.Get(ctx, uri)
		if !errors.Is(err, ErrInvalidURI) {
			t.Fatalf("Get(%q) error = %v, want ErrInvalidURI", uri, err)
		}
	}
}

// testGetRejectsUnknownURI verifies the missing-file mapping: a URI
// that's well-formed and inside the configured root but points at no
// real file must surface as [ErrNotFound], not a generic I/O error.
func testGetRejectsUnknownURI(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	// Put one snapshot so we have a known-good URI prefix to mutate.
	uri, err := store.Put(ctx, validAgentID, bytes.NewReader([]byte("seed")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Replace the timestamp portion with a name that does not exist on
	// disk. We strip the basename to guarantee the substituted name
	// stays under the configured root.
	idx := strings.LastIndex(uri, "/")
	if idx < 0 {
		t.Fatalf("Put returned URI without path separator: %q", uri)
	}
	missing := uri[:idx+1] + "9999-99-99T99-99-99Z.tar.gz"

	_, err = store.Get(ctx, missing)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(%q) error = %v, want ErrNotFound", missing, err)
	}
}

// testGetRejectsNonFileScheme verifies a non-`file://` URI hits
// [ErrInvalidURI] without a filesystem touch. Future backends will
// each accept their own scheme; until the router lands (M2b.3.b),
// the LocalFS backend simply rejects everything else.
func testGetRejectsNonFileScheme(t *testing.T, factory func(t *testing.T) ArchiveStore) {
	t.Helper()
	store := factory(t)
	ctx := context.Background()

	for _, uri := range []string{
		"s3://bucket/key",
		"https://example.com/snap.tar.gz",
		"",
	} {
		_, err := store.Get(ctx, uri)
		if !errors.Is(err, ErrInvalidURI) {
			t.Fatalf("Get(%q) error = %v, want ErrInvalidURI", uri, err)
		}
	}
}
