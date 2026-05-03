package notebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// fakeStore is an in-memory [archivestore.ArchiveStore] used by the
// ArchiveOnRetire tests. It records the bytes streamed into Put so the
// happy-path test can re-seed a fresh notebook via Import; it also exposes
// an injectable error and an optional gate channel for the
// context-cancellation test.
type fakeStore struct {
	putErr           error
	putErrBeforeRead bool          // when true, Put returns putErr immediately without reading src
	putBlock         chan struct{} // optional: gate Put returning, for ctx-cancel tests
	putBytes         bytes.Buffer
	putAgent         string
	putCalled        atomic.Int32
	getCalled        atomic.Int32
	listCalled       atomic.Int32
	nextURI          string
}

func (f *fakeStore) Put(ctx context.Context, agentID string, src io.Reader) (string, error) {
	f.putCalled.Add(1)
	f.putAgent = agentID
	// Simulate a real ArchiveStore that fails before reading any bytes
	// (e.g. auth failure, ECONNREFUSED). This exercises the goroutine-leak
	// fix in ArchiveOnRetire: without pr.CloseWithError, the Archive
	// goroutine would block on pw.Write indefinitely.
	if f.putErrBeforeRead && f.putErr != nil {
		return "", f.putErr
	}
	if _, err := io.Copy(&f.putBytes, src); err != nil {
		return "", err
	}
	if f.putBlock != nil {
		select {
		case <-f.putBlock:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.putErr != nil {
		return "", f.putErr
	}
	if f.nextURI != "" {
		return f.nextURI, nil
	}
	return "fake://test/" + agentID + "/" + time.Now().UTC().Format("2006-01-02T15-04-05Z") + ".tar.gz", nil
}

func (f *fakeStore) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	f.getCalled.Add(1)
	return io.NopCloser(bytes.NewReader(f.putBytes.Bytes())), nil
}

func (f *fakeStore) List(_ context.Context, _ string) ([]string, error) {
	f.listCalled.Add(1)
	if f.nextURI == "" {
		return nil, nil
	}
	return []string{f.nextURI}, nil
}

// fakeLogger is the [Logger] stand-in. It captures the request a
// successful LogAppend would have observed and exposes an injectable
// error so the LogAppend-failure path can be exercised.
type fakeLogger struct {
	logErr   error
	received keepclient.LogAppendRequest
	called   atomic.Int32
}

func (f *fakeLogger) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.called.Add(1)
	f.received = req
	if f.logErr != nil {
		return nil, f.logErr
	}
	return &keepclient.LogAppendResponse{ID: "fake-log-id"}, nil
}

// retireSeed is the canonical 2-row fixture for the ArchiveOnRetire happy-
// path round-trip. Two distinct categories with two non-parallel
// embeddings so the importer can confirm both rows survived the
// Archive→Put→Import dance.
type retireSeed struct {
	id        string
	category  string
	content   string
	embedding []float32
}

var retireSeeds = []retireSeed{
	{
		id:        "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		category:  CategoryLesson,
		content:   "lesson one",
		embedding: makeEmbedding(21),
	},
	{
		id:        "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		category:  CategoryPreference,
		content:   "preference one",
		embedding: makeEmbedding(22),
	},
}

const retireAgentID = "cccccccc-cccc-cccc-cccc-cccccccccccc"

// seedRetireDB seeds `db` with [retireSeeds] so each test starts from the
// same on-disk shape. Extracted so the per-test Arrange step is one line.
func seedRetireDB(ctx context.Context, t *testing.T, db *DB) {
	t.Helper()
	for _, s := range retireSeeds {
		if _, err := db.Remember(ctx, Entry{
			ID:        s.id,
			Category:  s.category,
			Content:   s.content,
			Embedding: s.embedding,
		}); err != nil {
			t.Fatalf("seed Remember %s: %v", s.id, err)
		}
	}
}

// assertRetireSeed verifies one [retireSeed] survived the
// Archive→Put→Import dance against `dst`: the row recalls back with the
// expected id/content AND the entry_vec embedding bytes are byte-equal
// to a fresh SerializeFloat32 of the seed (M2b.2.b LESSONS guard).
// Extracted from TestArchiveOnRetire_HappyPath to keep the test body
// under the gocyclo threshold.
func assertRetireSeed(ctx context.Context, t *testing.T, dst *DB, s retireSeed) {
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

// assertHappyPathLogger verifies that [TestArchiveOnRetire_HappyPath]'s
// fake logger saw exactly one `notebook_archived` event whose payload
// names the expected agent id and the URI returned by Put. The deeper
// payload-shape assertions (every documented field, RFC3339Nano parse)
// live in [TestArchiveOnRetire_PayloadShape].
func assertHappyPathLogger(t *testing.T, logger *fakeLogger, uri string) {
	t.Helper()
	if got := logger.called.Load(); got != 1 {
		t.Fatalf("logger.LogAppend called %d times, want 1", got)
	}
	if logger.received.EventType != retireEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, retireEventType)
	}
	var payload retirePayload
	if err := json.Unmarshal(logger.received.Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload.AgentID != retireAgentID {
		t.Fatalf("payload.AgentID = %q, want %q", payload.AgentID, retireAgentID)
	}
	if payload.URI != uri {
		t.Fatalf("payload.URI = %q, want %q", payload.URI, uri)
	}
}

// TestArchiveOnRetire_HappyPath — AC2/AC5: 2-entry seed, in-memory fake
// store + logger; assert the URI is non-empty, the bytes captured by the
// store round-trip into a fresh notebook with the same 2 entries (with
// embedding-byte byte-equality per M2b.2.b LESSONS), and the logger saw
// exactly one `notebook_archived` event whose payload carries the agent
// id, the URI, and a parseable RFC3339Nano timestamp.
func TestArchiveOnRetire_HappyPath(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{nextURI: "fake://test/" + retireAgentID + "/snap.tar.gz"}
	logger := &fakeLogger{}

	uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
	if err != nil {
		t.Fatalf("ArchiveOnRetire: %v", err)
	}
	if uri != store.nextURI {
		t.Fatalf("uri = %q, want %q", uri, store.nextURI)
	}
	if got := store.putCalled.Load(); got != 1 {
		t.Fatalf("store.Put called %d times, want 1", got)
	}
	if store.putAgent != retireAgentID {
		t.Fatalf("store.Put agentID = %q, want %q", store.putAgent, retireAgentID)
	}

	// Round-trip: re-seed a fresh notebook from the captured bytes and
	// confirm both rows came back via the helper.
	dst, _, _ := freshDBAt(t, filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err := dst.Import(ctx, &store.putBytes); err != nil {
		t.Fatalf("Import captured bytes: %v", err)
	}
	for _, s := range retireSeeds {
		assertRetireSeed(ctx, t, dst, s)
	}

	assertHappyPathLogger(t, logger, uri)
}

// TestArchiveOnRetire_ArchiveFails — close the notebook before calling so
// db.Archive returns a "database is closed" error from VACUUM INTO. The
// helper must wrap the error and refuse to call Logger.LogAppend. Put may
// be entered (the goroutine starts before Put is called), but the
// returned uri must be empty.
func TestArchiveOnRetire_ArchiveFails(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)
	if err := src.Close(); err != nil {
		t.Fatalf("Close src: %v", err)
	}

	store := &fakeStore{}
	logger := &fakeLogger{}

	uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
	if err == nil {
		t.Fatal("ArchiveOnRetire on closed DB returned nil error")
	}
	if uri != "" {
		t.Fatalf("uri = %q, want empty on Archive failure", uri)
	}
	if !strings.Contains(err.Error(), "archive:") {
		t.Fatalf("err = %v, want one wrapped with 'archive:'", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on Archive failure", got)
	}
}

// TestArchiveOnRetire_PutFails — fakeStore.Put returns an injected error;
// the helper must surface it wrapped, return an empty URI, and never call
// the logger.
func TestArchiveOnRetire_PutFails(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	putBoom := errors.New("put boom")
	store := &fakeStore{putErr: putBoom, putErrBeforeRead: true}
	logger := &fakeLogger{}

	uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
	if err == nil {
		t.Fatal("ArchiveOnRetire returned nil error on Put failure")
	}
	if uri != "" {
		t.Fatalf("uri = %q, want empty on Put failure", uri)
	}
	if !errors.Is(err, putBoom) {
		t.Fatalf("err = %v, want one wrapping put boom", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on Put failure", got)
	}
}

// TestArchiveOnRetire_LogAppendFails — happy Archive + Put, logger
// returns an error. The helper must return the URI (the snapshot is in
// the store and the caller should be able to retry the audit emit) AND
// the wrapped error.
func TestArchiveOnRetire_LogAppendFails(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{nextURI: "fake://test/snapshot.tar.gz"}
	auditBoom := errors.New("audit boom")
	logger := &fakeLogger{logErr: auditBoom}

	uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
	if err == nil {
		t.Fatal("ArchiveOnRetire returned nil error on LogAppend failure")
	}
	if uri == "" {
		t.Fatal("uri = empty on LogAppend failure; partial-failure contract requires the URI")
	}
	if uri != store.nextURI {
		t.Fatalf("uri = %q, want %q", uri, store.nextURI)
	}
	if !errors.Is(err, auditBoom) {
		t.Fatalf("err = %v, want one wrapping audit boom", err)
	}
	if !strings.Contains(err.Error(), "audit emit:") {
		t.Fatalf("err = %v, want prefix 'audit emit:'", err)
	}
}

// TestArchiveOnRetire_ContextCancellation — fakeStore blocks inside Put
// on a never-ready channel, the test cancels ctx mid-flight; the helper
// must return an error wrapping context.Canceled and never invoke the
// logger.
func TestArchiveOnRetire_ContextCancellation(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	store := &fakeStore{putBlock: make(chan struct{})}
	logger := &fakeLogger{}

	type result struct {
		uri string
		err error
	}
	done := make(chan result, 1)
	go func() {
		uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
		done <- result{uri: uri, err: err}
	}()

	// Give the goroutine time to enter Put / start blocking. The fake
	// store consumes the pipe via io.Copy first and only then blocks on
	// putBlock, so a small wait covers both phases.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("ArchiveOnRetire returned nil error on ctx cancel")
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", r.err)
		}
		if r.uri != "" {
			t.Fatalf("uri = %q, want empty on ctx cancel", r.uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ArchiveOnRetire did not return within 2s of ctx cancel")
	}

	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 on ctx cancel", got)
	}
}

// TestArchiveOnRetire_RejectsBadAgentID — a non-canonical UUID must
// return ErrInvalidEntry synchronously without touching the store or the
// logger. (The substrate uses ErrInvalidEntry for shape errors —
// validate(*Entry) does the same, see entry.go.)
func TestArchiveOnRetire_RejectsBadAgentID(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{}
	logger := &fakeLogger{}

	uri, err := ArchiveOnRetire(ctx, src, "not-a-uuid", store, logger)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if uri != "" {
		t.Fatalf("uri = %q, want empty for bad agent id", uri)
	}
	if got := store.putCalled.Load(); got != 0 {
		t.Fatalf("store.Put called %d times, want 0 for bad agent id", got)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times, want 0 for bad agent id", got)
	}
}

// TestArchiveOnRetire_PayloadShape — the audit payload must carry exactly
// the three documented fields and `archived_at` must parse as
// RFC3339Nano. The downstream keepers_log subscribers depend on the
// field names verbatim.
func TestArchiveOnRetire_PayloadShape(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{nextURI: "fake://test/payload-shape.tar.gz"}
	logger := &fakeLogger{}

	before := time.Now().UTC()
	uri, err := ArchiveOnRetire(ctx, src, retireAgentID, store, logger)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("ArchiveOnRetire: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(logger.received.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "uri", "archived_at"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("payload missing %q field: %v", key, raw)
		}
	}
	if got := raw["agent_id"]; got != retireAgentID {
		t.Fatalf("payload.agent_id = %v, want %q", got, retireAgentID)
	}
	if got := raw["uri"]; got != uri {
		t.Fatalf("payload.uri = %v, want %q", got, uri)
	}
	archivedAt, ok := raw["archived_at"].(string)
	if !ok {
		t.Fatalf("payload.archived_at = %v, want string", raw["archived_at"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, archivedAt)
	if err != nil {
		t.Fatalf("payload.archived_at = %q does not parse as RFC3339Nano: %v", archivedAt, err)
	}
	// Sanity: timestamp is within the test's wall-clock window. Allow a
	// 1s slack on either side because the helper formats with UTC and the
	// before/after captures bracket the helper without it.
	if parsed.Before(before.Add(-time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Fatalf("payload.archived_at = %v, want between %v and %v", parsed, before, after)
	}
}

// TestArchiveOnRetire_LoggerInterfaceSatisfiedByKeepclient — compile-time
// check that the production `*keepclient.Client` still satisfies the
// `Logger` interface defined in this package. A future refactor of the
// keepclient signature would otherwise silently make ArchiveOnRetire
// uncallable from real callers.
func TestArchiveOnRetire_LoggerInterfaceSatisfiedByKeepclient(_ *testing.T) {
	var _ Logger = (*keepclient.Client)(nil)
}
