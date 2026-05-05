package slack

import (
	"context"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// usersInfoMethod / botsInfoMethod / usersLookupByEmailMethod are the
// Slack Web API method names LookupUser routes to depending on the
// supplied [messenger.UserQuery] discriminator. Hoisted to package
// constants so the rate-limiter registry (`defaultMethodTiers`) and
// the request path stay in sync via the compiler.
const (
	usersInfoMethod          = "users.info"
	botsInfoMethod           = "bots.info"
	usersLookupByEmailMethod = "users.lookupByEmail"
)

// usersInfoRequest is the JSON envelope users.info expects. Slack
// accepts the `user` parameter as a query-string OR as a JSON-body
// field; we POST the JSON form to mirror the rest of the package's
// calling convention (Client.Do POSTs JSON).
type usersInfoRequest struct {
	User          string `json:"user"`
	IncludeLocale bool   `json:"include_locale,omitempty"`
}

// botsInfoRequest is the JSON envelope bots.info expects.
type botsInfoRequest struct {
	Bot string `json:"bot"`
}

// usersLookupByEmailRequest is the JSON envelope users.lookupByEmail
// expects.
type usersLookupByEmailRequest struct {
	Email string `json:"email"`
}

// usersInfoResponse is the subset of the Slack response LookupUser
// decodes for the users.info / users.lookupByEmail paths. The full
// response carries dozens of fields; we map only the cross-platform
// User spine plus a curated Metadata bag. Slack-specific extensions
// (team_id, tz, color, two_factor_type, …) ride in Metadata per the
// M4.1 LESSON.
type usersInfoResponse struct {
	OK   bool          `json:"ok"`
	User slackUserInfo `json:"user"`
}

// slackUserInfo is the user record nested inside [usersInfoResponse].
// Phase-1 maps id, name, real_name, is_bot, profile.display_name,
// profile.email, team_id, tz; everything else lives behind a future
// Metadata key registration.
type slackUserInfo struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	RealName string           `json:"real_name"`
	IsBot    bool             `json:"is_bot"`
	TeamID   string           `json:"team_id"`
	TZ       string           `json:"tz"`
	Profile  slackUserProfile `json:"profile"`
}

// slackUserProfile is the profile sub-object on a users.info response.
// Phase-1 maps display_name + email; the rest (status_text,
// status_emoji, image_72, …) is available through a future Metadata
// key registration if downstream features need it.
type slackUserProfile struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

// botsInfoResponse is the subset of the Slack response LookupUser
// decodes for the bots.info path. Slack's bot record is shaped
// differently from the user record (no profile sub-object; flat
// `name` + `app_id` + `user_id` fields) so we decode into a
// dedicated struct and map onto the portable User shape inside
// LookupUser.
type botsInfoResponse struct {
	OK  bool         `json:"ok"`
	Bot slackBotInfo `json:"bot"`
}

// slackBotInfo is the bot record nested inside [botsInfoResponse].
// `app_id` and `user_id` ride in [messenger.User.Metadata] under
// Slack-specific keys (`app_id`, `bot_user_id`) because the portable
// [messenger.User] does not carry them.
type slackBotInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	AppID  string `json:"app_id"`
	UserID string `json:"user_id"`
}

// LookupUser resolves `query` to a [messenger.User] record. Exactly
// one of [messenger.UserQuery.ID] / [messenger.UserQuery.Email] /
// [messenger.UserQuery.Handle] must be populated; otherwise returns
// [messenger.ErrInvalidQuery] synchronously.
//
// Routing (messenger.UserQuery → Slack method):
//
//   - Query.ID with prefix `U` or `W` → users.info
//   - Query.ID with prefix `B`        → bots.info
//   - Query.ID with any other prefix  → [messenger.ErrInvalidQuery]
//   - Query.Email                     → users.lookupByEmail
//   - Query.Handle                    → [messenger.ErrUnsupported]
//     (Slack does not expose a `users.info`-by-handle endpoint;
//     callers that have only an @handle must page users.list to
//     resolve it — a separate concern. The adapter deflects the
//     field rather than silently doing the wrong thing.)
//
// Slack `error` codes map per the existing [APIError.Unwrap] table
// plus the lookup-specific extensions below:
//
//   - user_not_found / users_not_found / bot_not_found
//     → [messenger.ErrUserNotFound]
//   - invalid_auth / not_authed    → [ErrInvalidAuth]
//   - token_expired                → [ErrTokenExpired]
//   - ratelimited / HTTP 429       → [ErrRateLimited]
//   - other codes                  → [*APIError] with Code populated
func (c *Client) LookupUser(ctx context.Context, query messenger.UserQuery) (messenger.User, error) {
	// ctx cancellation takes precedence over input-shape validation —
	// matches the M4.2.b/c.1 discipline.
	if err := ctx.Err(); err != nil {
		return messenger.User{}, err
	}
	method, body, err := routeUserQuery(query)
	if err != nil {
		return messenger.User{}, err
	}

	switch method {
	case usersInfoMethod:
		return c.lookupUserViaInfo(ctx, method, body.(usersInfoRequest))
	case usersLookupByEmailMethod:
		return c.lookupUserViaInfo(ctx, method, body)
	case botsInfoMethod:
		return c.lookupBot(ctx, body.(botsInfoRequest))
	}

	// Unreachable in practice — routeUserQuery guarantees one of the
	// above. Defensive return for the compiler.
	return messenger.User{}, fmt.Errorf("slack: lookupUser: unrouted query")
}

// routeUserQuery validates `query` and selects the Slack method + the
// matching request envelope. Returns an error when the query is
// empty, over-populated, or carries an ID with an unrecognised prefix.
//
// The returned request envelope is typed `any` so the caller can
// dispatch on the method name; Go's lack of sum types makes this the
// least-bad shape.
func routeUserQuery(q messenger.UserQuery) (string, any, error) {
	populated := 0
	if q.ID != "" {
		populated++
	}
	if q.Handle != "" {
		populated++
	}
	if q.Email != "" {
		populated++
	}
	if populated == 0 {
		return "", nil, fmt.Errorf("slack: lookupUser: %w", messenger.ErrInvalidQuery)
	}
	if populated > 1 {
		return "", nil, fmt.Errorf("slack: lookupUser: %w (over-populated)", messenger.ErrInvalidQuery)
	}

	if q.Handle != "" {
		return "", nil, fmt.Errorf("slack: lookupUser: handle: %w", messenger.ErrUnsupported)
	}
	if q.Email != "" {
		return usersLookupByEmailMethod, usersLookupByEmailRequest{Email: q.Email}, nil
	}

	// Discriminate on Slack's documented id-prefix family.
	switch q.ID[0] {
	case 'U', 'W':
		return usersInfoMethod, usersInfoRequest{User: q.ID}, nil
	case 'B':
		return botsInfoMethod, botsInfoRequest{Bot: q.ID}, nil
	default:
		return "", nil, fmt.Errorf("slack: lookupUser: id prefix %q: %w", string(q.ID[0]), messenger.ErrInvalidQuery)
	}
}

// lookupUserViaInfo handles both users.info and users.lookupByEmail —
// they share the same response shape (a `user` sub-object on
// success). Splits out so the bots.info path can have its own
// dedicated decoder without an `if method ==` ladder.
func (c *Client) lookupUserViaInfo(ctx context.Context, method string, body any) (messenger.User, error) {
	var resp usersInfoResponse
	if err := c.Do(ctx, method, body, &resp); err != nil {
		return messenger.User{}, liftUserNotFound(err)
	}
	return userFromSlackInfo(resp.User), nil
}

// lookupBot handles the bots.info path. Slack's bot record is shaped
// differently from the user record so the decoder + mapper are
// dedicated.
func (c *Client) lookupBot(ctx context.Context, body botsInfoRequest) (messenger.User, error) {
	var resp botsInfoResponse
	if err := c.Do(ctx, botsInfoMethod, body, &resp); err != nil {
		return messenger.User{}, liftUserNotFound(err)
	}
	return userFromSlackBot(resp.Bot), nil
}

// userFromSlackInfo maps a [slackUserInfo] onto the portable
// [messenger.User]. Slack-specific fields (team_id, tz) ride in
// Metadata per the M4.1 LESSON — exposing them as typed fields would
// force the portable shape to grow per-platform.
func userFromSlackInfo(u slackUserInfo) messenger.User {
	out := messenger.User{
		ID:          u.ID,
		Handle:      u.Name,
		DisplayName: u.Profile.DisplayName,
		Email:       u.Profile.Email,
		IsBot:       u.IsBot,
	}
	if out.DisplayName == "" {
		out.DisplayName = u.RealName
	}
	meta := make(map[string]string, 2)
	if u.TeamID != "" {
		meta["team_id"] = u.TeamID
	}
	if u.TZ != "" {
		meta["tz"] = u.TZ
	}
	if len(meta) > 0 {
		out.Metadata = meta
	}
	return out
}

// userFromSlackBot maps a [slackBotInfo] onto the portable
// [messenger.User]. The `is_bot` field is hard-set to true (Slack's
// bots.info endpoint returns only bot records by construction). The
// Slack-specific app_id + bot user_id ride in Metadata under stable
// keys (`app_id`, `bot_user_id`) — downstream identity-mapping code
// (M4.4) keys off these.
func userFromSlackBot(b slackBotInfo) messenger.User {
	out := messenger.User{
		ID:          b.ID,
		Handle:      b.Name,
		DisplayName: b.Name,
		IsBot:       true,
	}
	meta := make(map[string]string, 2)
	if b.AppID != "" {
		meta["app_id"] = b.AppID
	}
	if b.UserID != "" {
		meta["bot_user_id"] = b.UserID
	}
	if len(meta) > 0 {
		out.Metadata = meta
	}
	return out
}

// liftUserNotFound rewraps the slack-package APIError carrying any of
// the documented "not found" codes (`user_not_found`,
// `users_not_found`, `bot_not_found`) as the portable
// messenger.ErrUserNotFound so callers that match against the
// portable sentinel via errors.Is succeed. The original *APIError
// remains accessible via errors.As for callers that want the Code /
// Status / Method fields.
//
// Symmetric with liftChannelNotFound (send_message.go) and
// liftInvalidManifest (create_app.go) — adapter methods consistently
// lift the documented Slack codes onto their portable counterparts.
func liftUserNotFound(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "user_not_found", "users_not_found", "bot_not_found":
		return fmt.Errorf("%w: %w", messenger.ErrUserNotFound, err)
	}
	return err
}
