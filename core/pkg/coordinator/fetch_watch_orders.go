package coordinator

// fetch_watch_orders — Coordinator read tool (M8.2.c).
//
// Resolution order (handler closure body):
//
//  1. ctx.Err() pre-check (no network on cancel).
//  2. Lead user-id arg → typed string + Slack-user-id shape
//     pre-validation. Refusal text NEVER echoes the raw value.
//  3. Lookback-minutes arg → typed int (accepts JSON-decoded float64)
//     + range pre-validation. Refusal text NEVER echoes raw value.
//  4. Open the 1:1 IM channel between the calling bot and the lead via
//     [SlackIMOpener.OpenIMChannel]. PII-classified failures
//     (user_not_found / cannot_dm_bot / missing_scope) surface as
//     refusal text (no Go-error wrap) so the agent re-plans rather than
//     the runtime's reflector layer ingesting a raw user id via `%q`.
//  5. Auto-paginate via [SlackHistoryReader.ConversationsHistory] with
//     `oldest` derived from `clock() - lookback_minutes` so messages
//     older than the window are excluded server-side. Caps:
//     [maxFetchMessages] / [maxFetchPages].
//  6. Project each [slack.HistoryMessage] onto a flat map shape;
//     surface `truncated=true` when the cap fired.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only;
// the runtime's tool-result reflection layer (M5.6.b) is the audit
// boundary. NO direct keeperslog.Append from this file (asserted via
// source-grep AC).
//
// PII discipline: every refusal text uses the [fetchWatchRefusalPrefix]
// + constant suffix; raw user-supplied arg values NEVER appear. The
// `lead_user_id` is INTENTIONALLY OMITTED from the success Output
// scope echo — same M8.2.b/M8.2.a lesson (#10) discipline.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// FetchWatchOrdersName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/026_coordinator_manifest_v3_seed.sql`. Callers
// use this const rather than the bare string so a future rename is a
// one-line change here.
const FetchWatchOrdersName = "fetch_watch_orders"

// fetchWatchRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. Per
// the M8.2.b convention each Coordinator handler carries a per-tool
// prefix const so the package-scoped `refusalPrefix` from M8.2.a
// (defined in update_ticket_field.go) does NOT collide.
const fetchWatchRefusalPrefix = "coordinator: " + FetchWatchOrdersName + ": "

// fetch_watch_orders argument keys — read from
// [agentruntime.ToolCall.Arguments].
const (
	// ToolArgLeadUserID carries the Slack user id of the lead (e.g.
	// `"U12345678"` / `"W12345678"`). Required; validated against
	// [slackUserIDPattern]. The handler resolves this to an IM channel
	// id via [SlackIMOpener.OpenIMChannel].
	ToolArgLeadUserID = "lead_user_id"

	// ToolArgLookbackMinutes carries the time window to scan, in
	// whole minutes. Required; validated 1 ≤ N ≤ [maxLookbackMinutes].
	// Accepts JSON-decoded float64.
	ToolArgLookbackMinutes = "lookback_minutes"
)

// Pagination + scope caps for fetch_watch_orders.
const (
	// maxFetchMessages caps the total messages collected across every
	// page before the handler stops paginating and surfaces
	// `truncated=true`. 200 covers an unusually chatty lead-DM window
	// while keeping the agent's prompt-window cost predictable.
	maxFetchMessages = 200

	// maxFetchPages caps the number of [SlackHistoryReader.ConversationsHistory]
	// calls the handler makes per dispatch. Defence-in-depth on top of
	// [maxFetchMessages].
	maxFetchPages = 10

	// maxLookbackMinutes is the upper bound on `lookback_minutes`.
	// 1440 covers 24h scans (the "what did the lead DM me overnight?"
	// use case) while rejecting nonsensical week-scale values.
	maxLookbackMinutes = 1440

	// fetchPageSize is the Slack `conversations.history.limit` knob
	// the handler sends per call. 50 matches the Slack-tier-3 friendly
	// default and balances per-call latency vs page count.
	fetchPageSize = 50
)

// slackUserIDPattern is the conservative whitelist for Slack user ids.
// Slack documents `U…` (workspace users), `W…` (Enterprise Grid users),
// and `B…` (bot user ids). The character set after the prefix is
// uppercase-alphanumeric. Path traversal, quote injection, JQL
// operators, and whitespace all reject. Iter-1-style discriminant
// (mandatory `U`/`W`/`B` prefix) is the cheapest way to reject
// token-shaped leaks like `THE_API_KEY_VALUE` from satisfying a
// character-class-only check.
var slackUserIDPattern = regexp.MustCompile(`^[UWB][A-Z0-9]{2,}$`)

// SlackIMOpener is the single-method interface
// [NewFetchWatchOrdersHandler] consumes for the IM-channel resolve.
// Mirrors `slack.Client.OpenIMChannel`'s signature exactly so
// production code passes a `*slack.Client` through verbatim; tests
// inject a hand-rolled fake without touching the HTTP client.
type SlackIMOpener interface {
	OpenIMChannel(ctx context.Context, userID string) (string, error)
}

// SlackHistoryReader is the single-method interface
// [NewFetchWatchOrdersHandler] consumes for the history read. Mirrors
// `slack.Client.ConversationsHistory`'s signature exactly.
type SlackHistoryReader interface {
	ConversationsHistory(
		ctx context.Context,
		channelID string,
		opts slack.HistoryOptions,
	) (slack.HistoryResult, error)
}

// Compile-time assertions that the production [*slack.Client] satisfies
// both consumer interfaces. A future signature drift on the M4.2
// adapter surface fails build, not production.
var (
	_ SlackIMOpener      = (*slack.Client)(nil)
	_ SlackHistoryReader = (*slack.Client)(nil)
)

// defaultFetchNow is the production clock the public factory binds.
// Tests reach for the unexported [newFetchWatchOrdersHandlerWithClock]
// to substitute a fixed `time.Time` so the per-test override stays
// scoped to one handler instance — no package-level mutable shared
// state, no race under -parallel. Mirrors the M8.2.b lesson #3
// clock-injection precedent.
var defaultFetchNow = time.Now

// NewFetchWatchOrdersHandler constructs the [agentruntime.ToolHandler]
// the Coordinator dispatcher registers under [FetchWatchOrdersName].
// Wraps the M4.2 slack read path with the M8.2.c authority discipline
// (validate every arg before the network exchange, refuse token-shaped
// inputs, cap pagination).
//
// Panics on a nil `opener` or `reader` per the M*.c.* / M8.2.a /
// M8.2.b "panic on nil deps" discipline.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `lead_user_id`     (string, required): Slack user id matching
//     [slackUserIDPattern] (`[UWB][A-Z0-9]{2,}`).
//   - `lookback_minutes` (number, required): integer in
//     [1, [maxLookbackMinutes]]. Accepts JSON-decoded float64 so long
//     as the value is a non-negative whole number.
//
// Refusal text NEVER echoes a raw arg value — the agent already has
// the value in its call args and can re-plan. Mirrors the M8.2.a/M8.2.b
// iter-1 PII finding.
//
// Output (success) keys on the returned [agentruntime.ToolResult.Output]:
//
//   - `messages`       (array of object): one entry per fetched message
//     with keys `ts`, `user_id`, `text`, `thread_ts`, `subtype`.
//     Slack returns newest-first; the handler preserves wire order.
//   - `total_returned` (int): `len(messages)`.
//   - `truncated`      (bool): true when the handler stopped
//     paginating because [maxFetchMessages] OR [maxFetchPages] fired
//     before Slack reported `has_more=false` AND empty `next_cursor`.
//     A `has_more=false` paired with a non-empty cursor (symmetric
//     server-contract violation) also surfaces as `truncated=true`.
//   - `scope`          (object): the structured scope summary echoed
//     back for the agent's self-audit — keys `lookback_minutes`,
//     `oldest_ts`. `lead_user_id` is INTENTIONALLY OMITTED — same
//     M8.2.b lesson #10 discipline (every echo of a user-supplied
//     identifier into Output must be audited as a PII reach). The
//     resolved IM channel id is similarly NOT echoed because it
//     uniquely identifies the lead-bot relationship inside the
//     workspace.
//
// Forwarded errors — returned as Go `error`:
//
//   - non-PII-classified Slack failures (HTTP/transport/rate-limit)
//     wrap with `"coordinator: fetch_watch_orders: %w"`. PII-classified
//     failures (user_not_found, cannot_dm_bot, missing_scope) surface
//     via [agentruntime.ToolResult.Error] with a refusal text so the
//     M5.6.b reflector never ingests a Slack user id via `%q`.
func NewFetchWatchOrdersHandler(opener SlackIMOpener, reader SlackHistoryReader) agentruntime.ToolHandler {
	return newFetchWatchOrdersHandlerWithClock(opener, reader, defaultFetchNow)
}

// newFetchWatchOrdersHandlerWithClock is the test-internal factory
// that lets tests inject a fixed clock without mutating package state.
// Production code uses [NewFetchWatchOrdersHandler] which wraps this
// with [defaultFetchNow]. Same nil-dep panic discipline; clock MUST
// also be non-nil.
func newFetchWatchOrdersHandlerWithClock(
	opener SlackIMOpener,
	reader SlackHistoryReader,
	clock func() time.Time,
) agentruntime.ToolHandler {
	if opener == nil {
		panic("coordinator: NewFetchWatchOrdersHandler: opener must not be nil")
	}
	if reader == nil {
		panic("coordinator: NewFetchWatchOrdersHandler: reader must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewFetchWatchOrdersHandler: clock must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		leadID, refusal := readLeadUserIDArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		lookback, refusal := readLookbackMinutesArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		channelID, openRefusal, err := openIMOrLiftRefusal(ctx, opener, leadID)
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: fetch_watch_orders: %w", err)
		}
		if openRefusal != "" {
			return agentruntime.ToolResult{Error: openRefusal}, nil
		}

		oldestTS := slackTSFromTime(clock().Add(-time.Duration(lookback) * time.Minute))

		messages, truncated, err := paginateHistory(ctx, reader, channelID, oldestTS)
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: fetch_watch_orders: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"messages":       projectMessages(messages),
				"total_returned": len(messages),
				"truncated":      truncated,
				"scope": map[string]any{
					"lookback_minutes": lookback,
					"oldest_ts":        oldestTS,
				},
			},
		}, nil
	}
}

// readLeadUserIDArg projects the `lead_user_id` arg into a typed
// string. Returns (id, "") on success; ("", refusalText) on
// validation failure. Refusal text NEVER echoes the raw value (PII
// discipline — mirrors M8.2.a [readIssueKeyArg] / M8.2.b
// [readAssigneeAccountIDArg]).
func readLeadUserIDArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgLeadUserID]
	if !present {
		return "", fetchWatchRefusalPrefix + "missing required arg: " + ToolArgLeadUserID
	}
	str, ok := raw.(string)
	if !ok {
		return "", fetchWatchRefusalPrefix + ToolArgLeadUserID + " must be a string"
	}
	if str == "" {
		return "", fetchWatchRefusalPrefix + ToolArgLeadUserID + " must be non-empty"
	}
	if !slackUserIDPattern.MatchString(str) {
		return "", fetchWatchRefusalPrefix + ToolArgLeadUserID +
			" must match Slack user-id shape [UWB][A-Z0-9]{2,}"
	}
	return str, ""
}

// readLookbackMinutesArg projects the `lookback_minutes` arg into a
// typed int. Accepts `int`, `int64`, and JSON-decoded `float64`.
// Rejects non-integer floats, negative values, zero, and values
// exceeding [maxLookbackMinutes]. Refusal text NEVER echoes the raw
// value.
func readLookbackMinutesArg(args map[string]any) (int, string) {
	raw, present := args[ToolArgLookbackMinutes]
	if !present {
		return 0, fetchWatchRefusalPrefix + "missing required arg: " + ToolArgLookbackMinutes
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		if v < int64(-(1<<31)) || v > int64(1<<31-1) {
			return 0, fetchWatchRefusalPrefix + ToolArgLookbackMinutes + " out of range"
		}
		n = int(v)
	case float64:
		if v != float64(int(v)) {
			return 0, fetchWatchRefusalPrefix + ToolArgLookbackMinutes + " must be an integer"
		}
		n = int(v)
	default:
		return 0, fetchWatchRefusalPrefix + ToolArgLookbackMinutes + " must be a number"
	}
	if n < 1 {
		return 0, fetchWatchRefusalPrefix + ToolArgLookbackMinutes + " must be ≥ 1"
	}
	if n > maxLookbackMinutes {
		return 0, fetchWatchRefusalPrefix + ToolArgLookbackMinutes +
			" must be ≤ " + strconv.Itoa(maxLookbackMinutes)
	}
	return n, ""
}

// openIMOrLiftRefusal calls [SlackIMOpener.OpenIMChannel] and
// classifies the error into "refuse via ToolResult.Error" (PII-class
// failures: user_not_found / cannot_dm_bot / missing_scope) vs
// "surface via Go-error wrap" (transport / rate-limit / unclassified).
//
// PII discipline: the refusal text NEVER echoes the lead's user id.
// The Go-error path is reserved for failures that do NOT carry the
// user id verbatim (Slack's `*APIError` for these codes carries only
// the method name + status + code; the user id is in the request
// body that the adapter does not echo into the error).
func openIMOrLiftRefusal(
	ctx context.Context,
	opener SlackIMOpener,
	leadID string,
) (string, string, error) {
	channelID, err := opener.OpenIMChannel(ctx, leadID)
	if err == nil {
		return channelID, "", nil
	}
	// PII-classified: refuse via ToolResult.Error (no Go-error wrap).
	switch {
	case errors.Is(err, slack.ErrUserNotFound) || errors.Is(err, messenger.ErrUserNotFound):
		return "", fetchWatchRefusalPrefix + "lead user id did not resolve to a Slack user", nil
	case errors.Is(err, slack.ErrCannotDMBot):
		return "", fetchWatchRefusalPrefix + "lead user id resolves to a bot account; cannot DM", nil
	case errors.Is(err, slack.ErrMissingScope):
		return "", fetchWatchRefusalPrefix + "Slack OAuth scope missing for conversations.open (im:write)", nil
	}
	return "", "", err
}

// paginateHistory drives the M4.2 cursor pagination up to the
// [maxFetchMessages] / [maxFetchPages] caps. Returns the collected
// messages, a `truncated` flag, and the underlying ConversationsHistory
// error if the M4.2 layer surfaced one. Mirrors the M8.2.b
// [paginateOverdue] discipline:
//
//   - The message cap fires INSIDE a page → `truncated=true` whenever
//     ANY of: more pages remain (`HasMore`), the cursor is non-empty
//     (server contract violation when `HasMore=false && NextCursor!=""`),
//     or there are unconsumed messages in the current page after the
//     cap fires (server overshot the requested limit).
//   - The page cap fires BEFORE `HasMore=false` → unconditionally
//     `truncated=true`.
//   - `HasMore=false` with NO unread tail AND empty cursor →
//     `truncated=false` (clean termination).
func paginateHistory(
	ctx context.Context,
	reader SlackHistoryReader,
	channelID string,
	oldestTS string,
) ([]slack.HistoryMessage, bool, error) {
	collected := make([]slack.HistoryMessage, 0, maxFetchMessages)
	cursor := ""
	for page := 0; page < maxFetchPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		res, err := reader.ConversationsHistory(ctx, channelID, slack.HistoryOptions{
			Limit:     fetchPageSize,
			Cursor:    cursor,
			Oldest:    oldestTS,
			Inclusive: true,
		})
		if err != nil {
			return nil, false, err
		}
		for i, m := range res.Messages {
			collected = append(collected, m)
			if len(collected) >= maxFetchMessages {
				more := res.HasMore || res.NextCursor != "" || i+1 < len(res.Messages)
				return collected, more, nil
			}
		}
		if !res.HasMore {
			if res.NextCursor != "" {
				// Server-contract violation symmetric to the M8.1 jira
				// guard: HasMore=false with a non-empty cursor → treat
				// the cursor as authoritative and surface truncated.
				return collected, true, nil
			}
			return collected, false, nil
		}
		// iter-1 codex Major: HasMore=true with an EMPTY NextCursor is
		// a server-contract violation in the opposite direction. The
		// loop body assigned `cursor = res.NextCursor` unconditionally
		// before this guard landed, which would have re-requested
		// page 1 on the next iteration (the same empty cursor) and
		// duplicated every message until the page-cap or message-cap
		// fired. Treat the empty-cursor case as a truncation signal
		// so the agent re-plans rather than receiving silent
		// duplicates.
		if res.NextCursor == "" {
			return collected, true, nil
		}
		cursor = res.NextCursor
	}
	return collected, true, nil
}

// projectMessages flattens [slack.HistoryMessage] values into the wire
// shape the agent receives. The Slack-specific Metadata bag is dropped
// — the Coordinator agent reads watch orders for content (text +
// thread context), not Slack-internal bot/app ids.
//
// PII discipline (iter-1 codex Major #3): the per-message `user_id`
// field is INTENTIONALLY OMITTED from the projected shape. The lead-DM
// channel is a 1:1 conversation between the calling bot and the lead,
// so every human-authored message carries the same user_id — echoing
// it back per-message multiplies the lead-identifier surface across
// the success Output. The handler already refused to echo
// `lead_user_id` in `scope`; the per-message omission completes the
// discipline. If a future use case needs to disambiguate author roles
// inside the DM (e.g. bot self-replies vs lead messages), prefer a
// boolean `is_bot` derived from [slack.HistoryMessage.Subtype] or
// [slack.HistoryMessage.Metadata]["bot_id"] over the raw user id.
func projectMessages(messages []slack.HistoryMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		out = append(out, map[string]any{
			"ts":        m.TS,
			"text":      m.Text,
			"thread_ts": m.ThreadTS,
			"subtype":   m.Subtype,
		})
	}
	return out
}

// slackTSFromTime renders a `time.Time` as the Slack `ts` string shape
// (`"<unix-seconds>.000000"`). Slack accepts the integer-seconds form
// as a valid `oldest`/`latest` bound; the microsecond field is
// documented as optional for filters (it disambiguates messages with
// the same wall-clock second when used as a message id, but the filter
// semantics treat the whole value as a numeric threshold).
func slackTSFromTime(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10) + ".000000"
}
