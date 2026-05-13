package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// maxPRBodyBytes is the upper bound on [CreatePullRequestOptions.Body].
// GitHub itself rejects payloads larger than ~64 KiB with 422;
// the pre-flight cap saves the round-trip on an obviously-too-
// large body. Iter-1 m3 fix (reviewer A).
const maxPRBodyBytes = 65535

// CreatePullRequestOptions is the [Client.CreatePullRequest] input.
type CreatePullRequestOptions struct {
	// Owner / Repo identify the target repo.
	Owner RepoOwner
	Repo  RepoName

	// Title is the PR title.
	Title string

	// Body is the PR description (markdown).
	Body string

	// Head is the branch the PR proposes — the name of the
	// just-created share branch (NOT prefixed with `refs/`). For
	// cross-fork PRs the form is `username:branch`; the M9.6 share
	// flow always opens in the target repo so the simple form
	// suffices.
	Head string

	// Base is the branch the PR targets for merge (typically
	// `main`).
	Base string
}

// CreatePullRequestResult carries the response — the new PR number
// and its HTML URL (for the operator's audit + Slack notification).
type CreatePullRequestResult struct {
	Number  int
	HTMLURL string
	NodeID  string
}

// CreatePullRequest opens a new PR on a repo via
// `POST /repos/{owner}/{repo}/pulls`. Used by the M9.6 share flow
// after [Client.CreateOrUpdateFile] has staged the changes onto a
// fresh share branch.
//
// Validation (pre-flight): non-empty owner / repo / title / head /
// base. Head and base are branch names (no `refs/` prefix); empty
// body is permitted (GitHub's UI accepts it).
//
// Failure modes:
//
//   - Pre-flight invalid → wrapped [ErrInvalidArgs].
//   - 404 → wrapped [ErrRepoNotFound].
//   - 401 → wrapped [ErrInvalidAuth].
//   - 422 (no diff between head and base; branch absent; PR
//     already open from head→base) → [*APIError] with Status=422.
//   - 429 / 403-with-remaining=0 → wrapped [ErrRateLimited].
func (c *Client) CreatePullRequest(ctx context.Context, opts CreatePullRequestOptions) (CreatePullRequestResult, error) {
	if err := validateOwner(opts.Owner); err != nil {
		return CreatePullRequestResult{}, err
	}
	if err := validateRepo(opts.Repo); err != nil {
		return CreatePullRequestResult{}, err
	}
	if strings.TrimSpace(opts.Title) == "" {
		return CreatePullRequestResult{}, fmt.Errorf("%w: empty title", ErrInvalidArgs)
	}
	if len(opts.Title) > 256 {
		return CreatePullRequestResult{}, fmt.Errorf("%w: title %d bytes (max 256)", ErrInvalidArgs, len(opts.Title))
	}
	// Iter-1 m3 fix (reviewer A): cap Body. GitHub itself caps at
	// ~64 KiB and rejects with 422; pre-flighting saves a round
	// trip on an obviously-too-large payload.
	if len(opts.Body) > maxPRBodyBytes {
		return CreatePullRequestResult{}, fmt.Errorf("%w: body %d bytes (max %d)", ErrInvalidArgs, len(opts.Body), maxPRBodyBytes)
	}
	if strings.TrimSpace(opts.Head) == "" {
		return CreatePullRequestResult{}, fmt.Errorf("%w: empty head", ErrInvalidArgs)
	}
	if strings.TrimSpace(opts.Base) == "" {
		return CreatePullRequestResult{}, fmt.Errorf("%w: empty base", ErrInvalidArgs)
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(string(opts.Owner)), url.PathEscape(string(opts.Repo)))
	body := createPullRequestRequest{
		Title: opts.Title,
		Body:  opts.Body,
		Head:  opts.Head,
		Base:  opts.Base,
	}
	var wire createPullRequestResponse
	if err := c.do(ctx, doParams{
		method:   "POST",
		path:     endpoint,
		body:     body,
		dst:      &wire,
		kind:     endpointRepo,
		expected: map[int]bool{201: true},
	}); err != nil {
		return CreatePullRequestResult{}, err
	}
	return CreatePullRequestResult(wire), nil
}

type createPullRequestRequest struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

type createPullRequestResponse struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	NodeID  string `json:"node_id"`
}
