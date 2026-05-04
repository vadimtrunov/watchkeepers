package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// profileSetRequest mirrors the JSON envelope users.profile.set
// expects. Slack wraps the user-editable fields in a `profile` object
// and accepts the rest of the envelope at the top level.
type profileSetRequest struct {
	Profile map[string]string `json:"profile"`
}

// TestSetBotProfile_HappyPath asserts the cross-platform spine
// (DisplayName + StatusText) lands on the Slack profile object verbatim
// and the request hits /users.profile.set with a 200 ok response.
func TestSetBotProfile_HappyPath(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.profile.set" {
			t.Errorf("path = %q, want /users.profile.set", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"profile":{"display_name":"watchkeeper"}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{
		DisplayName: "watchkeeper",
		StatusText:  "on duty",
	})
	if err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	var got profileSetRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.Profile["display_name"] != "watchkeeper" {
		t.Errorf("profile.display_name = %q, want watchkeeper", got.Profile["display_name"])
	}
	if got.Profile["status_text"] != "on duty" {
		t.Errorf("profile.status_text = %q, want on duty", got.Profile["status_text"])
	}
	// Empty fields must NOT be sent (per messenger.BotProfile contract:
	// "Empty leaves unchanged"). status_emoji was never set, so it
	// MUST be absent from the wire payload.
	if _, present := got.Profile["status_emoji"]; present {
		t.Errorf("profile.status_emoji set but BotProfile.Metadata did not provide it")
	}
}

// TestSetBotProfile_EmptyFields_NotSent asserts the contract: an empty
// DisplayName / StatusText leaves the existing value unchanged on the
// platform. We assert by checking the wire payload omits them entirely
// (Slack treats absent fields as "unchanged"; sending empty strings
// would actually CLEAR them).
func TestSetBotProfile_EmptyFields_NotSent(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{
		DisplayName: "watchkeeper",
		// StatusText empty intentionally.
	})
	if err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	var got profileSetRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, present := got.Profile["status_text"]; present {
		t.Errorf("profile.status_text present but BotProfile left it empty")
	}
	if got.Profile["display_name"] != "watchkeeper" {
		t.Errorf("profile.display_name = %q, want watchkeeper", got.Profile["display_name"])
	}
}

// TestSetBotProfile_AllFieldsEmpty_NoOp asserts that an entirely-empty
// BotProfile (no DisplayName, no StatusText, no AvatarPNG, no
// Metadata) results in NO network call — there's nothing to update,
// and burning a tier-3 rate-limit token for a no-op would be wasteful.
func TestSetBotProfile_AllFieldsEmpty_NoOp(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	if err := c.SetBotProfile(context.Background(), messenger.BotProfile{}); err != nil {
		t.Errorf("SetBotProfile(empty): err = %v, want nil", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (empty profile must not hit network)", calls)
	}
}

// TestSetBotProfile_AvatarPNG_Unsupported asserts that the typed
// AvatarPNG byte slice triggers messenger.ErrUnsupported — Slack's
// avatar upload requires users.setPhoto with multipart encoding,
// which is a follow-up implementation. Per the messenger contract,
// "Adapters that don't support avatar set return ErrUnsupported".
func TestSetBotProfile_AvatarPNG_Unsupported(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{
		DisplayName: "x",
		AvatarPNG:   []byte{0x89, 'P', 'N', 'G'},
	})
	if !errors.Is(err, messenger.ErrUnsupported) {
		t.Errorf("err = %v, want messenger.ErrUnsupported", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (avatar upload not yet wired)", calls)
	}
}

// TestSetBotProfile_MetadataKeys_PassedThrough asserts that the
// documented Slack-specific Metadata keys (status_emoji,
// status_expiration, real_name) flow into the profile object.
func TestSetBotProfile_MetadataKeys_PassedThrough(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{
		DisplayName: "wk",
		Metadata: map[string]string{
			"status_emoji":      ":robot_face:",
			"status_expiration": "0",
			"real_name":         "Watch Keeper",
			"unknown_extra_key": "ignored",
		},
	})
	if err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	var got profileSetRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.Profile["status_emoji"] != ":robot_face:" {
		t.Errorf("profile.status_emoji = %q, want :robot_face:", got.Profile["status_emoji"])
	}
	if got.Profile["status_expiration"] != "0" {
		t.Errorf("profile.status_expiration = %q, want 0", got.Profile["status_expiration"])
	}
	if got.Profile["real_name"] != "Watch Keeper" {
		t.Errorf("profile.real_name = %q, want Watch Keeper", got.Profile["real_name"])
	}
	if _, present := got.Profile["unknown_extra_key"]; present {
		t.Errorf("profile.unknown_extra_key set; adapter must drop unrecognised metadata")
	}
}

// TestSetBotProfile_NotAuthed_PortableSentinel asserts that a Slack
// not_authed / invalid_auth envelope surfaces wrapped so callers can
// match the slack-package ErrInvalidAuth sentinel via errors.Is.
func TestSetBotProfile_NotAuthed_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"not_authed"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{DisplayName: "x"})
	if !errors.Is(err, ErrInvalidAuth) {
		t.Errorf("errors.Is(err, ErrInvalidAuth) = false, want true; got %v", err)
	}
}

// TestSetBotProfile_InvalidProfile_RetainsAPIError asserts that
// non-portable error codes (invalid_profile, profile_set_failed) come
// through as *APIError so callers can inspect Code.
func TestSetBotProfile_InvalidProfile_RetainsAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_profile"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	err := c.SetBotProfile(context.Background(), messenger.BotProfile{DisplayName: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.Code != "invalid_profile" {
		t.Errorf("Code = %q, want invalid_profile", apiErr.Code)
	}
}

// TestSetBotProfile_CtxCancellation asserts a pre-cancelled ctx
// returns ctx.Err() without contacting the platform.
func TestSetBotProfile_CtxCancellation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.SetBotProfile(ctx, messenger.BotProfile{DisplayName: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}
