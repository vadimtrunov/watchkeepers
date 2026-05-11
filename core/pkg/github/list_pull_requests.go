package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// ListPullRequestsOptions configures a [Client.ListPullRequests] call.
// The zero value is valid — all knobs default to GitHub's documented
// defaults (`state=open`, server-default page size, `sort=created`,
// `direction=desc`, first page).
type ListPullRequestsOptions struct {
	// State filters the lifecycle state — `open`, `closed`, `all`.
	// Empty uses GitHub's documented default `open`.
	State string

	// PerPage caps the number of PRs returned per page. Zero uses
	// GitHub's documented default (30). GitHub caps the max at 100;
	// values above 100 are silently clamped by GitHub.
	PerPage int

	// Page selects the 1-indexed page number to fetch. Zero requests
	// the first page (no `page=` query parameter — GitHub treats
	// absent + 1 identically).
	Page int

	// Sort orders the result. GitHub-documented values: `created`,
	// `updated`, `popularity`, `long-running`. Empty uses GitHub's
	// default `created`.
	Sort string

	// Direction is `asc` or `desc`. Empty uses GitHub's default `desc`.
	Direction string
}

// ListPullRequests fetches one page of pull requests on the given
// repository via `GET /repos/{owner}/{repo}/pulls`. Auth failures
// surface as wrapped [ErrInvalidAuth]; missing-repo failures as
// [ErrRepoNotFound]; rate-limit failures as [ErrRateLimited].
//
// `owner` and `repo` are rejected synchronously when empty or
// malformed (per [validateOwner] / [validateRepo]).
//
// Pagination is page-number-based via GitHub's Link header (parsed
// into [ListPullRequestsResult.NextPage]). Callers iterate by passing
// the returned NextPage back as [ListPullRequestsOptions.Page] until
// NextPage is zero.
func (c *Client) ListPullRequests(
	ctx context.Context,
	owner RepoOwner,
	repo RepoName,
	opts ListPullRequestsOptions,
) (ListPullRequestsResult, error) {
	if err := validateOwner(owner); err != nil {
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests: %w", err)
	}
	if err := validateRepo(repo); err != nil {
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests: %w", err)
	}
	// Iter-1 codex Minor: reject negative Page/PerPage explicitly
	// rather than silently coercing to the GitHub default. The
	// handler passes constants today so this is invariance not
	// correctness, but the adapter is a public package surface and
	// downstream consumers (M9 Linear / Notion adapters cribbing
	// this shape) should not pay for a hidden coercion.
	if opts.Page < 0 {
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests: %w: Page must be ≥ 0", ErrInvalidArgs)
	}
	if opts.PerPage < 0 {
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests: %w: PerPage must be ≥ 0", ErrInvalidArgs)
	}

	q := make(map[string][]string)
	if opts.State != "" {
		q["state"] = []string{opts.State}
	}
	if opts.PerPage > 0 {
		q["per_page"] = []string{strconv.Itoa(opts.PerPage)}
	}
	if opts.Page > 0 {
		q["page"] = []string{strconv.Itoa(opts.Page)}
	}
	if opts.Sort != "" {
		q["sort"] = []string{opts.Sort}
	}
	if opts.Direction != "" {
		q["direction"] = []string{opts.Direction}
	}

	var wire []pullRequestWire
	var nextPage int
	var linkParseErr error
	err := c.do(ctx, doParams{
		method: "GET",
		path:   "/repos/" + string(owner) + "/" + string(repo) + "/pulls",
		query:  q,
		dst:    &wire,
		kind:   endpointRepo,
		headerFn: func(h http.Header) {
			nextPage, linkParseErr = parseLinkHeaderNextPage(h.Get("Link"))
		},
	})
	if err != nil {
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests %s/%s: %w", owner, repo, err)
	}
	if linkParseErr != nil {
		// Iter-1 codex Major: surface Link-header drift loudly rather
		// than silently truncating the scan. Without this guard a
		// rel="next" with an unparseable page= would set nextPage=0
		// and the handler would treat the current page as the
		// terminus. The caller (find_stale_prs handler) wraps + the
		// runtime's M5.6.b reflector ingests; an obvious diagnostic
		// beats a silent data-loss bug.
		return ListPullRequestsResult{}, fmt.Errorf("github: ListPullRequests %s/%s: %w", owner, repo, linkParseErr)
	}

	out := ListPullRequestsResult{
		PullRequests: make([]PullRequest, 0, len(wire)),
		NextPage:     nextPage,
	}
	for _, w := range wire {
		out.PullRequests = append(out.PullRequests, w.toPullRequest())
	}
	return out, nil
}

// pullRequestWire is the JSON shape GitHub returns for a single pull
// request inside the `/pulls` array. Fields not consumed by the
// canonical [PullRequest] surface are intentionally omitted to keep
// the decoder small; callers needing fidelity can extend the wire
// shape per-need.
type pullRequestWire struct {
	Number             int             `json:"number"`
	Title              string          `json:"title"`
	State              string          `json:"state"`
	HTMLURL            string          `json:"html_url"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
	User               json.RawMessage `json:"user"`
	RequestedReviewers json.RawMessage `json:"requested_reviewers"`
}

// toPullRequest projects the wire shape onto the public [PullRequest]
// surface. Failures to parse a sub-field leave the corresponding
// accessor empty / zero; the canonical Number / Title / State / HTMLURL
// fields are passed through verbatim (GitHub never returns null on
// these for a PR row in the list response).
func (w pullRequestWire) toPullRequest() PullRequest {
	out := PullRequest{
		Number:    w.Number,
		Title:     w.Title,
		State:     w.State,
		HTMLURL:   w.HTMLURL,
		CreatedAt: parseTime(w.CreatedAt),
		UpdatedAt: parseTime(w.UpdatedAt),
	}
	if len(w.User) > 0 && !isJSONNull(w.User) {
		var u struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(w.User, &u); err == nil {
			out.AuthorLogin = u.Login
		}
	}
	if len(w.RequestedReviewers) > 0 && !isJSONNull(w.RequestedReviewers) {
		var reviewers []struct {
			Login string `json:"login"`
		}
		if err := json.Unmarshal(w.RequestedReviewers, &reviewers); err == nil {
			out.RequestedReviewers = make([]string, 0, len(reviewers))
			for _, r := range reviewers {
				if r.Login != "" {
					out.RequestedReviewers = append(out.RequestedReviewers, r.Login)
				}
			}
		}
	}
	return out
}

// isJSONNull reports whether the raw payload is the literal `null`.
// GitHub uses null for optional embedded objects (e.g. `user` on a
// PR whose author has been deleted); the canonical accessors collapse
// null and "absent" to the same empty-string / nil state.
func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}
