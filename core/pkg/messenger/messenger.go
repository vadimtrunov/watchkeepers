package messenger

import (
	"context"
	"time"
)

// MessageID is the opaque platform-assigned identifier for a sent
// message. Slack's `ts` (e.g. `1700000000.000100`), Discord's snowflake
// id, etc. The bytes are platform-defined; callers treat them as
// opaque strings and pass them back to the adapter for follow-up
// operations (edits, threads).
type MessageID string

// AppID is the opaque platform-assigned identifier for a provisioned
// app (e.g. Slack's `A0123ABCDEF`). Returned by
// [Adapter.CreateApp] and consumed by
// [Adapter.InstallApp].
type AppID string

// Message is the value supplied to [Adapter.SendMessage]. The
// shape is intentionally minimal — a body plus optional thread anchor,
// optional attachments, and a metadata bag for platform-specific
// extensions. Future fields are additive only; callers that need
// platform-specific knobs reach for [Message.Metadata] until a portable
// concept exists.
type Message struct {
	// Text is the message body. Plain text or platform-flavoured
	// markdown (Slack mrkdwn, Discord markdown). Adapters MAY translate
	// a portable subset; full fidelity is the caller's responsibility.
	Text string

	// ThreadID, when non-empty, anchors this message to an existing
	// thread. Treated as a [MessageID] of the parent message. An empty
	// ThreadID posts to the channel root.
	ThreadID MessageID

	// Attachments is the optional list of file / link attachments.
	// Each entry is an opaque platform descriptor; adapters validate
	// at SendMessage time. Nil or empty is fine.
	Attachments []Attachment

	// Metadata carries platform-specific extensions (Slack
	// `unfurl_links`, Discord `tts`, …). Adapters consume only the
	// keys they recognise and ignore the rest. Nil is fine.
	Metadata map[string]string
}

// Attachment is the portable shape for a file or link attached to a
// [Message]. Phase 1 supports URLs and inline blobs; richer types
// (interactive components, blocks) live in platform metadata until a
// portable abstraction emerges.
type Attachment struct {
	// Name is the human-readable display name. Required.
	Name string

	// MIMEType is the IETF media type (e.g. `image/png`). Optional;
	// adapters MAY default to `application/octet-stream`.
	MIMEType string

	// URL points at an externally-hosted asset. Mutually exclusive
	// with [Attachment.Data] — adapters MUST reject an Attachment
	// where both are set.
	URL string

	// Data carries an inline blob. Adapters that don't support inline
	// uploads return [ErrUnsupported]. Mutually exclusive with
	// [Attachment.URL].
	Data []byte
}

// IncomingMessage is the read-side counterpart of [Message] delivered
// to a [MessageHandler] from [Adapter.Subscribe]. The shape
// captures everything a handler needs to reply, look up the sender, or
// thread back into the conversation.
type IncomingMessage struct {
	// ID is the platform-assigned id of THIS message. Treat as opaque.
	ID MessageID

	// ChannelID is the platform-assigned channel identifier the
	// message arrived on. Treat as opaque; pass it back to
	// [Adapter.SendMessage] to reply in-channel.
	ChannelID string

	// SenderID is the platform-assigned id of the human (or bot) who
	// sent the message. Pass it to [Adapter.LookupUser] for
	// the [User] record.
	SenderID string

	// Text is the message body verbatim from the platform. Adapters
	// do NOT strip mentions, emojis, or markup — handlers parse as
	// they see fit.
	Text string

	// ThreadID, when non-empty, identifies the parent message of the
	// thread this message belongs to. Empty for channel-root messages.
	ThreadID MessageID

	// Timestamp is the platform-reported send time. UTC.
	Timestamp time.Time

	// Metadata carries platform-specific extensions (Slack
	// `channel_type`, Discord `guild_id`, …). Handlers consume only
	// what they recognise.
	Metadata map[string]string
}

// MessageHandler is the callback supplied to
// [Adapter.Subscribe]. The handler runs in a goroutine the
// adapter owns; returning a non-nil error is logged by the adapter but
// does NOT redeliver the message (Phase 1 is at-most-once at this
// layer; durable redelivery lives in the M3.7 outbox upstream).
type MessageHandler func(ctx context.Context, msg IncomingMessage) error

// Subscription is the lifecycle handle returned by
// [Adapter.Subscribe]. Calling [Subscription.Stop] terminates
// the inbound stream and returns once the in-flight [MessageHandler]
// (if any) has completed. Stop is idempotent.
type Subscription interface {
	// Stop signals the underlying stream to close and blocks until
	// the receive loop exits. Idempotent — a second Stop returns the
	// same (typically nil) result without re-running the shutdown.
	Stop() error
}

// AppManifest describes a platform app at provisioning time. Slack
// reads it as a Manifest API document; Discord reads the equivalent
// `application` schema. The portable subset captured here is
// name + description + scopes + a metadata bag for platform-specific
// fields the manifest API requires (Slack interactivity URLs, event
// subscriptions, slash commands, …).
type AppManifest struct {
	// Name is the app's display name in the workspace UI. Required.
	Name string

	// Description is the long-form blurb shown in app catalogues.
	Description string

	// Scopes is the list of platform-defined permission scopes the
	// app requests at install time (Slack OAuth scopes, Discord
	// permission integers as decimal strings, …). The adapter
	// validates the values per platform.
	Scopes []string

	// Metadata carries platform-specific manifest extensions. Adapters
	// merge it into the platform's manifest schema verbatim.
	Metadata map[string]string
}

// WorkspaceRef identifies the platform workspace / guild / team an app
// is being installed into. The [WorkspaceRef.ID] is the platform-side
// id (Slack's `T01234`, Discord's guild snowflake); the optional
// [WorkspaceRef.Name] is a display label adapters MAY surface in logs.
type WorkspaceRef struct {
	// ID is the platform-assigned workspace identifier. Required.
	ID string

	// Name is the human-readable workspace name. Optional; adapters
	// fall back to ID in log entries when empty.
	Name string
}

// Installation is the value returned by [Adapter.InstallApp].
// It captures the post-install state the caller needs to drive the
// adapter — bot id, workspace ref, optional metadata. Tokens that
// authorise subsequent calls are the adapter's internal concern;
// callers do NOT receive raw OAuth bearer values through this struct.
type Installation struct {
	// AppID is the platform-assigned app id (echoed from
	// [Adapter.CreateApp]).
	AppID AppID

	// Workspace is the workspace the app was installed into.
	Workspace WorkspaceRef

	// BotUserID is the platform-assigned id of the bot user the
	// install created. Pass to [Adapter.LookupUser] when the
	// caller needs the bot's [User] record.
	BotUserID string

	// InstalledAt is the platform-reported install time. UTC.
	InstalledAt time.Time

	// Metadata carries platform-specific install artefacts (Slack
	// `enterprise_id`, `app_configuration_token` references, …).
	// Tokens themselves are stored in the secrets interface, NOT
	// here.
	Metadata map[string]string
}

// BotProfile is the value supplied to
// [Adapter.SetBotProfile]. Captures the cross-platform subset
// of bot identity: display name, status text, avatar bytes. Platform
// extensions (Slack status emoji, Discord activity types) live in
// [BotProfile.Metadata].
type BotProfile struct {
	// DisplayName is the bot's visible name in the workspace. Empty
	// leaves the existing value unchanged (adapters do NOT clear on
	// empty).
	DisplayName string

	// StatusText is the short status line shown next to the bot
	// avatar. Empty leaves unchanged.
	StatusText string

	// AvatarPNG is the optional avatar bytes (PNG-encoded). Nil leaves
	// unchanged. Adapters that don't support avatar set return
	// [ErrUnsupported].
	AvatarPNG []byte

	// Metadata carries platform-specific extensions (Slack
	// `status_emoji`, `status_expiration`, …).
	Metadata map[string]string
}

// UserQuery is the value supplied to [Adapter.LookupUser].
// The fields are alternative lookup keys; callers populate exactly one
// of [UserQuery.ID] / [UserQuery.Handle] / [UserQuery.Email]. An empty
// or over-populated query returns [ErrInvalidQuery] synchronously
// without contacting the platform.
type UserQuery struct {
	// ID is the platform-assigned user id (Slack `U01234`, Discord
	// snowflake). Most efficient lookup path.
	ID string

	// Handle is the platform handle (Slack `@username`, Discord
	// `username#discriminator`). Adapters MAY normalise leading `@`.
	Handle string

	// Email is the email address registered with the platform.
	// Adapters that don't support email lookup return
	// [ErrUnsupported].
	Email string
}

// User is the portable record returned by
// [Adapter.LookupUser]. Captures the cross-platform subset
// downstream features (M4.4 human-identity mapping, audit log) need.
// Platform-specific fields ride in [User.Metadata].
type User struct {
	// ID is the platform-assigned user id. Always populated on a
	// successful lookup.
	ID string

	// Handle is the platform handle (without leading `@`).
	Handle string

	// DisplayName is the human-readable name shown in the workspace
	// UI.
	DisplayName string

	// Email is the registered email address. Empty when the platform
	// does not expose it (privacy settings, scope restrictions).
	Email string

	// IsBot reports whether the user is a bot account. Used by
	// downstream code that filters bot-to-bot traffic.
	IsBot bool

	// Metadata carries platform-specific extensions (Slack `team_id`,
	// Discord `guild_id`, timezone, …).
	Metadata map[string]string
}

// Adapter is the portable interface every platform
// implementation satisfies. The six methods cover the four lifecycle
// phases of a messenger integration: provision the app
// ([Adapter.CreateApp]), install it into a workspace
// ([Adapter.InstallApp]), configure the bot identity
// ([Adapter.SetBotProfile]), and exchange messages with
// humans ([Adapter.SendMessage] /
// [Adapter.Subscribe]) plus a user lookup helper
// ([Adapter.LookupUser]) for downstream identity mapping.
//
// All methods accept a context and return an error. Adapters MUST
// honour ctx cancellation promptly. Methods MAY return
// [ErrUnsupported] for capabilities the underlying platform genuinely
// cannot provide; callers wrapping the adapter MUST check for it
// where the feature is optional.
//
// Implementations are expected to be safe for concurrent use after
// construction; the interface itself does not impose synchronization
// requirements but every Phase 1 implementation does.
type Adapter interface {
	// SendMessage posts `msg` to `channelID` and returns the
	// platform-assigned [MessageID] of the sent message. Returns
	// [ErrChannelNotFound] when the channel does not exist or the
	// bot lacks access.
	SendMessage(ctx context.Context, channelID string, msg Message) (MessageID, error)

	// Subscribe opens an inbound message stream and dispatches each
	// received [IncomingMessage] to `handler`. The returned
	// [Subscription] terminates the stream when [Subscription.Stop]
	// is called. A nil handler is a programmer error and adapters
	// SHOULD return [ErrInvalidHandler] synchronously.
	Subscribe(ctx context.Context, handler MessageHandler) (Subscription, error)

	// CreateApp provisions a new platform app from `manifest` and
	// returns the platform-assigned [AppID]. Returns
	// [ErrInvalidManifest] when the manifest fails platform-side
	// validation; adapters MAY surface platform errors via the
	// wrap chain.
	CreateApp(ctx context.Context, manifest AppManifest) (AppID, error)

	// InstallApp installs the app `appID` into `workspace` and
	// returns the [Installation] handle the caller needs to drive
	// subsequent operations. Returns [ErrAppNotFound] when the app
	// id does not match a provisioned app.
	InstallApp(ctx context.Context, appID AppID, workspace WorkspaceRef) (Installation, error)

	// SetBotProfile updates the calling bot's profile fields per
	// `profile`. Empty fields leave the existing values unchanged
	// (adapters do NOT clear on empty). Returns nil on success.
	SetBotProfile(ctx context.Context, profile BotProfile) error

	// LookupUser resolves `query` to a [User] record. Exactly one of
	// [UserQuery.ID] / [UserQuery.Handle] / [UserQuery.Email] must
	// be populated; otherwise returns [ErrInvalidQuery]. Returns
	// [ErrUserNotFound] when the platform reports no match.
	LookupUser(ctx context.Context, query UserQuery) (User, error)
}
