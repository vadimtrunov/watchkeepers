package archivestore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// dirMode is the file mode applied to the on-disk archive directory tree.
// Snapshots can carry the agent's private memory bytes verbatim, so only
// the owner reads (`0o700`). Mirrors the notebook substrate's directory
// mode discipline (`core/pkg/notebook/path.go`).
const dirMode os.FileMode = 0o700

// fileMode is the file mode applied to the persisted `.tar.gz` and to the
// inner tar entry's header. `0o600` keeps the snapshot owner-readable
// only, mirroring the parent directory's `0o700`.
const fileMode os.FileMode = 0o600

// timestampLayout is the format passed to [time.Time.Format] for the
// per-snapshot filename. UTC-only, fixed-length, no colons (which are
// illegal in Windows filenames), and lexicographically sortable so
// `List` can produce newest-first output by sorting filenames.
const timestampLayout = "2006-01-02T15-04-05Z"

// fileScheme is the URI scheme produced by [LocalFS.Put] and accepted by
// [LocalFS.Get]. Future backends (e.g. `S3Compatible` in M2b.3.b) define
// their own schemes and the ArchiveStore router rejects mismatches with
// [ErrInvalidURI].
const fileScheme = "file://"

// LocalFS is the filesystem-backed [ArchiveStore]. Snapshots land at
// `<root>/notebook/<agent_id>/<timestamp>.tar.gz`; each tarball contains
// exactly one entry named `<agent_id>.sqlite` carrying the bytes from
// [notebook.DB.Archive].
//
// LocalFS is safe for concurrent use across goroutines: every Put runs
// against an os-allocated temp file under the per-agent dir, the rename
// into the final filename is atomic on POSIX, and Get / List only open
// files for reading.
type LocalFS struct {
	root  string
	clock func() time.Time
}

// LocalFSOption configures a [LocalFS] at construction time.
type LocalFSOption func(*localFSConfig)

// localFSConfig is the internal mutable bag the [LocalFSOption] callbacks
// populate. Held in a separate type so [LocalFS] itself stays immutable
// after [NewLocalFS] returns.
type localFSConfig struct {
	root  string
	clock func() time.Time
}

// WithRoot sets the on-disk root under which the per-agent snapshot
// subtree is created. The path is resolved to its absolute form via
// [filepath.Abs] inside [NewLocalFS]; relative paths are accepted but
// stored as absolute so the path-traversal defence in [LocalFS.Get] has
// a single canonical reference.
//
// Mandatory: passing [NewLocalFS] no [WithRoot] returns an error rather
// than silently defaulting to the current working directory.
func WithRoot(path string) LocalFSOption {
	return func(c *localFSConfig) { c.root = path }
}

// WithClock injects the clock used to stamp snapshot filenames. The zero
// value defaults to [time.Now]. Tests pass a deterministic source to
// assert ordering and timestamp-format invariants.
func WithClock(clock func() time.Time) LocalFSOption {
	return func(c *localFSConfig) { c.clock = clock }
}

// NewLocalFS constructs a [LocalFS]. [WithRoot] is mandatory; omitting it
// returns a non-nil error so callers don't silently land on the working
// directory.
//
// On success the root directory is created (`0o700`) if missing — the
// per-agent subdirectory is created lazily on the first [LocalFS.Put]
// for that agent.
func NewLocalFS(opts ...LocalFSOption) (*LocalFS, error) {
	cfg := localFSConfig{
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.root == "" {
		return nil, errors.New("archivestore: WithRoot is required")
	}
	abs, err := filepath.Abs(cfg.root)
	if err != nil {
		return nil, fmt.Errorf("archivestore: resolve root %q: %w", cfg.root, err)
	}
	cleaned := filepath.Clean(abs)
	if err := os.MkdirAll(cleaned, dirMode); err != nil {
		return nil, fmt.Errorf("archivestore: create root %q: %w", cleaned, err)
	}
	// Defeat a permissive umask so a fresh-on-disk root matches a re-used
	// one. Same idiom as `core/pkg/notebook/path.go`.
	if err := os.Chmod(cleaned, dirMode); err != nil {
		return nil, fmt.Errorf("archivestore: chmod root %q: %w", cleaned, err)
	}
	return &LocalFS{root: cleaned, clock: cfg.clock}, nil
}

// Put writes the snapshot bytes from `snapshot` into a gzipped tarball
// under `<root>/notebook/<agentID>/<timestamp>.tar.gz` and returns the
// resulting `file://<absolute-path>` URI. The reader is consumed exactly
// once.
//
// Implementation: spool the snapshot to a temp file inside the per-agent
// directory (so [tar.Header.Size] is known without buffering the whole
// snapshot in memory), then stream from the temp file into the gzipped
// tar. The temp file is removed before Put returns regardless of error.
func (s *LocalFS) Put(ctx context.Context, agentID string, snapshot io.Reader) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	agentDir := filepath.Join(s.root, "notebook", agentID)
	if err := os.MkdirAll(agentDir, dirMode); err != nil {
		return "", fmt.Errorf("archivestore: create agent dir %q: %w", agentDir, err)
	}
	if err := os.Chmod(agentDir, dirMode); err != nil {
		return "", fmt.Errorf("archivestore: chmod agent dir %q: %w", agentDir, err)
	}

	// Spool the snapshot into a same-dir temp file so we can size the
	// tar header without buffering the snapshot in memory. Same-dir is
	// chosen so the eventual rename of the .tar.gz stays on the same
	// filesystem (POSIX [os.Rename] limitation).
	spool, err := os.CreateTemp(agentDir, ".archive-spool-*.sqlite")
	if err != nil {
		return "", fmt.Errorf("archivestore: create spool: %w", err)
	}
	spoolPath := spool.Name()
	defer func() { _ = os.Remove(spoolPath) }()

	if _, err := io.Copy(spool, snapshot); err != nil {
		_ = spool.Close()
		return "", fmt.Errorf("archivestore: spool snapshot: %w", err)
	}
	if err := spool.Close(); err != nil {
		return "", fmt.Errorf("archivestore: close spool: %w", err)
	}

	info, err := os.Stat(spoolPath)
	if err != nil {
		return "", fmt.Errorf("archivestore: stat spool: %w", err)
	}

	filename := s.clock().UTC().Format(timestampLayout) + ".tar.gz"
	finalPath := filepath.Join(agentDir, filename)

	if err := writeTarball(spoolPath, finalPath, agentID, info.Size()); err != nil {
		_ = os.Remove(finalPath)
		return "", err
	}

	return fileScheme + finalPath, nil
}

// writeTarball produces a gzipped tar at `dstPath` containing exactly one
// entry named `<agentID>.sqlite` (mode [fileMode]) whose bytes come from
// `srcPath`. Pulled out of [LocalFS.Put] so the happy path stays linear
// and the deferred close-and-cleanup chain is easier to read.
func writeTarball(srcPath, dstPath, agentID string, size int64) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("archivestore: reopen spool: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("archivestore: create archive %q: %w", dstPath, err)
	}
	// Track close success so we can return its error if the io path
	// otherwise succeeded; redundant Close after success is a no-op
	// guarded by the `closed` flag.
	closed := false
	defer func() {
		if !closed {
			_ = dst.Close()
		}
	}()

	gz := gzip.NewWriter(dst)
	tw := tar.NewWriter(gz)

	hdr := &tar.Header{
		Name:    agentID + ".sqlite",
		Mode:    int64(fileMode),
		Size:    size,
		ModTime: time.Now().UTC(),
		Format:  tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("archivestore: write tar header: %w", err)
	}
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("archivestore: write tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("archivestore: close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("archivestore: close gzip writer: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("archivestore: close archive: %w", err)
	}
	closed = true
	return nil
}

// Get opens the snapshot at `uri` and returns an [io.ReadCloser] that
// streams the inner SQLite bytes — the caller does not see the gzip or
// tar wrappers and can feed the returned reader straight into
// [notebook.DB.Import].
//
// Errors:
//   - [ErrInvalidURI] when the scheme is not `file://`, the path is not
//     under the configured root (path-traversal defence), or the file is
//     not a valid single-entry gzipped tar;
//   - [ErrNotFound] when the URI is well-formed and inside the root but
//     no file exists at that path.
func (s *LocalFS) Get(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolveURI(uri)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("archivestore: open %q: %w", path, err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("archivestore: open gzip %q: %w: %w", path, err, ErrInvalidURI)
	}
	tr := tar.NewReader(gz)
	if _, err := tr.Next(); err != nil {
		_ = gz.Close()
		_ = f.Close()
		return nil, fmt.Errorf("archivestore: read tar header %q: %w: %w", path, err, ErrInvalidURI)
	}
	return &snapshotReader{tar: tr, gz: gz, file: f}, nil
}

// snapshotReader wraps the tar entry's body so the caller sees a flat
// [io.ReadCloser]. Close releases the gzip reader and the underlying
// file in that order; the tar reader has no Close (the gzip Close
// covers the underlying gzip stream).
type snapshotReader struct {
	tar  *tar.Reader
	gz   *gzip.Reader
	file *os.File
}

// Read forwards to the tar reader, which itself reads from the gzip
// stream. EOF on the tar entry signals the end of the snapshot bytes.
func (r *snapshotReader) Read(p []byte) (int, error) { return r.tar.Read(p) }

// Close closes the gzip reader and the underlying file. Both errors are
// surfaced — the gzip error wins when both fail because the gzip layer
// is logically "closer to" the data the caller saw.
func (r *snapshotReader) Close() error {
	gerr := r.gz.Close()
	ferr := r.file.Close()
	if gerr != nil {
		return gerr
	}
	return ferr
}

// List returns every snapshot URI for `agentID` under this LocalFS root,
// newest first. Filenames are fixed-length RFC3339 timestamps so a
// reverse lexicographical sort matches reverse chronological order.
//
// Returns (nil, nil) when the per-agent directory does not exist (i.e.
// the agent has never been Put against this root); the absence of a
// directory is not an error here because List is also the discovery
// path that callers use to decide whether to import anything.
func (s *LocalFS) List(ctx context.Context, agentID string) ([]string, error) {
	if err := validateAgentID(agentID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	agentDir := filepath.Join(s.root, "notebook", agentID)
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("archivestore: read agent dir %q: %w", agentDir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		names = append(names, name)
	}
	// Reverse-lex sort -> newest first because the timestamp layout is
	// fixed-length and lexicographically ordered.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	uris := make([]string, len(names))
	for i, name := range names {
		uris[i] = fileScheme + filepath.Join(agentDir, name)
	}
	return uris, nil
}

// resolveURI parses a `file://` URI into an absolute filesystem path,
// rejecting non-`file://` schemes and any path that resolves outside the
// configured root with [ErrInvalidURI]. The returned path is in cleaned,
// absolute form.
func (s *LocalFS) resolveURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, fileScheme) {
		return "", fmt.Errorf("archivestore: scheme not %s in %q: %w", fileScheme, uri, ErrInvalidURI)
	}
	raw := strings.TrimPrefix(uri, fileScheme)
	if raw == "" {
		return "", fmt.Errorf("archivestore: empty path in %q: %w", uri, ErrInvalidURI)
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("archivestore: resolve %q: %w: %w", raw, err, ErrInvalidURI)
	}
	cleaned := filepath.Clean(abs)
	rootCleaned := filepath.Clean(s.root)
	// Allow the root itself (defensive — Get against the root will fall
	// through to os.Open and fail naturally) and any descendant. Reject
	// everything else, including sibling paths whose prefix accidentally
	// matches without a separator (`/foo` vs `/foobar`).
	if cleaned != rootCleaned && !strings.HasPrefix(cleaned, rootCleaned+string(filepath.Separator)) {
		return "", fmt.Errorf("archivestore: path %q escapes root %q: %w", cleaned, rootCleaned, ErrInvalidURI)
	}
	return cleaned, nil
}
