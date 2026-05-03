package archivestore

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"time"
)

// writeTarballStream produces a gzipped tar containing exactly one entry
// named `<agentID>.sqlite` (mode [fileMode]) whose bytes come from `src`.
// The tar header carries `size` as the entry length when known; pass `-1`
// only when the producer cannot pre-size the snapshot, in which case the
// caller is responsible for buffering — `archive/tar` itself does not
// support unknown sizes (the header is fixed-length).
//
// Shared between the LocalFS and S3Compatible backends so both produce a
// byte-identical tarball layout: callers can move a `.tar.gz` from one
// backend to the other and feed it through the matching reader without
// reformatting. M2b.3.b extracts this from `localfs.go` so S3Compatible
// can reuse it.
func writeTarballStream(dst io.Writer, src io.Reader, agentID string, size int64) error {
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
	return nil
}

// openTarballStream wraps `src` (a stream of gzipped tar bytes produced by
// [writeTarballStream]) into an [io.ReadCloser] yielding the inner
// `<agentID>.sqlite` body. The caller's `closeUnderlying` callback runs
// after the gzip reader is closed so backend-specific cleanup (file
// handle, minio.Object, etc.) lives outside this helper.
//
// Returns (reader, nil) on success, or (nil, error) if the gzip header is
// invalid or the tar contains no entries — both are wrapped with
// [ErrInvalidURI] so callers can surface a single sentinel.
func openTarballStream(src io.ReadCloser) (io.ReadCloser, error) {
	gz, err := gzip.NewReader(src)
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("archivestore: open gzip: %w: %w", err, ErrInvalidURI)
	}
	tr := tar.NewReader(gz)
	if _, err := tr.Next(); err != nil {
		_ = gz.Close()
		_ = src.Close()
		return nil, fmt.Errorf("archivestore: read tar header: %w: %w", err, ErrInvalidURI)
	}
	return &tarballReader{tar: tr, gz: gz, src: src}, nil
}

// tarballReader is the shared `io.ReadCloser` returned by
// [openTarballStream]. It mirrors the per-LocalFS [snapshotReader] but is
// generic over the underlying source — `src` is closed last so backends
// can pass an `*os.File`, a `*minio.Object`, or any other
// [io.ReadCloser] without wiring custom close logic.
type tarballReader struct {
	tar *tar.Reader
	gz  *gzip.Reader
	src io.ReadCloser
}

// Read forwards to the tar reader, which itself pulls from the gzip
// stream. EOF on the tar entry signals the end of the snapshot bytes.
func (r *tarballReader) Read(p []byte) (int, error) { return r.tar.Read(p) }

// Close closes the gzip reader then the underlying source. The gzip
// error wins when both fail because the gzip layer is logically "closer
// to" the data the caller saw.
func (r *tarballReader) Close() error {
	gerr := r.gz.Close()
	serr := r.src.Close()
	if gerr != nil {
		return gerr
	}
	return serr
}
