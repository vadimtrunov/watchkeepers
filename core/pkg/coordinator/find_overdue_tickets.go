package coordinator

// find_overdue_tickets — Coordinator read tool (M8.2.b).
//
// Resolution order (handler closure body):
//
//  1. ctx.Err() pre-check (no JQL composition / network on cancel).
//  2. Project key arg → typed string + Atlassian project-key shape
//     pre-validation. Refusal text NEVER echoes the raw value.
//  3. Assignee account-id arg → typed string + conservative whitelist
//     pre-validation. Refusal text NEVER echoes the raw value.
//  4. Status arg → typed []string + per-entry shape pre-validation.
//     Refusal text NEVER echoes raw entry values.
//  5. Age threshold arg → typed int (accepts JSON-decoded float64)
//     + range pre-validation. Refusal text NEVER echoes raw value.
//  6. JQL composition with safe quote-escaping per Atlassian's JQL
//     string-literal rules (`"`, `\` escape with leading `\`).
//  7. Auto-paginate via M8.1 [jira.Client.Search] up to a hard cap
//     ([maxOverdueIssues] / [maxOverduePages]) so the Coordinator does
//     not have to drive cursor loops.
//  8. Project each [jira.Issue] onto a flat map shape with computed
//     `age_days`; surface `truncated=true` when the cap fired.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only;
// the runtime's tool-result reflection layer (M5.6.b) is the audit
// boundary. NO direct keeperslog.Append from this file (asserted via
// source-grep AC).
//
// PII discipline: every refusal text uses the [refusalPrefix] +
// constant suffix; raw user-supplied arg values NEVER appear. The
// composed JQL appears in the success Output (the agent needs it to
// debug the query) but only after every input passed shape validation
// — token-shaped inputs are refused before composition. Forwarded
// errors from the M8.1 layer wrap with
// `coordinator: find_overdue_tickets: %w`; the M8.1 layer's own
// redaction discipline (errors carry status + endpoint metadata, never
// auth headers / response bodies) holds at that boundary.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/jira"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// defaultNow is the production clock the public factory binds. Tests
// reach for the unexported [newFindOverdueTicketsHandlerWithClock] to
// substitute a fixed `time.Time` so the per-test override stays
// scoped to one handler instance — no package-level mutable shared
// state, no race under -parallel. Mirrors the M8.1 jira-adapter
// clock-injection precedent (config-bag clock field, no global).
var defaultNow = time.Now

// FindOverdueTicketsName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/025_coordinator_manifest_v2_seed.sql`. Callers
// use this const rather than the bare string so a future rename is a
// one-line change here.
const FindOverdueTicketsName = "find_overdue_tickets"

// findOverdueRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. Mirrors
// the M8.2.a [refusalPrefix] convention — `coordinator: <tool>: <reason>`
// — so the agent reads multi-tool refusal text under one parsing
// convention.
const findOverdueRefusalPrefix = "coordinator: " + FindOverdueTicketsName + ": "

// FindOverdueTickets argument keys — read from
// [agentruntime.ToolCall.Arguments]. Pulled into constants so the
// future tool schema (M8.2 follow-up) references the same identifiers.
const (
	// ToolArgProjectKey carries the Atlassian project key (e.g. `"WK"`).
	// Required; validated against [projectKeyPattern].
	ToolArgProjectKey = "project_key"

	// ToolArgAssigneeAccountID carries the Atlassian Cloud `accountId`
	// of the assignee to scan (e.g. `"557058:abc-uuid"`). Required;
	// validated against [accountIDPattern].
	ToolArgAssigneeAccountID = "assignee_account_id"

	// ToolArgStatus carries a JSON array of status names to include
	// (e.g. `["In Progress", "To Do"]`). Required; non-empty;
	// per-entry validation against [statusNamePattern].
	ToolArgStatus = "status"

	// ToolArgAgeThresholdDays carries a positive integer day count
	// (e.g. 7). The JQL clause becomes `updated < -<N>d`. Required;
	// validated 1 ≤ N ≤ [maxAgeThresholdDays].
	ToolArgAgeThresholdDays = "age_threshold_days"
)

// Pagination + scope caps. Bound the blast radius of a single
// `find_overdue_tickets` call so a typo in `age_threshold_days`
// (e.g. 1 day instead of 90) cannot pull thousands of issues into
// the agent's context window.
const (
	// maxOverdueIssues caps the total issues collected across every
	// page before the handler stops paginating and surfaces
	// `truncated=true`. 200 covers an extreme reviewer backlog while
	// keeping the agent's prompt-window cost predictable.
	maxOverdueIssues = 200

	// maxOverduePages caps the number of [jira.Client.Search] calls
	// the handler makes per dispatch. A defence-in-depth bound on top
	// of [maxOverdueIssues] in case Atlassian returns very small
	// pages — at the documented 50/page default, 10 pages = 500
	// issues, but the issue cap fires first.
	maxOverduePages = 10

	// maxAgeThresholdDays is the upper bound on `age_threshold_days`.
	// 365 covers "any ticket assigned to me in the last year" while
	// rejecting nonsensical values (e.g. JSON encode of an int as a
	// huge number from a buggy upstream).
	maxAgeThresholdDays = 365

	// searchPageSize is the Atlassian `maxResults` knob the handler
	// sends per call. 50 matches the documented Atlassian default
	// and balances per-call latency vs page count.
	searchPageSize = 50
)

// projectKeyPattern matches the Atlassian project-key shape: an
// uppercase letter followed by ≥1 uppercase-letter / digit / underscore.
// Mirrors `core/pkg/jira/client.go::issueKeyPattern`'s prefix shape.
var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

// accountIDPattern is the character-class whitelist for Atlassian
// Cloud account ids. Atlassian publishes ids of the shape
// `557058:abc-uuid` (legacy) and the migrated 24-char hex shape
// (post-2018 GDPR-compliant); the regex permits the union of both
// shapes plus `_`. Path traversal, quote injection, JQL operators, and
// whitespace all reject. Iter-1 codex Major: this pattern alone is
// not enough — it accepts all-uppercase token-shaped strings
// (e.g. `"API_KEY_ABCDEF"`) that would echo into the Search call and
// the success Output. The companion [accountIDDiscriminantPattern]
// closes that gap by requiring at least one digit OR colon — every
// real Atlassian accountId carries one or the other.
var accountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_\-]*$`)

// accountIDDiscriminantPattern is the second-stage check for the
// `assignee_account_id` arg. Token-shaped secrets without digits or
// colons (the canonical anti-pattern: an env var leak like
// `THE_API_KEY_VALUE`) would otherwise satisfy
// [accountIDPattern]'s character class. Atlassian's documented
// accountId shapes always carry at least one digit (legacy `557058:`
// prefix is numeric; modern 24-char hex contains 0-9) or a colon
// (legacy separator), so the discriminant adds zero false-positives
// against real ids while rejecting the token-shape leak path. Pinned
// via the iter-1 sibling table-driven canary test.
var accountIDDiscriminantPattern = regexp.MustCompile(`[0-9:]`)

// statusNamePattern is the per-entry shape for the `status` arg.
// Atlassian status names are human-readable strings: "In Progress",
// "To Do", and admin-defined names like "QA/Review", "Blocked
// (External)", "1st Response", "L2 Support". The regex pins
// alphanumeric / space / underscore / dash / slash / parenthesis as
// the closed character set so a JQL injection via a status entry
// (e.g. `") OR 1=1 --`) rejects before composition. Iter-1 codex
// minor: the original `^[A-Za-z][A-Za-z0-9 _\-]*$` was too narrow
// for real workflows; the widened class still excludes `"`, `\`, `;`,
// `=`, JQL operators, and any control character.
var statusNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _\-/()]*$`)

// JiraSearcher is the single-method interface
// [NewFindOverdueTicketsHandler] consumes for the Jira read. Mirrors
// `jira.Client.Search`'s signature exactly so production code passes a
// `*jira.Client` through verbatim; tests inject a hand-rolled fake
// without touching the HTTP client. The interface lives at the consumer
// (this package) per the project's "interfaces belong to the consumer"
// convention, mirroring [JiraFieldUpdater] from M8.2.a.
type JiraSearcher interface {
	Search(ctx context.Context, jql string, opts jira.SearchOptions) (jira.SearchResult, error)
}

// Compile-time assertion that the production [*jira.Client] satisfies
// the consumer interface. A future signature drift on the M8.1 adapter
// surface fails build, not production.
var _ JiraSearcher = (*jira.Client)(nil)

// NewFindOverdueTicketsHandler constructs the
// [agentruntime.ToolHandler] the Coordinator dispatcher registers
// under [FindOverdueTicketsName]. Wraps the M8.1 jira read path with
// the M8.2.b authority discipline (validate every arg before the
// network exchange, refuse JQL injection shapes, cap pagination).
//
// Panics on a nil `searcher` per the M*.c.* / M8.2.a "panic on nil
// deps" discipline.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `project_key`         (string, required): Atlassian project key
//     matching `[A-Z][A-Z0-9_]+` (e.g. `"WK"`).
//   - `assignee_account_id` (string, required): Atlassian Cloud
//     `accountId` matching `[A-Za-z0-9:_-]+`.
//   - `status`              (array of string, required, ≥1 entry):
//     each entry matches `[A-Za-z][A-Za-z0-9 _-]*`.
//   - `age_threshold_days`  (number, required): integer in
//     [1, [maxAgeThresholdDays]]. Accepts JSON-decoded `float64` so
//     long as the value is a non-negative whole number.
//
// Refusal contract — returned via [agentruntime.ToolResult.Error]
// (NOT a Go error so the agent can re-plan; mirrors the M8.2.a
// channel discipline):
//
//   - missing / non-string / empty / shape-violating `project_key`
//   - missing / non-string / empty / shape-violating `assignee_account_id`
//   - missing / non-array / empty / non-string-entry / shape-violating `status`
//   - missing / non-number / out-of-range / non-integer `age_threshold_days`
//
// Refusal text NEVER echoes a raw arg value — the agent already has
// the value in its call args and can re-plan; surfacing it here would
// defeat the pre-network gate (mirrors the M8.2.a iter-1 PII finding).
//
// Output (success) — keys on the returned [agentruntime.ToolResult.Output]:
//
//   - `issues`         (array of object): one entry per overdue issue
//     with keys `key`, `summary`, `status`, `assignee_id`, `updated`
//     (RFC3339 UTC), `age_days` (int — APPROXIMATE; days since the
//     `updated` timestamp, computed against the closure-captured
//     clock; may differ by ±1 from the JQL-side `updated < -Nd`
//     boundary because the JQL filter was evaluated by Atlassian's
//     clock, not the handler's; treat `age_days` as a hint, not a
//     contract).
//   - `total_returned` (int): `len(issues)`.
//   - `truncated`      (bool): true when the handler stopped
//     paginating because [maxOverdueIssues] OR [maxOverduePages] fired
//     before the M8.1 layer reported a clean `IsLast=true && cursor==""`
//     terminal page. The agent treats this as "more pages exist;
//     narrow the scope or lower the threshold". An `IsLast=true` with
//     a non-empty cursor (server contract violation, symmetric to
//     the `IsLast=false && cursor==""` guard the M8.1 layer enforces)
//     also surfaces as `truncated=true`.
//   - `scope`          (object): the structured scope summary echoed
//     back for the agent's self-audit — keys `project_key`, `status`,
//     `age_threshold_days`. Iter-1 critic Major M2: the
//     `assignee_account_id` is INTENTIONALLY OMITTED because Atlassian
//     classifies accountIds as PII subject to GDPR; echoing it into
//     the success Output would set a precedent for downstream
//     audit-pipe integrations to silently leak it. The agent already
//     has the value in its [agentruntime.ToolCall.Arguments] — it
//     does not need a copy in [agentruntime.ToolResult.Output]. The
//     composed JQL is similarly NOT echoed (it would carry the
//     accountId verbatim). Same discipline applies to M8.2.c/d
//     handlers consuming Slack user ids / GitHub login names.
//
// Forwarded errors — returned as Go `error` (the runtime treats this
// as transport failure and reflects via the M5.6.b auto-reflection):
//
//   - jira.Client.Search returns wrapped errors (network / API /
//     pagination contract violation) surface verbatim with prefix
//     `"coordinator: find_overdue_tickets: %w"`.
func NewFindOverdueTicketsHandler(searcher JiraSearcher) agentruntime.ToolHandler {
	return newFindOverdueTicketsHandlerWithClock(searcher, defaultNow)
}

// newFindOverdueTicketsHandlerWithClock is the test-internal factory
// that lets tests inject a fixed clock without mutating package state.
// The public [NewFindOverdueTicketsHandler] wraps this with
// [defaultNow]; production code never reaches this surface. Same
// nil-searcher panic discipline; clock MUST also be non-nil.
func newFindOverdueTicketsHandlerWithClock(searcher JiraSearcher, clock func() time.Time) agentruntime.ToolHandler {
	if searcher == nil {
		panic("coordinator: NewFindOverdueTicketsHandler: searcher must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewFindOverdueTicketsHandler: clock must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		projectKey, refusal := readProjectKeyArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		assigneeID, refusal := readAssigneeAccountIDArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		statuses, refusal := readStatusArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		ageDays, refusal := readAgeThresholdDaysArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		jql := composeOverdueJQL(projectKey, assigneeID, statuses, ageDays)

		issues, truncated, err := paginateOverdue(ctx, searcher, jql)
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: find_overdue_tickets: %w", err)
		}

		_ = jql // composed for the underlying Search call only; never echoed
		return agentruntime.ToolResult{
			Output: map[string]any{
				"issues":         projectIssues(issues, clock()),
				"total_returned": len(issues),
				"truncated":      truncated,
				"scope": map[string]any{
					"project_key":        projectKey,
					"status":             statuses,
					"age_threshold_days": ageDays,
				},
			},
		}, nil
	}
}

// readProjectKeyArg projects the `project_key` arg into a typed string.
// Returns (key, "") on success; ("", refusalText) on validation
// failure. Refusal text NEVER echoes the raw value (PII discipline —
// mirrors [readIssueKeyArg] from M8.2.a).
func readProjectKeyArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgProjectKey]
	if !present {
		return "", findOverdueRefusalPrefix + "missing required arg: " + ToolArgProjectKey
	}
	str, ok := raw.(string)
	if !ok {
		return "", findOverdueRefusalPrefix + ToolArgProjectKey + " must be a string"
	}
	if str == "" {
		return "", findOverdueRefusalPrefix + ToolArgProjectKey + " must be non-empty"
	}
	if !projectKeyPattern.MatchString(str) {
		return "", findOverdueRefusalPrefix + ToolArgProjectKey +
			" must match Atlassian project-key shape [A-Z][A-Z0-9_]+"
	}
	return str, ""
}

// readAssigneeAccountIDArg projects the `assignee_account_id` arg into
// a typed string. Refusal text NEVER echoes the raw value.
func readAssigneeAccountIDArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgAssigneeAccountID]
	if !present {
		return "", findOverdueRefusalPrefix + "missing required arg: " + ToolArgAssigneeAccountID
	}
	str, ok := raw.(string)
	if !ok {
		return "", findOverdueRefusalPrefix + ToolArgAssigneeAccountID + " must be a string"
	}
	if str == "" {
		return "", findOverdueRefusalPrefix + ToolArgAssigneeAccountID + " must be non-empty"
	}
	if !accountIDPattern.MatchString(str) || !accountIDDiscriminantPattern.MatchString(str) {
		// Iter-1 codex Major: refusal text must NOT echo the raw
		// value (PII discipline) AND must reject token-shaped strings
		// that pass the character-class check. The discriminant
		// (`[0-9:]`) requires at least one digit or colon — every
		// Atlassian accountId carries one; an all-uppercase env-var
		// leak does not.
		return "", findOverdueRefusalPrefix + ToolArgAssigneeAccountID +
			" must match Atlassian accountId shape (alphanumeric/_/-/colon, with at least one digit or colon)"
	}
	return str, ""
}

// readStatusArg projects the `status` arg into a typed []string with
// per-entry shape validation. The JSON wire shape is `[]any`; each
// entry must be a non-empty string matching [statusNamePattern].
// Refusal text NEVER echoes raw entry values.
func readStatusArg(args map[string]any) ([]string, string) {
	raw, present := args[ToolArgStatus]
	if !present {
		return nil, findOverdueRefusalPrefix + "missing required arg: " + ToolArgStatus
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, findOverdueRefusalPrefix + ToolArgStatus + " must be an array"
	}
	if len(rawSlice) == 0 {
		return nil, findOverdueRefusalPrefix + ToolArgStatus + " must contain at least one entry"
	}
	out := make([]string, 0, len(rawSlice))
	for _, item := range rawSlice {
		s, ok := item.(string)
		if !ok {
			return nil, findOverdueRefusalPrefix + ToolArgStatus + " entries must be strings"
		}
		if s == "" {
			return nil, findOverdueRefusalPrefix + ToolArgStatus + " entries must be non-empty"
		}
		if !statusNamePattern.MatchString(s) {
			return nil, findOverdueRefusalPrefix + ToolArgStatus +
				" entries must match status-name shape [A-Za-z][A-Za-z0-9 _-]*"
		}
		out = append(out, s)
	}
	return out, ""
}

// readAgeThresholdDaysArg projects the `age_threshold_days` arg into a
// typed int. Accepts `int`, `int64`, and JSON-decoded `float64`. The
// runtime's [agentruntime.ToolCall.Arguments] is `map[string]any` and
// JSON numbers decode as `float64`; in-process Go callers may pass
// either of the int forms. Rejects non-integer floats, negative
// values, zero, and values exceeding [maxAgeThresholdDays]. Refusal
// text NEVER echoes the raw value.
func readAgeThresholdDaysArg(args map[string]any) (int, string) {
	raw, present := args[ToolArgAgeThresholdDays]
	if !present {
		return 0, findOverdueRefusalPrefix + "missing required arg: " + ToolArgAgeThresholdDays
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		// In-process Go callers may pass int64; iter-1 codex nit.
		// Atlassian-friendly cap [maxAgeThresholdDays] is well
		// within int range so the down-cast is safe AFTER bounds
		// check, but defensively reject values that overflow int.
		if v < int64(-(1<<31)) || v > int64(1<<31-1) {
			return 0, findOverdueRefusalPrefix + ToolArgAgeThresholdDays + " out of range"
		}
		n = int(v)
	case float64:
		// Atlassian-friendly: reject non-integer floats so the agent
		// cannot accidentally pass `7.5` and silently round.
		if v != float64(int(v)) {
			return 0, findOverdueRefusalPrefix + ToolArgAgeThresholdDays + " must be an integer"
		}
		n = int(v)
	default:
		return 0, findOverdueRefusalPrefix + ToolArgAgeThresholdDays + " must be a number"
	}
	if n < 1 {
		return 0, findOverdueRefusalPrefix + ToolArgAgeThresholdDays + " must be ≥ 1"
	}
	if n > maxAgeThresholdDays {
		return 0, findOverdueRefusalPrefix + ToolArgAgeThresholdDays +
			" must be ≤ " + strconv.Itoa(maxAgeThresholdDays)
	}
	return n, ""
}

// composeOverdueJQL builds the JQL string the M8.1 [jira.Client.Search]
// receives. All four user-supplied inputs have ALREADY passed
// [readProjectKeyArg] / [readAssigneeAccountIDArg] / [readStatusArg] /
// [readAgeThresholdDaysArg] validation by the time this runs — the
// quoting here is defence-in-depth (a future regression that admits a
// raw quote into a status entry would still emit syntactically
// well-formed JQL). The composed shape is:
//
//	project = "<KEY>" AND assignee = "<accountId>" AND status in ("S1", "S2") AND updated < -<N>d ORDER BY updated ASC
//
// `ORDER BY updated ASC` surfaces the oldest issues first so the
// truncation cap, when it fires, drops the freshest tail rather than
// the staler head — the agent gets the most-overdue tickets first,
// which is the operationally useful set.
func composeOverdueJQL(projectKey, assigneeID string, statuses []string, ageDays int) string {
	statusList := make([]string, 0, len(statuses))
	for _, s := range statuses {
		statusList = append(statusList, jqlQuote(s))
	}
	return fmt.Sprintf(
		`project = %s AND assignee = %s AND status in (%s) AND updated < -%dd ORDER BY updated ASC`,
		jqlQuote(projectKey),
		jqlQuote(assigneeID),
		strings.Join(statusList, ", "),
		ageDays,
	)
}

// jqlQuote wraps `s` in JQL string literal quotes, escaping `\` and
// `"` per Atlassian's JQL string-literal rules. Backslash MUST be
// escaped before the quote so a `\"` in the input becomes `\\\"` in
// the output (not `\\"`, which closes the literal).
func jqlQuote(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// paginateOverdue drives the M8.1 cursor pagination up to the
// [maxOverdueIssues] / [maxOverduePages] caps. Returns the collected
// issues, a `truncated` flag, and the underlying Search error if the
// M8.1 layer surfaced one. The handler dispatches Search with
// `maxResults=[searchPageSize]` and a fixed `Fields` set so the
// canonical accessors the projection consumes are always populated.
//
// Truncation semantics (iter-1 review M1):
//
//   - The issue cap fires INSIDE a page → `truncated=true` whenever
//     ANY of: more pages remain (`!IsLast`), the cursor is non-empty
//     (server contract violation when `IsLast=true && NextPageToken!=""`),
//     or there are still unconsumed issues in the current `res.Issues`
//     after the cap fires. The last condition matters when Atlassian
//     overshoots `MaxResults` on a final page (a documented possibility:
//     "may return fewer than requested" framing is one-sided; the
//     server can also return more) — without this branch, an exact-cap
//     last page silently drops the unread tail and reports
//     `truncated=false`.
//   - The page cap fires BEFORE `IsLast` → unconditionally `truncated=true`
//     (the for-loop boundary fall-through).
//   - `res.IsLast=true` with NO unread issues AND empty cursor →
//     `truncated=false` (clean termination).
func paginateOverdue(ctx context.Context, searcher JiraSearcher, jql string) ([]jira.Issue, bool, error) {
	collected := make([]jira.Issue, 0, maxOverdueIssues)
	cursor := ""
	for page := 0; page < maxOverduePages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		res, err := searcher.Search(ctx, jql, jira.SearchOptions{
			Fields:     []string{"summary", "status", "assignee", "updated"},
			MaxResults: searchPageSize,
			PageToken:  cursor,
		})
		if err != nil {
			return nil, false, err
		}
		for i, iss := range res.Issues {
			collected = append(collected, iss)
			if len(collected) >= maxOverdueIssues {
				more := !res.IsLast || res.NextPageToken != "" || i+1 < len(res.Issues)
				return collected, more, nil
			}
		}
		if res.IsLast {
			// Defensive: a server-contract-conformant terminal page
			// has empty NextPageToken. If the server ALSO sets a
			// non-empty cursor with IsLast=true, treat the cursor as
			// the truth (more data exists) and surface as truncated
			// rather than silently dropping the next page. M8.1 only
			// guards the inverse (`IsLast=false && cursor==""`); this
			// is the symmetric defence (iter-1 codex Major).
			if res.NextPageToken != "" {
				return collected, true, nil
			}
			return collected, false, nil
		}
		cursor = res.NextPageToken
	}
	// Page cap fired before [jira.SearchResult.IsLast] reported true.
	// The M8.1 adapter guarantees a non-empty NextPageToken on every
	// non-terminal page, so unconditionally returning `truncated=true`
	// here is correct.
	return collected, true, nil
}

// projectIssues flattens [jira.Issue] values into the wire shape the
// agent receives. `age_days` is derived from `Updated` against the
// supplied `now`; a zero `Updated` (parse failure / field-not-requested)
// surfaces `age_days=0` rather than panicking. The handler passes the
// closure-captured clock per call so tests can pin a deterministic
// `now` without mutating package state.
//
// Boundary caveat (iter-1 codex minor): the JQL filter
// `updated < -<N>d` was evaluated by Atlassian's clock; `age_days`
// here is computed against the handler's clock. Around the threshold
// boundary, or with even modest clock skew between the two, the tool
// can return an issue that matched `age_threshold_days=7` server-side
// while reporting `age_days=6` (or vice versa) client-side. Treat
// `age_days` as an approximate hint, not a contract — the JQL filter
// IS the authoritative "is it overdue?" answer for an issue in the
// result set.
func projectIssues(issues []jira.Issue, now time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(issues))
	for _, iss := range issues {
		entry := map[string]any{
			"key":         string(iss.Key),
			"summary":     iss.Summary,
			"status":      iss.Status,
			"assignee_id": iss.AssigneeID,
		}
		if !iss.Updated.IsZero() {
			entry["updated"] = iss.Updated.UTC().Format("2006-01-02T15:04:05Z")
			entry["age_days"] = int(now.UTC().Sub(iss.Updated.UTC()).Hours() / 24)
		} else {
			entry["updated"] = ""
			entry["age_days"] = 0
		}
		out = append(out, entry)
	}
	return out
}
