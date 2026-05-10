package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// conversationsOpenMethod is the Slack Web API method name
// OpenIMChannel targets. Hoisted to a package constant so the
// rate-limiter registry (`defaultMethodTiers`) and the request path
// stay in sync via the compiler.
const conversationsOpenMethod = "conversations.open"

// conversationsOpenRequest is the JSON envelope `conversations.open`
// expects for the one-on-one DM case. Multi-party DMs (the `users`
// CSV form) are intentionally NOT exposed — `fetch_watch_orders`
// only needs a 1:1 channel with the lead human.
type conversationsOpenRequest struct {
	Users           string `json:"users"`
	ReturnIM        bool   `json:"return_im,omitempty"`
	PreventCreation bool   `json:"prevent_creation,omitempty"`
}

// conversationsOpenResponse is the subset of the Slack response
// [Client.OpenIMChannel] decodes. The `channel.id` is the IM channel
// id callers pass to [Client.ConversationsHistory].
type conversationsOpenResponse struct {
	OK      bool                  `json:"ok"`
	Channel conversationsOpenChan `json:"channel"`
}

// conversationsOpenChan is the channel sub-object on a
// `conversations.open` success response.
type conversationsOpenChan struct {
	ID string `json:"id"`
}

// OpenIMChannel resolves `userID` to the 1:1 IM channel id between
// the calling bot and the human user. Slack auto-creates the channel
// the first time this is called; subsequent calls return the same id.
// The `prevent_creation` Slack knob is left at false because the
// Coordinator MUST be able to read the lead-DM channel even if it has
// not yet been used (first nudge ever).
//
// Empty userID returns [messenger.ErrUserNotFound] synchronously
// without contacting Slack — same fast-reject discipline as
// [Client.SendMessage].
//
// Slack `error` codes lifted to portable sentinels:
//
//   - user_not_found / users_not_found → [messenger.ErrUserNotFound]
//   - cannot_dm_bot                    → [ErrCannotDMBot]
//   - missing_scope                    → [ErrMissingScope]
//   - invalid_auth / not_authed        → [ErrInvalidAuth]
//   - token_expired                    → [ErrTokenExpired]
//   - ratelimited / HTTP 429           → [ErrRateLimited]
func (c *Client) OpenIMChannel(ctx context.Context, userID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if userID == "" {
		return "", fmt.Errorf(
			"slack: %s: %w", conversationsOpenMethod, messenger.ErrUserNotFound,
		)
	}

	req := conversationsOpenRequest{
		Users:    userID,
		ReturnIM: true,
	}

	var resp conversationsOpenResponse
	if err := c.Do(ctx, conversationsOpenMethod, req, &resp); err != nil {
		return "", liftOpenIMError(err)
	}
	return resp.Channel.ID, nil
}

// liftOpenIMError rewraps the documented Slack codes onto their
// portable counterparts so adapter callers can match without importing
// the slack package. Symmetric with [liftHistoryError] /
// [liftChannelNotFound] / [liftUserNotFound].
func liftOpenIMError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "user_not_found", "users_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrUserNotFound, err)
	case "cannot_dm_bot":
		return fmt.Errorf("%w: %w", ErrCannotDMBot, err)
	case "missing_scope":
		return fmt.Errorf("%w: %w", ErrMissingScope, err)
	}
	return err
}
