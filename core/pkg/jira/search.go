package jira

import (
	"context"
	"fmt"
)

// SearchOptions configures a [Client.Search] call beyond the
// mandatory JQL string. The zero value is valid — all knobs default
// to Atlassian's documented defaults (Atlassian-supplied `fields`
// surface, server-default page size, no cursor → first page).
type SearchOptions struct {
	// Fields restricts the per-issue field surface in the response
	// (e.g. `[]string{"summary", "status"}`). Empty / nil requests
	// the Atlassian default `*navigable` set.
	Fields []string

	// MaxResults caps the number of issues returned per page. Zero
	// uses Atlassian's server default (currently 50). Atlassian
	// may return fewer than requested; iterate via NextPageToken
	// until [SearchResult.IsLast] is true.
	MaxResults int

	// PageToken is the cursor returned from a previous
	// [Client.Search] call's [SearchResult.NextPageToken]. Empty
	// requests the first page.
	PageToken string
}

// Search runs a JQL query against `/rest/api/3/search/jql` and
// returns one page of results. JQL syntax errors surface as wrapped
// [ErrInvalidJQL]; auth failures as [ErrInvalidAuth]; rate-limit
// failures as [ErrRateLimited].
//
// `jql` is rejected synchronously when empty.
//
// Pagination is cursor-based: callers iterate by passing
// [SearchResult.NextPageToken] back as [SearchOptions.PageToken]
// until [SearchResult.IsLast] is true. Atlassian's cursor pagination
// makes total counts unavailable from this endpoint per the 2024
// deprecation; callers needing a count drive the separate
// `/search/approximate-count` endpoint (out of M8.1 scope).
func (c *Client) Search(ctx context.Context, jql string, opts SearchOptions) (SearchResult, error) {
	if err := errMustNotBeEmpty("Search: jql", jql); err != nil {
		return SearchResult{}, err
	}

	body := map[string]any{
		"jql": jql,
	}
	if len(opts.Fields) > 0 {
		body["fields"] = opts.Fields
	}
	if opts.MaxResults > 0 {
		body["maxResults"] = opts.MaxResults
	}
	if opts.PageToken != "" {
		body["nextPageToken"] = opts.PageToken
	}

	var wire searchWire
	err := c.do(ctx, doParams{
		method: "POST",
		path:   "/rest/api/3/search/jql",
		body:   body,
		dst:    &wire,
		kind:   endpointSearch,
	})
	if err != nil {
		return SearchResult{}, fmt.Errorf("jira: Search: %w", err)
	}

	out := SearchResult{
		Issues:        make([]Issue, 0, len(wire.Issues)),
		NextPageToken: wire.NextPageToken,
		IsLast:        wire.IsLast,
	}
	for _, w := range wire.Issues {
		out.Issues = append(out.Issues, w.toIssue())
	}
	// Adapter-side guard against a server contract violation: an
	// `isLast=false` page MUST carry a non-empty NextPageToken,
	// otherwise a naive caller-side `for !res.IsLast` loop infinite-
	// loops on the same first page. Surface as [ErrInvalidArgs] so
	// the caller treats it as a programmer-detectable bug rather
	// than retrying the same cursor.
	if !out.IsLast && out.NextPageToken == "" {
		return SearchResult{}, fmt.Errorf("jira: Search: server contract violation: isLast=false but nextPageToken is empty: %w", ErrInvalidArgs)
	}
	return out, nil
}

// searchWire is the JSON shape of the `/search/jql` response. The
// `issues` array holds [issueWire] elements decoded by the shared
// [issueWire.toIssue] projector.
type searchWire struct {
	IsLast        bool        `json:"isLast"`
	Issues        []issueWire `json:"issues"`
	NextPageToken string      `json:"nextPageToken"`
}
