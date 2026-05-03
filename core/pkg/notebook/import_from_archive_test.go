package notebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/vadimtrunov/watchkeepers/core/pkg/archivestore"
)

// fakeFetcher is the in-memory [Fetcher] used by the ImportFromArchive
// tests. It returns a fixed byte slice as the snapshot body and exposes
// injectable error and gate channel for the context-cancellation test.
// `getCalled` lets assertions confirm Get was (or was not) invoked,
// mirroring the discipline in [fakeStore].
type fakeFetcher struct {
	bytesIn   []byte
	getErr    error
	getBlock  chan struct{} // optional: gate Get returning, for ctx-cancel tests
	getCalled atomic.Int32
	getURI    string
}

func (f *fakeFetcher) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	f.getCalled.Add(1)
	f.getURI = uri
	if f.getBlock != nil {
		select {
		case <-f.getBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return io.NopCloser(bytes.NewReader(f.bytesIn)), nil
}

// importTestAgentSrc is the UUID of the source notebook that produces
// the snapshot in the round-trip happy-path test. Distinct from
// importTestAgentDst so the destination's notebook file is created in a
// separate $WATCHKEEPER_DATA tree.
const (
	importTestAgentSrc = "11111111-1111-1111-1111-111111111111"
	importTestAgentDst = "22222222-2222-2222-2222-222222222222"
	importTestURI      = "fake://test/import-from-archive.tar.gz"
)

// importSeeds is the canonical 3-row fixture for the round-trip happy-path
// test. Three distinct categories with three non-parallel embeddings so the
// importer can confirm every row survived the Archive→Get→Import dance and
// the embedding bytes are byte-equal post-roundtrip.
var importSeeds = []retireSeed{
	{
		id:        "aaaa1111-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		category:  CategoryLesson,
		content:   "lesson alpha",
		embedding: makeEmbedding(31),
	},
	{
		id:        "bbbb2222-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		category:  CategoryPreference,
		content:   "preference beta",
		embedding: makeEmbedding(32),
	},
	{
		id:        "cccc3333-cccc-cccc-cccc-cccccccccccc",
		category:  CategoryObservation,
		content:   "observation gamma",
		embedding: makeEmbedding(33),
	},
}

// seedAndArchive opens a notebook for `agentID` against the current
// $WATCHKEEPER_DATA, seeds [importSeeds], runs Archive into a buffer,
// closes the source DB, and returns the snapshot bytes. Extracted so the
// happy-path test body stays under the gocyclo threshold.
func seedAndArchive(ctx context.Context, t *testing.T, agentID string) []byte {
	t.Helper()
	src, err := Open(ctx, agentID)
	if err != nil {
		t.Fatalf("Open src: %v", err)
	}
	for _, s := range importSeeds {
		if _, err := src.Remember(ctx, Entry{
			ID:        s.id,
			Category:  s.category,
			Content:   s.content,
			Embedding: s.embedding,
		}); err != nil {
			_ = src.Close()
			t.Fatalf("seed Remember %s: %v", s.id, err)
		}
	}
	var buf bytes.Buffer
	if err := src.Archive(ctx, &buf); err != nil {
		_ = src.Close()
		t.Fatalf("Archive src: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("Close src: %v", err)
	}
	return buf.Bytes()
}

// assertImportPayload verifies the fakeLogger captured exactly one
// `notebook_imported` event whose payload carries the documented
// (`agent_id`, `archive_uri`, `imported_at`) fields with the expected
// values, and whose `imported_at` parses as RFC3339Nano within the
// test's wall-clock window. Extracted from the happy-path test body to
// keep it under the gocyclo threshold.
func assertImportPayload(t *testing.T, logger *fakeLogger, agentID, uri string, before, after time.Time) {
	t.Helper()
	if got := logger.called.Load(); got != 1 {
		t.Fatalf("logger.LogAppend called %d times, want 1", got)
	}
	if logger.received.EventType != importEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, importEventType)
	}
	var raw map[string]any
	if err := json.Unmarshal(logger.received.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "archive_uri", "imported_at"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("payload missing %q field: %v", key, raw)
		}
	}
	if got := raw["agent_id"]; got != agentID {
		t.Fatalf("payload.agent_id = %v, want %q", got, agentID)
	}
	if got := raw["archive_uri"]; got != uri {
		t.Fatalf("payload.archive_uri = %v, want %q", got, uri)
	}
	importedAt, ok := raw["imported_at"].(string)
	if !ok {
		t.Fatalf("payload.imported_at = %v, want string", raw["imported_at"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, importedAt)
	if err != nil {
		t.Fatalf("payload.imported_at = %q does not parse as RFC3339Nano: %v", importedAt, err)
	}
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Fatalf("payload.imported_at = %v, want between %v and %v", parsed, before, after)
	}
}

// assertImportedSeed verifies one [retireSeed] survived the
// Get→Import dance against `dst`: the row recalls back with the
// expected id/content AND the entry_vec embedding bytes are byte-equal
// to a fresh SerializeFloat32 of the seed (M2b.2.b LESSONS guard).
func assertImportedSeed(ctx context.Context, t *testing.T, dst *DB, s retireSeed) {
	t.Helper()
	results, err := dst.Recall(ctx, RecallQuery{
		Embedding: s.embedding,
		TopK:      3,
		Category:  s.category,
	})
	if err != nil {
		t.Fatalf("Recall(%s): %v", s.category, err)
	}
	if len(results) != 1 || results[0].ID != s.id || results[0].Content != s.content {
		t.Fatalf("Recall(%s) = %+v, want one row id=%s content=%q", s.category, results, s.id, s.content)
	}
	want, err := sqlitevec.SerializeFloat32(s.embedding)
	if err != nil {
		t.Fatalf("SerializeFloat32: %v", err)
	}
	var got []byte
	if err := dst.sql.QueryRowContext(ctx,
		"SELECT embedding FROM entry_vec WHERE id = ?", s.id,
	).Scan(&got); err != nil {
		t.Fatalf("SELECT entry_vec.embedding: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("entry %s embedding bytes differ post-roundtrip", s.id)
	}
}

// TestImportFromArchive_HappyPath — AC4: produce a real snapshot via
// Archive from a 3-entry source notebook; load the bytes into a
// fakeFetcher; call ImportFromArchive against a fresh agentID in a
// separate $WATCHKEEPER_DATA tree; verify the destination notebook has
// the same 3 entries (Recall + embedding-byte byte-equality per
// M2b.2.b LESSONS); verify fakeLogger received one notebook_imported
// event with the expected payload shape.
func TestImportFromArchive_HappyPath(t *testing.T) {
	ctx := context.Background()

	// Source agent's data tree.
	t.Setenv(envDataDir, t.TempDir())
	snapshot := seedAndArchive(ctx, t, importTestAgentSrc)

	// Destination agent's data tree (fresh) — switching $WATCHKEEPER_DATA
	// makes Open(ctx, dst) resolve to a brand-new file, mirroring the
	// "successor agent inherits predecessor archive" call site.
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: snapshot}
	logger := &fakeLogger{}

	before := time.Now().UTC()
	if err := ImportFromArchive(ctx, importTestAgentDst, importTestURI, fetcher, logger); err != nil {
		t.Fatalf("ImportFromArchive: %v", err)
	}
	after := time.Now().UTC()

	// Re-open the destination notebook to verify the imported rows.
	dst, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst post-import: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })

	for _, s := range importSeeds {
		assertImportedSeed(ctx, t, dst, s)
	}

	assertImportPayload(t, logger, importTestAgentDst, importTestURI, before, after)

	if got := fetcher.getCalled.Load(); got != 1 {
		t.Fatalf("fetcher.Get called %d times, want 1", got)
	}
	if fetcher.getURI != importTestURI {
		t.Fatalf("fetcher.Get URI = %q, want %q", fetcher.getURI, importTestURI)
	}
}

// TestImportFromArchive_BadAgentID — non-canonical UUID returns
// ErrInvalidEntry synchronously without any fetcher / logger touch.
func TestImportFromArchive_BadAgentID(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{}
	logger := &fakeLogger{}

	err := ImportFromArchive(context.Background(), "not-a-uuid", importTestURI, fetcher, logger)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if got := fetcher.getCalled.Load(); got != 0 {
		t.Fatalf("fetcher.Get called %d times, want 0 for bad agent id", got)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 for bad agent id", got)
	}
}

// TestImportFromArchive_EmptyURI — empty archiveURI returns
// ErrInvalidEntry synchronously without any fetcher / logger touch.
func TestImportFromArchive_EmptyURI(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{}
	logger := &fakeLogger{}

	err := ImportFromArchive(context.Background(), importTestAgentDst, "", fetcher, logger)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if got := fetcher.getCalled.Load(); got != 0 {
		t.Fatalf("fetcher.Get called %d times, want 0 for empty URI", got)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 for empty URI", got)
	}
}

// TestImportFromArchive_FetcherGetFails — fetcher.Get returns an
// injected error; helper surfaces it wrapped, never opens the
// destination DB, and never invokes the logger.
func TestImportFromArchive_FetcherGetFails(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())

	fetchBoom := errors.New("fetch boom")
	fetcher := &fakeFetcher{getErr: fetchBoom}
	logger := &fakeLogger{}

	err := ImportFromArchive(context.Background(), importTestAgentDst, importTestURI, fetcher, logger)
	if err == nil {
		t.Fatal("ImportFromArchive returned nil error on fetcher.Get failure")
	}
	if !errors.Is(err, fetchBoom) {
		t.Fatalf("err = %v, want one wrapping fetch boom", err)
	}
	if !strings.Contains(err.Error(), "fetch:") {
		t.Fatalf("err = %v, want prefix 'fetch:'", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on fetch failure", got)
	}
}

// TestImportFromArchive_CorruptArchive — fetcher returns a non-SQLite
// byte stream; helper surfaces ErrCorruptArchive (wrapped) and never
// invokes the logger.
func TestImportFromArchive_CorruptArchive(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: []byte("definitely not a sqlite file just plain text")}
	logger := &fakeLogger{}

	err := ImportFromArchive(context.Background(), importTestAgentDst, importTestURI, fetcher, logger)
	if err == nil {
		t.Fatal("ImportFromArchive returned nil error on corrupt archive")
	}
	if !errors.Is(err, ErrCorruptArchive) {
		t.Fatalf("err = %v, want one wrapping ErrCorruptArchive", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on corrupt archive", got)
	}
}

// TestImportFromArchive_TargetNotEmpty — pre-populate the destination
// notebook with one entry; ImportFromArchive must surface
// ErrTargetNotEmpty (wrapped) and the destination's existing entry must
// still be Recall-able afterwards.
func TestImportFromArchive_TargetNotEmpty(t *testing.T) {
	ctx := context.Background()

	// Stage 1: source data tree — produce a valid snapshot.
	t.Setenv(envDataDir, t.TempDir())
	snapshot := seedAndArchive(ctx, t, importTestAgentSrc)

	// Stage 2: destination data tree — pre-populate so Import refuses.
	t.Setenv(envDataDir, t.TempDir())
	const liveID = "dddd4444-dddd-dddd-dddd-dddddddddddd"
	dst, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst: %v", err)
	}
	if _, err := dst.Remember(ctx, Entry{
		ID:        liveID,
		Category:  CategoryLesson,
		Content:   "live row",
		Embedding: makeEmbedding(41),
	}); err != nil {
		_ = dst.Close()
		t.Fatalf("seed live: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst pre-import: %v", err)
	}

	fetcher := &fakeFetcher{bytesIn: snapshot}
	logger := &fakeLogger{}

	err = ImportFromArchive(ctx, importTestAgentDst, importTestURI, fetcher, logger)
	if err == nil {
		t.Fatal("ImportFromArchive returned nil error on non-empty target")
	}
	if !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("err = %v, want one wrapping ErrTargetNotEmpty", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on target-not-empty", got)
	}

	// Live row must still be Recall-able post-rejection.
	dst2, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst post-rejection: %v", err)
	}
	t.Cleanup(func() { _ = dst2.Close() })
	results, recallErr := dst2.Recall(ctx, RecallQuery{
		Embedding: makeEmbedding(41),
		TopK:      5,
	})
	if recallErr != nil {
		t.Fatalf("post-rejection Recall: %v", recallErr)
	}
	if len(results) != 1 || results[0].ID != liveID {
		t.Fatalf("post-rejection Recall = %+v, want exactly the live row", results)
	}
}

// TestImportFromArchive_LogAppendFails — happy fetch + import, logger
// returns an error. Helper must return the wrapped error AND the
// destination notebook must contain the imported rows (the
// import-then-audit ordering means data IS imported when audit fails).
func TestImportFromArchive_LogAppendFails(t *testing.T) {
	ctx := context.Background()

	// Source data tree.
	t.Setenv(envDataDir, t.TempDir())
	snapshot := seedAndArchive(ctx, t, importTestAgentSrc)

	// Destination data tree.
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: snapshot}
	auditBoom := errors.New("audit boom")
	logger := &fakeLogger{logErr: auditBoom}

	err := ImportFromArchive(ctx, importTestAgentDst, importTestURI, fetcher, logger)
	if err == nil {
		t.Fatal("ImportFromArchive returned nil error on LogAppend failure")
	}
	if !errors.Is(err, auditBoom) {
		t.Fatalf("err = %v, want one wrapping audit boom", err)
	}
	if !strings.Contains(err.Error(), "audit emit:") {
		t.Fatalf("err = %v, want prefix 'audit emit:'", err)
	}

	// Critical contract: the import succeeded before LogAppend ran, so
	// the destination must hold the imported rows.
	dst, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst post-LogAppend-failure: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	for _, s := range importSeeds {
		assertImportedSeed(ctx, t, dst, s)
	}
}

// TestImportFromArchive_NilLogger — logger=nil works without panic and
// the destination notebook holds the imported rows.
func TestImportFromArchive_NilLogger(t *testing.T) {
	ctx := context.Background()

	t.Setenv(envDataDir, t.TempDir())
	snapshot := seedAndArchive(ctx, t, importTestAgentSrc)

	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: snapshot}

	if err := ImportFromArchive(ctx, importTestAgentDst, importTestURI, fetcher, nil); err != nil {
		t.Fatalf("ImportFromArchive(nil logger): %v", err)
	}

	dst, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst post-import: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	for _, s := range importSeeds {
		assertImportedSeed(ctx, t, dst, s)
	}
}

// TestImportFromArchive_ContextCancellation — fakeFetcher blocks inside
// Get on a never-ready channel, the test cancels ctx mid-flight; the
// helper must return an error wrapping context.Canceled and never
// invoke the logger.
func TestImportFromArchive_ContextCancellation(t *testing.T) {
	t.Setenv(envDataDir, t.TempDir())

	parentCtx := context.Background()
	ctx, cancel := context.WithCancel(parentCtx)

	fetcher := &fakeFetcher{getBlock: make(chan struct{})}
	logger := &fakeLogger{}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		err := ImportFromArchive(ctx, importTestAgentDst, importTestURI, fetcher, logger)
		done <- result{err: err}
	}()

	// Give the goroutine time to enter Get / start blocking.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("ImportFromArchive returned nil error on ctx cancel")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ImportFromArchive did not return within 2s of ctx cancel")
	}

	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on ctx cancel", got)
	}
}

// TestImportFromArchive_FetcherInterfaceSatisfiedByLocalFS — compile-
// time check that the production `*archivestore.LocalFS` satisfies the
// `Fetcher` interface defined in this package. A future refactor of
// either type would otherwise silently make ImportFromArchive
// uncallable from real callers wiring archivestore values straight in.
//
// This test-side import of archivestore does NOT create a cycle:
// archivestore production code does not import notebook (only
// archivestore _test.go files do, and those compile separately).
func TestImportFromArchive_FetcherInterfaceSatisfiedByLocalFS(_ *testing.T) {
	var _ Fetcher = (*archivestore.LocalFS)(nil)
	var _ Fetcher = (*archivestore.S3Compatible)(nil)
}
