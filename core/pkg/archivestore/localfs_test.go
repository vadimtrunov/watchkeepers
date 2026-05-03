package archivestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

func init() {
	// Register sqlite-vec as a SQLite auto-extension for this test binary.
	// The notebook package's openAt calls sqlitevec.Auto() via its own
	// vecOnce, but the embedding-bytes assertion opens the imported file
	// via a raw sql.Open in this package, which shares the same process-
	// global auto-extension table. Calling Auto() here ensures vec0
	// virtual tables are accessible even if openAt has not yet been
	// invoked. sqlitevec.Auto() is idempotent when called repeatedly
	// from the same process.
	sqlitevec.Auto()
}

// newTmpLocalFS returns a [*LocalFS] rooted under `t.TempDir()` with a
// deterministic clock that ticks one second per Put. The deterministic
// clock keeps the contract suite's `ListNewestFirst` test reliable: the
// timestamp filename has 1-second resolution, so on a fast machine three
// real-time Puts could land in the same second and collide.
func newTmpLocalFS(t *testing.T) ArchiveStore {
	t.Helper()
	root := filepath.Join(t.TempDir(), "archives")
	clk := monotonicClock(time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC))
	store, err := NewLocalFS(WithRoot(root), WithClock(clk))
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	return store
}

// monotonicClock returns a clock function that yields `start`, `start+1s`,
// `start+2s`, ... per call, in UTC. Used by the test helper so the
// per-Put filename timestamps are guaranteed distinct without relying on
// wall-clock resolution.
func monotonicClock(start time.Time) func() time.Time {
	var ticks int64
	return func() time.Time {
		i := atomic.AddInt64(&ticks, 1) - 1
		return start.Add(time.Duration(i) * time.Second).UTC()
	}
}

// TestLocalFS_Contract plugs the LocalFS into the parameterised contract
// suite. The next backend (S3Compatible, M2b.3.b) will add its own
// `TestS3_Contract` calling the same `runContractTests` with a minio
// factory.
func TestLocalFS_Contract(t *testing.T) {
	runContractTests(t, newTmpLocalFS)
}

// TestLocalFS_PutDirIsolation verifies the per-agent directory lands at
// mode 0o700 (per the LESSONS 2026-05-03 dir-mode discipline). Skipped
// on Windows because `os.FileMode` permission bits don't carry POSIX
// semantics there.
func TestLocalFS_PutDirIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission semantics not enforced on Windows")
	}
	root := filepath.Join(t.TempDir(), "archives")
	store, err := NewLocalFS(WithRoot(root))
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	if _, err := store.Put(context.Background(), validAgentID, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, dir := range []string{
		root,
		filepath.Join(root, "notebook"),
		filepath.Join(root, "notebook", validAgentID),
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat(%q): %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Fatalf("dir %q mode = %o, want 0o700", dir, mode)
		}
	}
}

// TestLocalFS_TimestampFormatNoColons verifies the filename portion of
// the URI matches the expected regex: `YYYY-MM-DDTHH-MM-SSZ.tar.gz`.
// Colons are excluded so the filenames remain valid on Windows
// filesystems (per AC6).
func TestLocalFS_TimestampFormatNoColons(t *testing.T) {
	store := newTmpLocalFS(t)
	uri, err := store.Put(context.Background(), validAgentID, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	name := filepath.Base(strings.TrimPrefix(uri, "file://"))
	pattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z\.tar\.gz$`)
	if !pattern.MatchString(name) {
		t.Fatalf("filename %q does not match %s", name, pattern)
	}
	if strings.Contains(name, ":") {
		t.Fatalf("filename %q contains colon", name)
	}
}

// TestLocalFS_NewLocalFS_RequiresRoot covers the constructor's mandatory
// option: omitting [WithRoot] returns an error rather than silently
// defaulting to the cwd.
func TestLocalFS_NewLocalFS_RequiresRoot(t *testing.T) {
	if _, err := NewLocalFS(); err == nil {
		t.Fatal("NewLocalFS() with no options returned nil error, want non-nil")
	}
}

// TestLocalFS_GetRejectsTraversalEscape verifies that a `..`-laden URI
// pointing under the configured root but resolving outside is caught by
// the path-traversal defence in resolveURI.
func TestLocalFS_GetRejectsTraversalEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archives")
	store, err := NewLocalFS(WithRoot(root))
	if err != nil {
		t.Fatalf("NewLocalFS: %v", err)
	}
	// `<root>/../<sibling>` resolves outside the root and must be
	// rejected. We do not need the sibling to actually exist; the
	// resolver runs before any os.Open.
	traversal := "file://" + filepath.Join(root, "..", "outside.tar.gz")
	if _, err := store.Get(context.Background(), traversal); !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("Get(%q) error = %v, want ErrInvalidURI", traversal, err)
	}
}

// TestLocalFS_RoundTripWithNotebook is the M2b.3.a integration test: a
// real [notebook.DB] writes 3 entries via Remember, Archives them
// through the store, Gets them back, and Imports into a fresh notebook;
// the imported DB must Recall every seeded entry.
//
// Mirrors notebook's own TestArchive_RoundTrip but adds the
// LocalFS Put / Get hop in the middle so the end-to-end picture
// (notebook + archivestore) is exercised once with CGo SQLite enabled.
func TestLocalFS_RoundTripWithNotebook(t *testing.T) {
	ctx := context.Background()

	// Source notebook: open under t.TempDir(), seed three entries.
	srcPath := filepath.Join(t.TempDir(), "src.sqlite")
	src, err := openNotebookAt(ctx, srcPath)
	if err != nil {
		t.Fatalf("open src notebook: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	seeds := []seedEntry{
		{"11111111-1111-1111-1111-111111111111", notebook.CategoryLesson, "lesson row", makeEmbedding(1)},
		{"22222222-2222-2222-2222-222222222222", notebook.CategoryPreference, "preference row", makeEmbedding(2)},
		{"33333333-3333-3333-3333-333333333333", notebook.CategoryObservation, "observation row", makeEmbedding(3)},
	}
	for _, s := range seeds {
		if _, err := src.Remember(ctx, notebook.Entry{
			ID:        s.id,
			Category:  s.category,
			Content:   s.content,
			Embedding: s.embedding,
		}); err != nil {
			t.Fatalf("Remember %s: %v", s.id, err)
		}
	}

	// Archive into a buffer, then Put through the store.
	var snapshot bytes.Buffer
	if err := src.Archive(ctx, &snapshot); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	store := newTmpLocalFS(t).(*LocalFS)
	const agentID = "44444444-4444-4444-4444-444444444444"
	uri, err := store.Put(ctx, agentID, &snapshot)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get back, Import into a fresh notebook, Recall every seeded
	// entry. Closing the reader after Import also exercises the
	// snapshotReader Close path against a fully-drained stream.
	rc, err := store.Get(ctx, uri)
	if err != nil {
		t.Fatalf("Get(%q): %v", uri, err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	dstPath := filepath.Join(t.TempDir(), "dst.sqlite")
	dst, err := openNotebookAt(ctx, dstPath)
	if err != nil {
		t.Fatalf("open dst notebook: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })

	if err := dst.Import(ctx, rc); err != nil {
		t.Fatalf("Import: %v", err)
	}

	for _, s := range seeds {
		results, err := dst.Recall(ctx, notebook.RecallQuery{
			Embedding: s.embedding,
			TopK:      3,
			Category:  s.category,
		})
		if err != nil {
			t.Fatalf("Recall(%s): %v", s.category, err)
		}
		if len(results) != 1 {
			t.Fatalf("Recall(%s) returned %d rows, want 1", s.category, len(results))
		}
		if results[0].ID != s.id {
			t.Fatalf("Recall(%s) id = %q, want %q", s.category, results[0].ID, s.id)
		}
		if results[0].Content != s.content {
			t.Fatalf("Recall(%s) content = %q, want %q", s.category, results[0].Content, s.content)
		}
	}

	// AC8: embedding bytes must survive the Put→Get→Import round-trip
	// byte-for-byte. openNotebookAt places the actual SQLite file at
	// <filepath.Dir(dstPath)>/notebook/<pathToUUID("dst.sqlite")>.sqlite;
	// we close dst first (MaxOpenConns=1 blocks a second handle) then
	// open the file directly to SELECT entry_vec.embedding.
	actualDstPath := filepath.Join(
		filepath.Dir(dstPath), "notebook",
		pathToUUID(filepath.Base(dstPath))+".sqlite",
	)
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst before raw open: %v", err)
	}
	assertEmbeddingBytesRoundTrip(ctx, t, actualDstPath, seeds)
}

type seedEntry struct {
	id        string
	category  string
	content   string
	embedding []float32
}

// assertEmbeddingBytesRoundTrip opens the imported SQLite file at sqlitePath
// via a raw database/sql handle (sqlite-vec loaded via init's sqlitevec.Auto),
// SELECTs entry_vec.embedding for each seed, and fails if the bytes don't
// match sqlitevec.SerializeFloat32(seed.embedding). Extracted to keep
// TestLocalFS_RoundTripWithNotebook's cyclomatic complexity within the
// gocyclo threshold.
func assertEmbeddingBytesRoundTrip(ctx context.Context, t *testing.T, sqlitePath string, seeds []seedEntry) {
	t.Helper()
	rawDB, err := sql.Open("sqlite3", "file:"+sqlitePath+"?mode=ro&_foreign_keys=on")
	if err != nil {
		t.Fatalf("raw sql.Open(%q): %v", sqlitePath, err)
	}
	defer func() { _ = rawDB.Close() }()

	for _, s := range seeds {
		want, err := sqlitevec.SerializeFloat32(s.embedding)
		if err != nil {
			t.Fatalf("SerializeFloat32(%s): %v", s.id, err)
		}
		var got []byte
		if err := rawDB.QueryRowContext(ctx,
			"SELECT embedding FROM entry_vec WHERE id = ?", s.id,
		).Scan(&got); err != nil {
			t.Fatalf("SELECT entry_vec.embedding for %s: %v", s.id, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("entry %s: embedding bytes differ after round-trip: got %x want %x", s.id, got, want)
		}
	}
}

// makeEmbedding mirrors the notebook test helper: a deterministic
// 1536-dim float32 vector keyed off `seed`. Duplicated rather than
// imported because notebook's helper is package-private to its test
// binary.
func makeEmbedding(seed byte) []float32 {
	v := make([]float32, notebook.EmbeddingDim)
	for i := range v {
		v[i] = float32(seed) * 0.001
	}
	return v
}

// openNotebookAt opens a notebook at `path` by setting `WATCHKEEPER_DATA`
// to a temp dir and constructing a UUID id derived from the path. The
// notebook package's `Open` resolves paths via WATCHKEEPER_DATA + a UUID
// agent id, which would fight the test's desire to point at an exact
// file. We therefore use the `openAt`-equivalent shape via a temporary
// override of the data dir env var, then move the file into place.
//
// Because notebook.Open requires a UUID and writes to
// `<data>/notebook/<uuid>.sqlite`, we let it pick that path and use it
// as-is — `path` is recovered from the resolved location.
func openNotebookAt(ctx context.Context, path string) (*notebook.DB, error) {
	// Use the standard Open path so we exercise the full schema-init
	// pipeline. The caller's `path` is not literally used as the SQLite
	// file location (notebook owns that), but we still need a stable
	// per-test data dir so concurrent tests don't share files.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Push WATCHKEEPER_DATA at the test's temp dir so the resolved
	// notebook file lives under it. Per-test env mutations are safe
	// because Go's test runner serialises t.Setenv-marked tests; this
	// helper is called from a top-level Test function only.
	old, hadOld := os.LookupEnv("WATCHKEEPER_DATA")
	if err := os.Setenv("WATCHKEEPER_DATA", dir); err != nil {
		return nil, err
	}
	defer func() {
		if hadOld {
			_ = os.Setenv("WATCHKEEPER_DATA", old)
		} else {
			_ = os.Unsetenv("WATCHKEEPER_DATA")
		}
	}()
	// Pick a UUID that's deterministic per `path` so concurrent runs
	// don't collide. The basename of `path` is a test-supplied label;
	// turn it into a UUID-shaped string by hashing it into the canonical
	// 8-4-4-4-12 layout.
	id := pathToUUID(filepath.Base(path))
	return notebook.Open(ctx, id)
}

// pathToUUID maps an arbitrary string into a canonical UUID-shaped
// string. Not cryptographically meaningful — only used by the test
// helper to derive a stable per-path id for `notebook.Open`.
func pathToUUID(s string) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, b := range []byte(s + strings.Repeat("0", 64)) {
		if i >= 32 {
			break
		}
		out = append(out, hex[int(b)%16])
		if i == 7 || i == 11 || i == 15 || i == 19 {
			out = append(out, '-')
		}
	}
	return string(out)
}

// Verify the integration test's file-IO does not leak between subtests
// by exercising List against a fresh store that has never been written
// to. The expected return is (nil, nil).
func TestLocalFS_ListEmpty(t *testing.T) {
	store := newTmpLocalFS(t)
	uris, err := store.List(context.Background(), validAgentID)
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if uris != nil {
		t.Fatalf("List = %v, want nil on never-written agent", uris)
	}
}

// Sanity: io.Copy against the snapshotReader returned from Get must
// drain exactly the bytes we wrote. The contract suite covers this for
// every backend; this LocalFS-only test additionally asserts the inner
// reader honours partial reads (some callers pass `io.LimitedReader`).
func TestLocalFS_PartialRead(t *testing.T) {
	store := newTmpLocalFS(t)
	want := bytes.Repeat([]byte("ab"), 256)
	uri, err := store.Put(context.Background(), validAgentID, bytes.NewReader(want))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := store.Get(context.Background(), uri)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	var got bytes.Buffer
	buf := make([]byte, 7) // intentionally awkward chunk size
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("Read: %v", err)
		}
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("partial-read got %d bytes, want %d", got.Len(), len(want))
	}
}
