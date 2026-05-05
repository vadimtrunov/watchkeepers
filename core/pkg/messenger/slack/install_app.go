package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// oauthV2AccessMethod is the Slack Web API method [Client.InstallApp]
// targets. Hoisted to a package constant so the rate-limiter registry
// (`defaultMethodTiers`) and the request path stay in sync via the
// compiler.
//
// The endpoint exchanges an authorization code (issued by Slack's OAuth
// flow — for the admin-preapproval path the dev-workspace administrator
// pre-approves the app via `admin.apps.approve`, which causes Slack to
// emit the authorization code on the first end-user install) for a set
// of bot / user tokens. The exchange itself is a server-to-server POST
// with the `code`, `client_id`, `client_secret`, and `redirect_uri`
// query parameters supplied in the request body; modern Slack accepts
// the body as JSON with `application/json` Content-Type, which is what
// [Client.Do] sends for every method (M4.2.a foundation).
const oauthV2AccessMethod = "oauth.v2.access"

// InstallParams carries the per-install OAuth secrets the caller
// resolves from its secrets interface (vault / SSM / keychain) just
// before invoking [Client.InstallApp]. Slack's `oauth.v2.access`
// requires four pieces of state that are workspace-specific AND
// install-specific (the authorization code is single-use); the portable
// [messenger.WorkspaceRef] has no Metadata bag, so the adapter exposes
// a typed [InstallParamsResolver] hook the caller wires via
// [WithInstallParamsResolver].
//
// All four fields are bytestrings — none of them appear in any log
// entry (the [Client.Do] redaction discipline applies). The
// [InstallParamsResolver] runs INSIDE the InstallApp call, so the
// caller's secrets-interface lookup naturally inherits the InstallApp
// ctx for cancellation.
type InstallParams struct {
	// Code is the Slack-issued OAuth authorization code. For the
	// admin-preapproval flow, the dev-workspace administrator pre-
	// approves the app via `admin.apps.approve` and Slack emits the
	// code on first install; the bootstrap script captures it then
	// surfaces it here. Required.
	Code string

	// ClientID is the Slack-side client_id for the app being installed
	// (returned by [Client.CreateApp] inside the credentials envelope
	// the M3.4.b secrets interface stored). Required.
	ClientID string

	// ClientSecret is the Slack-side client_secret matching ClientID.
	// Required. The adapter never logs it.
	ClientSecret string

	// RedirectURI must match the redirect_uri the OAuth flow consented
	// to. Slack rejects mismatched values as `bad_redirect_uri`.
	// Optional for the admin-preapproval path when the app manifest
	// declared a single redirect URI; Slack defaults to the manifest
	// value in that case.
	RedirectURI string
}

// Installation metadata keys this adapter writes onto
// [messenger.Installation.Metadata]. All carry stable, documented
// Slack-specific identifiers; NONE carry tokens (which ride OUT-OF-BAND
// via the configured [InstallTokenSink]).
const (
	installMetaSlackBotUserID           = "slack:bot_user_id"
	installMetaSlackTeamID              = "slack:team_id"
	installMetaSlackEnterpriseID        = "slack:enterprise_id"
	installMetaSlackAuthedUserID        = "slack:authed_user_id"
	installMetaSlackScope               = "slack:scope"
	installMetaSlackTokenType           = "slack:token_type"
	installMetaSlackIsEnterpriseInstall = "slack:is_enterprise_install"
)

// ErrInstallTokenSinkUnset surfaces synchronously from
// [Client.InstallApp] when no [InstallTokenSink] has been wired via
// [WithInstallTokenSink]. The security invariant: the OAuth response
// carries multiple long-lived secrets (bot access_token, optional
// refresh_token, optional user access_token); the adapter MUST hand them
// off to a caller-controlled sink rather than discard them silently or
// embed them in the returned [messenger.Installation] (the latter would
// violate the M4.1 design — "Tokens themselves are stored in the secrets
// interface, NOT here"). Matchable via [errors.Is].
var ErrInstallTokenSinkUnset = errors.New("slack: install: token sink not configured")

// ErrInstallParamsUnset surfaces synchronously from [Client.InstallApp]
// when no [InstallParamsResolver] has been wired via
// [WithInstallParamsResolver]. Slack's `oauth.v2.access` requires the
// authorization code + client credentials per-install; the portable
// [messenger.WorkspaceRef] does not carry them, so the adapter relies
// on a caller-controlled resolver to surface them just-in-time from
// the secrets interface. Matchable via [errors.Is].
var ErrInstallParamsUnset = errors.New("slack: install: params resolver not configured")

// InstallTokens is the value passed to a configured [InstallTokenSink]
// when [Client.InstallApp] succeeds. It carries every token Slack
// returned plus the corresponding [messenger.AppID] / workspace context
// the caller needs to key the secrets store.
//
// Persistence is the caller's job; the adapter does not retain any field
// after the sink callback returns.
type InstallTokens struct {
	// AppID is the Slack-assigned app id this install belongs to.
	AppID messenger.AppID

	// TeamID is the Slack-assigned workspace id this install lives in.
	TeamID string

	// EnterpriseID is the Slack-assigned enterprise grid id when the
	// install targets a grid; empty for non-grid workspaces.
	EnterpriseID string

	// AccessToken is the bot user `xoxb-*` token returned by Slack.
	// Always non-empty on a successful exchange.
	AccessToken string

	// RefreshToken is the rotation refresh token Slack returns when
	// token rotation is enabled on the app manifest. Empty when
	// rotation is disabled.
	RefreshToken string

	// ExpiresIn is the lifetime of [InstallTokens.AccessToken] in
	// seconds when token rotation is enabled. Zero when rotation is
	// disabled (the token never expires).
	ExpiresIn int

	// UserAccessToken is the `xoxp-*` user token Slack returns when the
	// authorising user granted user-scope OAuth permissions. Empty when
	// the install requested only bot scopes.
	UserAccessToken string

	// UserScope is the comma-separated list of user scopes that
	// authorise [InstallTokens.UserAccessToken]. Empty when no user
	// token was issued.
	UserScope string

	// IsEnterpriseInstall reports whether the install targets the
	// entire grid (true) or a single workspace within a grid / standalone
	// workspace (false).
	IsEnterpriseInstall bool
}

// InstallTokenSink is the function shape [WithInstallTokenSink]
// accepts. The hook receives every secret bytestring Slack returned;
// the caller writes them to its own secrets interface (vault, AWS SSM,
// keychain, env, etc.) and returns a non-nil error to abort the install
// when the secret-store write fails. Returning an error causes
// [Client.InstallApp] to surface it wrapped — the adapter does NOT
// rollback the install (Slack's exchange is server-side complete; the
// caller must handle reconciliation upstream).
type InstallTokenSink func(ctx context.Context, tokens InstallTokens) error

// InstallParamsResolver is the function shape
// [WithInstallParamsResolver] accepts. [Client.InstallApp] calls it
// once per install with the supplied appID + workspace; the resolver
// returns the four OAuth secrets the caller pulled from its secrets
// interface. A non-nil error surfaces wrapped from InstallApp without
// contacting Slack (resolution is the security boundary — if it fails,
// the request is never sent).
type InstallParamsResolver func(ctx context.Context, appID messenger.AppID, workspace messenger.WorkspaceRef) (InstallParams, error)

// WithInstallTokenSink wires the [InstallTokenSink] consulted at the
// end of every successful [Client.InstallApp] call. A nil hook is
// ignored so callers can apply a conditional override without explicit
// branching.
//
// Without a sink wired, [Client.InstallApp] fails synchronously with
// [ErrInstallTokenSinkUnset] BEFORE contacting Slack — the adapter
// refuses to issue the token-exchange request when the response would
// have nowhere to land.
func WithInstallTokenSink(sink InstallTokenSink) ClientOption {
	return func(c *clientConfig) {
		if sink != nil {
			c.installTokenSink = sink
		}
	}
}

// WithInstallParamsResolver wires the [InstallParamsResolver]
// [Client.InstallApp] consults to source the OAuth code + client
// credentials per-install. A nil hook is ignored so callers can apply a
// conditional override without explicit branching.
//
// Without a resolver wired, [Client.InstallApp] fails synchronously
// with [ErrInstallParamsUnset] BEFORE contacting Slack — the request
// cannot be assembled without the secrets-interface lookup.
func WithInstallParamsResolver(r InstallParamsResolver) ClientOption {
	return func(c *clientConfig) {
		if r != nil {
			c.installParamsResolver = r
		}
	}
}

// oauthV2AccessRequest is the JSON envelope `oauth.v2.access` expects.
// All four fields ride as plain JSON strings; the modern endpoint
// accepts the JSON body alongside the legacy form-urlencoded form.
type oauthV2AccessRequest struct {
	Code         string `json:"code"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
}

// oauthV2AccessResponse is the subset of the Slack response
// [Client.InstallApp] decodes. The full response carries additional
// fields (`incoming_webhook` for legacy hooks, `bot_id` etc.); we
// surface only what the [messenger.Installation] contract + the
// [InstallTokens] sink need.
type oauthV2AccessResponse struct {
	OK                  bool   `json:"ok"`
	AppID               string `json:"app_id"`
	AccessToken         string `json:"access_token"`
	TokenType           string `json:"token_type"`
	Scope               string `json:"scope"`
	BotUserID           string `json:"bot_user_id"`
	RefreshToken        string `json:"refresh_token"`
	ExpiresIn           int    `json:"expires_in"`
	IsEnterpriseInstall bool   `json:"is_enterprise_install"`
	Team                struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
	Enterprise struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"enterprise"`
	AuthedUser struct {
		ID          string `json:"id"`
		Scope       string `json:"scope"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	} `json:"authed_user"`
}

// InstallApp installs the app `appID` into `workspace` by exchanging the
// caller-supplied OAuth authorization code for a set of bot / user
// tokens via Slack's `oauth.v2.access`. The returned
// [messenger.Installation] carries only non-secret platform-side
// identifiers (bot user id, team id, enterprise id, scope, token type,
// enterprise-install flag); every raw token the response carried rides
// OUT-OF-BAND via the [InstallTokenSink] configured on the [Client] —
// per the M4.1 design "Tokens themselves are stored in the secrets
// interface, NOT here".
//
// Mapping ([InstallParamsResolver] → oauth.v2.access):
//
//   - InstallParams.Code         → code
//   - InstallParams.ClientID     → client_id
//   - InstallParams.ClientSecret → client_secret
//   - InstallParams.RedirectURI  → redirect_uri (optional)
//
// The portable [messenger.WorkspaceRef] does not carry a Metadata bag,
// so the adapter relies on a typed [InstallParamsResolver] hook the
// caller wires via [WithInstallParamsResolver]. The resolver runs
// inside the InstallApp ctx so the secrets-interface lookup inherits
// cancellation; a non-nil resolver error surfaces wrapped without
// contacting Slack (resolution failure is a security boundary).
//
// The admin-preapproval flow for a dev workspace is a two-actor protocol
// the adapter does NOT mediate end-to-end: an administrator pre-approves
// the app via `admin.apps.approve` (a separate, one-shot operator step
// the M4.3 bootstrap script will drive), which causes Slack to issue an
// authorization code WITHOUT requiring a per-install user-consent click.
// The caller's bootstrap script then surfaces that code (and the
// matching client credentials, resolved from the secrets interface) via
// [messenger.WorkspaceRef.Metadata] and calls InstallApp to complete the
// token exchange. Wiring the admin.apps.approve call into InstallApp
// would conflate a one-off operator action with the per-install path
// every subsequent workspace will follow — kept separate for clarity.
//
// Empty appID returns [messenger.ErrAppNotFound] synchronously.
// Empty workspace.ID returns [messenger.ErrInvalidQuery] synchronously
// (the install target is required and missing it never reaches Slack).
// Missing [InstallTokenSink] returns [ErrInstallTokenSinkUnset]
// synchronously — the adapter refuses to discard the response tokens.
//
// Slack `error` codes map per the existing [APIError.Unwrap] table plus
// the install-specific extensions below:
//
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
//   - invalid_code, code_already_used, invalid_client_id,
//     bad_redirect_uri, oauth_authorization_url_mismatch →
//     [*APIError] (Code populated; no portable sentinel — caller
//     inspects `Code`)
//
// Note (token redaction): the bearer token used for the request itself
// is the configuration token resolved by the [TokenSource] (typically
// an `xoxe-*` app configuration token); [Client.Do] never logs the
// token, the request body, or the response body. The
// [InstallTokenSink] is the single observation point for the
// just-exchanged secrets.
func (c *Client) InstallApp(ctx context.Context, appID messenger.AppID, workspace messenger.WorkspaceRef) (messenger.Installation, error) {
	// ctx cancellation takes precedence over input-shape validation —
	// matches the M4.2.b/c.1/d.1 discipline.
	if err := ctx.Err(); err != nil {
		return messenger.Installation{}, err
	}
	if appID == "" {
		return messenger.Installation{}, fmt.Errorf("slack: %s: %w", oauthV2AccessMethod, messenger.ErrAppNotFound)
	}
	if workspace.ID == "" {
		return messenger.Installation{}, fmt.Errorf("slack: %s: workspace id: %w", oauthV2AccessMethod, messenger.ErrInvalidQuery)
	}
	if c.cfg.installTokenSink == nil {
		return messenger.Installation{}, fmt.Errorf("slack: %s: %w", oauthV2AccessMethod, ErrInstallTokenSinkUnset)
	}
	if c.cfg.installParamsResolver == nil {
		return messenger.Installation{}, fmt.Errorf("slack: %s: %w", oauthV2AccessMethod, ErrInstallParamsUnset)
	}

	params, err := c.cfg.installParamsResolver(ctx, appID, workspace)
	if err != nil {
		return messenger.Installation{}, fmt.Errorf("slack: %s: install params: %w", oauthV2AccessMethod, err)
	}

	req := oauthV2AccessRequest(params)

	var resp oauthV2AccessResponse
	if err := c.Do(ctx, oauthV2AccessMethod, req, &resp); err != nil {
		return messenger.Installation{}, err
	}

	tokens := InstallTokens{
		AppID:               messenger.AppID(resp.AppID),
		TeamID:              resp.Team.ID,
		EnterpriseID:        resp.Enterprise.ID,
		AccessToken:         resp.AccessToken,
		RefreshToken:        resp.RefreshToken,
		ExpiresIn:           resp.ExpiresIn,
		UserAccessToken:     resp.AuthedUser.AccessToken,
		UserScope:           resp.AuthedUser.Scope,
		IsEnterpriseInstall: resp.IsEnterpriseInstall,
	}
	if err := c.cfg.installTokenSink(ctx, tokens); err != nil {
		return messenger.Installation{}, fmt.Errorf("slack: %s: token sink: %w", oauthV2AccessMethod, err)
	}

	return buildInstallation(appID, workspace, resp, c.cfg.clock()), nil
}

// buildInstallation assembles the [messenger.Installation] returned by
// [Client.InstallApp] from the OAuth response. Carries ONLY non-secret
// fields; every token rides OUT-OF-BAND via the [InstallTokenSink].
//
// `now` stamps [messenger.Installation.InstalledAt] — Slack's OAuth
// response does not carry an install timestamp, so we record the
// adapter-side observation time. Callers that need server-authoritative
// timestamps query `team.info` separately.
func buildInstallation(appID messenger.AppID, workspace messenger.WorkspaceRef, resp oauthV2AccessResponse, now time.Time) messenger.Installation {
	return messenger.Installation{
		AppID:       appID,
		Workspace:   workspace,
		BotUserID:   resp.BotUserID,
		InstalledAt: now.UTC(),
		Metadata:    buildInstallationMetadata(resp, workspace),
	}
}

// buildInstallationMetadata assembles the metadata bag rolled into the
// returned [messenger.Installation]. Carries ONLY non-secret keys —
// every token is delivered to the caller's [InstallTokenSink], NOT
// embedded here.
func buildInstallationMetadata(resp oauthV2AccessResponse, workspace messenger.WorkspaceRef) map[string]string {
	meta := make(map[string]string, 7)
	if resp.BotUserID != "" {
		meta[installMetaSlackBotUserID] = resp.BotUserID
	}
	teamID := resp.Team.ID
	if teamID == "" {
		teamID = workspace.ID
	}
	if teamID != "" {
		meta[installMetaSlackTeamID] = teamID
	}
	if resp.Enterprise.ID != "" {
		meta[installMetaSlackEnterpriseID] = resp.Enterprise.ID
	}
	if resp.AuthedUser.ID != "" {
		meta[installMetaSlackAuthedUserID] = resp.AuthedUser.ID
	}
	if resp.Scope != "" {
		meta[installMetaSlackScope] = resp.Scope
	}
	if resp.TokenType != "" {
		meta[installMetaSlackTokenType] = resp.TokenType
	}
	meta[installMetaSlackIsEnterpriseInstall] = strconv.FormatBool(resp.IsEnterpriseInstall)
	return meta
}
