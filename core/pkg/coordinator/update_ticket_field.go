package coordinator

import (
	"context"
	"fmt"
	"maps"
	"regexp"

	"github.com/vadimtrunov/watchkeepers/core/pkg/jira"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// issueKeyPattern is the project-key + numeric-tail shape Atlassian
// documents for issue keys (e.g. `WK-42`, `PROJ-123`). Mirrors the
// internal regex on `core/pkg/jira/client.go::issueKeyPattern` —
// duplicated here rather than re-exported because the iter-1 PII
// finding required a Coordinator-side pre-validation BEFORE the
// adapter call so that a malformed key never reaches the M8.1 layer
// (which echoes the raw key with `%q` in its `validateIssueKey`
// error). Pre-validating here means the refusal text we surface
// never includes the raw value.
var issueKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]+-[1-9][0-9]*$`)

// UpdateTicketFieldName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/024_coordinator_manifest_seed.sql`. Callers
// use this const rather than the bare string so a future rename is a
// one-line change here.
const UpdateTicketFieldName = "update_ticket_field"

// AssigneeFieldKey is the Atlassian field id the handler refuses to
// write — reassignment is gated through the lead-approval flow per
// the M8.2.a discipline. Until the lead-approval surface lands, the
// handler returns a [agentruntime.ToolResult.Error] explaining the
// boundary so the lead can intervene manually. Dual-defense: the
// M8.1 jira whitelist also excludes `assignee` per the M8.2.a
// deployment configuration, so a bypass at one layer fails closed at
// the other.
const AssigneeFieldKey = "assignee"

// ToolArg* constants are the manifest-declared argument keys the
// handler reads from [agentruntime.ToolCall.Arguments]. Pulled into
// constants so the manifest tool schema (a future M8.2 follow-up)
// references the same identifiers as the handler.
const (
	// ToolArgIssueKey carries the Jira issue key (e.g. `"WK-42"`).
	// Required; the handler refuses with a [ToolResult.Error] when
	// missing / non-string / empty.
	ToolArgIssueKey = "issue_key"

	// ToolArgFields carries the Atlassian-keyed fields-to-update
	// map. Required; the handler refuses with a [ToolResult.Error]
	// when missing / non-map / empty.
	ToolArgFields = "fields"
)

// refusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. The
// `coordinator: <tool>: <reason>` shape mirrors the M8.1 jira layer
// (`jira: <op>: <reason>`) and the runtime sentinels
// (`runtime: <signal>`) so an agent reading multiple refusal texts
// across the toolset sees a uniform parsing convention.
const refusalPrefix = "coordinator: " + UpdateTicketFieldName + ": "

// errReassignmentBlocked is the surface text returned via
// [agentruntime.ToolResult.Error] when the handler refuses an
// `assignee` write. The agent reads the text and surfaces it to the
// lead per the system-prompt's "ALWAYS surface a question to the
// lead" discipline. Intentionally NOT scoped to a milestone identifier
// (an earlier draft included "out of scope for M8.2.a"; iter-1 review
// flagged that as runtime-visible vocabulary that goes stale once the
// lead-approval surface lands).
const errReassignmentBlocked = refusalPrefix +
	"reassignment requires lead approval; surface as a question to the lead"

// JiraFieldUpdater is the single-method interface
// [NewUpdateTicketFieldHandler] consumes for the Jira write. Mirrors
// `jira.Client.UpdateFields`'s signature exactly so production code
// passes a `*jira.Client` through verbatim; tests inject a
// hand-rolled fake without touching the HTTP client. The interface
// lives at the consumer (this package) per the project's
// "interfaces belong to the consumer" convention — see the
// [github.com/vadimtrunov/watchkeepers/core/pkg/manifest.ManifestFetcher]
// precedent in `core/pkg/manifest/loader.go`.
type JiraFieldUpdater interface {
	UpdateFields(ctx context.Context, key jira.IssueKey, fields map[string]any) error
}

// Compile-time assertion that the production [*jira.Client] satisfies
// the consumer interface. A future signature drift on the M8.1
// adapter surface fails build, not production.
var _ JiraFieldUpdater = (*jira.Client)(nil)

// NewUpdateTicketFieldHandler constructs the
// [agentruntime.ToolHandler] the Coordinator dispatcher registers
// under [UpdateTicketFieldName]. Wraps the M8.1 jira write path with
// the M8.2.a authority discipline (refuse `assignee` writes, validate
// args before the network call).
//
// Panics on a nil `updater` per the M*.c.* "panic on nil deps"
// discipline — see `core/pkg/spawn/*_step.go` for prior art.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `issue_key` (string, required): Jira issue key (e.g. `"WK-42"`)
//     matching `[A-Z][A-Z0-9_]+-[1-9][0-9]*`.
//   - `fields`    (map,    required): Atlassian-keyed fields to write.
//
// Refusal contract — returned via [agentruntime.ToolResult.Error]
// (NOT a Go error so the agent can react and re-plan; mirrors the
// `tool_result.Error` channel documented on the runtime interface):
//
//   - args contain the `assignee` key (reassignment-blocked)
//   - missing / non-string / empty `issue_key`
//   - `issue_key` does not match the Atlassian shape (the iter-1
//     PII finding required pre-network refusal so the M8.1 layer
//     never echoes a malformed key with `%q`)
//   - missing / non-map / empty `fields`
//
// Defensive copy: the `fields` map is cloned BEFORE dispatch to the
// updater so a future updater impl that retains the map cannot
// observe post-call mutations from the LLM-driven runtime that
// re-uses [agentruntime.ToolCall.Arguments]. Mirrors the M7.1.c.c
// "defensive deep-copy of reference-typed config" lesson.
//
// Forwarded errors — returned as Go `error` (the runtime treats this
// as transport failure and reflects the error class via the M5.6.b
// auto-reflection path):
//
//   - jira.Client.UpdateFields returns wrapped errors (network / API
//     / whitelist) surface verbatim with prefix
//     `"coordinator: update_ticket_field: %w"`.
func NewUpdateTicketFieldHandler(updater JiraFieldUpdater) agentruntime.ToolHandler {
	if updater == nil {
		panic("coordinator: NewUpdateTicketFieldHandler: updater must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		issueKey, refusal := readIssueKeyArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		fields, refusal := readFieldsArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		if _, hasAssignee := fields[AssigneeFieldKey]; hasAssignee {
			return agentruntime.ToolResult{Error: errReassignmentBlocked}, nil
		}

		// Defensive copy: see method doc-block. maps.Clone produces a
		// shallow copy, which is sufficient for the JSON-marshalled
		// payload jira.Client.UpdateFields builds — value mutations on
		// a nested slice or map post-call would still bleed through.
		// The Coordinator's tool-arg shapes are flat (string, slice
		// of string, scalar), so the shallow clone is enough today; a
		// future tool with nested mutable values needs to upgrade.
		fieldsCopy := maps.Clone(fields)

		if err := updater.UpdateFields(ctx, jira.IssueKey(issueKey), fieldsCopy); err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: update_ticket_field: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"issue_key":      issueKey,
				"fields_updated": len(fieldsCopy),
			},
		}, nil
	}
}

// readIssueKeyArg projects the `issue_key` arg into a Jira issue key
// string. Returns (key, "") on success; ("", refusalText) on any
// validation failure. The refusal text lands in
// [agentruntime.ToolResult.Error] so the agent can re-plan; the helper
// MUST NOT echo the raw value back (PII discipline — non-string args
// could carry tokens the agent shouldn't re-prompt with).
//
// Shape validation runs HERE (not just in the M8.1 layer) so a
// malformed key never reaches `jira.validateIssueKey`, which echoes
// the raw value via `%q` in its `ErrInvalidArgs` wrap (verified
// against `core/pkg/jira/client.go:421`). The iter-1 PII finding
// surfaced this as a Major: a token-shaped `issue_key` would otherwise
// ride up through the Go-error chain into the M5.6.b reflection layer.
func readIssueKeyArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgIssueKey]
	if !present {
		return "", refusalPrefix + "missing required arg: " + ToolArgIssueKey
	}
	str, ok := raw.(string)
	if !ok {
		return "", refusalPrefix + ToolArgIssueKey + " must be a string"
	}
	if str == "" {
		return "", refusalPrefix + ToolArgIssueKey + " must be non-empty"
	}
	if !issueKeyPattern.MatchString(str) {
		// Refusal text NEVER includes the raw value — the agent
		// already has it in the call args and can re-plan; surfacing
		// it here would defeat the point of the pre-network gate.
		return "", refusalPrefix + ToolArgIssueKey +
			" must match Atlassian shape [A-Z][A-Z0-9_]+-[1-9][0-9]*"
	}
	return str, ""
}

// readFieldsArg projects the `fields` arg into the map[string]any
// the M8.1 jira UpdateFields call consumes. Returns (fields, "") on
// success; (nil, refusalText) on failure. Same PII discipline as
// [readIssueKeyArg] — refusal text never includes the raw arg value.
//
// The single concrete-type assertion `raw.(map[string]any)` is safe
// because [agentruntime.ToolCall.Arguments] is fixed at
// `map[string]any` (`core/pkg/runtime/runtime.go:227`), and JSON
// decode produces nested maps in the same shape. A future runtime
// shift to `map[string]json.RawMessage` would fail this assertion and
// surface here.
func readFieldsArg(args map[string]any) (map[string]any, string) {
	raw, present := args[ToolArgFields]
	if !present {
		return nil, refusalPrefix + "missing required arg: " + ToolArgFields
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, refusalPrefix + ToolArgFields + " must be an object"
	}
	if len(fields) == 0 {
		return nil, refusalPrefix + ToolArgFields + " must contain at least one entry"
	}
	return fields, ""
}
