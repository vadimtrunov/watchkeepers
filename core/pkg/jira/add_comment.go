package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AddComment appends a comment to the issue identified by `key`. The
// caller supplies plain text; the adapter wraps it in a minimal
// Atlassian Document Format (ADF) document on the wire (one paragraph
// per `\n`-delimited line of input, paragraphs containing one text
// leaf each; empty lines render as empty paragraphs).
//
// The Atlassian endpoint is `POST /rest/api/3/issue/{key}/comment`.
// On success the returned [Comment] carries:
//
//   - the platform-assigned [CommentID];
//   - the author's `accountId`;
//   - the plain-text projection of the server-stored ADF in
//     [Comment.Body] (paragraphs joined with `\n`, complex blocks
//     best-effort);
//   - the unmodified server-stored ADF in [Comment.RawBody] for
//     callers needing fidelity;
//   - the server-reported creation time (UTC).
//
// `key` and `body` are rejected synchronously when empty. A 404
// surfaces as wrapped [ErrIssueNotFound]; a 401 / 403 as
// [ErrInvalidAuth]; a 429 as [ErrRateLimited]; other failures as the
// raw [*APIError].
func (c *Client) AddComment(ctx context.Context, key IssueKey, body string) (Comment, error) {
	if err := validateIssueKey(key); err != nil {
		return Comment{}, fmt.Errorf("jira: AddComment: %w", err)
	}
	if err := errMustNotBeEmpty("AddComment: body", body); err != nil {
		return Comment{}, err
	}

	requestBody := map[string]any{
		"body": adfWrapText(body),
	}

	var wire commentWire
	err := c.do(ctx, doParams{
		method: "POST",
		path:   "/rest/api/3/issue/" + string(key) + "/comment",
		body:   requestBody,
		dst:    &wire,
		kind:   endpointIssue,
	})
	if err != nil {
		return Comment{}, fmt.Errorf("jira: AddComment %s: %w", key, err)
	}
	return wire.toComment(), nil
}

// commentWire is the JSON shape Atlassian returns for a comment.
// Both the POST .../comment response and the embedded comments inside
// a fetched issue use this shape (minus the top-level `self` URI we
// ignore).
type commentWire struct {
	ID     string `json:"id"`
	Author struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
	Body    json.RawMessage `json:"body"`
	Created string          `json:"created"`
}

func (w commentWire) toComment() Comment {
	return Comment{
		ID:       CommentID(w.ID),
		AuthorID: w.Author.AccountID,
		Body:     adfExtractText(w.Body),
		RawBody:  w.Body,
		Created:  parseTime(w.Created),
	}
}

// adfWrapText converts a plain-text body into a minimal ADF document.
// `\n` splits paragraphs (one paragraph per line). Empty lines
// render as empty paragraphs — they are valid ADF and preserve the
// caller's blank-line intent on the server side.
//
// The wrapper is deliberately tiny: text leaves only, no marks, no
// hardBreak nodes. Callers needing rich formatting (mentions,
// inline-code, links) drive the Atlassian REST endpoints directly —
// out of M8.1 scope.
func adfWrapText(plain string) map[string]any {
	lines := strings.Split(plain, "\n")
	paragraphs := make([]any, 0, len(lines))
	for _, line := range lines {
		para := map[string]any{"type": "paragraph"}
		if line != "" {
			para["content"] = []any{
				map[string]any{"type": "text", "text": line},
			}
		}
		paragraphs = append(paragraphs, para)
	}
	return map[string]any{
		"version": 1,
		"type":    "doc",
		"content": paragraphs,
	}
}

// adfExtractText projects an ADF document onto plain text. The
// projector recognises:
//
//   - `doc` — top-level container; child block joins with `\n`.
//   - `paragraph`, `heading`, `bulletList`, `orderedList`,
//     `listItem`, `blockquote`, `codeBlock` — block containers;
//     children concatenated, contributing one line each at the doc
//     level.
//   - `text` — leaf carrying [adfNode.Text].
//   - `hardBreak` — emits `\n`.
//   - `mention` — emits `@<displayName>` when [adfNode.Attrs.text] is
//     present; otherwise falls back to `displayName` then to "".
//
// Unknown node types walk their children with no separator. The
// projection NEVER fails — it returns "" for an unparseable input
// rather than propagating a decode error (the M8.1 contract is
// best-effort plain-text extraction; complex blocks are documented
// lossy).
func adfExtractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var node adfNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	return node.text()
}

// adfNode is the minimal ADF node shape the projector consumes.
// Atlassian's full ADF spec carries marks, attrs, and platform-
// specific extensions; the projector ignores everything outside the
// shape captured here.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []adfNode      `json:"content,omitempty"`
}

// text projects this subtree onto plain text per the rules
// documented on [adfExtractText].
func (n adfNode) text() string {
	switch n.Type {
	case "text":
		return n.Text
	case "hardBreak":
		return "\n"
	case "mention":
		if n.Attrs == nil {
			return ""
		}
		if v, ok := n.Attrs["text"].(string); ok && v != "" {
			return v
		}
		if v, ok := n.Attrs["displayName"].(string); ok && v != "" {
			return "@" + v
		}
		return ""
	case "doc", "bulletList", "orderedList":
		parts := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			parts = append(parts, c.text())
		}
		return strings.Join(parts, "\n")
	default:
		var b strings.Builder
		for _, c := range n.Content {
			b.WriteString(c.text())
		}
		return b.String()
	}
}
