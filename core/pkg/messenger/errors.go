package messenger

import "errors"

// ErrUnsupported is returned by a [Adapter] method when the
// underlying platform genuinely cannot implement the requested
// operation (e.g. an SMS adapter cannot [Adapter.CreateApp];
// an IRC adapter has no [Adapter.SetBotProfile] avatar).
// Callers wrapping the adapter for an optional feature MUST check for
// this sentinel and degrade gracefully. Matchable via [errors.Is].
var ErrUnsupported = errors.New("messenger: unsupported")

// ErrChannelNotFound is returned by [Adapter.SendMessage]
// when the requested channel does not exist on the platform OR the
// bot lacks access to it. Adapters MUST NOT distinguish the two cases
// in this sentinel — exposing "channel exists but you can't see it"
// would leak workspace topology. Matchable via [errors.Is].
var ErrChannelNotFound = errors.New("messenger: channel not found")

// ErrUserNotFound is returned by [Adapter.LookupUser] when
// the platform reports no match for the supplied [UserQuery].
// Adapters MUST distinguish this from [ErrInvalidQuery] — the former
// means "the platform answered no", the latter means "the query was
// malformed and never reached the platform". Matchable via
// [errors.Is].
var ErrUserNotFound = errors.New("messenger: user not found")

// ErrAppNotFound is returned by [Adapter.InstallApp] when the
// supplied [AppID] does not match a provisioned app on the platform.
// Distinct from [ErrUnsupported] — the platform supports the
// operation, the id just doesn't resolve. Matchable via [errors.Is].
var ErrAppNotFound = errors.New("messenger: app not found")

// ErrInvalidManifest is returned synchronously by
// [Adapter.CreateApp] when the supplied [AppManifest] fails
// platform-side validation (empty name, malformed scope, …). Adapters
// MAY wrap a platform-specific reason via [fmt.Errorf]; the sentinel
// stays matchable via [errors.Is].
var ErrInvalidManifest = errors.New("messenger: invalid manifest")

// ErrInvalidQuery is returned synchronously by
// [Adapter.LookupUser] when the supplied [UserQuery] is empty
// (none of [UserQuery.ID] / [UserQuery.Handle] / [UserQuery.Email]
// populated) or over-populated (more than one populated). The
// platform is NOT contacted on this path. Matchable via [errors.Is].
var ErrInvalidQuery = errors.New("messenger: invalid query")

// ErrInvalidHandler is returned synchronously by
// [Adapter.Subscribe] when the supplied [MessageHandler] is
// nil. A nil handler is a programmer error at the call site;
// surfacing the sentinel rather than panicking lets the caller
// recover and report. Matchable via [errors.Is].
var ErrInvalidHandler = errors.New("messenger: invalid handler")

// ErrSubscriptionClosed is returned by [Subscription.Stop] when the
// receive loop exited with a transport error before Stop was called
// (the wrapped error rides via the [errors.Is] chain). A clean
// shutdown returns nil. Matchable via [errors.Is].
var ErrSubscriptionClosed = errors.New("messenger: subscription closed")
