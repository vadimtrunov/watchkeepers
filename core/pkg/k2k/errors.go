package k2k

import "errors"

// ErrConversationNotFound is the typed error returned by
// [Repository.Get], [Repository.Close], and [Repository.IncTokens] when
// the supplied id does not match any row. Matchable via [errors.Is].
// Distinct from any underlying SQL miss (e.g. `pgx.ErrNoRows`) so the
// caller can branch on "unknown conversation" without parsing the
// underlying error chain. Mirrors `saga.ErrSagaNotFound`'s discipline
// (see `core/pkg/spawn/saga/dao.go`).
var ErrConversationNotFound = errors.New("k2k: conversation not found")

// ErrInvalidStatus is returned by [Status.Validate] when the value is
// outside the closed set ([StatusOpen], [StatusArchived]). The set is
// closed by design — the lifecycle transitions are open→archived only
// — so a typo in a future caller is a compile-time-shaped failure at
// the validator boundary rather than a silent default. Symmetric with
// the `SagaState` enum guard in
// `core/pkg/spawn/saga/dao.go`'s SQL CHECK constraint mirror.
var ErrInvalidStatus = errors.New("k2k: invalid status")

// ErrEmptySubject is returned by [Repository.Open] when the supplied
// subject is empty / whitespace-only. The subject is load-bearing for
// the M1.4 audit emission (`k2k_conversation_opened.subject`) and for
// the M1.1.b channel-name derivation; an empty subject would produce
// an unreadable audit row and a collision-prone channel name. Fail
// fast at the entry boundary, same discipline as M7.1.b's
// `ErrEmptyIdempotencyKey`.
var ErrEmptySubject = errors.New("k2k: empty subject")

// ErrEmptyParticipants is returned by [Repository.Open] when the
// supplied participants slice is empty (a 0-bot conversation is a
// degenerate state — at minimum the requesting bot belongs to it) OR
// contains an empty-string entry (a whitespace-only participant id is
// a caller bug — the M1.1.c wiring resolves slack user ids upstream
// and an empty value here would smuggle through the M1.1.b
// `InviteToChannel` call). The validator trims whitespace before the
// empty check so a `"   "` entry cannot bypass.
var ErrEmptyParticipants = errors.New("k2k: empty participants")

// ErrInvalidTokenBudget is returned by [Repository.Open] when the
// supplied token budget is negative. Zero is allowed (it disables the
// budget — M1.5 enforces "budget == 0 means no enforcement"); a
// negative value is a programmer bug. Catch it at the entry boundary
// so the row never lands in the store with a value the M1.5
// enforcement layer would have to special-case.
var ErrInvalidTokenBudget = errors.New("k2k: invalid token budget")

// ErrInvalidTokenDelta is returned by [Repository.IncTokens] when the
// supplied delta is non-positive. A zero or negative increment would
// either be a no-op (zero) or a credit (negative) — neither is part
// of the M1.5 contract, which is monotonic "tokens used so far".
// Mirrors the "fail-fast on degenerate inputs" discipline established
// in M9.4.a `Proposer.Submit`.
var ErrInvalidTokenDelta = errors.New("k2k: invalid token delta")

// ErrEmptyOrganization is returned by [Repository.Open] when the
// supplied `organizationID` is the zero UUID. The organization id is
// the RLS key for the row (migration 029's `watchkeeper.org` GUC
// policy); a zero value would either fail the FK to
// `watchkeeper.organization` (Postgres path) or silently land in the
// in-memory store with an unscoped row that no production query
// could ever look up. Fail-fast at the entry boundary.
var ErrEmptyOrganization = errors.New("k2k: empty organization id")

// ErrAlreadyArchived is returned by [Repository.Close] when the row
// is already in [StatusArchived]. The lifecycle transition is
// strictly open→archived; a second close attempt is the caller's bug
// (likely a double-fire of the M1.1.c lifecycle wiring's
// `ArchiveChannel` callback). Surfaced as a typed sentinel so the
// caller can distinguish "idempotent replay" from "fresh archive
// failure" — the M1.1.c wiring treats the sentinel as a silent OK
// per the saga-replay discipline.
var ErrAlreadyArchived = errors.New("k2k: conversation already archived")

// ErrEmptySlackChannelID is returned by [Repository.BindSlackChannel]
// when the supplied channel id is empty / whitespace-only. The M1.1.c
// lifecycle wiring only calls Bind after a successful
// `conversations.create`, which returns a non-empty platform-assigned
// id; an empty value at this boundary is a programmer bug (a forgotten
// error-check on the upstream Slack call).
var ErrEmptySlackChannelID = errors.New("k2k: empty slack channel id")

// ErrSlackChannelAlreadyBound is returned by
// [Repository.BindSlackChannel] when the target row already carries a
// non-empty `slack_channel_id`. The M1.1.c lifecycle wiring binds at
// most once per conversation; a second Bind indicates either a
// duplicate Open() with the same conversation id (impossible because
// the repository mints fresh ids) or a programmer bug in the consumer
// layer. The sentinel is distinct from [ErrAlreadyArchived] so the
// consumer can branch on "wrong lifecycle state" vs "stale
// post-create idempotent replay" without parsing the error chain.
var ErrSlackChannelAlreadyBound = errors.New("k2k: slack channel already bound")

// ErrInvalidMessageDirection is returned by [MessageDirection.Validate]
// when the value is outside the closed [MessageDirectionRequest] /
// [MessageDirectionReply] set. The set is closed by design — `peer.Ask`
// emits exactly one of the two values; a typo in a future caller
// surfaces at the validator boundary rather than as a silent default.
// Mirrors [ErrInvalidStatus]'s discipline.
var ErrInvalidMessageDirection = errors.New("k2k: invalid message direction")

// ErrEmptyConversationID is returned by [Repository.AppendMessage] and
// [Repository.WaitForReply] when the supplied conversation id is the
// zero UUID. A zero value would smuggle past the FK at the SQL layer
// (every uuid serialises) but produce an orphan message visible to no
// consumer; fail-fast at the entry boundary mirrors the
// [ErrEmptyOrganization] discipline.
var ErrEmptyConversationID = errors.New("k2k: empty conversation id")

// ErrEmptySenderWatchkeeperID is returned by [Repository.AppendMessage]
// when the supplied sender id is empty / whitespace-only. The acting
// watchkeeper id is load-bearing for the M1.4 audit emission
// (`k2k_message_sent.sender_watchkeeper_id`) and for the M1.3.b
// participant-membership gate; an empty sender would produce an
// unattributable message. Fail fast at the entry boundary, same
// discipline as [ErrEmptyParticipants].
var ErrEmptySenderWatchkeeperID = errors.New("k2k: empty sender watchkeeper id")

// ErrEmptyMessageBody is returned by [Repository.AppendMessage] when
// the supplied body slice is empty (nil or zero-length). A zero-byte
// payload is a degenerate state — `peer.Ask` always carries a request
// body; `peer.Reply` always carries a response body. The
// [Repository.AppendMessage] doc-block forbids smuggling an empty
// payload past the validator to mark a conversation closed; M1.3.b's
// `peer.Close` is the canonical path for that.
var ErrEmptyMessageBody = errors.New("k2k: empty message body")

// ErrInvalidWaitTimeout is returned by [Repository.WaitForReply] when
// the supplied timeout is non-positive. A non-positive timeout would
// either be a no-op (zero) or a logic bug (negative); both are caller
// bugs. The peer-tool layer's `peer.Ask(timeout=0)` semantic for fire-
// and-forget broadcasts is resolved BEFORE the WaitForReply call (no
// wait happens at all), so the repository never sees a 0 timeout.
var ErrInvalidWaitTimeout = errors.New("k2k: invalid wait timeout")

// ErrWaitForReplyTimeout is returned by [Repository.WaitForReply] when
// no `reply`-direction row appears in the requested conversation
// before the timeout elapses. The peer-tool layer translates this
// sentinel into `peer.ErrPeerTimeout`; the repository sentinel exists
// so callers driving the seam directly (e.g. an M1.6 escalation saga
// step) can branch on errors.Is without depending on the peer package.
var ErrWaitForReplyTimeout = errors.New("k2k: wait for reply timeout")

// ErrConversationNotArchived is returned by [Repository.SetCloseSummary]
// when the target row is still in [StatusOpen]. The M1.3.b `peer.Close`
// writer is sequenced strictly after `Lifecycle.Close`, so a
// non-archived row at this surface is a programmer bug at the call
// site (a forgotten Close call) rather than a legitimate concurrent
// state. The sentinel is distinct from [ErrAlreadyArchived] so the
// caller can distinguish "wrong direction" from "duplicate close"
// without parsing the error chain. Mirrors the [ErrAlreadyArchived]
// discipline at the inverse state.
var ErrConversationNotArchived = errors.New("k2k: conversation not archived")
