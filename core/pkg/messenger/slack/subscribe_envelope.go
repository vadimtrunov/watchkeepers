package slack

import (
	"encoding/json"
	"net/url"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// Slack Socket Mode envelope types. The wire shape is documented at
// https://api.slack.com/apis/connections/socket-implement; the structs
// below capture only the fields the M4.2.c.1 happy-path needs to
// dispatch a `message` event to a [messenger.MessageHandler] and ack
// it back to Slack.
//
// Wire-format discipline (M4.2.b lesson): typed structs lock the wire
// shape; unknown fields ride through as raw json.RawMessage when the
// adapter actually needs them. M4.2.c.1 does not introspect the
// non-event-types (interactive, slash_commands), so they are not
// modelled — the dispatcher routes them through `disconnect`-or-event
// classification only.

// envelopeType is the discriminator field every Socket Mode frame
// carries on the top-level `type` field. The four documented values:
//
//   - "hello"           — first frame after the WS handshake; carries
//     the connection metadata Slack used (num_connections, app_id, …).
//   - "events_api"      — wraps a Slack Events-API payload.
//   - "slash_commands"  — wraps a slash command invocation.
//   - "interactive"     — wraps a button / modal / shortcut interaction.
//   - "disconnect"      — Slack asking the client to close
//     gracefully and reconnect; reason is "warning" (planned
//     maintenance) or "refresh_requested" (token rotation).
//
// M4.2.c.1 dispatches `events_api` only; the other three event-types
// surface to the handler as IncomingMessage with empty Text + raw
// payload metadata, OR are dropped, depending on `type`. See
// [decodeEvent] for the routing.
const (
	envelopeTypeHello         = "hello"
	envelopeTypeEventsAPI     = "events_api"
	envelopeTypeSlashCommands = "slash_commands"
	envelopeTypeInteractive   = "interactive"
	envelopeTypeDisconnect    = "disconnect"
)

// rawEnvelope is the minimal generic shape every Socket Mode frame
// matches. We always decode this first to read `type` + `envelope_id`;
// the per-type payload is parked in `Payload` as raw JSON until the
// dispatcher knows which concrete shape to apply.
type rawEnvelope struct {
	// Type is the discriminator (see envelopeType* constants).
	Type string `json:"type"`

	// EnvelopeID is the Slack-assigned id we MUST echo back as the
	// ack. Absent on `hello` and on `disconnect` (no ack required
	// for either).
	EnvelopeID string `json:"envelope_id"`

	// AcceptsResponsePayload is set on event_api/slash/interactive
	// frames; tells the client whether Slack will accept an inline
	// response in the ack. M4.2.c.1 always replies with a bare
	// `{"envelope_id": "..."}` (no inline response) — the field is
	// captured for forward-compatibility.
	AcceptsResponsePayload bool `json:"accepts_response_payload"`

	// Payload is the per-type body (e.g. for events_api, the Slack
	// Events-API event envelope). Decoded lazily by the dispatcher.
	Payload json.RawMessage `json:"payload"`

	// Reason is set on `disconnect` frames; one of
	// "warning" / "refresh_requested" per Slack docs. Captured so
	// the c.1 logger can distinguish planned-vs-rotation closes.
	Reason string `json:"reason"`
}

// eventsAPIPayload mirrors the Slack Events-API envelope nested under
// `payload` for an `events_api` Socket Mode frame.
type eventsAPIPayload struct {
	TeamID string          `json:"team_id"`
	Event  json.RawMessage `json:"event"`
}

// messageEvent mirrors the documented Slack `message` event shape (the
// most common Events-API event for a Socket Mode bot). Other event
// types (`reaction_added`, `app_mention`, …) ride through with the
// matching field; the Type discriminator routes them.
//
// Slack docs: https://api.slack.com/events/message
type messageEvent struct {
	Type        string `json:"type"`
	Channel     string `json:"channel"`
	User        string `json:"user"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	ChannelType string `json:"channel_type"`
	Team        string `json:"team"`
	BotID       string `json:"bot_id"`
}

// ackPayload is the wire shape the adapter writes back to Slack as an
// envelope ack. Slack documents bare-form acks as
// `{"envelope_id": "<id>"}`; the M4.2.c.1 client uses exactly that —
// no inline response payload.
type ackPayload struct {
	EnvelopeID string `json:"envelope_id"`
}

// decodeIncoming converts a successfully-acked `events_api` envelope
// into a [messenger.IncomingMessage] for handler dispatch. Returns
// (false, nil) when the envelope is non-message OR cannot decode —
// the read loop logs and skips, never panics.
//
// Mapping (Slack message event → messenger.IncomingMessage):
//
//   - event.ts          → IncomingMessage.ID
//   - event.channel     → IncomingMessage.ChannelID
//   - event.user        → IncomingMessage.SenderID
//   - event.text        → IncomingMessage.Text
//   - event.thread_ts   → IncomingMessage.ThreadID
//   - parse(event.ts)   → IncomingMessage.Timestamp (UTC; Slack ts
//     is `seconds.microseconds` since epoch)
//   - event.channel_type, event.team, event.bot_id → Metadata
func decodeIncoming(env rawEnvelope) (messenger.IncomingMessage, bool) {
	if env.Type != envelopeTypeEventsAPI || len(env.Payload) == 0 {
		return messenger.IncomingMessage{}, false
	}
	var p eventsAPIPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return messenger.IncomingMessage{}, false
	}
	if len(p.Event) == 0 {
		return messenger.IncomingMessage{}, false
	}
	var ev messageEvent
	if err := json.Unmarshal(p.Event, &ev); err != nil {
		return messenger.IncomingMessage{}, false
	}
	if ev.Type != "message" {
		return messenger.IncomingMessage{}, false
	}

	out := messenger.IncomingMessage{
		ID:        messenger.MessageID(ev.TS),
		ChannelID: ev.Channel,
		SenderID:  ev.User,
		Text:      ev.Text,
		ThreadID:  messenger.MessageID(ev.ThreadTS),
		Timestamp: parseSlackTS(ev.TS),
	}
	meta := map[string]string{}
	if ev.ChannelType != "" {
		meta["channel_type"] = ev.ChannelType
	}
	if p.TeamID != "" {
		meta["team_id"] = p.TeamID
	} else if ev.Team != "" {
		meta["team_id"] = ev.Team
	}
	if ev.BotID != "" {
		meta["bot_id"] = ev.BotID
	}
	if len(meta) > 0 {
		out.Metadata = meta
	}
	return out, true
}

// parseSlackTS converts Slack's `1700000000.000100` timestamp string
// to UTC time. Returns the zero time on parse failure (the dispatcher
// still delivers the message — Timestamp is informational).
//
// Slack `ts` format: `<seconds>.<microseconds>` since epoch. Parsing
// with stdlib avoids pulling in a duration helper.
func parseSlackTS(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	// Slack's microsecond suffix is fixed-width (6 digits). The
	// stdlib `time.Parse` does not natively handle "seconds with
	// fractional part" of unix epoch — split and feed each side.
	dot := -1
	for i := 0; i < len(ts); i++ {
		if ts[i] == '.' {
			dot = i
			break
		}
	}
	var secs, micros int64
	if dot < 0 {
		// No fractional part — try whole string as seconds.
		if _, err := fmtParseInt(ts, &secs); err != nil {
			return time.Time{}
		}
		return time.Unix(secs, 0).UTC()
	}
	if _, err := fmtParseInt(ts[:dot], &secs); err != nil {
		return time.Time{}
	}
	if _, err := fmtParseInt(ts[dot+1:], &micros); err != nil {
		return time.Unix(secs, 0).UTC()
	}
	return time.Unix(secs, micros*1000).UTC()
}

// fmtParseInt is a tiny strconv-free integer parser; avoids the
// unused-import noise of pulling strconv just for this one helper.
// Returns (true, nil) on a clean parse, (false, err) on any
// non-digit byte.
func fmtParseInt(s string, out *int64) (bool, error) {
	if s == "" {
		return false, errEmptyInt
	}
	var v int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false, errBadInt
		}
		v = v*10 + int64(c-'0')
	}
	*out = v
	return true, nil
}

// errEmptyInt and errBadInt are sentinel errors fmtParseInt returns;
// callers ignore them and fall back to zero time so a malformed ts
// never crashes the read loop.
var (
	errEmptyInt = jsonErrorString("empty int")
	errBadInt   = jsonErrorString("bad int")
)

// jsonErrorString is a tiny error-with-string shim so the parse
// helpers stay zero-alloc on the happy path (no fmt.Errorf wrapping
// inside a hot read loop).
type jsonErrorString string

func (e jsonErrorString) Error() string { return string(e) }

// redactWSURL strips the query string from a `wss://` URL so a logger
// receiving the URL never sees Slack's per-connection ticket. The
// scheme + host + path are kept (useful for diagnostic correlation);
// only the query is redacted.
//
// On parse failure (defensive — Slack always returns a well-formed
// URL) the helper returns the literal string `"<unparseable wss url>"`
// so the logger gets a stable redacted value.
func redactWSURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable wss url>"
	}
	if u.RawQuery != "" {
		u.RawQuery = ""
	}
	if u.Fragment != "" {
		u.Fragment = ""
	}
	return u.String()
}
