// Package peer provides the M1.3.* built-in peer.* tool family used
// by every Watchkeeper to talk to its peers over the K2K conversation
// domain.
//
// M1.3.a scope — this package ships:
//
//   - [Ask]: request-reply primitive. Resolves a `target` (watchkeeper
//     id or role name) over [keepclient.Client.ListPeers] from M1.2,
//     mints a K2K conversation via [k2k.Lifecycle.Open] from M1.1.c,
//     appends a `request`-direction [k2k.Message] via
//     [k2k.Repository.AppendMessage], and blocks until the matching
//     reply or `timeout` via [k2k.Repository.WaitForReply].
//   - [Reply]: the companion primitive that appends a
//     `reply`-direction [k2k.Message], signalling the waiting [Ask].
//   - Capability-broker gate: every call validates a per-tenant
//     capability token via [capability.Broker.ValidateForOrg] under
//     scope `peer:ask` / `peer:reply` BEFORE any K2K state mutation.
//     A failed gate surfaces as [ErrPeerCapabilityDenied] without
//     touching the conversation repository.
//   - [BuiltinAskManifest] / [BuiltinReplyManifest]: tool-registry
//     manifest values stamped with `Source: "built-in"` and the
//     matching capability ids, satisfying the M1.3.a AC's tool-
//     registry entry contract.
//
// M1.3.b adds:
//
//   - [Close]: lifecycle finalize. Composes [k2k.Lifecycle.Close]
//     (archives the Slack channel + transitions
//     `k2k_conversations.status` to `archived`) with the M1.3.b
//     `k2k.Repository.SetCloseSummary` write of a one-line operator
//     summary onto the row. Idempotent on double-close (preserves the
//     first close's summary) and recovers a half-completed close
//     (archive succeeded, summary write failed).
//   - [BuiltinCloseManifest]: tool-registry manifest entry stamped with
//     `Source: "built-in"` and the `peer:close` capability.
//
// M1.3.c adds:
//
//   - [Subscribe]: event-stream seam. Validates the supplied filter,
//     gates on `peer:subscribe` capability, and forwards to the
//     constructor-supplied [EventBus]. Returns a read-only delivery
//     channel + [CancelFunc]; the channel closes on ctx cancel or
//     CancelFunc invocation. Slow-consumer drop policy: events are
//     non-blocking-delivered into a bounded buffer; a full buffer
//     drops the event and increments the bus-wide drop counter.
//   - [EventBus] interface: Publish + Subscribe seam. In-memory
//     implementation [MemoryEventBus] for tests + dev loops; Postgres
//     LISTEN/NOTIFY implementation [PostgresEventBus] for production
//     wiring. The Postgres adapter uses migration `032_peer_events.sql`
//     (`peer_events` table + `peer_event_published` trigger).
//   - [BuiltinSubscribeManifest]: tool-registry manifest entry stamped
//     with `Source: "built-in"` and the `peer:subscribe` capability.
//
// What this package deliberately does NOT ship at M1.3.c:
//
//   - No `peer.Broadcast` fan-out — that is M1.3.d's scope (and
//     introduces the `peer.RoleFilter` resolver).
//   - No audit emission. The K2K event taxonomy
//     (`k2k_message_sent` / `k2k_conversation_opened` / etc.) lives
//     in M1.4's subscriber; this package is the call surface, not the
//     audit sink. A source-grep AC test pins this so a future
//     contributor adding inline audit emission inside `ask.go` /
//     `reply.go` trips a fast-failing test before the change can
//     ride out of review.
//
// Concurrency: every exported method on [Tool] is safe for concurrent
// use across goroutines. The wrapped [k2k.Repository] /
// [k2k.Lifecycle] / [capability.Broker] / [keepclient.Client] seams
// each document their own concurrency contract; this package adds no
// process-local state beyond the constructor-validated [Deps].
//
// Per-tenant pinning: every capability token issued for `peer:ask` /
// `peer:reply` is bound to the acting watchkeeper's organization id
// via [capability.Broker.IssueForOrg]; validation calls
// [capability.Broker.ValidateForOrg] with the same org so a token
// minted for tenant A cannot pivot to tenant B even when the scope
// matches. Mirrors the M3.5.a tenant-pin discipline.
//
// PII discipline: the `body` payload is treated as opaque bytes. The
// package never logs, returns, or embeds the resolved body in any
// error or diagnostic. Future M1.4 audit emission carries the
// redaction contract; this package's only PII responsibility is
// defensive deep-copy of the input + result bytes so caller-side
// mutation cannot bleed.
//
// See `docs/ROADMAP-phase2.md` §M1 → M1.3 → M1.3.a for the AC and
// `docs/lessons/M1.md` for the patterns settled in this PR.
package peer
