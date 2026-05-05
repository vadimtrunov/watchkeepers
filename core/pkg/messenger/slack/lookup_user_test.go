package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// TestLookupUser_HappyPath_UserID asserts the simplest case: a
// UserQuery with ID prefixed `U` routes to /users.info and returns a
// populated User record. The form-encoded `user` parameter carries the
// id; Slack's users.info accepts only application/x-www-form-urlencoded
// for the `user` query parameter via POST body or URL — we POST a JSON
// envelope mirroring the rest of the package's calling convention.
func TestLookupUser_HappyPath_UserID(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.info" {
			t.Errorf("path = %q, want /users.info", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"U0123ABC","name":"alice","real_name":"Alice","is_bot":false,"profile":{"display_name":"Alice","email":"alice@example.com"},"team_id":"T0123","tz":"America/Los_Angeles"}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	user, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "U0123ABC"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if user.ID != "U0123ABC" {
		t.Errorf("ID = %q, want U0123ABC", user.ID)
	}
	if user.Handle != "alice" {
		t.Errorf("Handle = %q, want alice", user.Handle)
	}
	if user.DisplayName != "Alice" {
		t.Errorf("DisplayName = %q, want Alice", user.DisplayName)
	}
	if user.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", user.Email)
	}
	if user.IsBot {
		t.Errorf("IsBot = true, want false")
	}
	if user.Metadata["team_id"] != "T0123" {
		t.Errorf("Metadata[team_id] = %q, want T0123", user.Metadata["team_id"])
	}
	if user.Metadata["tz"] != "America/Los_Angeles" {
		t.Errorf("Metadata[tz] = %q, want America/Los_Angeles", user.Metadata["tz"])
	}

	// Wire-format: the request body must carry user="U..." per Slack's
	// JSON-or-form envelope for users.info.
	if !strings.Contains(string(captured), `"user":"U0123ABC"`) {
		t.Errorf("request body missing user param: %s", string(captured))
	}
}

// TestLookupUser_HappyPath_BotID asserts that a UserQuery with ID
// prefixed `B` routes to /bots.info and returns a User record with
// IsBot=true. The Slack id-prefix discriminates: `U`/`W` → user,
// `B` → bot. Documented at
// https://api.slack.com/events/team_join#example_event-payload.
func TestLookupUser_HappyPath_BotID(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bots.info" {
			t.Errorf("path = %q, want /bots.info", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"bot":{"id":"B0123BOT","name":"watchkeeper-bot","app_id":"A0123","user_id":"U0123BOT","icons":{"image_72":"https://example.invalid/bot.png"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	user, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "B0123BOT"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if user.ID != "B0123BOT" {
		t.Errorf("ID = %q, want B0123BOT", user.ID)
	}
	if user.Handle != "watchkeeper-bot" {
		t.Errorf("Handle = %q, want watchkeeper-bot", user.Handle)
	}
	if !user.IsBot {
		t.Errorf("IsBot = false, want true")
	}
	if user.Metadata["app_id"] != "A0123" {
		t.Errorf("Metadata[app_id] = %q, want A0123", user.Metadata["app_id"])
	}
	if user.Metadata["bot_user_id"] != "U0123BOT" {
		t.Errorf("Metadata[bot_user_id] = %q, want U0123BOT", user.Metadata["bot_user_id"])
	}

	// Wire-format: the request body must carry bot="B..." per Slack's
	// JSON-or-form envelope for bots.info.
	if !strings.Contains(string(captured), `"bot":"B0123BOT"`) {
		t.Errorf("request body missing bot param: %s", string(captured))
	}
}

// TestLookupUser_HappyPath_WorkspaceUserID asserts that a UserQuery
// with ID prefixed `W` (Slack Connect / Enterprise Grid workspace user
// id) also routes to /users.info — the documented Slack id family for
// non-bot human users.
func TestLookupUser_HappyPath_WorkspaceUserID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.info" {
			t.Errorf("path = %q, want /users.info", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"W0123XYZ","name":"bob","is_bot":false,"profile":{"display_name":"Bob"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	user, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "W0123XYZ"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if user.ID != "W0123XYZ" {
		t.Errorf("ID = %q, want W0123XYZ", user.ID)
	}
}

// TestLookupUser_Email_LookupByEmail asserts that a UserQuery with only
// Email populated routes to /users.lookupByEmail.
func TestLookupUser_Email_LookupByEmail(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.lookupByEmail" {
			t.Errorf("path = %q, want /users.lookupByEmail", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"U0EMAIL","name":"carol","is_bot":false,"profile":{"display_name":"Carol","email":"carol@example.com"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	user, err := c.LookupUser(context.Background(), messenger.UserQuery{Email: "carol@example.com"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if user.ID != "U0EMAIL" {
		t.Errorf("ID = %q, want U0EMAIL", user.ID)
	}
	if !strings.Contains(string(captured), `"email":"carol@example.com"`) {
		t.Errorf("request body missing email param: %s", string(captured))
	}
}

// TestLookupUser_Handle_Unsupported asserts that a UserQuery with only
// Handle populated returns messenger.ErrUnsupported — Slack does not
// expose a `users.info`-by-handle endpoint, and lookupByEmail requires
// an email rather than the @handle. Callers that have a handle must
// page users.list to resolve it (a separate concern; M4.2.d.1 deflects
// the field rather than silently doing the wrong thing).
func TestLookupUser_Handle_Unsupported(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{Handle: "alice"})
	if !errors.Is(err, messenger.ErrUnsupported) {
		t.Errorf("err = %v, want messenger.ErrUnsupported", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (handle lookup must not hit network)", calls)
	}
}

// TestLookupUser_EmptyQuery_ErrInvalidQuery asserts that a query with
// none of ID/Handle/Email populated returns messenger.ErrInvalidQuery
// synchronously — per the messenger.UserQuery contract.
func TestLookupUser_EmptyQuery_ErrInvalidQuery(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{})
	if !errors.Is(err, messenger.ErrInvalidQuery) {
		t.Errorf("err = %v, want messenger.ErrInvalidQuery", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestLookupUser_OverpopulatedQuery_ErrInvalidQuery asserts that a
// query with more than one of ID/Handle/Email populated returns
// messenger.ErrInvalidQuery synchronously — per the messenger.UserQuery
// contract ("populates exactly one").
func TestLookupUser_OverpopulatedQuery_ErrInvalidQuery(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	cases := []messenger.UserQuery{
		{ID: "U1", Email: "a@b.c"},
		{ID: "U1", Handle: "alice"},
		{Handle: "alice", Email: "a@b.c"},
		{ID: "U1", Handle: "alice", Email: "a@b.c"},
	}
	for i, q := range cases {
		_, err := c.LookupUser(context.Background(), q)
		if !errors.Is(err, messenger.ErrInvalidQuery) {
			t.Errorf("case %d: err = %v, want messenger.ErrInvalidQuery", i, err)
		}
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestLookupUser_UnknownIDPrefix_ErrInvalidQuery asserts that an ID
// without a recognised Slack prefix (must be U / W / B) is rejected
// synchronously rather than misrouted. Catching it client-side avoids
// burning a tier-3/4 rate-limit token on a known-bad request.
func TestLookupUser_UnknownIDPrefix_ErrInvalidQuery(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	for _, id := range []string{"X1234", "12345", "u-lower-prefix"} {
		_, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: id})
		if !errors.Is(err, messenger.ErrInvalidQuery) {
			t.Errorf("id=%q: err = %v, want messenger.ErrInvalidQuery", id, err)
		}
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestLookupUser_UserNotFound_PortableSentinel asserts that a Slack
// `error: "user_not_found"` envelope surfaces as
// messenger.ErrUserNotFound (the portable sentinel).
func TestLookupUser_UserNotFound_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"user_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "U404"})
	if !errors.Is(err, messenger.ErrUserNotFound) {
		t.Errorf("errors.Is(err, messenger.ErrUserNotFound) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("error is not *APIError underneath: %T", err)
	}
}

// TestLookupUser_BotNotFound_PortableSentinel asserts that a Slack
// `error: "bot_not_found"` envelope (returned by bots.info) surfaces as
// messenger.ErrUserNotFound — both `users.info` and `bots.info` map to
// the same portable sentinel because the caller's intent is the same
// ("is this principal resolvable?").
func TestLookupUser_BotNotFound_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"bot_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "B404"})
	if !errors.Is(err, messenger.ErrUserNotFound) {
		t.Errorf("errors.Is(err, messenger.ErrUserNotFound) = false, want true; got %v", err)
	}
}

// TestLookupUser_UsersNotFound_FromLookupByEmail asserts that the
// Slack `error: "users_not_found"` envelope (returned by
// users.lookupByEmail — note the plural) also surfaces as
// messenger.ErrUserNotFound.
func TestLookupUser_UsersNotFound_FromLookupByEmail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"users_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{Email: "ghost@example.com"})
	if !errors.Is(err, messenger.ErrUserNotFound) {
		t.Errorf("errors.Is(err, messenger.ErrUserNotFound) = false, want true; got %v", err)
	}
}

// TestLookupUser_RateLimited_PropagatesAPIError asserts that an HTTP
// 429 from Slack surfaces as *APIError wrapping ErrRateLimited.
func TestLookupUser_RateLimited_PropagatesAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "U1"})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; got %v", err)
	}
}

// TestLookupUser_CtxCancellation asserts a pre-cancelled ctx returns
// ctx.Err() and never contacts the platform.
func TestLookupUser_CtxCancellation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.LookupUser(ctx, messenger.UserQuery{ID: "U1"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestLookupUser_CtxCancelled_BeforeValidation asserts that ctx
// cancellation takes precedence over input-shape validation: a
// cancelled ctx with an empty UserQuery surfaces ctx.Err() rather than
// ErrInvalidQuery.
func TestLookupUser_CtxCancelled_BeforeValidation(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.LookupUser(ctx, messenger.UserQuery{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if errors.Is(err, messenger.ErrInvalidQuery) {
		t.Errorf("err = %v, must NOT be ErrInvalidQuery when ctx is cancelled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestLookupUser_LoggerRedacted asserts the redaction discipline
// carries through LookupUser: the bearer token, the queried ID/Email,
// and the returned User fields (DisplayName, Email — both PII) NEVER
// appear in log entries.
func TestLookupUser_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"user":{"id":"U1","name":"alice","real_name":"PII-DISPLAY-NAME","is_bot":false,"profile":{"display_name":"PII-DISPLAY-NAME","email":"PII-EMAIL@example.com"}}}`)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN")),
		WithLogger(logger),
	)
	_, err := c.LookupUser(context.Background(), messenger.UserQuery{ID: "U1"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	assertNoBannedSubstrings(t, logger.snapshot(), []string{
		"xoxb-LEAKED-TOKEN",
		"PII-DISPLAY-NAME",
		"PII-EMAIL@example.com",
		"Bearer ",
	})
}
