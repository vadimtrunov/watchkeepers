package slack

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// appsManifestCreateMethod is the Slack Web API method name CreateApp
// targets. Hoisted to a package constant so the rate-limiter registry
// (`defaultMethodTiers`) and the request path stay in sync via the
// compiler.
//
// The endpoint requires an `xoxe-*` configuration token (the Slack
// "app configuration token" family — distinct from `xoxb-*` bot tokens
// and `xoxp-*` user tokens). Callers wire the configuration token via
// the same [TokenSource] mechanism — the [Client] does not distinguish
// token families on the wire.
const appsManifestCreateMethod = "apps.manifest.create"

// appsManifestCreateRequest is the JSON envelope apps.manifest.create
// expects. Slack wraps the manifest document inside a `manifest` field
// (alongside `app_id` for `apps.manifest.update`-style calls — not
// relevant for CreateApp).
//
// Manifest is typed `map[string]any` because Slack's manifest schema
// nests heterogeneous sub-objects (display_information, oauth_config,
// settings, features) and several leaves are non-string (numeric,
// bool — see the M4.2.b LESSON on raw-byte assertions). A typed struct
// would force premature commitments to the schema; the metadata-bag
// design from M4.1 lets the portable AppManifest extend without
// breaking the wire shape.
type appsManifestCreateRequest struct {
	Manifest map[string]any `json:"manifest"`
}

// appsManifestCreateResponse is the subset of the Slack response
// CreateApp decodes. Slack returns the assigned `app_id` plus a
// `credentials` object (client_id, client_secret, verification_token,
// signing_secret). The portable [messenger.Adapter] contract surfaces
// only the AppID; the credentials are routed OUT-OF-BAND to the
// caller-controlled [CreateAppCredsSink] so the tokens are never
// embedded in any return value (mirrors the M4.2.d.2 install-token-sink
// design — see [InstallTokenSink]).
type appsManifestCreateResponse struct {
	OK          bool                          `json:"ok"`
	AppID       string                        `json:"app_id"`
	Credentials appsManifestCreateCredentials `json:"credentials"`
}

// appsManifestCreateCredentials is the `credentials` sub-object Slack
// returns from `apps.manifest.create`. Every field is a long-lived
// secret; the [Client] never logs them, never embeds them in a return
// value, and only forwards them to a configured [CreateAppCredsSink].
type appsManifestCreateCredentials struct {
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	VerificationToken string `json:"verification_token"`
	SigningSecret     string `json:"signing_secret"`
}

// CreateAppCredentials is the value passed to a configured
// [CreateAppCredsSink] when [Client.CreateApp] succeeds. It carries
// every credential `apps.manifest.create` returned plus the assigned
// [messenger.AppID] the caller needs to key its secrets store.
//
// Persistence is the caller's job; the adapter does not retain any field
// after the sink callback returns.
type CreateAppCredentials struct {
	// AppID is the Slack-assigned app id this credentials bundle belongs
	// to. Echoed from the response so a sink that fans multiple bootstraps
	// into a single store has the natural keying context.
	AppID messenger.AppID

	// ClientID is the OAuth client_id Slack assigned to the app. Required
	// at install time (`oauth.v2.access`).
	ClientID string

	// ClientSecret is the OAuth client_secret matching ClientID. Required
	// at install time. The adapter never logs it.
	ClientSecret string

	// VerificationToken is the legacy verification token Slack issues
	// for outbound event verification. Modern apps verify via the
	// signing secret instead; included here for completeness.
	VerificationToken string

	// SigningSecret is the request-signing secret used to verify
	// inbound Events API / Interactivity payloads. Required if the app
	// ever falls back from Socket Mode to HTTP event delivery.
	SigningSecret string
}

// CreateAppCredsSink is the function shape [WithCreateAppCredsSink]
// accepts. The hook receives every secret bytestring `apps.manifest.create`
// returned; the caller writes them to its own secrets interface (vault,
// AWS SSM, keychain, env, structured JSON file the operator pipes into
// their store, …) and returns a non-nil error to abort the bootstrap
// when the secret-store write fails. Returning an error causes
// [Client.CreateApp] to surface it wrapped — the adapter does NOT
// rollback the app creation (Slack's manifest.create is server-side
// complete; the caller must handle reconciliation upstream).
type CreateAppCredsSink func(ctx context.Context, creds CreateAppCredentials) error

// WithCreateAppCredsSink wires the [CreateAppCredsSink] consulted at
// the end of every successful [Client.CreateApp] call. A nil hook is
// ignored so callers can apply a conditional override without explicit
// branching.
//
// When no sink is wired, [Client.CreateApp] silently discards the
// returned credentials — the historical behaviour from M4.2.d.1, kept
// for backward compatibility with callers that only need the AppID.
// The bootstrap script (M4.3) wires a sink that writes the credentials
// to a structured JSON file the operator ingests into the secrets store.
func WithCreateAppCredsSink(sink CreateAppCredsSink) ClientOption {
	return func(c *clientConfig) {
		if sink != nil {
			c.createAppCredsSink = sink
		}
	}
}

// recognisedManifestDisplayKeys is the closed set of metadata keys
// that ride into the manifest's display_information sub-object.
// Documented at https://api.slack.com/reference/manifests#display_information.
//
// Unknown keys are dropped at the adapter boundary (M4.1 lesson —
// adapters consume what they recognise). Callers that need a new
// display field send a PR adding it here so the contract stays
// explicit.
var recognisedManifestDisplayKeys = []string{
	"long_description",
	"background_color",
}

// recognisedManifestSettingsKeys is the closed set of metadata keys
// that ride into the manifest's settings sub-object. Documented at
// https://api.slack.com/reference/manifests#settings.
//
// Each entry maps to a documented JSON type — booleans land on the
// wire as JSON bools (per the M4.2.b LESSON: typed map envelopes lie
// about wire format; sending `"true"` would trigger
// `manifest_validation_error` on real workspaces).
var recognisedManifestSettingsKeys = []manifestSettingKey{
	{name: "socket_mode_enabled", kind: settingKindBool},
	{name: "token_rotation_enabled", kind: settingKindBool},
}

// settingKind is the JSON-type discriminator for entries in
// [recognisedManifestSettingsKeys]. Slack documents each settings
// field as a specific JSON type; a string-typed envelope would fail
// validation for boolean leaves.
type settingKind int

const (
	settingKindString settingKind = iota
	settingKindBool
)

// manifestSettingKey pairs a settings field name with its documented
// JSON type. Hoisted out of the keys slice so the table stays
// scannable — the type column makes wire-format obligations explicit
// at the registration site.
type manifestSettingKey struct {
	name string
	kind settingKind
}

// CreateApp provisions a new Slack app from `manifest` and returns
// the platform-assigned [messenger.AppID].
//
// Mapping (messenger.AppManifest → apps.manifest.create):
//
//   - manifest.Name        → manifest.display_information.name (REQUIRED)
//   - manifest.Description → manifest.display_information.description
//   - manifest.Scopes      → manifest.oauth_config.scopes.bot (string list)
//   - manifest.Metadata    → forwarded for the documented keys listed
//     in [recognisedManifestDisplayKeys] and
//     [recognisedManifestSettingsKeys]; other keys are dropped.
//
// Empty Name returns [messenger.ErrInvalidManifest] synchronously
// without contacting the platform — Slack would reject the call
// anyway with the same code, but catching it client-side avoids
// burning a tier-2 rate-limit token on a known-bad request.
//
// Slack `error` codes map per the existing [APIError.Unwrap] table
// plus the manifest-specific extension below:
//
//   - invalid_manifest          → [messenger.ErrInvalidManifest]
//   - manifest_validation_error → [*APIError] (Code populated; no
//     portable sentinel — caller inspects `Code`)
//   - manifest_too_long         → [*APIError] (Code populated)
//   - not_allowed_token_type    → [*APIError] (the supplied token is
//     not an `xoxe-*` configuration token; caller fixes the secrets
//     wiring)
//   - invalid_auth / not_authed → [ErrInvalidAuth]
//   - token_expired             → [ErrTokenExpired]
//   - ratelimited / HTTP 429    → [ErrRateLimited]
//
// IMPORTANT (token discipline): apps.manifest.create requires an
// `xoxe-*` Slack app configuration token. The [Client] does NOT
// distinguish token families on the wire — passing a `xoxb-*` /
// `xoxp-*` token through the configured [TokenSource] surfaces as
// Slack's `not_allowed_token_type` error. Callers wire the
// configuration token via the secrets interface (M3.4.b) and the
// [Client] redaction discipline (M4.2.a) ensures the token never
// appears in log entries.
func (c *Client) CreateApp(ctx context.Context, manifest messenger.AppManifest) (messenger.AppID, error) {
	// ctx cancellation takes precedence over input-shape validation —
	// matches the convention from M4.2.b/c.1 (caller's "abandon work"
	// signal trumps any precondition).
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if manifest.Name == "" {
		return "", fmt.Errorf("slack: %s: %w", appsManifestCreateMethod, messenger.ErrInvalidManifest)
	}

	body := buildManifestBody(manifest)
	req := appsManifestCreateRequest{Manifest: body}

	var resp appsManifestCreateResponse
	if err := c.Do(ctx, appsManifestCreateMethod, req, &resp); err != nil {
		// invalid_manifest surfaces underneath as *APIError; lift it to
		// the portable sentinel so adapter callers match without
		// importing the slack package.
		return "", liftInvalidManifest(err)
	}

	// Hand the just-issued credentials off to the configured sink (if
	// any) BEFORE returning. Mirrors the M4.2.d.2 install-token-sink
	// pattern: secrets ride OUT-OF-BAND through a caller-controlled
	// callback so the portable [messenger.Adapter] surface stays free of
	// raw token bytes. A nil sink silently discards the credentials —
	// callers that only need the AppID stay backwards compatible.
	if sink := c.cfg.createAppCredsSink; sink != nil {
		creds := CreateAppCredentials{
			AppID:             messenger.AppID(resp.AppID),
			ClientID:          resp.Credentials.ClientID,
			ClientSecret:      resp.Credentials.ClientSecret,
			VerificationToken: resp.Credentials.VerificationToken,
			SigningSecret:     resp.Credentials.SigningSecret,
		}
		if err := sink(ctx, creds); err != nil {
			return "", fmt.Errorf("slack: %s: credentials sink: %w", appsManifestCreateMethod, err)
		}
	}

	return messenger.AppID(resp.AppID), nil
}

// buildManifestBody assembles the `manifest` map sent to
// apps.manifest.create from the portable [messenger.AppManifest]
// fields plus any documented metadata extensions. Returns the map
// with at minimum `display_information.name` populated.
//
// The map is typed `map[string]any` so leaves can land on the wire
// with their documented JSON types — string for free-form descriptive
// fields, []string for scope lists, bool for settings flags. See the
// M4.2.b LESSON: a `map[string]string` envelope would force every
// leaf onto the wire as a JSON string, breaking Slack's manifest
// schema for boolean settings fields.
func buildManifestBody(m messenger.AppManifest) map[string]any {
	manifest := make(map[string]any, 4)

	display := map[string]any{
		"name": m.Name,
	}
	if m.Description != "" {
		display["description"] = m.Description
	}
	for _, key := range recognisedManifestDisplayKeys {
		if v, ok := m.Metadata[key]; ok && v != "" {
			display[key] = v
		}
	}
	manifest["display_information"] = display

	if len(m.Scopes) > 0 {
		manifest["oauth_config"] = map[string]any{
			"scopes": map[string]any{
				"bot": append([]string(nil), m.Scopes...),
			},
		}
	}

	if settings := buildManifestSettings(m.Metadata); len(settings) > 0 {
		manifest["settings"] = settings
	}

	return manifest
}

// buildManifestSettings assembles the manifest's settings sub-object
// from the documented keys in [recognisedManifestSettingsKeys]. Each
// entry's documented JSON type is honoured: bool keys fall through
// [strconv.ParseBool] and unparseable values are silently dropped
// (mirrors the optionalBool fall-through-on-bad-input discipline in
// send_message.go and bot_profile.go's status_expiration handling —
// adapter does not panic on malformed caller input, and forwarding
// garbage produces a less actionable error than omitting the field).
//
// Returns nil when no recognised key resolves; the caller omits the
// settings object entirely so the manifest stays minimal.
func buildManifestSettings(meta map[string]string) map[string]any {
	if len(meta) == 0 {
		return nil
	}
	settings := make(map[string]any, len(recognisedManifestSettingsKeys))
	for _, key := range recognisedManifestSettingsKeys {
		raw, ok := meta[key.name]
		if !ok || raw == "" {
			continue
		}
		switch key.kind {
		case settingKindBool:
			v, err := strconv.ParseBool(raw)
			if err != nil {
				continue
			}
			settings[key.name] = v
		case settingKindString:
			settings[key.name] = raw
		}
	}
	if len(settings) == 0 {
		return nil
	}
	return settings
}

// liftInvalidManifest rewraps the slack-package APIError carrying
// `error: "invalid_manifest"` as the portable
// messenger.ErrInvalidManifest so callers that match against the
// portable sentinel via errors.Is succeed. The original *APIError
// remains accessible via errors.As for callers that want the Code /
// Status / Method fields.
//
// Symmetric with liftChannelNotFound in send_message.go — adapter
// methods consistently lift the documented Slack codes onto their
// portable counterparts.
func liftInvalidManifest(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "invalid_manifest" {
		return fmt.Errorf("%w: %w", messenger.ErrInvalidManifest, err)
	}
	return err
}
