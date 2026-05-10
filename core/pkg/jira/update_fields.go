package jira

import (
	"context"
	"fmt"
)

// UpdateFields PUTs a partial-update payload to
// `/rest/api/3/issue/{key}`. The `fields` map contains Atlassian
// field IDs (`summary`, `description`, `labels`,
// `customfield_10001`, …) keyed onto the values Atlassian expects
// for that field type.
//
// Whitelist enforcement (synchronous, BEFORE the network):
//
//   - The configured [WithFieldWhitelist] set is consulted; ANY field
//     id in `fields` that is NOT in the whitelist causes the call to
//     return wrapped [ErrFieldNotWhitelisted] — the HTTP exchange is
//     never attempted.
//   - A nil / empty whitelist refuses ALL writes (fail-closed default).
//   - The whitelist is the transport-layer security boundary; the
//     M8.2 role authority matrix sits ON TOP of, not INSTEAD of, this
//     guard.
//
// Field-value shape:
//
//   - String fields (e.g. `summary`, `description`) accept Go
//     `string` values. `description` per Atlassian Cloud expects ADF;
//     callers writing prose into `description` are expected to supply
//     ADF themselves (M8.1 ships the [adfWrapText] helper as an
//     unexported convenience but does not currently expose ADF
//     conversion for arbitrary fields — only [Client.AddComment]
//     wraps automatically; this asymmetry is intentional pending
//     M8.2's tool-layer needs).
//   - Slice / map / scalar / null values are passed verbatim.
//
// `key` is rejected synchronously when empty; a nil / empty `fields`
// map is rejected as wrapped [ErrInvalidArgs].
//
// Success returns nil (Atlassian responds with HTTP 204 No Content).
// A 404 surfaces as wrapped [ErrIssueNotFound]; a 401 / 403 as
// [ErrInvalidAuth]; a 429 as [ErrRateLimited]; other failures as the
// raw [*APIError].
func (c *Client) UpdateFields(ctx context.Context, key IssueKey, fields map[string]any) error {
	if err := validateIssueKey(key); err != nil {
		return fmt.Errorf("jira: UpdateFields: %w", err)
	}
	if err := c.fieldWhitelistContains(fields); err != nil {
		return fmt.Errorf("jira: UpdateFields %s: %w", key, err)
	}

	body := map[string]any{
		"fields": fields,
	}

	err := c.do(ctx, doParams{
		method: "PUT",
		path:   "/rest/api/3/issue/" + string(key),
		body:   body,
		kind:   endpointIssue,
	})
	if err != nil {
		return fmt.Errorf("jira: UpdateFields %s: %w", key, err)
	}
	return nil
}
