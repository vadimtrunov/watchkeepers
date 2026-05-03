package archivestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// minioImage is the testcontainer image tag used for every S3-backed
// test. Pinned to a stable RELEASE-tagged image so the suite is
// deterministic across machines and CI runs. Bump deliberately when a
// minio-side fix matters; otherwise the cached layer makes container
// startup nearly free.
const minioImage = "minio/minio:RELEASE.2024-08-29T01-40-52Z"

// minioRootUser/minioRootPassword are the access/secret credentials the
// testcontainer module wires into the launched MinIO. The module does
// not expose getters for these, so the test uses the same constants
// when constructing the [*S3Compatible] client below.
const (
	minioRootUser     = "minioadmin"
	minioRootPassword = "minioadmin"
)

// testMinioOnce / testMinioContainer / testMinioErr form a per-process
// singleton: the first test to need the container starts it, every
// subsequent test in the same `go test` invocation reuses the same
// MinIO instance. Per-test isolation comes from a fresh, UUID-named
// bucket created in the factory below.
//
// This trades cleanup-on-first-failure simplicity for a 5–15 second
// speedup across the suite — once the image is cached locally the bulk
// of the cost is the MinIO cold-start, and that only happens once.
var (
	testMinioOnce      sync.Once
	testMinioContainer *tcminio.MinioContainer
	testMinioErr       error
)

// sharedMinioContainer returns the per-process MinIO test container,
// starting it on first call. Returns the launch error verbatim so the
// caller can decide whether to `t.Skip` (Docker unavailable) or
// `t.Fatalf` (container started but then died, etc.).
func sharedMinioContainer(t *testing.T) (*tcminio.MinioContainer, error) {
	t.Helper()
	testMinioOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		c, err := tcminio.Run(
			ctx,
			minioImage,
			tcminio.WithUsername(minioRootUser),
			tcminio.WithPassword(minioRootPassword),
		)
		if err != nil {
			testMinioErr = err
			return
		}
		testMinioContainer = c
	})
	return testMinioContainer, testMinioErr
}

// isDockerUnavailable returns true when `err` looks like the
// testcontainers runtime failed to reach a Docker daemon. The check is
// substring-based because testcontainers wraps the underlying error in
// several layers depending on the container backend; this captures the
// common dev-laptop and CI cases.
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"Cannot connect to the Docker daemon",
		"docker daemon",
		"connection refused",
		"no such file or directory",
		"failed to find rootful",
		"failed to find rootless",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// minioBucketCounter generates monotonically distinct bucket suffixes
// inside a single test run. Used together with a UUID prefix so the
// resulting bucket name is both globally unique (across CI shards
// running concurrently) and deterministic-per-invocation order.
var minioBucketCounter int64

// minioEndpoint extracts the host:port from the testcontainer's
// connection URL. The module exposes ConnectionString as
// `host:port` directly (no scheme), but we pass it through `url.Parse`
// defensively in case a future module version returns a scheme-prefixed
// form.
func minioEndpoint(t *testing.T, c *tcminio.MinioContainer) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("minio connection string: %v", err)
	}
	if u, perr := url.Parse(cs); perr == nil && u.Host != "" {
		return u.Host
	}
	return cs
}

// monotonicS3Clock mirrors the LocalFS test helper: a 1-second-tick
// deterministic clock so the timestamp portion of each object key is
// distinct even on fast machines (the timestamp layout has 1-second
// resolution).
func monotonicS3Clock(start time.Time) func() time.Time {
	var ticks int64
	return func() time.Time {
		i := atomic.AddInt64(&ticks, 1) - 1
		return start.Add(time.Duration(i) * time.Second).UTC()
	}
}

// newTmpS3Compatible is the contract-suite factory: returns a fresh
// [*S3Compatible] backed by a UUID-named bucket inside the shared
// MinIO container. Skips the test cleanly when Docker is unavailable
// instead of failing — local `go test` against an offline laptop must
// not see a red bar from the S3 lane.
func newTmpS3Compatible(t *testing.T) ArchiveStore {
	t.Helper()
	c, err := sharedMinioContainer(t)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skip("Docker not available: " + err.Error())
		}
		t.Fatalf("minio container: %v", err)
	}

	endpoint := minioEndpoint(t, c)
	idx := atomic.AddInt64(&minioBucketCounter, 1)
	bucket := strings.ToLower(fmt.Sprintf("ws-as-%s-%d", uuid.NewString()[:8], idx))

	store, err := NewS3Compatible(
		S3Config{
			Endpoint:  endpoint,
			AccessKey: minioRootUser,
			SecretKey: minioRootPassword,
			Bucket:    bucket,
			Secure:    false,
		},
		WithCreateBucket(true),
		WithS3Clock(monotonicS3Clock(time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC))),
	)
	if err != nil {
		t.Fatalf("NewS3Compatible: %v", err)
	}
	return store
}

// TestS3Compatible_Contract plugs the S3Compatible backend into the
// parameterised contract suite from `contract_test.go`. The exact same
// six cases LocalFS passes are exercised here, sharing zero
// per-backend code — the whole point of the M2b.3.a interface
// refactor.
func TestS3Compatible_Contract(t *testing.T) {
	runContractTests(t, newTmpS3Compatible)
}

// TestS3Compatible_RoundTripWithNotebook is the integration round-trip:
// a real notebook seeds 3 entries via Remember, Archives them through
// the S3 store, Gets them back, Imports into a fresh notebook, and
// asserts both the high-level Recall path and the embedding-bytes
// byte-equality. Mirrors the LocalFS round-trip test so the two
// backends are exercised symmetrically.
func TestS3Compatible_RoundTripWithNotebook(t *testing.T) {
	store, bucket := newRoundTripS3Store(t)
	ctx := context.Background()

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
	rememberSeeds(ctx, t, src, seeds)

	var snapshot bytes.Buffer
	if err := src.Archive(ctx, &snapshot); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	const agentID = "44444444-4444-4444-4444-444444444444"
	uri, err := store.Put(ctx, agentID, &snapshot)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(uri, "s3://"+bucket+"/") {
		t.Fatalf("Put URI = %q, want prefix %q", uri, "s3://"+bucket+"/")
	}

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

	assertRecallRoundTrip(ctx, t, dst, seeds)

	// Embedding-bytes round-trip via raw sql.Open against the imported
	// SQLite file. Mirrors the LocalFS round-trip helper exactly so any
	// drift between the two backends shows up as an immediate diff.
	actualDstPath := filepath.Join(
		filepath.Dir(dstPath), "notebook",
		pathToUUID(filepath.Base(dstPath))+".sqlite",
	)
	if err := dst.Close(); err != nil {
		t.Fatalf("close dst before raw open: %v", err)
	}
	assertEmbeddingBytesRoundTrip(ctx, t, actualDstPath, seeds)
}

// newRoundTripS3Store builds a fresh [*S3Compatible] with a UUID-named
// bucket inside the shared MinIO container. Returns the store and the
// bucket name so the caller can assert URI prefixes. Skips the test
// cleanly when Docker is unavailable.
func newRoundTripS3Store(t *testing.T) (ArchiveStore, string) {
	t.Helper()
	c, err := sharedMinioContainer(t)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skip("Docker not available: " + err.Error())
		}
		t.Fatalf("minio container: %v", err)
	}
	endpoint := minioEndpoint(t, c)
	bucket := strings.ToLower("ws-rt-" + uuid.NewString()[:8])
	store, err := NewS3Compatible(
		S3Config{
			Endpoint:  endpoint,
			AccessKey: minioRootUser,
			SecretKey: minioRootPassword,
			Bucket:    bucket,
			Secure:    false,
		},
		WithCreateBucket(true),
	)
	if err != nil {
		t.Fatalf("NewS3Compatible: %v", err)
	}
	return store, bucket
}

// rememberSeeds writes every seed entry through `src.Remember`, failing
// the test on the first error. Extracted so the round-trip orchestrator
// stays inside the gocyclo threshold.
func rememberSeeds(ctx context.Context, t *testing.T, src *notebook.DB, seeds []seedEntry) {
	t.Helper()
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
}

// assertRecallRoundTrip asserts every seed is reachable via `dst.Recall`
// after Import, including ID and content equality. Bytes-level checks
// happen separately in [assertEmbeddingBytesRoundTrip].
func assertRecallRoundTrip(ctx context.Context, t *testing.T, dst *notebook.DB, seeds []seedEntry) {
	t.Helper()
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
}

// TestS3Compatible_BucketAutoCreate verifies the [WithCreateBucket]
// option creates the bucket when it does not already exist. Uses a
// UUID-named bucket so it never collides with the contract suite's
// per-test buckets.
func TestS3Compatible_BucketAutoCreate(t *testing.T) {
	c, err := sharedMinioContainer(t)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skip("Docker not available: " + err.Error())
		}
		t.Fatalf("minio container: %v", err)
	}
	endpoint := minioEndpoint(t, c)
	bucket := strings.ToLower("ws-ac-" + uuid.NewString()[:8])

	if _, err := NewS3Compatible(
		S3Config{
			Endpoint:  endpoint,
			AccessKey: minioRootUser,
			SecretKey: minioRootPassword,
			Bucket:    bucket,
			Secure:    false,
		},
		WithCreateBucket(true),
	); err != nil {
		t.Fatalf("NewS3Compatible(create=true): %v", err)
	}

	// Verify the bucket actually exists post-construction by querying
	// minio directly (bypasses the [*S3Compatible] wrapper so a bug in
	// the wrapper can't false-positive this test).
	mc, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioRootUser, minioRootPassword, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	exists, err := mc.BucketExists(context.Background(), bucket)
	if err != nil {
		t.Fatalf("BucketExists: %v", err)
	}
	if !exists {
		t.Fatalf("bucket %q was not created", bucket)
	}
}

// TestS3Compatible_RejectsMissingBucket verifies that, without
// [WithCreateBucket], constructing a store against a non-existent
// bucket fails. Companion to TestS3Compatible_BucketAutoCreate.
func TestS3Compatible_RejectsMissingBucket(t *testing.T) {
	c, err := sharedMinioContainer(t)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skip("Docker not available: " + err.Error())
		}
		t.Fatalf("minio container: %v", err)
	}
	endpoint := minioEndpoint(t, c)
	bucket := strings.ToLower("ws-mb-" + uuid.NewString()[:8])

	_, err = NewS3Compatible(S3Config{
		Endpoint:  endpoint,
		AccessKey: minioRootUser,
		SecretKey: minioRootPassword,
		Bucket:    bucket,
		Secure:    false,
	})
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("NewS3Compatible(missing bucket) error = %v, want ErrInvalidURI", err)
	}
}

// TestS3Compatible_GetWrongBucket verifies that a Get against a URI
// referencing a different bucket than the one configured surfaces as
// [ErrInvalidURI] without any network call to the foreign bucket.
func TestS3Compatible_GetWrongBucket(t *testing.T) {
	store := newTmpS3Compatible(t)
	uri := "s3://other-bucket-not-mine/notebook/" + validAgentID + "/2026-05-03T12-00-00Z.tar.gz"
	_, err := store.Get(context.Background(), uri)
	if !errors.Is(err, ErrInvalidURI) {
		t.Fatalf("Get(%q) error = %v, want ErrInvalidURI", uri, err)
	}
}

// TestS3Compatible_ListEmpty mirrors the LocalFS coverage: a fresh
// store returns (nil, nil) for an agent that has never been Put
// against this bucket.
func TestS3Compatible_ListEmpty(t *testing.T) {
	store := newTmpS3Compatible(t)
	uris, err := store.List(context.Background(), validAgentID)
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if uris != nil {
		t.Fatalf("List = %v, want nil on never-written agent", uris)
	}
}

// TestS3Compatible_NewS3Compatible_RequiresFields covers the constructor's
// mandatory field validation. Each subtest leaves exactly one field
// empty and asserts the constructor refuses with a non-nil error before
// touching the network.
func TestS3Compatible_NewS3Compatible_RequiresFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  S3Config
	}{
		{"missing endpoint", S3Config{AccessKey: "k", SecretKey: "s", Bucket: "b"}},
		{"missing accesskey", S3Config{Endpoint: "e", SecretKey: "s", Bucket: "b"}},
		{"missing secretkey", S3Config{Endpoint: "e", AccessKey: "k", Bucket: "b"}},
		{"missing bucket", S3Config{Endpoint: "e", AccessKey: "k", SecretKey: "s"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewS3Compatible(tc.cfg); err == nil {
				t.Fatalf("NewS3Compatible(%+v) returned nil error, want non-nil", tc.cfg)
			}
		})
	}
}

// TestS3Compatible_GetRejectsKeyOutsidePrefix locks in the
// path-traversal defence: a URI whose key is well-formed but does not
// sit under `notebook/` is rejected with [ErrInvalidURI] before any
// network call. Complements the contract suite's traversal case.
func TestS3Compatible_GetRejectsKeyOutsidePrefix(t *testing.T) {
	store := newTmpS3Compatible(t).(*S3Compatible)
	for _, uri := range []string{
		"s3://" + store.bucket + "/etc/passwd",
		"s3://" + store.bucket + "/notebook/../etc/passwd",
		"s3://" + store.bucket + "/",
	} {
		if _, err := store.Get(context.Background(), uri); !errors.Is(err, ErrInvalidURI) {
			t.Fatalf("Get(%q) error = %v, want ErrInvalidURI", uri, err)
		}
	}
}

// TestS3Compatible_IsDockerUnavailableMatcher exercises the substring
// detector used by the skip path. Pure unit test: no container, no
// network. Guards against accidental loosening that would let a real
// failure look like a Docker-absent skip.
func TestS3Compatible_IsDockerUnavailableMatcher(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"docker daemon down", errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), true},
		{"connection refused", errors.New("dial tcp 127.0.0.1:2375: connection refused"), true},
		{"unrelated", errors.New("PutObject: 403 access denied"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := isDockerUnavailable(tc.err); got != tc.want {
				t.Fatalf("isDockerUnavailable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
