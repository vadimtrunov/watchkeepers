package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GetIssue reads a single issue by [IssueKey]. The optional `fields`
// argument restricts the Atlassian response to a subset of fields
// (e.g. `[]string{"summary", "status"}`). An empty / nil `fields`
// requests the Atlassian default surface (`*navigable`); the response
// will populate every canonical accessor the issue has.
//
// The Atlassian endpoint is `GET /rest/api/3/issue/{key}`. A 404
// surfaces as wrapped [ErrIssueNotFound]; a 401 / 403 as
// [ErrInvalidAuth]; a 429 as [ErrRateLimited]; other failures as the
// raw [*APIError].
//
// `key` is rejected synchronously when empty.
func (c *Client) GetIssue(ctx context.Context, key IssueKey, fields []string) (Issue, error) {
	if err := validateIssueKey(key); err != nil {
		return Issue{}, fmt.Errorf("jira: GetIssue: %w", err)
	}

	q := make(map[string][]string)
	if len(fields) > 0 {
		q["fields"] = []string{strings.Join(fields, ",")}
	}

	var wire issueWire
	err := c.do(ctx, doParams{
		method: "GET",
		path:   "/rest/api/3/issue/" + string(key),
		query:  q,
		dst:    &wire,
		kind:   endpointIssue,
	})
	if err != nil {
		return Issue{}, fmt.Errorf("jira: GetIssue %s: %w", key, err)
	}
	return wire.toIssue(), nil
}

// issueWire is the JSON shape Atlassian returns for a single issue
// (both the standalone `/issue/{key}` endpoint and the search
// endpoint's `issues[]` element). The decoder lives on this type so
// search.go and get_issue.go share one canonical projection.
type issueWire struct {
	ID     json.RawMessage            `json:"id"`
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// toIssue projects the wire shape onto the public [Issue] surface.
// Failures to parse a sub-field leave the corresponding accessor
// empty / zero; the raw `Fields` map is preserved verbatim so callers
// driving custom-field extraction never lose fidelity.
func (w issueWire) toIssue() Issue {
	out := Issue{
		Key:    IssueKey(w.Key),
		ID:     jsonNumberOrString(w.ID),
		Fields: w.Fields,
	}
	if raw, ok := w.Fields["summary"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out.Summary = s
		}
	}
	if raw, ok := w.Fields["status"]; ok {
		var st struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &st); err == nil {
			out.Status = st.Name
		}
	}
	if raw, ok := w.Fields["assignee"]; ok && !isJSONNull(raw) {
		var u struct {
			AccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(raw, &u); err == nil {
			out.AssigneeID = u.AccountID
		}
	}
	if raw, ok := w.Fields["reporter"]; ok && !isJSONNull(raw) {
		var u struct {
			AccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(raw, &u); err == nil {
			out.ReporterID = u.AccountID
		}
	}
	if raw, ok := w.Fields["created"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out.Created = parseTime(s)
		}
	}
	if raw, ok := w.Fields["updated"]; ok {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out.Updated = parseTime(s)
		}
	}
	return out
}

// isJSONNull reports whether the raw payload is the literal `null`.
// Atlassian uses null for optional embedded objects (e.g. assignee on
// an unassigned issue); the canonical accessors collapse null and
// "absent" to the same empty-string state.
func isJSONNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// jsonNumberOrString extracts either a JSON string OR a JSON number-
// stringified value from a json.RawMessage. Atlassian's `id` field is
// a string in v3 but historically may surface as a number on edge
// endpoints; this helper tolerates both. Returns "" on any decode
// failure.
func jsonNumberOrString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}
