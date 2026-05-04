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

// profileSetRequest mirrors the JSON envelope users.profile.set
// expects. Slack wraps the user-editable fields in a `profile` object
// and accepts the rest of the envelope at the top level.
//
// Profile is typed `map[string]any` because Slack documents some
// leaves as numeric (notably `status_expiration` — Unix-timestamp
// integer); encoding everything as `string` would produce
// `invalid_profile` on real workspaces. Tests that compare a leaf
// value cast through fmt.Sprint so the assertion stays type-agnostic
// (a string leaf prints as itself; a numeric leaf prints in canonical
// form).
type profileSetRequest struct {
	Profile map[string]any `json:"profile"`
}

// profileLeaf returns the canonical string form of a profile-map leaf
// for assertions. JSON-decoded numbers come back as `float64` (or
// [json.Number] under UseNumber) — fmt.Sprint normalises both to a
// human-readable form. Returns "" when the leaf is absent.
func profileLeaf(p map[string]any, key string) string {
	v, ok := p[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		// Integer-valued floats render without trailing ".000000"
		// when they're exact integers (Slack's status_expiration is
		// always integral).
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	default:
		return fmt.Sprintf("%v", v)
	}
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
	if v := profileLeaf(got.Profile, "display_name"); v != "watchkeeper" {
		t.Errorf("profile.display_name = %q, want watchkeeper", v)
	}
	if v := profileLeaf(got.Profile, "status_text"); v != "on duty" {
		t.Errorf("profile.status_text = %q, want on duty", v)
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
	if v := profileLeaf(got.Profile, "display_name"); v != "watchkeeper" {
		t.Errorf("profile.display_name = %q, want watchkeeper", v)
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
	if v := profileLeaf(got.Profile, "status_emoji"); v != ":robot_face:" {
		t.Errorf("profile.status_emoji = %q, want :robot_face:", v)
	}
	// status_expiration must land as a JSON number (Slack documents
	// it as Unix-timestamp INT). profileLeaf normalises a numeric
	// leaf to canonical decimal form.
	if v := profileLeaf(got.Profile, "status_expiration"); v != "0" {
		t.Errorf("profile.status_expiration = %q, want 0", v)
	}
	if v := profileLeaf(got.Profile, "real_name"); v != "Watch Keeper" {
		t.Errorf("profile.real_name = %q, want Watch Keeper", v)
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

// TestSetBotProfile_CtxCancelled_BeforeValidation asserts that ctx
// cancellation takes precedence over the AvatarPNG-unsupported
// validation: a cancelled ctx surfaces ctx.Err() rather than
// messenger.ErrUnsupported. Mirrors the convention of most Go HTTP-
// style adapters (ctx is the canonical "abandon work" signal and
// trumps any input precondition).
func TestSetBotProfile_CtxCancelled_BeforeValidation(t *testing.T) {
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
	err := c.SetBotProfile(ctx, messenger.BotProfile{
		AvatarPNG: []byte{0x89, 'P', 'N', 'G'},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if errors.Is(err, messenger.ErrUnsupported) {
		t.Errorf("err = %v, must NOT be messenger.ErrUnsupported when ctx is cancelled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestSetBotProfile_StatusExpiration_JSONNumber asserts that
// `status_expiration` lands on the wire as a JSON NUMBER, not a JSON
// string. Slack's users.profile.set documents
// profile.status_expiration as a Unix-timestamp integer; sending
// `"1234567890"` (a JSON string) triggers `invalid_profile` on real
// workspaces.
//
// We decode the captured body with [json.Decoder.UseNumber] into a
// generic map and assert the leaf is typed [json.Number] (which
// Decoder produces only for genuine JSON numbers). A string-typed
// payload would surface as Go-typed `string` instead and fail the
// assertion. We also re-encode the captured body to canonical form
// and assert the substring `"status_expiration":1234567890` (without
// surrounding quotes around the number) appears — a belt-and-braces
// check against the wire format directly, since the [json.Number]
// type alias would happily round-trip a JSON-string source too.
func TestSetBotProfile_StatusExpiration_JSONNumber(t *testing.T) {
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
			"status_expiration": "1234567890",
		},
	})
	if err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	// Wire-format assertion: numeric literal, no surrounding quotes.
	if !strings.Contains(string(captured), `"status_expiration":1234567890`) {
		t.Fatalf("status_expiration must serialise as a JSON number; raw body: %s", string(captured))
	}
	if strings.Contains(string(captured), `"status_expiration":"1234567890"`) {
		t.Fatalf("status_expiration serialised as a JSON string; raw body: %s", string(captured))
	}

	// Type-level assertion via Decoder.UseNumber — only genuine JSON
	// numbers come back as [json.Number]; JSON strings come back as
	// Go `string`.
	dec := json.NewDecoder(strings.NewReader(string(captured)))
	dec.UseNumber()
	var generic struct {
		Profile map[string]any `json:"profile"`
	}
	if err := dec.Decode(&generic); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	raw, present := generic.Profile["status_expiration"]
	if !present {
		t.Fatalf("status_expiration absent from profile: %s", string(captured))
	}
	num, ok := raw.(json.Number)
	if !ok {
		t.Fatalf("status_expiration leaf type = %T (%v), want json.Number; raw body: %s", raw, raw, string(captured))
	}
	if got, err := num.Int64(); err != nil || got != 1234567890 {
		t.Fatalf("status_expiration Int64 = %d (err=%v), want 1234567890", got, err)
	}
}

// TestSetBotProfile_StatusExpiration_NonNumericDropped asserts that
// when the caller's Metadata["status_expiration"] is not parseable as
// int64, the entry is silently dropped from the profile map (mirrors
// the optionalBool fall-through-on-bad-input discipline in
// send_message.go — adapter does not panic on malformed caller input,
// and forwarding garbage produces a less actionable error than
// omitting the field).
func TestSetBotProfile_StatusExpiration_NonNumericDropped(t *testing.T) {
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
			"status_expiration": "not-a-number",
		},
	})
	if err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	var got struct {
		Profile map[string]any `json:"profile"`
	}
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if _, present := got.Profile["status_expiration"]; present {
		t.Errorf("profile.status_expiration present despite unparseable input; want dropped")
	}
}

// TestSetBotProfile_LoggerRedacted asserts the M4.2.a redaction
// discipline carries through the SetBotProfile path: the bearer
// token, the configured DisplayName (which may carry PII), and the
// `Bearer ` prefix NEVER appear in log entries. Symmetric with
// TestSendMessage_LoggerRedacted — every adapter method must obey
// the same redaction contract. Asserts both success and error paths.
func TestSetBotProfile_LoggerRedacted(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true,"profile":{"display_name":"PII-DISPLAY-NAME-REDACT"}}`)
		}))
		t.Cleanup(srv.Close)

		logger := &recordingLogger{}
		c := NewClient(
			WithBaseURL(srv.URL),
			WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN")),
			WithLogger(logger),
		)
		err := c.SetBotProfile(context.Background(), messenger.BotProfile{
			DisplayName: "PII-DISPLAY-NAME-REDACT",
		})
		if err != nil {
			t.Fatalf("SetBotProfile: %v", err)
		}
		assertNoBannedSubstrings(t, logger.snapshot(), []string{
			"xoxb-LEAKED-TOKEN", "PII-DISPLAY-NAME-REDACT", "Bearer ",
		})
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_profile"}`)
		}))
		t.Cleanup(srv.Close)

		logger := &recordingLogger{}
		c := NewClient(
			WithBaseURL(srv.URL),
			WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN")),
			WithLogger(logger),
		)
		err := c.SetBotProfile(context.Background(), messenger.BotProfile{
			DisplayName: "PII-DISPLAY-NAME-REDACT",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		assertNoBannedSubstrings(t, logger.snapshot(), []string{
			"xoxb-LEAKED-TOKEN", "PII-DISPLAY-NAME-REDACT", "Bearer ",
		})
	})
}

// assertNoBannedSubstrings is a shared helper used by the redaction
// tests across SendMessage and SetBotProfile. Mirrors the inline loop
// in TestSendMessage_LoggerRedacted; lifted so the symmetric coverage
// stays a one-liner per adapter method.
func assertNoBannedSubstrings(t *testing.T, entries []recordedLogEntry, banned []string) {
	t.Helper()
	for _, e := range entries {
		entryStr := fmt.Sprintf("msg=%q kv=%v", e.Msg, e.KV)
		for _, b := range banned {
			if strings.Contains(entryStr, b) {
				t.Errorf("log entry %q leaks banned substring %q", entryStr, b)
			}
		}
	}
}
