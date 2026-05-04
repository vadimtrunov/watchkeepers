package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// chatPostMessageMethod is the Slack Web API method name SendMessage
// targets. Hoisted to a package constant so the rate-limiter registry
// (`defaultMethodTiers`) and the request path stay in sync via the
// compiler.
const chatPostMessageMethod = "chat.postMessage"

// chatPostMessageRequest is the JSON shape SendMessage marshals onto
// the wire. Slack's `chat.postMessage` accepts ~25 documented fields;
// M4.2.b wires the cross-platform spine (channel + text + thread_ts)
// plus the metadata-driven knobs documented at the package boundary
// (mrkdwn, parse, link_names, unfurl_links, unfurl_media, icon_emoji,
// icon_url, username, reply_broadcast). Unknown metadata keys are
// silently dropped — adapters consume what they recognise.
//
// Nullable booleans are represented as `*bool` so JSON omitempty
// distinguishes "field absent" from "field explicitly false". Slack
// treats absent fields as defaulted; a `false` value would actively
// override the workspace default.
type chatPostMessageRequest struct {
	Channel        string `json:"channel"`
	Text           string `json:"text,omitempty"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	Mrkdwn         *bool  `json:"mrkdwn,omitempty"`
	Parse          string `json:"parse,omitempty"`
	LinkNames      *bool  `json:"link_names,omitempty"`
	UnfurlLinks    *bool  `json:"unfurl_links,omitempty"`
	UnfurlMedia    *bool  `json:"unfurl_media,omitempty"`
	IconEmoji      string `json:"icon_emoji,omitempty"`
	IconURL        string `json:"icon_url,omitempty"`
	Username       string `json:"username,omitempty"`
	ReplyBroadcast *bool  `json:"reply_broadcast,omitempty"`
}

// chatPostMessageResponse is the subset of the Slack response
// SendMessage decodes. The full response carries the entire echoed
// message + bot profile + team id; we only need `ts` (the
// platform-assigned MessageID).
type chatPostMessageResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// SendMessage posts `msg` to `channelID` and returns the
// platform-assigned [messenger.MessageID] of the sent message.
//
// Mapping (messenger.Message → chat.postMessage):
//
//   - msg.Text                    → text
//   - msg.ThreadID (typed field)  → thread_ts (preferred over Metadata)
//   - msg.Metadata["thread_ts"]   → thread_ts (fallback when typed empty)
//   - msg.Metadata["mrkdwn"]      → mrkdwn (parsed as bool)
//   - msg.Metadata["parse"]       → parse
//   - msg.Metadata["link_names"]  → link_names (parsed as bool)
//   - msg.Metadata["unfurl_links"] → unfurl_links (parsed as bool)
//   - msg.Metadata["unfurl_media"] → unfurl_media (parsed as bool)
//   - msg.Metadata["icon_emoji"]  → icon_emoji
//   - msg.Metadata["icon_url"]    → icon_url
//   - msg.Metadata["username"]    → username
//   - msg.Metadata["reply_broadcast"] → reply_broadcast (parsed as bool)
//
// Empty channelID returns [messenger.ErrChannelNotFound] synchronously
// without contacting the platform — Slack would reject it anyway with
// the same code, but catching it client-side avoids burning a
// rate-limit token.
//
// Empty msg.Text without attachments is permitted: the request flows
// to Slack which returns `error: "no_text"`. The adapter does not
// pre-reject because a future Slack `blocks` field (carried via
// metadata once the portable [messenger.Message] grows it) would make
// an empty Text legal.
//
// [messenger.Attachment] is NOT yet wired (M4.2.b is text-only). A
// non-empty Attachments slice returns [messenger.ErrUnsupported] —
// Slack files attachments require either a `blocks` payload (when the
// attachment is a hosted URL) or a `files.upload` multipart call (when
// the attachment is inline bytes). Both land in a follow-up PR; the
// portable contract reserves the field rather than silently dropping
// it.
//
// Slack `error` codes map per the existing [APIError.Unwrap] table:
//
//   - channel_not_found → [messenger.ErrChannelNotFound]
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired → [ErrTokenExpired]
//   - ratelimited / HTTP 429 → [ErrRateLimited]
//
// Codes without a portable mapping (msg_too_long, not_in_channel,
// is_archived, no_text) surface as [*APIError] with the Code field
// populated.
func (c *Client) SendMessage(ctx context.Context, channelID string, msg messenger.Message) (messenger.MessageID, error) {
	// ctx cancellation takes precedence over input-shape validation —
	// matches the convention of most Go HTTP-style adapters (caller's
	// "abandon work" signal trumps any precondition).
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if channelID == "" {
		return "", fmt.Errorf("slack: %s: %w", chatPostMessageMethod, messenger.ErrChannelNotFound)
	}
	if len(msg.Attachments) > 0 {
		return "", fmt.Errorf("slack: %s: attachments: %w", chatPostMessageMethod, messenger.ErrUnsupported)
	}

	req := chatPostMessageRequest{
		Channel:  channelID,
		Text:     msg.Text,
		ThreadTS: resolveThreadTS(msg),
	}
	applyMessageMetadata(&req, msg.Metadata)

	var resp chatPostMessageResponse
	if err := c.Do(ctx, chatPostMessageMethod, req, &resp); err != nil {
		// channel_not_found surfaces underneath as *APIError; lift it
		// to the portable sentinel so adapter callers match without
		// importing the slack package.
		return "", liftChannelNotFound(err)
	}
	return messenger.MessageID(resp.TS), nil
}

// resolveThreadTS picks the thread anchor for a chat.postMessage
// request. The typed [messenger.Message.ThreadID] field is preferred;
// when empty, Metadata["thread_ts"] is the fallback for callers that
// pre-date the typed field. Returns "" when neither is populated
// (channel-root post).
func resolveThreadTS(msg messenger.Message) string {
	if msg.ThreadID != "" {
		return string(msg.ThreadID)
	}
	if msg.Metadata != nil {
		return msg.Metadata["thread_ts"]
	}
	return ""
}

// applyMessageMetadata copies the documented Slack-specific knobs from
// `meta` onto `req`. Boolean knobs that fail to parse are silently
// dropped — the adapter will not panic on malformed caller input, and
// forwarding garbage to Slack would produce a less-actionable error
// than just omitting the field.
func applyMessageMetadata(req *chatPostMessageRequest, meta map[string]string) {
	if meta == nil {
		return
	}
	if v, ok := meta["parse"]; ok && v != "" {
		req.Parse = v
	}
	if v, ok := meta["icon_emoji"]; ok && v != "" {
		req.IconEmoji = v
	}
	if v, ok := meta["icon_url"]; ok && v != "" {
		req.IconURL = v
	}
	if v, ok := meta["username"]; ok && v != "" {
		req.Username = v
	}
	req.Mrkdwn = optionalBool(meta, "mrkdwn")
	req.LinkNames = optionalBool(meta, "link_names")
	req.UnfurlLinks = optionalBool(meta, "unfurl_links")
	req.UnfurlMedia = optionalBool(meta, "unfurl_media")
	req.ReplyBroadcast = optionalBool(meta, "reply_broadcast")
}

// optionalBool returns `*bool` for `meta[key]` parsed via
// [strconv.ParseBool]. Returns nil when the key is absent OR when the
// value fails to parse — a malformed boolean is treated as "caller did
// not supply this knob" rather than as an error, keeping the
// metadata-bag forwards-compatible.
func optionalBool(meta map[string]string, key string) *bool {
	raw, ok := meta[key]
	if !ok {
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &v
}

// liftChannelNotFound rewraps the slack-package ErrChannelNotFound as
// the portable messenger.ErrChannelNotFound so callers that match
// against the portable sentinel via errors.Is succeed. The original
// *APIError remains accessible via errors.As for callers that want
// the Code / Status / Method fields.
func liftChannelNotFound(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrChannelNotFound) {
		return fmt.Errorf("%w: %w", messenger.ErrChannelNotFound, err)
	}
	return err
}
