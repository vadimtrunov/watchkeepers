// Package messenger defines the portable [Adapter] interface
// and the value types its methods exchange. ROADMAP §M4 → M4.1.
//
// A messenger adapter lets a Watchkeeper appear as a chat-platform bot
// — sending messages, subscribing to incoming ones, provisioning the
// platform-side app, configuring the bot identity, and looking up
// human users. M4.1 covers ONLY the interface and its value types. The
// Slack implementation lives in `core/pkg/messenger/slack` (M4.2);
// future adapters (Discord, Teams, Matrix) live in sibling
// sub-packages.
//
// # Why a portable interface
//
// Phase 1 ships with Slack as the first adapter (M4.2) but the design
// must accommodate alternative platforms without refactoring the
// callers. The split is therefore:
//
//   - This package owns the interface, the value types, and the
//     sentinel-error vocabulary every adapter must speak.
//   - Each `messenger/<platform>` sub-package implements
//     [Adapter] and translates the platform's native API
//     calls / event payloads into the value types defined here.
//   - Higher-level orchestration (lifecycle, watchkeeper handlers, the
//     M4.3 bootstrap script) depends on [Adapter] and never
//     imports a concrete platform package directly.
//
// The interface is intentionally small (six methods) and avoids
// platform-specific concepts (no Slack thread_ts, no Discord guild,
// no Teams channel_type). Where a concept does not portably translate
// the type uses a `map[string]string` metadata bag the adapter
// populates and consumes opaquely.
//
// # Method surface
//
// The six methods reflect the four lifecycle phases of a messenger
// adapter — provision the app, install the app into a workspace,
// configure the bot identity, and exchange messages — plus a user
// lookup helper that downstream features (M4.4 human-identity mapping)
// require:
//
//   - [Adapter.SendMessage]   — outbound message to a channel.
//   - [Adapter.Subscribe]     — inbound message stream.
//   - [Adapter.CreateApp]     — provision a platform app from
//     a manifest (M4.2 dev workspace bootstrap).
//   - [Adapter.InstallApp]    — install a provisioned app into
//     a workspace (OAuth grant or admin pre-approval).
//   - [Adapter.SetBotProfile] — set the bot's display name,
//     avatar, status (Slack `users.profile.set` and equivalents).
//   - [Adapter.LookupUser]    — resolve a human user (by id,
//     handle, or email) to a portable [User] record.
//
// Adapters MAY return [ErrUnsupported] for any method the underlying
// platform genuinely cannot implement (e.g. an SMS adapter cannot
// `CreateApp`). Callers wrapping the adapter for a specific feature
// MUST check for [ErrUnsupported] and degrade gracefully where the
// feature is optional.
//
// # Subscribe lifecycle
//
// [Adapter.Subscribe] returns a [Subscription] handle. The
// handler runs in a goroutine the adapter owns; concurrency limits and
// ordering guarantees are platform-specific (Slack Socket Mode in
// M4.2 fans events out one-at-a-time per channel). Callers stop
// receiving events by calling [Subscription.Stop]; Stop is idempotent
// and blocks until the in-flight handler returns.
//
// The handler MUST be non-blocking on the adapter's terms: Slack
// Socket Mode acks within 3 seconds, so a slow handler stalls the
// stream. Adapters MAY surface their per-platform timing as a
// documentation contract; this package does not impose one.
//
// # Type opacity
//
// The id types ([MessageID], [AppID]) are string aliases so callers can
// pass them across boundaries without import cycles, but the bytes
// themselves are platform-defined. Code that needs to inspect or
// reconstruct ids belongs in the platform package, not here.
//
// Metadata maps on [Message], [IncomingMessage], [AppManifest],
// [Installation], [BotProfile], [UserQuery], and [User] carry
// platform-specific extensions (Slack channel_type, thread_ts, app
// scopes, …). The interface package never inspects them.
//
// # Out of scope (deferred)
//
//   - Concrete platform implementations — see `messenger/slack` (M4.2).
//   - Rate-limiting and retry middleware — adapters embed these per
//     platform (M4.2 Slack rate limiter is tier-2/tier-3 aware).
//   - Bot-to-bot messaging conventions — Phase 1 handlers are
//     human-to-bot only.
//   - Reactions / file attachments / interactive components — added
//     when concrete features need them; the [Message.Attachments] and
//     metadata fields leave room.
//   - Capability-token enforcement on adapter calls — token issuance
//     is the [capability] package's job (M3.5); wiring is deferred to
//     M5 where call sites are concrete.
package messenger
