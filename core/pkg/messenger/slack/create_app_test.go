package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// manifestRequest mirrors the JSON shape we expect CreateApp to place on
// the wire. The wrapper is `{"manifest": {...}}`; the inner object
// echoes Slack's apps.manifest.create document. Decoding the captured
// body into this struct keeps the assertions readable and independent
// of map ordering.
//
// Manifest is typed `map[string]any` because Slack's manifest schema
// nests several heterogeneous sub-objects (display_information,
// oauth_config, settings, features) and several leaves are non-string
// (numeric, bool). Tests that compare a leaf cast through fmt.Sprint
// so the assertion stays type-agnostic — see the M4.2.b LESSON on
// raw-byte assertions for numeric leaves.
type manifestRequest struct {
	Manifest map[string]any `json:"manifest"`
}

// TestCreateApp_HappyPath_MinimalManifest asserts the simplest case:
// an AppManifest with name + description + scopes flows to
// /apps.manifest.create as a JSON body wrapping the manifest in a
// `manifest` envelope, and the returned AppID equals the response
// `app_id`.
func TestCreateApp_HappyPath_MinimalManifest(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/apps.manifest.create", http.StatusOK,
		`{"ok":true,"app_id":"A0123ABCDEF","credentials":{"client_id":"x","client_secret":"y","verification_token":"z","signing_secret":"w"}}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	id, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "watchkeeper",
		Description: "test bot",
		Scopes:      []string{"chat:write", "users:read"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if id != "A0123ABCDEF" {
		t.Errorf("AppID = %q, want A0123ABCDEF", id)
	}

	var got manifestRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.Manifest == nil {
		t.Fatal("manifest envelope missing")
	}
	display, _ := got.Manifest["display_information"].(map[string]any)
	if display == nil {
		t.Fatalf("display_information missing: %v", got.Manifest)
	}
	if display["name"] != "watchkeeper" {
		t.Errorf("display_information.name = %v, want watchkeeper", display["name"])
	}
	if display["description"] != "test bot" {
		t.Errorf("display_information.description = %v, want test bot", display["description"])
	}
	oauth, _ := got.Manifest["oauth_config"].(map[string]any)
	if oauth == nil {
		t.Fatal("oauth_config missing")
	}
	scopes, _ := oauth["scopes"].(map[string]any)
	if scopes == nil {
		t.Fatal("oauth_config.scopes missing")
	}
	bot, _ := scopes["bot"].([]any)
	if len(bot) != 2 || bot[0] != "chat:write" || bot[1] != "users:read" {
		t.Errorf("oauth_config.scopes.bot = %v, want [chat:write users:read]", bot)
	}
}

// TestCreateApp_EmptyName_FailsSync asserts that an empty manifest name
// surfaces messenger.ErrInvalidManifest synchronously WITHOUT contacting
// the platform — Slack would reject it anyway with `invalid_manifest`,
// but catching it client-side avoids burning a tier-2 rate-limit token.
func TestCreateApp_EmptyName_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if !errors.Is(err, messenger.ErrInvalidManifest) {
		t.Errorf("err = %v, want messenger.ErrInvalidManifest", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (empty name must not hit network)", calls)
	}
}

// TestCreateApp_InvalidManifest_PortableSentinel asserts that a Slack
// `error: "invalid_manifest"` envelope surfaces as
// messenger.ErrInvalidManifest (the portable sentinel) — adapter
// callers match the portable form, not the slack-specific one.
func TestCreateApp_InvalidManifest_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_manifest"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if !errors.Is(err, messenger.ErrInvalidManifest) {
		t.Errorf("errors.Is(err, messenger.ErrInvalidManifest) = false, want true; got %v", err)
	}
	// Underlying APIError still accessible for callers that care about
	// the raw Slack code.
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("error is not *APIError underneath: %T", err)
	}
}

// TestCreateApp_OtherErrorCodes_RetainAPIError asserts that error codes
// WITHOUT a portable sentinel mapping (manifest_validation_error,
// not_allowed_token_type, manifest_too_long) still surface as
// *APIError so callers can inspect Code.
func TestCreateApp_OtherErrorCodes_RetainAPIError(t *testing.T) {
	t.Parallel()

	codes := []string{"manifest_validation_error", "not_allowed_token_type", "manifest_too_long"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, code)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(
				WithBaseURL(srv.URL),
				WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
			)
			_, err := c.CreateApp(context.Background(), messenger.AppManifest{
				Name:        "wk",
				Description: "x",
				Scopes:      []string{"chat:write"},
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not *APIError: %T %v", err, err)
			}
			if apiErr.Code != code {
				t.Errorf("Code = %q, want %q", apiErr.Code, code)
			}
		})
	}
}

// TestCreateApp_RateLimited_PropagatesAPIError asserts that an HTTP 429
// from Slack surfaces as *APIError wrapping ErrRateLimited (the
// slack-package sentinel — the portable messenger interface does not
// document a rate-limit sentinel, so callers match the slack one).
func TestCreateApp_RateLimited_PropagatesAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; got %v", err)
	}
}

// TestCreateApp_CtxCancellation asserts a pre-cancelled ctx returns
// ctx.Err() and never contacts the platform.
func TestCreateApp_CtxCancellation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CreateApp(ctx, messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestCreateApp_CtxCancelled_BeforeValidation asserts that ctx
// cancellation takes precedence over input-shape validation: a
// cancelled ctx with an empty manifest name surfaces ctx.Err() rather
// than ErrInvalidManifest. Mirrors the M4.2.b/c.1 discipline.
func TestCreateApp_CtxCancelled_BeforeValidation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CreateApp(ctx, messenger.AppManifest{Name: ""})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if errors.Is(err, messenger.ErrInvalidManifest) {
		t.Errorf("err = %v, must NOT be ErrInvalidManifest when ctx is cancelled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestCreateApp_LoggerRedacted asserts the redaction discipline carries
// through CreateApp: the bearer token (an `xoxe-*` configuration token,
// the most sensitive token family Slack issues), the manifest name /
// description, and the returned credentials NEVER appear in log
// entries.
func TestCreateApp_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"app_id":"A123","credentials":{"client_secret":"LEAKED-CLIENT-SECRET","signing_secret":"LEAKED-SIGNING-SECRET"}}`)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe-LEAKED-CONFIG-TOKEN")),
		WithLogger(logger),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "PII-MANIFEST-NAME-REDACT",
		Description: "PII-DESCRIPTION-REDACT",
		Scopes:      []string{"chat:write"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	assertNoBannedSubstrings(t, logger.snapshot(), []string{
		"xoxe-LEAKED-CONFIG-TOKEN",
		"LEAKED-CLIENT-SECRET",
		"LEAKED-SIGNING-SECRET",
		"PII-MANIFEST-NAME-REDACT",
		"PII-DESCRIPTION-REDACT",
		"Bearer ",
	})
}

// TestCreateApp_MetadataMerge asserts that AppManifest.Metadata
// extensions ride into the manifest object as documented top-level
// keys. The metadata map carries Slack-specific keys: `long_description`
// (display_information leaf), `background_color`, plus features
// extension keys forwarded verbatim. Unknown keys are ignored.
func TestCreateApp_MetadataMerge(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/apps.manifest.create", http.StatusOK,
		`{"ok":true,"app_id":"A1","credentials":{"client_id":"x","client_secret":"y"}}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "short",
		Scopes:      []string{"chat:write"},
		Metadata: map[string]string{
			"long_description":    "the long form blurb",
			"background_color":    "#4A154B",
			"unknown_extra_key":   "ignored",
			"socket_mode_enabled": "true",
		},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	var got manifestRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}

	display, _ := got.Manifest["display_information"].(map[string]any)
	if display == nil {
		t.Fatalf("display_information missing")
	}
	if display["long_description"] != "the long form blurb" {
		t.Errorf("display_information.long_description = %v, want the long form blurb", display["long_description"])
	}
	if display["background_color"] != "#4A154B" {
		t.Errorf("display_information.background_color = %v, want #4A154B", display["background_color"])
	}

	settings, _ := got.Manifest["settings"].(map[string]any)
	if settings == nil {
		t.Fatalf("settings missing")
	}
	if settings["socket_mode_enabled"] != true {
		t.Errorf("settings.socket_mode_enabled = %v, want true (bool)", settings["socket_mode_enabled"])
	}
	if strings.Contains(string(captured), "unknown_extra_key") {
		t.Errorf("body leaks unknown_extra_key: %s", string(captured))
	}
}

// TestCreateApp_SocketModeEnabled_JSONBool asserts that the
// `socket_mode_enabled` settings leaf lands on the wire as a JSON
// BOOLEAN, not a JSON string. Slack's manifest schema documents it as a
// boolean; sending `"true"` (a JSON string) triggers
// `manifest_validation_error` on real workspaces.
//
// Mirrors the M4.2.b LESSON: when a typed envelope can't carry the
// right shape, switch to `map[string]any` and parse-on-store. Required
// substring `"socket_mode_enabled":true` (bare literal) AND banned
// substring `"socket_mode_enabled":"true"` (quoted form).
func TestCreateApp_SocketModeEnabled_JSONBool(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/apps.manifest.create", http.StatusOK,
		`{"ok":true,"app_id":"A1","credentials":{"client_id":"x","client_secret":"y"}}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
		Metadata: map[string]string{
			"socket_mode_enabled": "true",
		},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	if !strings.Contains(string(captured), `"socket_mode_enabled":true`) {
		t.Fatalf("socket_mode_enabled must serialise as a JSON bool; raw body: %s", string(captured))
	}
	if strings.Contains(string(captured), `"socket_mode_enabled":"true"`) {
		t.Fatalf("socket_mode_enabled serialised as a JSON string; raw body: %s", string(captured))
	}

	dec := json.NewDecoder(strings.NewReader(string(captured)))
	dec.UseNumber()
	var generic struct {
		Manifest struct {
			Settings map[string]any `json:"settings"`
		} `json:"manifest"`
	}
	if err := dec.Decode(&generic); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	v, ok := generic.Manifest.Settings["socket_mode_enabled"]
	if !ok {
		t.Fatalf("socket_mode_enabled absent: %s", string(captured))
	}
	if _, isBool := v.(bool); !isBool {
		t.Fatalf("socket_mode_enabled leaf type = %T, want bool", v)
	}
}

// TestCreateApp_NoScopes_StillRequestsManifest asserts that an empty
// Scopes slice still produces a valid manifest envelope (Slack accepts
// scope-less manifests for apps that operate purely via webhooks /
// slash commands). The oauth_config.scopes.bot field is omitted when
// the slice is nil/empty, and features.bot_user is omitted in lockstep
// (Slack only requires bot_user when bot scopes are declared).
func TestCreateApp_NoScopes_StillRequestsManifest(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/apps.manifest.create", http.StatusOK,
		`{"ok":true,"app_id":"A1","credentials":{}}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      nil,
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	var got manifestRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	// oauth_config may be absent OR present with empty scopes — both
	// are documented as valid by Slack; the adapter prefers absence
	// to keep the manifest minimal.
	if oauth, ok := got.Manifest["oauth_config"].(map[string]any); ok {
		if scopes, ok := oauth["scopes"].(map[string]any); ok {
			if bot, _ := scopes["bot"].([]any); len(bot) != 0 {
				t.Errorf("oauth_config.scopes.bot = %v, want empty/absent for nil scopes", bot)
			}
		}
	}
	if features, ok := got.Manifest["features"].(map[string]any); ok {
		if _, hasBotUser := features["bot_user"]; hasBotUser {
			t.Errorf("features.bot_user must be absent when no bot scopes declared; raw body: %s", string(captured))
		}
	}
}

// TestCreateApp_BotScopes_EmitFeaturesBotUser asserts that whenever
// the manifest declares non-empty bot scopes, the wire payload carries
// `features.bot_user.display_name` (string, defaults to manifest name)
// and `features.bot_user.always_online` (JSON BOOL false).
//
// Slack's apps.manifest.create rejects `oauth_config.scopes.bot` with
// `requires_bot_user` when the manifest omits the features.bot_user
// section. The contract is documented at
// https://api.slack.com/reference/manifests#bot_user — discovered
// empirically while running `make spawn-dev-bot` during Phase 1
// finalization (ROADMAP §10 DoD §7 #1).
//
// `always_online` is asserted as a JSON BOOL via raw-byte substring
// search, mirroring TestCreateApp_SocketModeEnabled_JSONBool — Slack
// rejects `"always_online":"false"` (quoted) with
// `manifest_validation_error`.
func TestCreateApp_BotScopes_EmitFeaturesBotUser(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/apps.manifest.create", http.StatusOK,
		`{"ok":true,"app_id":"A1","credentials":{"client_id":"x","client_secret":"y"}}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "Watchkeeper Dev",
		Description: "x",
		Scopes:      []string{"chat:write", "users:read"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	var got manifestRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	features, _ := got.Manifest["features"].(map[string]any)
	if features == nil {
		t.Fatalf("features section missing; raw body: %s", string(captured))
	}
	botUser, _ := features["bot_user"].(map[string]any)
	if botUser == nil {
		t.Fatalf("features.bot_user missing; raw body: %s", string(captured))
	}
	if botUser["display_name"] != "Watchkeeper Dev" {
		t.Errorf("features.bot_user.display_name = %v, want %q", botUser["display_name"], "Watchkeeper Dev")
	}
	if v, ok := botUser["always_online"].(bool); !ok || v {
		t.Errorf("features.bot_user.always_online = %v (%T), want bool false", botUser["always_online"], botUser["always_online"])
	}

	// Raw-wire assertion: always_online must serialise as JSON bool,
	// not the quoted string `"false"` Slack rejects.
	if !strings.Contains(string(captured), `"always_online":false`) {
		t.Fatalf("always_online must serialise as JSON bool; raw body: %s", string(captured))
	}
	if strings.Contains(string(captured), `"always_online":"false"`) {
		t.Fatalf("always_online serialised as JSON string; raw body: %s", string(captured))
	}
}
