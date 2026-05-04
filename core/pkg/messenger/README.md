# messenger — portable chat-platform adapter interface

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/messenger`

This package defines the **portable `Adapter` interface** every chat-platform
implementation satisfies: send a message, subscribe to inbound messages,
provision an app, install it into a workspace, configure the bot identity,
look up a user. ROADMAP §M4 → M4.1.

The interface and its value types live here; concrete platform adapters
(Slack, Discord, Teams, …) live in sibling sub-packages
(`messenger/slack` ships in M4.2). Higher-level callers depend only on
the interface — they never import a concrete platform package directly.

## Public API

```go
type Adapter interface {
    SendMessage(ctx context.Context, channelID string, msg Message) (MessageID, error)
    Subscribe(ctx context.Context, handler MessageHandler) (Subscription, error)
    CreateApp(ctx context.Context, manifest AppManifest) (AppID, error)
    InstallApp(ctx context.Context, appID AppID, workspace WorkspaceRef) (Installation, error)
    SetBotProfile(ctx context.Context, profile BotProfile) error
    LookupUser(ctx context.Context, query UserQuery) (User, error)
}

type Subscription interface {
    Stop() error
}

type MessageHandler func(ctx context.Context, msg IncomingMessage) error
```

Value types: `Message`, `IncomingMessage`, `Attachment`, `AppManifest`,
`WorkspaceRef`, `Installation`, `BotProfile`, `UserQuery`, `User`.

ID aliases: `MessageID string`, `AppID string`.

## Sentinel errors

All matchable via `errors.Is`:

- `ErrUnsupported` — the underlying platform cannot implement the call.
- `ErrChannelNotFound` — channel absent or bot lacks access (the
  adapter MUST NOT distinguish the two cases).
- `ErrUserNotFound` — `LookupUser` query did not match anyone.
- `ErrAppNotFound` — `InstallApp` got an unknown `AppID`.
- `ErrInvalidManifest` — `CreateApp` rejected the manifest at the
  platform boundary.
- `ErrInvalidQuery` — `LookupUser` got an empty or over-populated
  `UserQuery`. The platform is NEVER contacted on this path.
- `ErrInvalidHandler` — `Subscribe` got a nil handler.
- `ErrSubscriptionClosed` — the receive loop exited (transport error
  or post-Stop delivery attempt).

## Adapter contract (Phase 1)

1. **Method-set fidelity**: every adapter implements all six methods.
   When the platform genuinely cannot satisfy one (e.g. SMS has no
   `CreateApp`, IRC has no avatar), return `ErrUnsupported`.
2. **Synchronous validation first**: `LookupUser` rejects an
   empty/over-populated query before contacting the platform;
   `Subscribe` rejects a nil handler synchronously.
3. **Sentinel discipline**: when an error has a portable meaning,
   return the package sentinel (wrapped via `fmt.Errorf` if a platform
   reason adds value). When the meaning is platform-specific, surface
   the platform error directly — but document it.
4. **`UserQuery` is mutually exclusive**: exactly one of `ID` /
   `Handle` / `Email` is populated. Empty or multiple → `ErrInvalidQuery`.
5. **`Attachment.URL` and `Attachment.Data` are mutually exclusive**:
   adapters MUST reject an attachment with both populated.
6. **Empty `BotProfile` fields mean "leave unchanged"**: adapters do
   NOT clear the bot's display name / status when the field is empty.
   Use `Metadata` to carry "explicit clear" intents per platform.
7. **Subscriptions are owned by the adapter**: the handler runs in a
   goroutine the adapter spawned. `Subscription.Stop` blocks until the
   in-flight handler returns; idempotent.
8. **No infrastructure metadata in payloads** (M2 cross-cutting
   constraint): `Message.Metadata` and `IncomingMessage.Metadata`
   carry platform-specific fields (`channel_type`, `thread_ts`, …),
   never `deployment_id`, `environment`, `host`, `pod`, etc.

## Quick start (consumer)

```go
import (
    "context"

    "github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

func reply(ctx context.Context, ad messenger.Adapter) error {
    sub, err := ad.Subscribe(ctx, func(ctx context.Context, in messenger.IncomingMessage) error {
        _, sendErr := ad.SendMessage(ctx, in.ChannelID, messenger.Message{
            Text:     "ack",
            ThreadID: in.ID,
        })
        return sendErr
    })
    if err != nil {
        return err
    }
    defer func() { _ = sub.Stop() }()

    <-ctx.Done()
    return nil
}
```

## Out of scope (deferred)

- Slack implementation — see M4.2 (`messenger/slack`).
- Rate-limiting middleware — embedded per-platform (M4.2 rate limiter
  is Slack tier-2/tier-3 aware).
- Reactions / interactive components — added when concrete features
  demand them; `Message.Attachments` and metadata fields leave room.
- Capability-token enforcement — token issuance is `capability`
  (M3.5); wiring is M5 work.

## Test fake

`FakeMessenger` (in `fake_messenger_test.go`) is a hand-rolled
`Adapter` stand-in available to in-package tests. Adapter test suites
that want a portable harness can copy the pattern; the fake is
intentionally test-only (not exported from the package) to keep
production builds free of test scaffolding — mirrors the lifecycle /
outbox / keeperslog pattern documented in `docs/LESSONS.md`.

## References

- ROADMAP `docs/ROADMAP-phase1.md` § M4.1
- Pattern siblings: `core/pkg/keeperslog`, `core/pkg/outbox`,
  `core/pkg/capability`
