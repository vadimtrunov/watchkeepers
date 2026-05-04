package slack

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// usersProfileSetMethod is the Slack Web API method name SetBotProfile
// targets. Hoisted to a package constant so the rate-limiter registry
// (`defaultMethodTiers`) and the request path stay in sync via the
// compiler.
const usersProfileSetMethod = "users.profile.set"

// usersProfileSetRequest is the JSON envelope users.profile.set
// expects. Slack wraps the user-editable fields inside a `profile`
// object; the rest of the envelope (`name`, `value`, `user`) targets
// admin-side single-field updates we do not use in the bot-profile
// path.
type usersProfileSetRequest struct {
	Profile map[string]string `json:"profile"`
}

// recognisedProfileMetadataKeys is the closed set of Slack-specific
// profile fields M4.2.b forwards from [messenger.BotProfile.Metadata].
// Unknown keys are dropped at the adapter boundary — adapters consume
// what they recognise (M4.1 lesson). Callers that need a new key send
// a PR adding it here so the contract stays explicit.
//
// Slack documents `users.profile.set` profile object fields at
// https://api.slack.com/methods/users.profile.set. The keys below are
// the ones a bot identity reasonably sets at provisioning time.
var recognisedProfileMetadataKeys = []string{
	"status_emoji",
	"status_expiration",
	"real_name",
	"first_name",
	"last_name",
	"title",
	"phone",
	"pronouns",
}

// SetBotProfile updates the calling bot's profile fields per `profile`.
// Empty fields leave the existing values unchanged (per the
// [messenger.BotProfile] contract — "adapters do NOT clear on empty").
//
// Mapping (messenger.BotProfile → users.profile.set):
//
//   - profile.DisplayName → profile.display_name (omitted when empty)
//   - profile.StatusText  → profile.status_text  (omitted when empty)
//   - profile.AvatarPNG   → returns [messenger.ErrUnsupported]
//     (Slack avatar upload requires `users.setPhoto` with multipart
//     encoding — deferred to a follow-up PR; the contract reserves
//     the field rather than silently dropping it)
//   - profile.Metadata    → forwarded for the documented keys listed
//     in [recognisedProfileMetadataKeys]; other keys are dropped.
//
// An entirely-empty BotProfile (no DisplayName, no StatusText, no
// AvatarPNG, no Metadata) returns nil WITHOUT contacting the platform
// — there is nothing to update, and burning a tier-3 rate-limit token
// for a no-op would be wasteful.
//
// Slack `error` codes map per the existing [APIError.Unwrap] table:
//
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
//
// Codes without a portable mapping (invalid_profile,
// profile_set_failed) surface as [*APIError] with the Code field
// populated.
//
// Note (M4.2.b scope): no follow-up `bots.info` round-trip. The
// [messenger.Adapter.SetBotProfile] contract returns only an error
// (no Profile read-back), so reading the resulting bot identity is
// unnecessary. M4.2.d's [messenger.Adapter.LookupUser] will own
// `bots.info` when bot-user resolution becomes a feature requirement.
func (c *Client) SetBotProfile(ctx context.Context, profile messenger.BotProfile) error {
	if len(profile.AvatarPNG) > 0 {
		return fmt.Errorf("slack: %s: avatar: %w", usersProfileSetMethod, messenger.ErrUnsupported)
	}

	body := buildProfileBody(profile)
	if len(body) == 0 {
		return nil
	}

	req := usersProfileSetRequest{Profile: body}
	if err := c.Do(ctx, usersProfileSetMethod, req, nil); err != nil {
		return err
	}
	return nil
}

// buildProfileBody assembles the `profile` map sent to
// users.profile.set, applying the "empty leaves unchanged" contract:
// only fields the caller populated land on the wire. The map is nil
// when nothing changes (caller can short-circuit the API round-trip).
func buildProfileBody(p messenger.BotProfile) map[string]string {
	body := make(map[string]string, 4)
	if p.DisplayName != "" {
		body["display_name"] = p.DisplayName
	}
	if p.StatusText != "" {
		body["status_text"] = p.StatusText
	}
	for _, key := range recognisedProfileMetadataKeys {
		if v, ok := p.Metadata[key]; ok && v != "" {
			body[key] = v
		}
	}
	if len(body) == 0 {
		return nil
	}
	return body
}
