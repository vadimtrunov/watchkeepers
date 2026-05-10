package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

type openIMRequestWire struct {
	Users           string `json:"users"`
	ReturnIM        bool   `json:"return_im,omitempty"`
	PreventCreation bool   `json:"prevent_creation,omitempty"`
}

func TestOpenIMChannel_HappyPath_ReturnsChannelID(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(t, "/conversations.open", http.StatusOK,
		`{"ok":true,"channel":{"id":"D9999"}}`, &captured)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	got, err := c.OpenIMChannel(context.Background(), "U123")
	if err != nil {
		t.Fatalf("OpenIMChannel: %v", err)
	}
	if got != "D9999" {
		t.Errorf("channel id = %q, want D9999", got)
	}

	var body openIMRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Users != "U123" {
		t.Errorf("users = %q, want U123", body.Users)
	}
	if !body.ReturnIM {
		t.Errorf("return_im = false, want true")
	}
}

func TestOpenIMChannel_EmptyUserID_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.OpenIMChannel(context.Background(), "")
	if !errors.Is(err, messenger.ErrUserNotFound) {
		t.Errorf("err = %v, want messenger.ErrUserNotFound", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

func TestOpenIMChannel_UserNotFound_PortableSentinel(t *testing.T) {
	t.Parallel()

	codes := []string{"user_not_found", "users_not_found"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":false,"error":"`+code+`"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.OpenIMChannel(context.Background(), "UXXX")
			if !errors.Is(err, messenger.ErrUserNotFound) {
				t.Errorf("errors.Is(messenger.ErrUserNotFound) = false, want true; got %v", err)
			}
		})
	}
}

func TestOpenIMChannel_CannotDMBot_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"cannot_dm_bot"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.OpenIMChannel(context.Background(), "B777")
	if !errors.Is(err, ErrCannotDMBot) {
		t.Errorf("errors.Is(ErrCannotDMBot) = false, want true; got %v", err)
	}
}

func TestOpenIMChannel_MissingScope_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.OpenIMChannel(context.Background(), "U1")
	if !errors.Is(err, ErrMissingScope) {
		t.Errorf("errors.Is(ErrMissingScope) = false, want true; got %v", err)
	}
}

// TestOpenIMChannel_RateLimited_PropagatesRetryAfter pins iter-1
// critic Missing #1: the 429 path on OpenIMChannel MUST surface as
// ErrRateLimited with RetryAfter populated (the path is shared with
// ConversationsHistory via Client.Do/handle429, but having no test
// here leaves the lift unguarded against a future refactor that
// shunts open_im through a parallel code path).
func TestOpenIMChannel_RateLimited_PropagatesRetryAfter(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "8")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.OpenIMChannel(context.Background(), "U1")
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(ErrRateLimited) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 8*time.Second {
		t.Errorf("RetryAfter = %v, want 8s", apiErr.RetryAfter)
	}
}

func TestOpenIMChannel_CtxCancelled_NoNetwork(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.OpenIMChannel(ctx, "U1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}
