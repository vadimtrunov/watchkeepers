package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// CreateRefOptions is the [Client.CreateRef] input.
type CreateRefOptions struct {
	// Owner / Repo identify the target repo.
	Owner RepoOwner
	Repo  RepoName

	// Ref is the full ref path the new ref will live at, INCLUDING
	// the `refs/` prefix (e.g. `refs/heads/share-promotion-1234`).
	// GitHub's create-ref endpoint requires the full prefixed form
	// — distinct from [Client.GetRef] which takes the prefix-less
	// relative form. Both shapes documented at
	// https://docs.github.com/en/rest/git/refs.
	Ref string

	// SHA is the 40-hex commit SHA the new ref will point at.
	// Typically obtained from [Client.GetRef] on the base branch.
	SHA string
}

// CreateRefResult carries the resolved [Client.CreateRef] response.
type CreateRefResult struct {
	// SHA is the commit SHA the new ref points at (echoed back by
	// GitHub from the request).
	SHA string

	// Ref is the canonical ref path the new ref lives at, echoed
	// back by GitHub. Always equal to [CreateRefOptions.Ref] on
	// success.
	Ref string
}

// CreateRef creates a new git ref on a repo via
// `POST /repos/{owner}/{repo}/git/refs`. Used by the M9.6 share
// flow to create a feature branch from a known base SHA.
//
// Validation (pre-flight): non-empty owner / repo; ref begins with
// `refs/`; sha is a 40-char lower-hex string.
//
// Failure modes:
//
//   - Pre-flight invalid → wrapped [ErrInvalidArgs].
//   - 404 → wrapped [ErrRepoNotFound].
//   - 401 → wrapped [ErrInvalidAuth].
//   - 422 (ref already exists, sha invalid) → [*APIError] with
//     Status=422.
//   - 429 / 403-with-remaining=0 → wrapped [ErrRateLimited].
func (c *Client) CreateRef(ctx context.Context, opts CreateRefOptions) (CreateRefResult, error) {
	if err := validateOwner(opts.Owner); err != nil {
		return CreateRefResult{}, err
	}
	if err := validateRepo(opts.Repo); err != nil {
		return CreateRefResult{}, err
	}
	if err := validateRefFull(opts.Ref); err != nil {
		return CreateRefResult{}, err
	}
	if err := validateSHA(opts.SHA); err != nil {
		return CreateRefResult{}, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/git/refs", url.PathEscape(string(opts.Owner)), url.PathEscape(string(opts.Repo)))
	body := createRefRequest{Ref: opts.Ref, SHA: opts.SHA}
	var wire refWire
	if err := c.do(ctx, doParams{
		method:   "POST",
		path:     endpoint,
		body:     body,
		dst:      &wire,
		kind:     endpointRepo,
		expected: map[int]bool{201: true},
	}); err != nil {
		return CreateRefResult{}, err
	}
	return CreateRefResult{SHA: wire.Object.SHA, Ref: wire.Ref}, nil
}

type createRefRequest struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// validateRefFull refuses an empty / over-bound / unsafe ref value.
// The full-form shape MUST begin with `refs/`.
//
// Iter-1 fix (reviewer A M3 + reviewer B M2): previously the
// validator was strictly weaker than [validateRefRelative] —
// accepted `..`, control chars, whitespace, `?`, `#`, `%`. Apply
// the same content-byte allowlist to defend the create-ref path
// for any future caller that bypasses the orchestrator's
// programmatic branch-name composer.
func validateRefFull(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty ref", ErrInvalidArgs)
	}
	if len(ref) > 256 {
		return fmt.Errorf("%w: ref %d bytes (max 256)", ErrInvalidArgs, len(ref))
	}
	if !startsWithRefsPrefix(ref) {
		return fmt.Errorf("%w: ref must begin with refs/ (full form)", ErrInvalidArgs)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("%w: ref contains '..'", ErrInvalidArgs)
	}
	if strings.HasSuffix(ref, "/") {
		return fmt.Errorf("%w: ref ends with '/'", ErrInvalidArgs)
	}
	if err := refContentBytesValid(ref); err != nil {
		return err
	}
	return nil
}

func startsWithRefsPrefix(s string) bool {
	return len(s) >= 5 && s[:5] == "refs/"
}

// validateSHA enforces lower-hex 40-char shape. GitHub accepts
// 7-char abbreviations on some endpoints but not on git/refs;
// require the full hash here to fail-fast on a truncation.
func validateSHA(sha string) error {
	if len(sha) != 40 {
		return fmt.Errorf("%w: sha length=%d want 40", ErrInvalidArgs, len(sha))
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return fmt.Errorf("%w: sha non-lower-hex at byte %d", ErrInvalidArgs, i)
		}
	}
	return nil
}
