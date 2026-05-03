package archivestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Scheme is the URI scheme produced by [S3Compatible.Put] and accepted
// by [S3Compatible.Get]. Any other scheme (including `file://`) is
// rejected with [ErrInvalidURI]. Mirrors the LocalFS [fileScheme]
// convention so URIs route unambiguously to the correct backend.
const s3Scheme = "s3://"

// gzipContentType is the MIME type set on every uploaded snapshot so
// downstream tooling (browsers, CLIs) recognises the body without
// sniffing the bytes. The wire format itself is fixed by
// [writeTarballStream]: a single-entry gzipped tar.
const gzipContentType = "application/gzip"

// keyPrefix is the per-agent object-key prefix in the bucket. Mirrors the
// LocalFS on-disk layout (`<root>/notebook/<agentID>/<timestamp>.tar.gz`)
// so an operator inspecting either backend sees the same shape.
const keyPrefix = "notebook/"

// S3Config carries every value needed to construct an [*S3Compatible]
// against a generic S3-compatible endpoint (AWS S3, Cloudflare R2,
// MinIO, Wasabi, SeaweedFS, Garage). Plain struct fields rather than a
// secrets-interface lookup — the cross-backend secrets shape lands in M9
// and replaces this struct with a getter callback.
//
//   - Endpoint   - host[:port], no scheme. AWS S3 expects
//     `s3.<region>.amazonaws.com`; MinIO expects whatever the operator
//     configured (commonly `localhost:9000` for local testcontainers).
//   - AccessKey, SecretKey - V4-signature credentials. Both required.
//   - Bucket   - bucket name. The implementation rejects any URI that
//     references a different bucket with [ErrInvalidURI].
//   - Region   - optional. AWS S3 needs the right region for SigV4;
//     MinIO/R2 accept "" or any value.
//   - Secure   - `true` for HTTPS, `false` for HTTP. Local testcontainer
//     MinIO is HTTP by default.
type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	Secure    bool
}

// S3Compatible is the S3-compatible-blob-store [ArchiveStore]. Snapshots
// land at `<bucket>/notebook/<agentID>/<timestamp>.tar.gz`, where
// `<timestamp>` mirrors the LocalFS [timestampLayout] (UTC,
// fixed-length, no colons, lex-sortable).
//
// Safe for concurrent use across goroutines: every method routes through
// the embedded `*minio.Client`, which is itself safe per minio-go's docs.
type S3Compatible struct {
	client       *minio.Client
	bucket       string
	region       string
	clock        func() time.Time
	createBucket bool
}

// S3Option configures an [*S3Compatible] at construction time. Mirrors
// the LocalFS functional-options shape so both backends feel the same to
// callers.
type S3Option func(*s3OptionState)

// s3OptionState is the internal mutable bag the [S3Option] callbacks
// populate. Held in a separate type so [*S3Compatible] itself stays
// immutable after [NewS3Compatible] returns.
type s3OptionState struct {
	clock        func() time.Time
	createBucket bool
}

// WithS3Clock injects the clock used to stamp object keys. The zero
// value defaults to [time.Now]. Tests pass a deterministic source to
// assert ordering and timestamp-format invariants — same idiom as
// [WithClock] for LocalFS.
func WithS3Clock(clock func() time.Time) S3Option {
	return func(s *s3OptionState) { s.clock = clock }
}

// WithCreateBucket toggles auto-creation of the configured bucket inside
// [NewS3Compatible]. Default is `false`: the constructor returns an
// error when the bucket does not exist. Set `true` for ephemeral test
// buckets and operator-controlled deployments where the agent process is
// allowed to provision its own bucket.
func WithCreateBucket(create bool) S3Option {
	return func(s *s3OptionState) { s.createBucket = create }
}

// NewS3Compatible constructs an [*S3Compatible] against the supplied
// endpoint and verifies the bucket is reachable. Required fields:
// `Endpoint`, `AccessKey`, `SecretKey`, `Bucket`. The constructor calls
// `BucketExists` (and `MakeBucket` when [WithCreateBucket] is set);
// either failure surfaces as a non-nil error so callers can fail fast
// rather than discovering the misconfiguration on first Put.
func NewS3Compatible(cfg S3Config, opts ...S3Option) (*S3Compatible, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("archivestore: S3Config.Endpoint is required")
	}
	if cfg.AccessKey == "" {
		return nil, errors.New("archivestore: S3Config.AccessKey is required")
	}
	if cfg.SecretKey == "" {
		return nil, errors.New("archivestore: S3Config.SecretKey is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("archivestore: S3Config.Bucket is required")
	}

	state := s3OptionState{clock: time.Now}
	for _, opt := range opts {
		opt(&state)
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("archivestore: minio client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("archivestore: bucket exists check: %w", err)
	}
	if !exists {
		if !state.createBucket {
			return nil, fmt.Errorf("archivestore: bucket %q does not exist: %w", cfg.Bucket, ErrInvalidURI)
		}
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("archivestore: create bucket %q: %w", cfg.Bucket, err)
		}
	}

	return &S3Compatible{
		client:       client,
		bucket:       cfg.Bucket,
		region:       cfg.Region,
		clock:        state.clock,
		createBucket: state.createBucket,
	}, nil
}

// Put streams `snapshot` into a single-entry gzipped tar named
// `<agentID>.sqlite` and uploads it to
// `<bucket>/notebook/<agentID>/<timestamp>.tar.gz`. Returns the resulting
// `s3://<bucket>/<key>` URI for audit emission (M2b.7).
//
// Implementation note: we buffer the gzipped tarball in memory before
// calling `PutObject` so the object size is known up-front. Snapshots
// are agent-local SQLite files (typically a few MiB) and the wire format
// itself is gzip-compressed, so the buffer cost is bounded; if this ever
// becomes a problem, swap the buffer for an `io.Pipe` + length-unknown
// `PutObject(..., -1, ...)`.
func (s *S3Compatible) Put(ctx context.Context, agentID string, snapshot io.Reader) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Buffer the snapshot so we know its exact size for the tar header
	// and the PutObject Content-Length. Same trade-off LocalFS makes via
	// the on-disk spool, just RAM-resident.
	var raw bytes.Buffer
	if _, err := io.Copy(&raw, snapshot); err != nil {
		return "", fmt.Errorf("archivestore: buffer snapshot: %w", err)
	}

	var packed bytes.Buffer
	if err := writeTarballStream(&packed, &raw, agentID, int64(raw.Len())); err != nil {
		return "", err
	}

	key := keyPrefix + agentID + "/" + s.clock().UTC().Format(timestampLayout) + ".tar.gz"
	body := bytes.NewReader(packed.Bytes())
	_, err := s.client.PutObject(ctx, s.bucket, key, body, int64(packed.Len()), minio.PutObjectOptions{
		ContentType: gzipContentType,
	})
	if err != nil {
		return "", fmt.Errorf("archivestore: put object %q: %w", key, err)
	}

	return s3Scheme + s.bucket + "/" + key, nil
}

// Get parses `uri`, fetches the matching object from the configured
// bucket, and returns the inner SQLite bytes via the shared
// [openTarballStream] helper. The returned reader streams from
// `*minio.Object` so callers don't pay the full snapshot size in RAM.
//
// Errors:
//   - [ErrInvalidURI] when the scheme is not `s3://`, the URI bucket
//     differs from the configured one, or the gzipped tar is malformed;
//   - [ErrNotFound] when minio surfaces `NoSuchKey` for the parsed key.
func (s *S3Compatible) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bucket, key, err := s.parseURI(uri)
	if err != nil {
		return nil, err
	}

	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("archivestore: get object %q: %w", key, err)
	}
	// minio-go's GetObject is lazy: it returns an `*minio.Object` even
	// when the key does not exist; the failure surfaces on the first
	// Stat()/Read(). Trigger the round-trip here so we can map
	// `NoSuchKey` → [ErrNotFound] before handing the reader off.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("archivestore: stat object %q: %w", key, err)
	}

	rc, err := openTarballStream(obj)
	if err != nil {
		// openTarballStream closes `obj` on failure; just enrich the error
		// with the object key for debuggability.
		return nil, fmt.Errorf("archivestore: read archive %q: %w", key, err)
	}
	return rc, nil
}

// List returns every snapshot URI for `agentID` in the configured
// bucket, newest first. Object names produced by [S3Compatible.Put] use
// the same fixed-length lex-sortable timestamp as LocalFS, so a
// reverse-lex sort matches reverse-chronological order.
//
// Returns (nil, nil) when no objects match the prefix; the absence of
// objects is not an error here because List is also the discovery path
// callers use to decide whether to import anything.
func (s *S3Compatible) List(ctx context.Context, agentID string) ([]string, error) {
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prefix := keyPrefix + agentID + "/"
	ch := s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	var keys []string
	for obj := range ch {
		if obj.Err != nil {
			return nil, fmt.Errorf("archivestore: list objects %q: %w", prefix, obj.Err)
		}
		if !strings.HasSuffix(obj.Key, ".tar.gz") {
			continue
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// Reverse-lex sort -> newest first because the timestamp layout is
	// fixed-length and lexicographically ordered.
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))

	uris := make([]string, len(keys))
	for i, k := range keys {
		uris[i] = s3Scheme + s.bucket + "/" + k
	}
	return uris, nil
}

// parseURI decodes an `s3://<bucket>/<key>` URI. Returns the bucket and
// key portions on success or [ErrInvalidURI] when the scheme is missing,
// the URI is malformed, the bucket does not match the configured one,
// or the cleaned key escapes its `notebook/` prefix (path-traversal
// defence — `..` segments in keys would otherwise let an attacker read
// arbitrary objects in the bucket through this method).
func (s *S3Compatible) parseURI(uri string) (string, string, error) {
	if !strings.HasPrefix(uri, s3Scheme) {
		return "", "", fmt.Errorf("archivestore: scheme not %s in %q: %w", s3Scheme, uri, ErrInvalidURI)
	}
	rest := strings.TrimPrefix(uri, s3Scheme)
	if rest == "" {
		return "", "", fmt.Errorf("archivestore: empty path in %q: %w", uri, ErrInvalidURI)
	}
	// Decode percent-escapes for parity with the on-wire URL form a
	// callers might paste in. minio-go itself accepts raw keys but URIs
	// can carry encoded slashes if produced by URL-aware tooling.
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		return "", "", fmt.Errorf("archivestore: decode %q: %w: %w", rest, err, ErrInvalidURI)
	}
	slash := strings.IndexByte(decoded, '/')
	if slash <= 0 || slash == len(decoded)-1 {
		return "", "", fmt.Errorf("archivestore: missing bucket or key in %q: %w", uri, ErrInvalidURI)
	}
	bucket := decoded[:slash]
	key := decoded[slash+1:]
	if bucket != s.bucket {
		return "", "", fmt.Errorf("archivestore: bucket %q does not match configured %q: %w", bucket, s.bucket, ErrInvalidURI)
	}
	// Path-traversal defence: the key must stay inside the `notebook/`
	// prefix. `..` segments in S3 keys are syntactically legal but the
	// agent-archive contract treats every key as a tarball under
	// `notebook/<agentID>/`; anything else is suspect.
	if !strings.HasPrefix(key, keyPrefix) {
		return "", "", fmt.Errorf("archivestore: key %q outside %q prefix: %w", key, keyPrefix, ErrInvalidURI)
	}
	if strings.Contains(key, "..") {
		return "", "", fmt.Errorf("archivestore: key %q contains traversal segment: %w", key, ErrInvalidURI)
	}
	return bucket, key, nil
}
