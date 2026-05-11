package github

import "time"

// RepoOwner is the GitHub owner login (user or organization handle)
// component of a `/repos/{owner}/{repo}` path. Validated synchronously
// against [ownerPattern] before any REST call.
type RepoOwner string

// RepoName is the GitHub repository name component of a
// `/repos/{owner}/{repo}` path. Validated synchronously against
// [repoPattern] before any REST call.
type RepoName string

// PullRequest is the value returned in the elements of
// [ListPullRequestsResult.PullRequests]. The shape captures the small
// set of fields every M8.2.d caller needs strongly typed — number,
// title, author login, state, html url, created/updated timestamps,
// and the requested-reviewer login list — sufficient to drive
// `find_stale_prs` filtering.
//
// Field-resolution discipline:
//
//   - The canonical fields below are extracted from the response
//     payload by [pullRequestWire.toPullRequest]. Empty values
//     (no requested reviewers, missing optional fields) leave the
//     corresponding accessor empty rather than panicking.
//   - Timestamps that fail to parse collapse to the zero value rather
//     than panicking; callers computing "PR is stale" against
//     [PullRequest.UpdatedAt] MUST guard against the zero value.
type PullRequest struct {
	// Number is the per-repository pull-request number (e.g. 42 for
	// `#42`). Always populated on a successful read.
	Number int

	// Title is the PR's short title. May be empty if GitHub returned
	// no title (rare; the field is required at PR-creation time).
	Title string

	// AuthorLogin is the GitHub login of the PR author. Empty when
	// GitHub returns a null `user` (rare, but possible for
	// PRs whose author has been deleted).
	AuthorLogin string

	// State is the PR's current lifecycle state (`open`, `closed`).
	// GitHub also distinguishes merged from closed via a separate
	// `merged` boolean; this surface does not expose it, but callers
	// driving custom filters can re-fetch the PR via [Client.GetPullRequest]
	// (out of M8.2.d scope) for the full shape.
	State string

	// HTMLURL is the web-UI URL of the PR (e.g.
	// `https://github.com/owner/repo/pull/42`). The agent surfaces
	// this as the clickable link in the daily briefing.
	HTMLURL string

	// CreatedAt is the server-reported PR-creation time, UTC. Zero
	// when GitHub returned a timestamp the adapter could not parse —
	// callers treat zero as "unknown / unset" rather than panicking.
	CreatedAt time.Time

	// UpdatedAt is the server-reported last-update time, UTC. Zero
	// shares the same parse-failure semantics as [PullRequest.CreatedAt].
	// M8.2.d's "PR is stale" computation MUST guard against the zero
	// value to avoid mis-classifying every parse-failure PR as stale.
	UpdatedAt time.Time

	// RequestedReviewers is the list of GitHub logins currently
	// requested as reviewers on this PR. Empty when no review has
	// been requested OR every requested reviewer has already submitted
	// a review (GitHub removes a login from this list once their
	// review is submitted). The M8.2.d `find_stale_prs` handler
	// filters on membership in this list.
	RequestedReviewers []string
}

// ListPullRequestsResult is the value returned by [Client.ListPullRequests].
// GitHub paginates `/repos/{owner}/{repo}/pulls` via the `Link`
// response header (rel="next" / rel="last" URLs). Callers iterate by
// passing [ListPullRequestsOptions.Page] back to [Client.ListPullRequests]
// until [ListPullRequestsResult.NextPage] is zero.
type ListPullRequestsResult struct {
	// PullRequests is the page of PRs returned for this call. May be
	// empty even when [ListPullRequestsResult.NextPage] is non-zero
	// (GitHub may return a partial page on internal boundaries).
	PullRequests []PullRequest

	// NextPage is the GitHub `Link` rel="next" page number parsed from
	// the response header. Zero when GitHub did not advertise a next
	// page — i.e. the current page is the last. Callers terminate
	// iteration when zero.
	NextPage int
}
