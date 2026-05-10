package jira

import (
	"encoding/json"
	"time"
)

// IssueKey is the human-readable, opaque-to-callers Jira issue
// identifier (e.g. `PROJ-123`). Treat as an opaque string — never
// parse the project prefix or the numeric tail; both are platform-
// defined and may evolve.
type IssueKey string

// CommentID is the platform-assigned identifier for a comment on an
// issue. Atlassian numbers comments globally; treat as opaque.
type CommentID string

// Issue is the value returned by [Client.GetIssue] and the elements of
// [SearchResult.Issues]. The shape captures the small set of fields
// every M8 caller needs strongly typed; everything else rides via the
// raw [Issue.Fields] map for caller-driven extraction.
//
// Field-resolution discipline:
//
//   - The four "canonical" fields ([Issue.Summary], [Issue.Status],
//     [Issue.AssigneeID], [Issue.ReporterID], [Issue.Created],
//     [Issue.Updated]) are extracted from the response payload BEFORE
//     [Issue.Fields] is populated. The raw map carries every field
//     Atlassian returned — including the ones the canonical accessors
//     already extracted — verbatim, so callers parsing custom fields
//     still see the canonical-field JSON they'd expect.
//   - Empty values (unassigned issue, no reporter recorded) leave the
//     corresponding string field empty rather than panicking. Callers
//     that need to distinguish "absent" from "empty string" inspect
//     [Issue.Fields].
type Issue struct {
	// Key is the platform-assigned issue key (e.g. `PROJ-123`).
	// Always populated on a successful read.
	Key IssueKey

	// ID is the platform-assigned numeric issue id as a string. Stable
	// across project renames; useful for downstream cross-references.
	ID string

	// Summary is the issue's short title. Empty when the caller did
	// not request `summary` in the field list (or Atlassian omitted
	// it for permission reasons).
	Summary string

	// Status is the issue's current workflow status name (e.g.
	// `In Progress`). Empty when not requested. NOTE: the raw payload
	// carries an embedded object with `id` and `name` — this surface
	// exposes the human-readable name only. Callers needing the id
	// inspect [Issue.Fields].
	Status string

	// AssigneeID is the Atlassian Cloud `accountId` of the current
	// assignee. Empty for unassigned issues OR when not requested.
	AssigneeID string

	// ReporterID is the Atlassian Cloud `accountId` of the issue
	// reporter. Empty when not requested or when the reporter has
	// been deleted.
	ReporterID string

	// Created is the server-reported issue creation time, UTC. Zero
	// when not requested OR when Atlassian returned a timestamp the
	// adapter could not parse (a strip-and-retry fractional fallback
	// covers most variants, but truly non-canonical formats collapse
	// to zero). M8.x callers computing "issue is overdue" against
	// this field MUST guard against the zero value (e.g. via
	// `!Created.IsZero() && time.Since(Created) > threshold`),
	// otherwise every parse-failure issue mis-classifies as overdue.
	Created time.Time

	// Updated is the server-reported last-update time, UTC. Zero
	// shares the same three causes as [Issue.Created.Zero] — not
	// requested, server-omitted, OR unparseable. Same overdue-
	// computation guard applies.
	Updated time.Time

	// Fields is the raw `fields` object from the Atlassian response,
	// preserved verbatim so callers driving custom-field extraction
	// can do so without a second network round-trip. Nil when the
	// response had no `fields` key.
	Fields map[string]json.RawMessage
}

// Comment is the value returned by [Client.AddComment]. The platform-
// canonical body shape is Atlassian Document Format (ADF, REST API
// v3); the [Comment.Body] surface carries the plain-text projection
// (text leaves joined with newlines per paragraph). Round-trips
// through complex ADF blocks (mentions, code blocks, tables) are
// LOSSY by design — the M8.1 adapter contract is text in, text out.
// Callers needing fidelity inspect [Comment.RawBody].
type Comment struct {
	// ID is the platform-assigned comment id. Treat as opaque.
	ID CommentID

	// AuthorID is the Atlassian Cloud `accountId` of the comment
	// author. Always populated (Atlassian never returns anonymous
	// comments via REST).
	AuthorID string

	// Body is the plain-text projection of the comment's ADF
	// payload. Paragraphs are joined with a single `\n`; nodes the
	// projector does not understand (mentions, code blocks, tables,
	// images) drop their contents.
	Body string

	// RawBody is the unmodified ADF JSON the platform returned. Nil
	// when Atlassian responded without a body (rare). Callers needing
	// fidelity decode this themselves.
	RawBody json.RawMessage

	// Created is the server-reported comment creation time, UTC.
	Created time.Time
}

// SearchResult is the value returned by [Client.Search]. Atlassian
// Cloud's modern `/rest/api/3/search/jql` endpoint paginates with a
// cursor (NextPageToken) and a flag (IsLast) — total counts are no
// longer returned per the 2024 deprecation. Callers iterate by
// passing [SearchResult.NextPageToken] back to [Client.Search] until
// [SearchResult.IsLast] is true.
type SearchResult struct {
	// Issues is the page of issues returned for this call. May be
	// empty even when [SearchResult.IsLast] is false (Atlassian may
	// return a cursor-only page on internal pagination boundaries).
	Issues []Issue

	// NextPageToken is the opaque cursor to pass back on the next
	// [Client.Search] call. Empty when [SearchResult.IsLast] is true.
	NextPageToken string

	// IsLast reports whether this is the final page of results.
	// Callers terminate iteration when true.
	IsLast bool
}
