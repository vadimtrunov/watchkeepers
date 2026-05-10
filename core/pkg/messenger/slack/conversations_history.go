package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// conversationsHistoryMethod is the Slack Web API method name
// ConversationsHistory targets. Hoisted to a package constant so the
// rate-limiter registry (`defaultMethodTiers`) and the request path
// stay in sync via the compiler.
const conversationsHistoryMethod = "conversations.history"

// HistoryOptions tunes a [Client.ConversationsHistory] call. Mirrors
// the [SearchOptions] shape from the M8.1 jira adapter — cursor
// pagination is opt-in via [HistoryOptions.Cursor], not implicit.
type HistoryOptions struct {
	// Limit caps the number of messages returned per page. Slack's
	// documented default is 100; values >1000 are clamped server-side.
	// Zero leaves the field absent from the request (Slack default).
	Limit int

	// Cursor is the opaque pagination handle the caller threads through
	// successive calls. Empty on the first page; subsequent pages copy
	// the previous result's [HistoryResult.NextCursor] verbatim.
	Cursor string

	// Oldest filters the result to messages with `ts > Oldest` by
	// Slack default (the bound is EXCLUSIVE). Callers wanting the
	// inclusive form (`ts >= Oldest`) MUST set
	// [HistoryOptions.Inclusive] = true. Slack `ts` format
	// (`"1700000000.000000"`). Empty leaves the field absent.
	Oldest string

	// Latest filters the result to messages with `ts <= Latest`. Empty
	// leaves the field absent (Slack default = now).
	Latest string

	// Inclusive controls whether the Oldest/Latest bounds are inclusive
	// (true) or exclusive (false). Zero value (false) is Slack's
	// documented default.
	Inclusive bool
}

// HistoryResult is the projection of a Slack `conversations.history`
// response onto the cross-platform surface the Coordinator's
// `fetch_watch_orders` tool consumes. Only the fields the M8.2.c
// handler needs are surfaced; the raw Slack envelope carries dozens
// more (response_metadata.warnings, channel_actions_ts, …) that ride
// in [HistoryMessage.Metadata] when relevant.
type HistoryResult struct {
	// Messages is the channel-ordered list of messages on this page.
	// Slack returns newest-first; the adapter does NOT re-sort —
	// callers that want oldest-first reverse the slice client-side.
	Messages []HistoryMessage

	// HasMore signals whether more pages exist past this one. Slack
	// documents `has_more` and `response_metadata.next_cursor` as
	// independent signals — `has_more=true` does NOT guarantee a
	// non-empty cursor (a documented edge case is `has_more=true`
	// with an empty cursor when the server believes more data
	// exists but cannot supply a continuation token). Callers driving
	// cursor pagination MUST guard the empty-cursor case explicitly
	// rather than assuming `HasMore` implies cursorability — see
	// `core/pkg/coordinator/fetch_watch_orders.go::paginateHistory`
	// for the canonical guard. Iter-1 codex Major #2 lesson.
	HasMore bool

	// NextCursor is the opaque pagination handle for the next page.
	// Empty when [HistoryResult.HasMore] is false. Pass verbatim to
	// [HistoryOptions.Cursor] on the next call.
	NextCursor string
}

// HistoryMessage is one channel message. Slack messages carry a rich
// shape (blocks, attachments, files, edited, …); the adapter projects
// the cross-platform spine the Coordinator's read-only tool needs.
// Slack-specific fields ride in [HistoryMessage.Metadata] keyed by
// stable Slack vocabulary (`subtype`, `team`, `bot_id`, …).
type HistoryMessage struct {
	// TS is the Slack-assigned message timestamp (`"1700000000.000000"`).
	// This doubles as the message id (Slack does not assign separate
	// ids — `ts` is the unique key within a channel).
	TS string

	// UserID is the Slack user id of the message author (empty for
	// bot-app posts that omit the user field; bot identity then rides
	// on `bot_id` in [HistoryMessage.Metadata]).
	UserID string

	// Text is the message body (Slack-mrkdwn syntax). Plain text /
	// formatted text / @mention tags all surface here verbatim — the
	// adapter does not rewrite.
	Text string

	// ThreadTS is the parent message's TS when this message is a thread
	// reply, OR equal to TS when this message itself is the head of a
	// thread that has replies. Empty for non-threaded channel posts.
	ThreadTS string

	// Subtype is the Slack message subtype (`bot_message`, `channel_join`,
	// `file_share`, …). Empty for plain user messages. The Coordinator
	// can filter on this to drop join/leave noise.
	Subtype string

	// Metadata carries Slack-specific fields not on the portable spine.
	// Stable keys: `team`, `bot_id`, `app_id`, `client_msg_id`.
	Metadata map[string]string
}

// conversationsHistoryRequest is the JSON envelope
// `conversations.history` expects.
type conversationsHistoryRequest struct {
	Channel   string `json:"channel"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Oldest    string `json:"oldest,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Inclusive bool   `json:"inclusive,omitempty"`
}

// conversationsHistoryResponse is the subset of the
// `conversations.history` envelope [Client.ConversationsHistory]
// decodes.
type conversationsHistoryResponse struct {
	OK               bool             `json:"ok"`
	Messages         []historyWire    `json:"messages"`
	HasMore          bool             `json:"has_more"`
	ResponseMetadata responseMetadata `json:"response_metadata"`
}

// responseMetadata is Slack's standard envelope sub-object carrying
// pagination cursors + warning strings. Only `next_cursor` is consumed.
type responseMetadata struct {
	NextCursor string `json:"next_cursor"`
}

// historyWire is the wire shape of one message in the
// `conversations.history` response.
type historyWire struct {
	TS          string `json:"ts"`
	User        string `json:"user"`
	Text        string `json:"text"`
	ThreadTS    string `json:"thread_ts"`
	Subtype     string `json:"subtype"`
	Team        string `json:"team"`
	BotID       string `json:"bot_id"`
	AppID       string `json:"app_id"`
	ClientMsgID string `json:"client_msg_id"`
}

// toHistoryMessage projects the wire shape onto the portable
// [HistoryMessage] surface. Slack-specific fields land in Metadata
// under stable keys; empty fields are NOT added so callers iterating
// the map see only what was populated.
func (w historyWire) toHistoryMessage() HistoryMessage {
	out := HistoryMessage{
		TS:       w.TS,
		UserID:   w.User,
		Text:     w.Text,
		ThreadTS: w.ThreadTS,
		Subtype:  w.Subtype,
	}
	meta := map[string]string{}
	if w.Team != "" {
		meta["team"] = w.Team
	}
	if w.BotID != "" {
		meta["bot_id"] = w.BotID
	}
	if w.AppID != "" {
		meta["app_id"] = w.AppID
	}
	if w.ClientMsgID != "" {
		meta["client_msg_id"] = w.ClientMsgID
	}
	if len(meta) > 0 {
		out.Metadata = meta
	}
	return out
}

// ConversationsHistory returns one page of `channelID`'s message
// history. Sibling to [Client.SendMessage] / [Client.LookupUser]:
// stdlib HTTP, JSON in / JSON out, Slack `error` codes lifted to
// portable sentinels via [APIError.Unwrap].
//
// Mapping (HistoryOptions → conversations.history):
//
//   - opts.Limit     → limit
//   - opts.Cursor    → cursor
//   - opts.Oldest    → oldest
//   - opts.Latest    → latest
//   - opts.Inclusive → inclusive
//
// Empty channelID returns [messenger.ErrChannelNotFound] synchronously
// without contacting Slack — same fast-reject discipline as
// [Client.SendMessage].
//
// Slack `error` codes map per the existing [APIError.Unwrap] table:
//
//   - channel_not_found → [messenger.ErrChannelNotFound]
//   - missing_scope     → [ErrMissingScope]
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired     → [ErrTokenExpired]
//   - ratelimited / HTTP 429 → [ErrRateLimited]
//
// Codes outside this table surface as [*APIError] with the Code field
// populated.
func (c *Client) ConversationsHistory(
	ctx context.Context,
	channelID string,
	opts HistoryOptions,
) (HistoryResult, error) {
	if err := ctx.Err(); err != nil {
		return HistoryResult{}, err
	}
	if channelID == "" {
		return HistoryResult{}, fmt.Errorf(
			"slack: %s: %w", conversationsHistoryMethod, messenger.ErrChannelNotFound,
		)
	}

	req := conversationsHistoryRequest{
		Channel:   channelID,
		Limit:     opts.Limit,
		Cursor:    opts.Cursor,
		Oldest:    opts.Oldest,
		Latest:    opts.Latest,
		Inclusive: opts.Inclusive,
	}

	var resp conversationsHistoryResponse
	if err := c.Do(ctx, conversationsHistoryMethod, req, &resp); err != nil {
		return HistoryResult{}, liftHistoryError(err)
	}

	out := HistoryResult{
		Messages:   make([]HistoryMessage, 0, len(resp.Messages)),
		HasMore:    resp.HasMore,
		NextCursor: resp.ResponseMetadata.NextCursor,
	}
	for _, m := range resp.Messages {
		out.Messages = append(out.Messages, m.toHistoryMessage())
	}
	return out, nil
}

// liftHistoryError rewraps the documented Slack codes onto their
// portable counterparts so adapter callers can match on
// [messenger.ErrChannelNotFound] / [ErrMissingScope] via errors.Is
// without importing the slack package. Symmetric with
// [liftChannelNotFound] / [liftUserNotFound] from
// `send_message.go` / `lookup_user.go`.
func liftHistoryError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "channel_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrChannelNotFound, err)
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}
