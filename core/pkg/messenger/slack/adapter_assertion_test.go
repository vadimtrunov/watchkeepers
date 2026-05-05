package slack

import "github.com/vadimtrunov/watchkeepers/core/pkg/messenger"

// Compile-time assertion that [*Client] satisfies the full portable
// [messenger.Adapter] interface. M4.2's five sub-bullets land exactly
// the six methods the interface requires:
//
//   - SendMessage    — M4.2.b (chat.postMessage)
//   - Subscribe      — M4.2.c (Socket Mode)
//   - CreateApp      — M4.2.d.1 (apps.manifest.create)
//   - LookupUser     — M4.2.d.1 (users.info / bots.info / users.lookupByEmail)
//   - SetBotProfile  — M4.2.b (users.profile.set)
//   - InstallApp     — M4.2.d.2 (oauth.v2.access — this PR)
//
// This declaration lives in a `*_test.go` file so the production binary
// does not pay the (zero-cost in this case) interface-assertion symbol;
// the compiler still type-checks the conformance during `go test`.
var _ messenger.Adapter = (*Client)(nil)
