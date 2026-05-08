// Package spawn implements the M6.1.b core-owned privileged RPC that
// wraps the existing [messenger.Adapter.CreateApp] flow with: claim
// authority validation, lead-approval token gate, secret-source
// credential reads, and a 2-event keepers_log audit chain emitted
// BEFORE and AFTER the privileged call.
//
// The package name is `spawn` because M6.2 (`list_watchkeepers`,
// `propose_spawn`, …) and M6.3 (operator surface / Slack DMs) will
// land additional spawn-side privileged RPCs here. M6.1.b ships the
// first one — `SlackAppRPC` — with the audit + auth pattern future
// callers reuse.
package spawn

import "errors"

// ErrUnauthorized is returned by [SlackAppRPC.CreateApp] when the
// supplied [Claim] does not carry the
// `slack_app_create == "lead_approval"` entry in its
// [Claim.AuthorityMatrix]. The Slack adapter is NOT touched and NO
// keepers_log event is written on this path — the call site simply
// is not allowed to ask. Matchable via [errors.Is].
//
// "lead_approval" is the only value that satisfies the gate; any
// other value (`"allowed"`, `"forbidden"`, an absent entry, an
// unknown enum value) fails closed. The value vocabulary mirrors the
// M5.5.b.c.c.b authority-matrix enum and the M6.1.a Watchmaster
// manifest seed.
var ErrUnauthorized = errors.New("spawn: unauthorized: claim lacks slack_app_create=lead_approval")

// ErrApprovalRequired is returned by [SlackAppRPC.CreateApp] when the
// claim is otherwise authorised but the request did not carry a
// non-empty [CreateAppRequest.ApprovalToken]. M6.1.b treats the
// token as opaque (any non-empty string passes); M6.3 owns its
// cryptography and lifecycle. The Slack adapter is NOT touched and
// NO keepers_log event is written on this path. Matchable via
// [errors.Is].
var ErrApprovalRequired = errors.New("spawn: approval required: empty approval_token")

// ErrInvalidClaim is returned by [SlackAppRPC.CreateApp] when the
// supplied [Claim] fails synchronous shape validation — currently:
// empty [Claim.OrganizationID] (the M3.5.a tenant-scoping
// discipline). The Slack adapter is NOT touched and NO keepers_log
// event is written on this path. Matchable via [errors.Is].
var ErrInvalidClaim = errors.New("spawn: invalid claim")

// ErrInvalidRequest is returned by [SlackAppRPC.CreateApp] when the
// supplied [CreateAppRequest] fails synchronous shape validation —
// currently: empty [CreateAppRequest.AgentID] or
// [CreateAppRequest.AppName]. The Slack adapter is NOT touched and
// NO keepers_log event is written on this path. Matchable via
// [errors.Is].
var ErrInvalidRequest = errors.New("spawn: invalid request")
