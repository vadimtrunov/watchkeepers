package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// GetRefResult carries the resolved [Client.GetRef] response — just
// the commit SHA the named ref points at. GitHub's single-ref
// endpoint returns the full ref object; the caller only needs the
// SHA for the M9.6 share flow.
type GetRefResult struct {
	// SHA is the 40-hex commit SHA the ref points at.
	SHA string
}

// GetRef resolves the commit SHA of a single ref on a repo via
// `GET /repos/{owner}/{repo}/git/ref/{ref}`. `ref` is supplied
// WITHOUT the `refs/` prefix per GitHub's API contract — pass
// `heads/main`, not `refs/heads/main`. Mirrors the rest API's
// "ref-path-relative-to-refs/" shape; the documentation URL is
// https://docs.github.com/en/rest/git/refs#get-a-reference.
//
// Used by the M9.6 share flow to look up the base branch's tip SHA
// before creating a new feature branch from it.
//
// Validation (pre-flight): non-empty owner / repo / ref; owner /
// repo match the same allowlist as [Client.ListPullRequests];
// `ref` MUST NOT begin with `refs/` (the API endpoint includes the
// prefix in its path) and MUST NOT contain `..` or trailing `/`.
//
// Failure modes:
//
//   - Pre-flight invalid → wrapped [ErrInvalidArgs].
//   - 404 → wrapped [ErrRepoNotFound] (the docs use 404 for both
//     "repo absent" and "ref absent"; callers triage via
//     [APIError.Endpoint] when the distinction matters).
//   - 401 → wrapped [ErrInvalidAuth].
//   - 429 / 403-with-remaining=0 → wrapped [ErrRateLimited].
func (c *Client) GetRef(ctx context.Context, owner RepoOwner, repo RepoName, ref string) (GetRefResult, error) {
	if err := validateOwner(owner); err != nil {
		return GetRefResult{}, err
	}
	if err := validateRepo(repo); err != nil {
		return GetRefResult{}, err
	}
	if err := validateRefRelative(ref); err != nil {
		return GetRefResult{}, err
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/git/ref/%s", url.PathEscape(string(owner)), url.PathEscape(string(repo)), ref)
	var wire refWire
	if err := c.do(ctx, doParams{
		method:   "GET",
		path:     endpoint,
		dst:      &wire,
		kind:     endpointRepo,
		expected: map[int]bool{200: true},
	}); err != nil {
		return GetRefResult{}, err
	}
	if wire.Object.SHA == "" {
		return GetRefResult{}, fmt.Errorf("github: GetRef: empty SHA in response")
	}
	return GetRefResult{SHA: wire.Object.SHA}, nil
}

// refWire is the wire shape of a single-ref response. Only `object.sha`
// is consumed by [GetResult] / [CreateRef]; the rest is informational.
type refWire struct {
	Ref    string `json:"ref"`
	NodeID string `json:"node_id"`
	URL    string `json:"url"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"object"`
}

// validateRefRelative refuses an empty / over-bound / unsafe ref
// value. `ref` is the path-relative-to-`refs/` form per GitHub's
// API contract (e.g. `heads/main`, `tags/v1.2.0`, `pull/42/head`).
//
// Iter-1 fixes (reviewer A m4 + reviewer B m9): refuse whitespace,
// control chars, `?`, `#`, `%`, NUL — all of which would either
// corrupt the composed URL or surface as confusing server-side
// 404s. The positive shape after this is: non-empty, ≤256 bytes,
// no `refs/` prefix, no `..`, no leading/trailing `/`, no chars
// in [whitespace, control, ?, #, %, \0].
func validateRefRelative(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty ref", ErrInvalidArgs)
	}
	if len(ref) > 256 {
		return fmt.Errorf("%w: ref %d bytes (max 256)", ErrInvalidArgs, len(ref))
	}
	if strings.HasPrefix(ref, "refs/") {
		return fmt.Errorf("%w: ref must not begin with refs/ (use the relative form)", ErrInvalidArgs)
	}
	if err := refContentBytesValid(ref); err != nil {
		return err
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") {
		return fmt.Errorf("%w: ref begins/ends with '/'", ErrInvalidArgs)
	}
	return nil
}
