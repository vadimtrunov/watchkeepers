package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// This file implements the channel-lifecycle primitives the M1.1.b
// K2K conversation domain consumes. Four method shapes ride on the
// existing [Client]:
//
//   - [Client.CreateChannel]  — `conversations.create` with idempotent
//     `name_taken` recovery via `conversations.list`.
//   - [Client.InviteToChannel] — `conversations.invite` for bot fan-out
//     at K2K Open() time. Idempotent on `already_in_channel`.
//   - [Client.ArchiveChannel]  — `conversations.archive` for K2K Close()
//     time. Idempotent on `already_archived`.
//   - [Client.RevealChannel]   — `conversations.invite` for the
//     `wk channel reveal <id>` CLI; single-user form keyed off the
//     caller's Slack user id.
//
// Slack `error` codes are lifted onto the package-local sentinels
// declared in errors.go via [APIError.Unwrap]; the K2K consumer matches
// on `errors.Is` rather than parsing strings. Idempotent recoveries
// (`name_taken`, `already_archived`, `already_in_channel`) are
// translated to success at this layer so callers do NOT branch on those
// sentinels — they are exported defensively for low-level [Client.Do]
// consumers only.
//
// PII discipline: channel ids and user ids ARE workspace-public
// identifiers (Slack documents them as opaque non-secret values that
// surface in event payloads and link URLs). The [Client]'s redaction
// discipline keeps them out of log entries by default — the optional
// [Logger] receives only the method name, HTTP status, and error code.
// Channel names supplied by callers are NEVER logged (a K2K channel
// name like `k2k-<conv-uuid-prefix>` could correlate to a tenant
// observable from elsewhere; we treat it as caller-private).

// conversationsCreateMethod is the Slack Web API method name
// CreateChannel targets. Hoisted to a package constant so the
// rate-limiter registry (`defaultMethodTiers`) and the request path
// stay in sync via the compiler.
const conversationsCreateMethod = "conversations.create"

// conversationsInviteMethod is the Slack Web API method name
// InviteToChannel and RevealChannel target.
const conversationsInviteMethod = "conversations.invite"

// conversationsArchiveMethod is the Slack Web API method name
// ArchiveChannel targets.
const conversationsArchiveMethod = "conversations.archive"

// conversationsListMethod is the Slack Web API method name the
// idempotent name_taken recovery path inside CreateChannel falls back
// to. The method is documented at https://api.slack.com/methods/conversations.list.
const conversationsListMethod = "conversations.list"

// conversationsListPageLimit caps the per-page request size used by
// the [Client.CreateChannel] name_taken recovery path. 1000 is Slack's
// documented maximum and keeps the worst-case page count bounded for
// workspaces with O(10^4) channels.
const conversationsListPageLimit = 1000

// conversationsCreateRequest is the JSON envelope `conversations.create`
// expects. `name` is required; `is_private` selects between a public
// channel (false) and a private channel (true). M1.1.b's K2K flow
// always passes `is_private=true`; the parameter is surfaced so the
// adapter stays general-purpose for future callers.
type conversationsCreateRequest struct {
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private,omitempty"`
}

// conversationsCreateResponse is the subset of the Slack response
// [Client.CreateChannel] decodes. Only the channel id is consumed; the
// rest of the rich `channel` object (topic, purpose, member_count, …)
// is irrelevant for the primitive's contract.
type conversationsCreateResponse struct {
	OK      bool                    `json:"ok"`
	Channel conversationsCreateChan `json:"channel"`
}

// conversationsCreateChan is the channel sub-object on a
// `conversations.create` success response.
type conversationsCreateChan struct {
	ID string `json:"id"`
}

// conversationsInviteRequest is the JSON envelope `conversations.invite`
// expects. `users` is a comma-separated list of Slack user ids (Slack
// accepts up to 1000 at once; the K2K consumer keeps the slice short).
type conversationsInviteRequest struct {
	Channel string `json:"channel"`
	Users   string `json:"users"`
}

// conversationsInviteResponse is the subset of the
// `conversations.invite` envelope decoded. The success body re-emits
// the full channel object, which is ignored — the contract surfaces
// success / failure only.
type conversationsInviteResponse struct {
	OK bool `json:"ok"`
}

// conversationsArchiveRequest is the JSON envelope `conversations.archive`
// expects. `channel` is the channel id (Slack rejects channel-name
// strings on this endpoint).
type conversationsArchiveRequest struct {
	Channel string `json:"channel"`
}

// conversationsArchiveResponse is the subset of the
// `conversations.archive` envelope decoded. Success body is empty
// beyond the `ok` field.
type conversationsArchiveResponse struct {
	OK bool `json:"ok"`
}

// conversationsListRequest is the JSON envelope `conversations.list`
// expects for the name-taken recovery path. Only the fields the
// recovery path needs are populated; Slack accepts a richer query
// (`team_id`, `exclude_archived`, …) we deliberately leave at defaults.
// Archived channels ARE returned by Slack's default
// (`exclude_archived=false`), and the response carries the
// `is_archived` flag per entry; [findChannelByName] uses that flag to
// SKIP archived hits (iter-1 codex P2). We could equivalently send
// `exclude_archived=true` and let Slack filter server-side; we filter
// client-side so the wire shape matches `conversations.list`'s
// "what would conversations.create's namespace-collision see" view
// for diagnostic logs.
type conversationsListRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	// Types is a comma-separated list of channel kinds. M1.1.b's
	// CreateChannel always operates on private channels, so the
	// recovery path queries `private_channel` only. Public channels
	// share the workspace name namespace, so a name_taken collision
	// against a public channel surfaces as a tier-1 ErrChannelNameTaken
	// rather than a successful idempotent resolution — by design, the
	// K2K consumer must not silently bind to a public channel of the
	// same name.
	Types string `json:"types,omitempty"`
}

// conversationsListResponse is the subset of the `conversations.list`
// envelope decoded by the name-taken recovery path. Each entry carries
// only the id + name + is_archived fields; richer attributes are
// ignored.
type conversationsListResponse struct {
	OK               bool             `json:"ok"`
	Channels         []conversationID `json:"channels"`
	ResponseMetadata responseMetadata `json:"response_metadata"`
}

// conversationID is one entry in a `conversations.list` response.
type conversationID struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsArchived bool   `json:"is_archived"`
}

// CreateChannel provisions a Slack channel named `name`. When
// `isPrivate` is true the channel is created as a private channel
// (visible only to invited members); otherwise it is a public channel
// joinable workspace-wide. Returns the platform-assigned channel id on
// success.
//
// Idempotency contract (M1.1.b AC): if a NON-ARCHIVED channel with
// the requested name already exists, CreateChannel transparently
// resolves and returns its existing id rather than surfacing
// [ErrChannelNameTaken]. The resolution path uses
// `conversations.list` filtered to the same `types` (private vs
// public) as the original request, paginated via the documented
// `response_metadata.next_cursor` cursor. Archived same-name same-kind
// hits are SKIPPED — Slack still rejects `conversations.create` with
// `name_taken` against an archived holder, but binding the K2K
// consumer to an archived channel id would surface as
// [ErrIsArchived] on the very next `InviteToChannel` (iter-1 codex
// P2). When the recovery path finds only archived matches the
// composite [ErrChannelNameTaken] surfaces wrapped with the
// "name X not found among non-archived <kind> entries" diagnostic
// so the caller can pick a fresh name.
//
// Empty `name` returns [ErrInvalidChannelName] synchronously without
// contacting Slack — same fast-reject discipline as
// [Client.OpenIMChannel].
//
// Slack `error` codes lifted to package sentinels:
//
//   - name_taken                → transparent recovery via
//     conversations.list (no error surfaces); if recovery fails to
//     find the channel, the original [ErrChannelNameTaken] surfaces
//     wrapped with the recovery diagnostic.
//   - invalid_name / invalid_name_*  → [ErrInvalidChannelName]
//   - missing_scope             → [ErrMissingScope]
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
//
// The idempotent recovery path itself burns a tier-2 rate-limit token
// on `conversations.list`; callers MUST size their rate-limit budget
// accordingly when the K2K lifecycle may collide with operator-created
// channels of the same name.
func (c *Client) CreateChannel(ctx context.Context, name string, isPrivate bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf(
			"slack: %s: %w", conversationsCreateMethod, ErrInvalidChannelName,
		)
	}

	req := conversationsCreateRequest{
		Name:      trimmed,
		IsPrivate: isPrivate,
	}

	var resp conversationsCreateResponse
	err := c.Do(ctx, conversationsCreateMethod, req, &resp)
	if err == nil {
		if resp.Channel.ID == "" {
			return "", fmt.Errorf(
				"slack: %s: response missing channel id", conversationsCreateMethod,
			)
		}
		return resp.Channel.ID, nil
	}

	// Idempotent recovery on name_taken: fall back to conversations.list
	// filtered to the same channel-kind as the original request and
	// return the existing id. Any other code surfaces unmodified
	// (after sentinel lifting).
	if errors.Is(err, ErrChannelNameTaken) {
		id, lookupErr := c.findChannelByName(ctx, trimmed, isPrivate)
		if lookupErr != nil {
			// Surface BOTH the original name_taken sentinel and the
			// recovery diagnostic so the caller can distinguish "name
			// collision but we couldn't recover" from "name collision
			// recovered successfully". The wrapped chain remains
			// matchable via errors.Is(ErrChannelNameTaken).
			return "", fmt.Errorf(
				"slack: %s: name_taken recovery via %s failed: %w (original: %w)",
				conversationsCreateMethod, conversationsListMethod, lookupErr, err,
			)
		}
		return id, nil
	}

	return "", liftCreateChannelError(err)
}

// InviteToChannel invites the supplied Slack user ids to `channelID`.
// Mirrors the K2K Open() fan-out path: the caller hands in the bot
// user-ids to invite into a freshly-created private channel.
//
// Idempotency contract: an `already_in_channel` response is translated
// to a NON-error success ONLY when the call was a SINGLE-user invite
// (after the whitespace-trim performed by [joinUserIDs] reduces the
// post-trim user count to one). For multi-user batches the translation
// is UNSAFE — Slack's `conversations.invite` is all-or-nothing per
// call: when ANY user in a multi-user batch is already a member,
// Slack returns `already_in_channel` AND does NOT invite the other
// listed users (iter-1 codex P1). Silencing that to nil would let
// the K2K Open() fan-out report success while leaving some bots
// outside the channel. In the multi-user case the original
// [ErrAlreadyInChannel] sentinel surfaces wrapped so the caller can
// retry per-user.
//
// Empty `channelID` returns [messenger.ErrChannelNotFound]
// synchronously. Empty `userIDs` slice is a no-op (returns nil without
// contacting Slack).
//
// Slack `error` codes lifted:
//
//   - channel_not_found         → [messenger.ErrChannelNotFound]
//   - is_archived               → [ErrIsArchived]
//   - already_in_channel        → translated to success ONLY when the
//     post-trim batch size is 1; surfaces as [ErrAlreadyInChannel]
//     for multi-user batches.
//   - cant_invite_self          → [ErrCantInviteSelf]
//   - user_not_found / users_not_found → [messenger.ErrUserNotFound]
//   - cannot_dm_bot             → [ErrCannotDMBot]
//   - missing_scope             → [ErrMissingScope]
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
func (c *Client) InviteToChannel(ctx context.Context, channelID string, userIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if channelID == "" {
		return fmt.Errorf(
			"slack: %s: %w", conversationsInviteMethod, messenger.ErrChannelNotFound,
		)
	}
	if len(userIDs) == 0 {
		return nil
	}

	// joinUserIDs trims whitespace and drops empty entries; we use the
	// returned post-trim count to decide whether `already_in_channel`
	// is safe to translate to nil (single-user batches only).
	csv, trimmedCount := joinUserIDs(userIDs)
	if csv == "" {
		// Every entry was whitespace. Mirrors the fail-fast discipline
		// from M1.1.a — a `"   "` smuggled into the invite list is a
		// caller bug, not a Slack request.
		return fmt.Errorf(
			"slack: %s: %w", conversationsInviteMethod, messenger.ErrUserNotFound,
		)
	}

	req := conversationsInviteRequest{
		Channel: channelID,
		Users:   csv,
	}

	var resp conversationsInviteResponse
	if err := c.Do(ctx, conversationsInviteMethod, req, &resp); err != nil {
		// `already_in_channel` is safe to translate to nil ONLY when
		// the batch was a single user — Slack's all-or-nothing
		// semantics on multi-user invites mean a multi-user
		// `already_in_channel` indicates the OTHER users were NOT
		// invited (iter-1 codex P1 fix).
		if trimmedCount == 1 && errors.Is(err, ErrAlreadyInChannel) {
			return nil
		}
		return liftInviteError(err)
	}
	return nil
}

// ArchiveChannel archives `channelID`, hiding it from member channel
// lists. Mirrors the K2K Close() path: once a conversation is
// resolved, the K2K consumer archives the underlying Slack channel so
// the workspace UI stays tidy. The channel and its history remain
// readable to anyone who held membership at archive time; archival is
// not a delete.
//
// Idempotency contract: an `already_archived` response is translated
// to a NON-error success. The caller can call ArchiveChannel
// unconditionally on Close() without first probing channel state.
//
// Empty `channelID` returns [messenger.ErrChannelNotFound]
// synchronously.
//
// Slack `error` codes lifted:
//
//   - channel_not_found         → [messenger.ErrChannelNotFound]
//   - already_archived          → translated to success (no error)
//   - missing_scope             → [ErrMissingScope]
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
func (c *Client) ArchiveChannel(ctx context.Context, channelID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if channelID == "" {
		return fmt.Errorf(
			"slack: %s: %w", conversationsArchiveMethod, messenger.ErrChannelNotFound,
		)
	}

	req := conversationsArchiveRequest{Channel: channelID}

	var resp conversationsArchiveResponse
	if err := c.Do(ctx, conversationsArchiveMethod, req, &resp); err != nil {
		// already_archived is the idempotent-close success path.
		if errors.Is(err, ErrAlreadyArchived) {
			return nil
		}
		return liftArchiveError(err)
	}
	return nil
}

// RevealChannel reveals `channelID` to the supplied human Slack
// `userID`. Mirrors the `wk channel reveal <conv-id>` CLI path: a
// human opting into a K2K conversation by their own user id.
//
// Implementation: thin wrapper over `conversations.invite` with a
// single-user payload. Distinct from [Client.InviteToChannel] only in
// argument shape (single id vs slice) and intent (human reveal vs bot
// fan-out) — the semantic split mirrors the M1.1 AC vocabulary so the
// M1.1.c lifecycle layer keeps a 1:1 call-site → primitive mapping.
//
// Idempotency contract: an `already_in_channel` response is translated
// to a NON-error success. A user who is already a member of the
// channel can re-run `wk channel reveal` without an error.
//
// Empty `channelID` returns [messenger.ErrChannelNotFound]; empty
// `userID` returns [messenger.ErrUserNotFound]. Both fail
// synchronously without contacting Slack.
//
// Slack `error` codes lifted: identical to [Client.InviteToChannel].
func (c *Client) RevealChannel(ctx context.Context, channelID, userID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if channelID == "" {
		return fmt.Errorf(
			"slack: %s: %w", conversationsInviteMethod, messenger.ErrChannelNotFound,
		)
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf(
			"slack: %s: %w", conversationsInviteMethod, messenger.ErrUserNotFound,
		)
	}

	// Single-user delegation through the existing primitive keeps the
	// liftInviteError table in one place. The defensive joinUserIDs
	// trim handles the `"   "` smuggle case symmetrically.
	return c.InviteToChannel(ctx, channelID, []string{userID})
}

// findChannelByName is the idempotent recovery path used by
// [Client.CreateChannel] on `name_taken`. It pages through
// `conversations.list` filtered to the same channel-kind as the
// original request and returns the id of the first NON-ARCHIVED entry
// whose name matches `name` (Slack channel names are unique within
// their kind PLUS archive state — an archived channel can hold a
// name that a freshly-created non-archived channel of the same kind
// would also accept, but Slack's `conversations.create` still
// returns `name_taken` against the archived holder).
//
// Archived matches are EXPLICITLY SKIPPED (iter-1 codex P2 fix):
// returning an archived channel id from CreateChannel's idempotent
// path would silently bind the K2K consumer to a channel that
// subsequent `InviteToChannel` calls would reject with
// [ErrIsArchived]. The K2K Open() workflow needs a usable channel,
// not a stale name-collision hit.
//
// Returns the empty string + a wrapped error when:
//
//   - the lookup itself fails (transport, auth, rate limit, …); or
//   - the page count is exhausted without a NON-ARCHIVED match (the
//     workspace has drifted between the create-call and the recovery
//     list-call, the original name_taken referred to a kind we did
//     not query, OR every same-name same-kind hit is archived and
//     therefore unusable).
//
// In all three failure cases the caller [Client.CreateChannel]
// surfaces the composite error per its documented contract.
func (c *Client) findChannelByName(ctx context.Context, name string, isPrivate bool) (string, error) {
	kinds := "public_channel"
	if isPrivate {
		kinds = "private_channel"
	}

	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		req := conversationsListRequest{
			Limit:  conversationsListPageLimit,
			Cursor: cursor,
			Types:  kinds,
		}
		var resp conversationsListResponse
		if err := c.Do(ctx, conversationsListMethod, req, &resp); err != nil {
			return "", liftListError(err)
		}
		for _, ch := range resp.Channels {
			if ch.Name != name {
				continue
			}
			if ch.IsArchived {
				// Archived same-name same-kind hit — Slack still
				// rejects `conversations.create` with `name_taken`
				// against it, but the K2K consumer cannot use an
				// archived channel. Skip and keep scanning; if no
				// non-archived match surfaces by the end of pagination
				// the caller surfaces a composite error so the K2K
				// consumer can pick a fresh name (iter-1 codex P2).
				continue
			}
			return ch.ID, nil
		}
		cursor = resp.ResponseMetadata.NextCursor
		if cursor == "" {
			break
		}
	}
	return "", fmt.Errorf(
		"slack: %s: name %q not found among non-archived %s entries", conversationsListMethod, name, kinds,
	)
}

// joinUserIDs is the trim-and-join helper InviteToChannel /
// RevealChannel use to build the `users` CSV field. Whitespace-only
// entries are dropped (the K2K consumer's `"   "`-smuggle case
// mirrors the M1.1.a `participants` validator). Returns the joined
// CSV plus the post-trim entry count so the caller can branch on
// single-vs-multi-user batch shape (the safety of translating
// `already_in_channel` to nil depends on the batch size — iter-1
// codex P1). Returns ("", 0) when every entry trims to empty so the
// caller can fail-fast with [messenger.ErrUserNotFound] without
// burning a Slack call.
func joinUserIDs(ids []string) (string, int) {
	if len(ids) == 0 {
		return "", 0
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, ","), len(parts)
}

// liftCreateChannelError rewraps the documented Slack codes onto the
// package sentinels for [Client.CreateChannel] callers. Symmetric with
// [liftHistoryError] / [liftInviteError] / [liftArchiveError].
//
// Codes already lifted on [APIError.Unwrap] (e.g. invalid_auth) ride
// through the [errors.Is] chain without a per-method wrap — the
// per-method lift is used only when a Slack code maps to a portable
// [messenger.*] sentinel that adapter callers should match without
// importing the slack package, OR when the wrap adds Slack-method
// context that callers need at the boundary.
func liftCreateChannelError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "invalid_name",
		"invalid_name_punctuation",
		"invalid_name_required",
		"invalid_name_specials",
		"invalid_name_maxlength":
		return fmt.Errorf("%w: %w", ErrInvalidChannelName, err)
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}

// liftInviteError rewraps documented codes for [Client.InviteToChannel]
// / [Client.RevealChannel] callers.
func liftInviteError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "channel_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrChannelNotFound, err)
	case "is_archived":
		return fmt.Errorf("%w: %w", ErrIsArchived, err)
	case "cant_invite_self":
		return fmt.Errorf("%w: %w", ErrCantInviteSelf, err)
	case "user_not_found", "users_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrUserNotFound, err)
	case "cannot_dm_bot":
		return fmt.Errorf("%w: %w", ErrCannotDMBot, err)
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}

// liftArchiveError rewraps documented codes for [Client.ArchiveChannel]
// callers.
func liftArchiveError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "channel_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrChannelNotFound, err)
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}

// liftListError rewraps documented codes for the
// [Client.CreateChannel] name_taken recovery path's
// `conversations.list` round-trip.
func liftListError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}
