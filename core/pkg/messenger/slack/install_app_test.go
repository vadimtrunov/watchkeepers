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
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// oauthAccessRequest mirrors the JSON shape we expect InstallApp to place
// on the wire for `oauth.v2.access`. The decoder keeps the raw-byte
// assertions readable while the test still checks substring forms for
// banned shapes (M4.2.b lesson).
type oauthAccessRequest struct {
	Code         string `json:"code"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
}

// recordingTokenSink captures InstallTokens callbacks so tests can assert
// the sink was invoked with exactly the values returned by Slack.
type recordingTokenSink struct {
	mu      sync.Mutex
	entries []InstallTokens
}

func (s *recordingTokenSink) sink(_ context.Context, tokens InstallTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, tokens)
	return nil
}

func (s *recordingTokenSink) snapshot() []InstallTokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InstallTokens, len(s.entries))
	copy(out, s.entries)
	return out
}

// staticInstallParams returns an [InstallParamsResolver] that always
// emits the supplied params. Tests use it to keep the wiring compact.
func staticInstallParams(p InstallParams) InstallParamsResolver {
	return func(context.Context, messenger.AppID, messenger.WorkspaceRef) (InstallParams, error) {
		return p, nil
	}
}

// canonicalInstallParams is the params bundle reused across most install
// tests so each setup stays compact. The values mirror the shape Slack
// expects in real wire traffic without leaking through any production
// secret material — every field is a synthetic placeholder.
func canonicalInstallParams() InstallParams {
	return InstallParams{
		Code:         "OAUTH-CODE-123",
		ClientID:     "0123456789.0123456789",
		ClientSecret: "client-secret-LEAKED",
		RedirectURI:  "https://example.invalid/oauth/callback",
	}
}

// oauthV2AccessOKBody is the canonical Slack `oauth.v2.access` happy-path
// response body. Tests reuse it so the wire-format assertions stay
// scannable. Mirrors the documented modern shape at
// https://api.slack.com/methods/oauth.v2.access#examples.
const oauthV2AccessOKBody = `{
		"ok": true,
		"app_id": "A0123ABCDEF",
		"access_token": "xoxb-LEAKED-BOT-TOKEN",
		"token_type": "bot",
		"scope": "chat:write,users:read,im:history",
		"bot_user_id": "U0BOTUSER",
		"refresh_token": "xoxe-1-LEAKED-REFRESH",
		"expires_in": 43200,
		"team": {"id": "T0123TEAM", "name": "Watchkeepers Dev"},
		"enterprise": {"id": "E0123ENT", "name": "Watchkeepers Enterprise"},
		"is_enterprise_install": false,
		"authed_user": {
			"id": "U0AUTHED",
			"scope": "search:read",
			"access_token": "xoxp-LEAKED-USER-TOKEN",
			"token_type": "user"
		}
	}`

// assertInstallationField fails the test when inst.Metadata[key] != want.
// Used to keep the metadata-pass-through assertions one line each.
func assertInstallationField(t *testing.T, inst messenger.Installation, key, want string) {
	t.Helper()
	if got := inst.Metadata[key]; got != want {
		t.Errorf("Metadata[%q] = %q, want %q", key, got, want)
	}
}

// assertInstallTokensString fails the test when got != want.
// Provides labelled single-line assertions for string fields on InstallTokens.
func assertInstallTokensString(t *testing.T, label, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("sink.%s = %q, want %q", label, got, want)
	}
}

// assertBodyContains fails the test when the request body is missing the
// expected substring. Used for wire-format byte-level assertions.
func assertBodyContains(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("request body missing %s; body=%s", want, body)
	}
}

// TestInstallApp_HappyPath asserts the canonical exchange: a valid
// authorization code (resolved via the InstallParamsResolver) is
// exchanged for an Installation populated with the platform-side
// identifiers, while every raw token is delivered out-of-band via the
// configured sink (NEVER on the Installation struct).
func TestInstallApp_HappyPath(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/oauth.v2.access", http.StatusOK,
		oauthV2AccessOKBody,
		&captured,
	)

	sink := &recordingTokenSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test-config-token")),
		WithInstallTokenSink(sink.sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	inst, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A0123ABCDEF"),
		messenger.WorkspaceRef{
			ID:   "T0123TEAM",
			Name: "Watchkeepers Dev",
		},
	)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}

	// Installation echoes the supplied AppID and workspace plus the
	// resolved bot-user id from the response.
	if inst.AppID != messenger.AppID("A0123ABCDEF") {
		t.Errorf("AppID = %q, want A0123ABCDEF", inst.AppID)
	}
	if inst.Workspace.ID != "T0123TEAM" {
		t.Errorf("Workspace.ID = %q, want T0123TEAM", inst.Workspace.ID)
	}
	if inst.InstalledAt.IsZero() {
		t.Errorf("InstalledAt is zero; want client-clock-stamped value")
	}

	// Metadata carries the documented Slack-specific install artefacts
	// (M4.1 lesson — adapter-specific keys ride in Metadata).
	assertInstallationField(t, inst, "slack:bot_user_id", "U0BOTUSER")
	assertInstallationField(t, inst, "slack:team_id", "T0123TEAM")
	assertInstallationField(t, inst, "slack:enterprise_id", "E0123ENT")
	assertInstallationField(t, inst, "slack:authed_user_id", "U0AUTHED")
	assertInstallationField(t, inst, "slack:scope", "chat:write,users:read,im:history")
	assertInstallationField(t, inst, "slack:token_type", "bot")
	assertInstallationField(t, inst, "slack:is_enterprise_install", "false")

	// The sink received the tokens exactly once.
	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("sink invoked %d times, want 1", len(got))
	}
	tokens := got[0]
	assertInstallTokensString(t, "AccessToken", tokens.AccessToken, "xoxb-LEAKED-BOT-TOKEN")
	assertInstallTokensString(t, "RefreshToken", tokens.RefreshToken, "xoxe-1-LEAKED-REFRESH")
	assertInstallTokensString(t, "UserAccessToken", tokens.UserAccessToken, "xoxp-LEAKED-USER-TOKEN")
	if tokens.ExpiresIn != 43200 {
		t.Errorf("sink.ExpiresIn = %d, want 43200", tokens.ExpiresIn)
	}
	if string(tokens.AppID) != "A0123ABCDEF" {
		t.Errorf("sink.AppID = %q, want A0123ABCDEF", tokens.AppID)
	}
	if tokens.IsEnterpriseInstall {
		t.Errorf("sink.IsEnterpriseInstall = true, want false")
	}

	// Wire-format byte-level assertions: the captured request body MUST
	// echo the resolver-supplied OAuth secrets exactly. The configuration
	// token on the client never appears in the body (it rides in the
	// Authorization header per [Client.Do]).
	body := string(captured)
	assertBodyContains(t, body, `"code":"OAUTH-CODE-123"`)
	assertBodyContains(t, body, `"client_id":"0123456789.0123456789"`)
	assertBodyContains(t, body, `"client_secret":"client-secret-LEAKED"`)
	if strings.Contains(body, "xoxe.xoxp-1-test-config-token") {
		t.Errorf("request body LEAKS configuration token: %s", body)
	}
}

// TestInstallApp_NoRawTokensInInstallation is the load-bearing assertion
// of the M4.1 design: the Installation struct (Marshalled to JSON for
// transport / persistence) MUST NOT carry any raw token. Banned
// substrings cover the actual token bytes Slack returned plus the keys
// that would only appear if the implementation accidentally embedded a
// token.
func TestInstallApp_NoRawTokensInInstallation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, oauthV2AccessOKBody)
	}))
	t.Cleanup(srv.Close)

	sink := &recordingTokenSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test-config-token")),
		WithInstallTokenSink(sink.sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	inst, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A0123ABCDEF"),
		messenger.WorkspaceRef{ID: "T0123TEAM"},
	)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}

	raw, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("marshal Installation: %v", err)
	}
	banned := []string{
		"xoxb-LEAKED-BOT-TOKEN",
		"xoxe-1-LEAKED-REFRESH",
		"xoxp-LEAKED-USER-TOKEN",
		"access_token",
		"refresh_token",
		"client_secret",
	}
	body := string(raw)
	for _, b := range banned {
		if strings.Contains(body, b) {
			t.Errorf("Installation JSON leaks banned substring %q: %s", b, body)
		}
	}
}

// TestInstallApp_WireFormat_TypedFields asserts the M4.2.b wire-format
// LESSON: typed JSON fields land in the InstallTokens callback with the
// documented Go type. A `map[string]string` envelope would force every
// leaf into a JSON string and break the typed access patterns
// downstream callers depend on (numeric ExpiresIn comparisons,
// IsEnterpriseInstall boolean branches).
func TestInstallApp_WireFormat_TypedFields(t *testing.T) {
	t.Parallel()

	// Use a response body with explicit typed-field shapes (bool literal,
	// number literal) — the typed sink fields verify the decoder treated
	// them per the documented JSON type, not as quoted strings.
	body := `{"ok":true,"app_id":"A1","access_token":"xoxb-x","token_type":"bot","scope":"chat:write","bot_user_id":"U1","refresh_token":"xoxe-1-x","expires_in":43200,"team":{"id":"T1"},"is_enterprise_install":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	sink := &recordingTokenSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test-config-token")),
		WithInstallTokenSink(sink.sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	inst, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}

	tokens := sink.snapshot()
	if len(tokens) != 1 {
		t.Fatalf("sink invoked %d times, want 1", len(tokens))
	}
	if tokens[0].ExpiresIn != 43200 {
		t.Errorf("ExpiresIn = %d, want 43200 (typed int from JSON number)", tokens[0].ExpiresIn)
	}
	if !tokens[0].IsEnterpriseInstall {
		t.Errorf("IsEnterpriseInstall = false, want true (typed bool from JSON literal)")
	}

	// The Installation metadata stringifies the typed bool — assert the
	// stringification produced "true" exactly (no quoted-bool form, no
	// extra whitespace) so downstream `errors.Is`-style key compares
	// remain stable.
	if got := inst.Metadata["slack:is_enterprise_install"]; got != "true" {
		t.Errorf("Metadata[slack:is_enterprise_install] = %q, want %q", got, "true")
	}
}

// TestInstallApp_EmptyAppID_FailsSync asserts that an empty AppID
// surfaces messenger.ErrAppNotFound synchronously WITHOUT contacting the
// platform — Slack would reject the call anyway, but catching it
// client-side avoids burning a tier-3 rate-limit token.
func TestInstallApp_EmptyAppID_FailsSync(t *testing.T) {
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
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID(""),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, messenger.ErrAppNotFound) {
		t.Errorf("err = %v, want messenger.ErrAppNotFound", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (empty appID must not hit network)", calls)
	}
}

// TestInstallApp_EmptyWorkspaceID_FailsSync asserts that an empty
// WorkspaceRef.ID surfaces messenger.ErrInvalidQuery synchronously — the
// install target is required and a missing workspace id never reaches
// Slack.
func TestInstallApp_EmptyWorkspaceID_FailsSync(t *testing.T) {
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
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{},
	)
	if !errors.Is(err, messenger.ErrInvalidQuery) {
		t.Errorf("err = %v, want messenger.ErrInvalidQuery", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (empty workspace must not hit network)", calls)
	}
}

// TestInstallApp_OAuthErrors_RetainAPIError asserts that documented OAuth
// `error` codes (invalid_code, code_already_used, invalid_client_id,
// bad_redirect_uri) surface as *APIError so callers can inspect Code and
// take rotation / retry decisions per the documented Slack semantics.
// None of these have a portable sentinel mapping in M4.2.d.2.
func TestInstallApp_OAuthErrors_RetainAPIError(t *testing.T) {
	t.Parallel()

	codes := []string{"invalid_code", "code_already_used", "invalid_client_id", "bad_redirect_uri"}
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
				WithInstallTokenSink((&recordingTokenSink{}).sink),
				WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
			)
			_, err := c.InstallApp(
				context.Background(),
				messenger.AppID("A1"),
				messenger.WorkspaceRef{ID: "T1"},
			)
			if err == nil {
				t.Fatalf("expected error, got nil (code=%q)", code)
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

// TestInstallApp_RateLimited_PropagatesAPIError asserts that an HTTP 429
// from Slack surfaces as *APIError wrapping ErrRateLimited (the
// slack-package sentinel).
func TestInstallApp_RateLimited_PropagatesAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; got %v", err)
	}
}

// TestInstallApp_CtxCancellation asserts a pre-cancelled ctx returns
// ctx.Err() and never contacts the platform.
func TestInstallApp_CtxCancellation(t *testing.T) {
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
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.InstallApp(
		ctx,
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestInstallApp_CtxCancelled_BeforeValidation asserts that ctx
// cancellation takes precedence over input-shape validation.
func TestInstallApp_CtxCancelled_BeforeValidation(t *testing.T) {
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
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.InstallApp(ctx, messenger.AppID(""), messenger.WorkspaceRef{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if errors.Is(err, messenger.ErrAppNotFound) {
		t.Errorf("err = %v, must NOT be ErrAppNotFound when ctx is cancelled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestInstallApp_NoSink_FailsSync asserts that running InstallApp WITHOUT
// WithInstallTokenSink fails synchronously with ErrInstallTokenSinkUnset
// — the security invariant: tokens MUST go somewhere, and the adapter
// refuses to discard them silently.
func TestInstallApp_NoSink_FailsSync(t *testing.T) {
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
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, ErrInstallTokenSinkUnset) {
		t.Errorf("err = %v, want ErrInstallTokenSinkUnset", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (no sink => no request)", calls)
	}
}

// TestInstallApp_NoParamsResolver_FailsSync asserts that running
// InstallApp WITHOUT WithInstallParamsResolver fails synchronously with
// ErrInstallParamsUnset — the request cannot be assembled without the
// secrets-interface lookup, so the adapter refuses to contact Slack.
func TestInstallApp_NoParamsResolver_FailsSync(t *testing.T) {
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
		WithInstallTokenSink((&recordingTokenSink{}).sink),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, ErrInstallParamsUnset) {
		t.Errorf("err = %v, want ErrInstallParamsUnset", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (no resolver => no request)", calls)
	}
}

// TestInstallApp_ResolverError_DoesNotContactSlack asserts that an
// InstallParamsResolver that returns a non-nil error surfaces wrapped
// from InstallApp WITHOUT contacting Slack — resolution failure is a
// security boundary (no auth code => no exchange).
func TestInstallApp_ResolverError_DoesNotContactSlack(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bootErr := errors.New("secrets store offline")
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithInstallTokenSink((&recordingTokenSink{}).sink),
		WithInstallParamsResolver(func(context.Context, messenger.AppID, messenger.WorkspaceRef) (InstallParams, error) {
			return InstallParams{}, bootErr
		}),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if !errors.Is(err, bootErr) {
		t.Errorf("errors.Is(err, bootErr) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (resolver failure must not hit network)", calls)
	}
}

// TestInstallApp_SinkError_FailsAfterExchange asserts that an
// InstallTokenSink that returns a non-nil error surfaces wrapped from
// InstallApp. The Slack-side exchange has already happened (server
// committed); the adapter does NOT rollback, but the caller observes the
// failure to drive its own reconciliation.
func TestInstallApp_SinkError_FailsAfterExchange(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, oauthV2AccessOKBody)
	}))
	t.Cleanup(srv.Close)

	sinkErr := errors.New("vault write rejected")
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithInstallTokenSink(func(context.Context, InstallTokens) error {
			return sinkErr
		}),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A0123ABCDEF"),
		messenger.WorkspaceRef{ID: "T0123TEAM"},
	)
	if !errors.Is(err, sinkErr) {
		t.Errorf("errors.Is(err, sinkErr) = false, want true; got %v", err)
	}
}

// TestInstallApp_LoggerRedacted asserts the redaction discipline carries
// through InstallApp: the bearer config token, the access_token /
// refresh_token / user access_token (every `xoxb-` / `xoxe-` / `xoxp-`
// family), and every doc-named secret-bearing field NEVER appear in log
// entries. Only structured metadata (method name, status, error type)
// surfaces.
func TestInstallApp_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, oauthV2AccessOKBody)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	sink := &recordingTokenSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe-LEAKED-CONFIG-TOKEN")),
		WithLogger(logger),
		WithInstallTokenSink(sink.sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A0123ABCDEF"),
		messenger.WorkspaceRef{ID: "T0123TEAM"},
	)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}
	assertNoBannedSubstrings(t, logger.snapshot(), []string{
		"xoxe-LEAKED-CONFIG-TOKEN",
		"xoxb-LEAKED-BOT-TOKEN",
		"xoxe-1-LEAKED-REFRESH",
		"xoxp-LEAKED-USER-TOKEN",
		"client-secret-LEAKED",
		"OAUTH-CODE-123",
		"Bearer ",
	})
}

// TestInstallApp_RequestBodyShape asserts the request body sent to
// /oauth.v2.access carries exactly the four documented oauth.v2.access
// fields and the values flow through unchanged from the resolver.
func TestInstallApp_RequestBodyShape(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/oauth.v2.access", http.StatusOK,
		oauthV2AccessOKBody,
		&captured,
	)

	sink := &recordingTokenSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithInstallTokenSink(sink.sink),
		WithInstallParamsResolver(staticInstallParams(canonicalInstallParams())),
	)
	_, err := c.InstallApp(
		context.Background(),
		messenger.AppID("A1"),
		messenger.WorkspaceRef{ID: "T1"},
	)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}

	var req oauthAccessRequest
	if err := json.Unmarshal(captured, &req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if req.Code != "OAUTH-CODE-123" {
		t.Errorf("Code = %q, want OAUTH-CODE-123", req.Code)
	}
	if req.ClientID != "0123456789.0123456789" {
		t.Errorf("ClientID = %q, want 0123456789.0123456789", req.ClientID)
	}
	if req.ClientSecret != "client-secret-LEAKED" {
		t.Errorf("ClientSecret = %q, want client-secret-LEAKED", req.ClientSecret)
	}
	if req.RedirectURI != "https://example.invalid/oauth/callback" {
		t.Errorf("RedirectURI = %q, want https://example.invalid/oauth/callback", req.RedirectURI)
	}
}

// TestInstallApp_RateLimiterTier_OAuthV2Access asserts that
// `oauth.v2.access` is registered in the default method-tier registry
// at tier-3 (the value M4.2.a wired in anticipation of M4.2.d.2).
func TestInstallApp_RateLimiterTier_OAuthV2Access(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	if got := rl.Tier("oauth.v2.access"); got != Tier3 {
		t.Errorf("Tier(oauth.v2.access) = %v, want %v", got, Tier3)
	}
}
