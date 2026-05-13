package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// CreateOrUpdateFileOptions is the [Client.CreateOrUpdateFile]
// input.
type CreateOrUpdateFileOptions struct {
	// Owner / Repo identify the target repo.
	Owner RepoOwner
	Repo  RepoName

	// Path is the in-repo path the file lives at (forward-slash
	// separated, no leading slash, no `..` segments).
	Path string

	// Content is the file contents — bytes, NOT base64. The method
	// base64-encodes before sending per GitHub's API contract.
	Content []byte

	// Message is the commit message for the create/update commit.
	Message string

	// Branch is the branch the commit lands on. Required for the
	// M9.6 flow (we always target a freshly-created share branch);
	// passing an empty branch is refused at validation time.
	Branch string

	// SHA, when non-empty, is the blob SHA of the file as it
	// currently exists at `Path` on `Branch`. Required for UPDATES,
	// MUST be empty for CREATES. The M9.6 share flow creates files
	// on a fresh branch so SHA is empty.
	SHA string
}

// CreateOrUpdateFileResult carries the response — the new file SHA
// and the commit SHA that landed the change.
type CreateOrUpdateFileResult struct {
	FileSHA   string
	CommitSHA string
}

// CreateOrUpdateFile creates (or updates) a single file in a repo
// via `PUT /repos/{owner}/{repo}/contents/{path}`. Each call
// produces ONE commit on the target branch — for a multi-file
// upload, the caller invokes once per file. The M9.6 share flow
// trades single-commit atomicity for a simpler API surface (Git
// Data API blob/tree/commit chain would land all files in one
// commit but adds 3 round-trips per share).
//
// Validation (pre-flight): non-empty owner / repo / path / message
// / branch; path forward-slash-separated; no `..` / leading slash.
// Content size is capped at 1 MiB by GitHub itself (the endpoint
// rejects larger payloads with HTTP 413); we don't pre-flight
// because the limit is documented per-endpoint and may change.
//
// Failure modes:
//
//   - Pre-flight invalid → wrapped [ErrInvalidArgs].
//   - 404 → wrapped [ErrRepoNotFound].
//   - 401 → wrapped [ErrInvalidAuth].
//   - 422 (sha-mismatch on update, malformed branch) → [*APIError].
//   - 429 / 403-with-remaining=0 → wrapped [ErrRateLimited].
func (c *Client) CreateOrUpdateFile(ctx context.Context, opts CreateOrUpdateFileOptions) (CreateOrUpdateFileResult, error) {
	if err := validateOwner(opts.Owner); err != nil {
		return CreateOrUpdateFileResult{}, err
	}
	if err := validateRepo(opts.Repo); err != nil {
		return CreateOrUpdateFileResult{}, err
	}
	if err := validateContentPath(opts.Path); err != nil {
		return CreateOrUpdateFileResult{}, err
	}
	if strings.TrimSpace(opts.Message) == "" {
		return CreateOrUpdateFileResult{}, fmt.Errorf("%w: empty commit message", ErrInvalidArgs)
	}
	if strings.TrimSpace(opts.Branch) == "" {
		return CreateOrUpdateFileResult{}, fmt.Errorf("%w: empty branch", ErrInvalidArgs)
	}
	endpoint := "/repos/" + url.PathEscape(string(opts.Owner)) + "/" + url.PathEscape(string(opts.Repo)) + "/contents/" + escapeContentPath(opts.Path)
	body := createOrUpdateFileRequest{
		Message: opts.Message,
		Content: base64.StdEncoding.EncodeToString(opts.Content),
		Branch:  opts.Branch,
	}
	if opts.SHA != "" {
		if err := validateSHA(opts.SHA); err != nil {
			return CreateOrUpdateFileResult{}, err
		}
		body.SHA = opts.SHA
	}
	var wire createOrUpdateFileResponse
	if err := c.do(ctx, doParams{
		method:   "PUT",
		path:     endpoint,
		body:     body,
		dst:      &wire,
		kind:     endpointRepo,
		expected: map[int]bool{200: true, 201: true},
	}); err != nil {
		return CreateOrUpdateFileResult{}, err
	}
	return CreateOrUpdateFileResult{FileSHA: wire.Content.SHA, CommitSHA: wire.Commit.SHA}, nil
}

type createOrUpdateFileRequest struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha,omitempty"`
}

type createOrUpdateFileResponse struct {
	Content struct {
		SHA  string `json:"sha"`
		Path string `json:"path"`
	} `json:"content"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// validateContentPath refuses an empty / over-bound / unsafe path
// value. Forward-slash separated; no leading slash; no `..` segments.
//
// Iter-1 fixes (reviewer A m2 + m11): refuse ASCII control bytes
// (including NUL, TAB, NL, CR, DEL) at the byte level — a path
// like `"src/\x00/foo"` or `"src/\n/file.go"` is never legitimate
// and lands either as a corrupt URL or as a confusing server-side
// 404. Unicode confusables (zero-width chars, BOM) are accepted
// at the byte level — GitHub rejects them server-side and the
// blast-radius is operator-supplied path, not credential.
func validateContentPath(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidArgs)
	}
	if len(p) > 1024 {
		return fmt.Errorf("%w: path %d bytes (max 1024)", ErrInvalidArgs, len(p))
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: path begins with '/'", ErrInvalidArgs)
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7F {
			return fmt.Errorf("%w: path control byte 0x%02x at %d", ErrInvalidArgs, c, i)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return fmt.Errorf("%w: empty path segment", ErrInvalidArgs)
		}
		if seg == ".." || seg == "." {
			return fmt.Errorf("%w: path segment %q", ErrInvalidArgs, seg)
		}
	}
	return nil
}

// escapeContentPath URL-encodes each segment of the in-repo path
// while preserving the `/` separator. Mirrors GitHub's Contents API
// path-segment-encoding convention.
func escapeContentPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
