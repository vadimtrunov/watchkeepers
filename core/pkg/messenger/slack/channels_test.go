package slack

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// createChannelRequestWire mirrors the JSON envelope CreateChannel
// places on the wire. Tests decode the captured request into this
// struct and assert each field. Mirroring keeps the assertions
// readable and independent of map ordering.
type createChannelRequestWire struct {
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
}

// inviteRequestWire mirrors the JSON envelope InviteToChannel and
// RevealChannel place on the wire.
type inviteRequestWire struct {
	Channel string `json:"channel"`
	Users   string `json:"users"`
}

// archiveRequestWire mirrors the JSON envelope ArchiveChannel places
// on the wire.
type archiveRequestWire struct {
	Channel string `json:"channel"`
}

// listRequestWire mirrors the JSON envelope the name_taken recovery
// path places on `conversations.list`.
type listRequestWire struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	Types  string `json:"types"`
}

// ============================================================
// CreateChannel
// ============================================================

func TestCreateChannel_HappyPath_PrivateChannel(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.create", http.StatusOK,
		`{"ok":true,"channel":{"id":"C1ABCD"}}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "k2k-deadbeef", true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "C1ABCD" {
		t.Errorf("channel id = %q, want C1ABCD", id)
	}

	var body createChannelRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Name != "k2k-deadbeef" {
		t.Errorf("name = %q, want k2k-deadbeef", body.Name)
	}
	if !body.IsPrivate {
		t.Errorf("is_private = false, want true")
	}
}

func TestCreateChannel_HappyPath_PublicChannel(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.create", http.StatusOK,
		`{"ok":true,"channel":{"id":"C2PUB"}}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "public-room", false)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "C2PUB" {
		t.Errorf("channel id = %q, want C2PUB", id)
	}

	var body createChannelRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.IsPrivate {
		t.Errorf("is_private = true, want false")
	}
}

func TestCreateChannel_TrimsLeadingTrailingWhitespace(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.create", http.StatusOK,
		`{"ok":true,"channel":{"id":"C1"}}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.CreateChannel(context.Background(), "  trimmed  ", true); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	var body createChannelRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Name != "trimmed" {
		t.Errorf("name = %q, want trimmed", body.Name)
	}
}

func TestCreateChannel_EmptyName_FailsSync(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "   ", "\t\n"} {
		name := name
		t.Run("name="+name, func(t *testing.T) {
			t.Parallel()

			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.CreateChannel(context.Background(), name, true)
			if !errors.Is(err, ErrInvalidChannelName) {
				t.Errorf("err = %v, want ErrInvalidChannelName", err)
			}
			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Errorf("server saw %d calls, want 0", got)
			}
		})
	}
}

func TestCreateChannel_MissingChannelIDInResponse_Errors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"channel":{}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.CreateChannel(context.Background(), "k2k-x", true)
	if err == nil {
		t.Fatalf("CreateChannel returned nil error on empty channel id")
	}
	if !strings.Contains(err.Error(), "response missing channel id") {
		t.Errorf("err = %v, want message containing 'response missing channel id'", err)
	}
}

func TestCreateChannel_InvalidName_PortableSentinel(t *testing.T) {
	t.Parallel()

	codes := []string{
		"invalid_name",
		"invalid_name_punctuation",
		"invalid_name_required",
		"invalid_name_specials",
		"invalid_name_maxlength",
	}
	for _, code := range codes {
		code := code
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":false,"error":"`+code+`"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.CreateChannel(context.Background(), "BAD!", true)
			if !errors.Is(err, ErrInvalidChannelName) {
				t.Errorf("errors.Is(ErrInvalidChannelName) = false, want true; got %v", err)
			}
		})
	}
}

func TestCreateChannel_MissingScope_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.CreateChannel(context.Background(), "k2k-x", true)
	if !errors.Is(err, ErrMissingScope) {
		t.Errorf("errors.Is(ErrMissingScope) = false, want true; got %v", err)
	}
}

func TestCreateChannel_RateLimited_PropagatesRetryAfter(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.CreateChannel(context.Background(), "k2k-x", true)
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(ErrRateLimited) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %v, want 12s", apiErr.RetryAfter)
	}
}

func TestCreateChannel_CtxCancelled_NoNetwork(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CreateChannel(ctx, "k2k-x", true)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

// TestCreateChannel_NameTaken_IdempotentRecovery pins the M1.1.b AC
// "Idempotent CreateChannel (existing channel-name returns its ID)".
// First request returns name_taken; recovery path calls
// conversations.list and returns the colliding channel's id.
func TestCreateChannel_NameTaken_IdempotentRecovery(t *testing.T) {
	t.Parallel()

	var (
		createCalls int32
		listCalls   int32
		listReq     []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			atomic.AddInt32(&createCalls, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			atomic.AddInt32(&listCalls, 1)
			body, _ := io.ReadAll(r.Body)
			listReq = body
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"ok": true,
				"channels": [
					{"id":"COTHER","name":"unrelated","is_archived":false},
					{"id":"CMATCH","name":"k2k-collision","is_archived":false}
				],
				"response_metadata": {"next_cursor":""}
			}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "k2k-collision", true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "CMATCH" {
		t.Errorf("channel id = %q, want CMATCH", id)
	}
	if got := atomic.LoadInt32(&createCalls); got != 1 {
		t.Errorf("create calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Errorf("list calls = %d, want 1", got)
	}

	var body listRequestWire
	if err := json.Unmarshal(listReq, &body); err != nil {
		t.Fatalf("decode list request: %v", err)
	}
	if body.Types != "private_channel" {
		t.Errorf("types = %q, want private_channel", body.Types)
	}
	if body.Limit != conversationsListPageLimit {
		t.Errorf("limit = %d, want %d", body.Limit, conversationsListPageLimit)
	}
}

// TestCreateChannel_NameTaken_RecoveryFiltersPublicVsPrivate pins the
// kind-aware filter: a private create that collides with a same-name
// public channel does NOT silently bind to the public id (which would
// flip the privacy posture of the K2K conversation).
func TestCreateChannel_NameTaken_RecoveryFiltersPublicVsPrivate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			// We were asked for private_channel only; return zero matches
			// (the colliding channel was public, not visible through this
			// filter).
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"ok": true,
				"channels": [],
				"response_metadata": {"next_cursor":""}
			}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.CreateChannel(context.Background(), "k2k-collision", true)
	if err == nil {
		t.Fatalf("CreateChannel returned nil error; want composite error")
	}
	if !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("errors.Is(ErrChannelNameTaken) = false, want true; got %v", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want message containing 'not found'", err)
	}
}

// TestCreateChannel_NameTaken_RecoveryPagination pins the cursor-paged
// recovery: a workspace with O(1000) private channels surfaces the
// match on a later page.
func TestCreateChannel_NameTaken_RecoveryPagination(t *testing.T) {
	t.Parallel()

	var listCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			n := atomic.AddInt32(&listCalls, 1)
			body, _ := io.ReadAll(r.Body)
			var req listRequestWire
			_ = json.Unmarshal(body, &req)
			switch n {
			case 1:
				if req.Cursor != "" {
					t.Errorf("first page cursor = %q, want empty", req.Cursor)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{
					"ok": true,
					"channels": [{"id":"C1","name":"unrelated","is_archived":false}],
					"response_metadata": {"next_cursor":"PAGE2"}
				}`)
			case 2:
				if req.Cursor != "PAGE2" {
					t.Errorf("second page cursor = %q, want PAGE2", req.Cursor)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{
					"ok": true,
					"channels": [{"id":"CFOUND","name":"k2k-late","is_archived":false}],
					"response_metadata": {"next_cursor":""}
				}`)
			default:
				t.Errorf("unexpected list page %d", n)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "k2k-late", true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "CFOUND" {
		t.Errorf("channel id = %q, want CFOUND", id)
	}
	if got := atomic.LoadInt32(&listCalls); got != 2 {
		t.Errorf("list calls = %d, want 2", got)
	}
}

// TestCreateChannel_NameTaken_RecoverySkipsArchived pins the iter-1
// codex P2 fix: an archived same-name same-kind channel must NOT be
// returned from CreateChannel's idempotent recovery path. The K2K
// consumer needs a usable channel; an archived id would surface as
// ErrIsArchived on the very next InviteToChannel.
func TestCreateChannel_NameTaken_RecoverySkipsArchived(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			// Only an ARCHIVED hit exists for the requested name.
			// The recovery path must skip it and surface a composite
			// error rather than return the archived id.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"ok": true,
				"channels": [
					{"id":"COTHER","name":"unrelated","is_archived":false},
					{"id":"CARCHIVED","name":"k2k-recycled","is_archived":true}
				],
				"response_metadata": {"next_cursor":""}
			}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "k2k-recycled", true)
	if err == nil {
		t.Fatalf("CreateChannel returned (%q, nil); want composite ErrChannelNameTaken because the only same-name hit was archived", id)
	}
	if !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("errors.Is(ErrChannelNameTaken) = false, want true; got %v", err)
	}
	if !strings.Contains(err.Error(), "non-archived") {
		t.Errorf("err = %v, want message mentioning 'non-archived'", err)
	}
}

// TestCreateChannel_NameTaken_RecoveryPrefersNonArchivedOverArchived
// pins that if both an archived and a non-archived same-name hit
// exist on the same page, the non-archived id wins (the K2K consumer
// can use it).
func TestCreateChannel_NameTaken_RecoveryPrefersNonArchivedOverArchived(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			// Archived appears FIRST; the recovery path must skip it
			// and continue scanning so the live id wins.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"ok": true,
				"channels": [
					{"id":"COLD","name":"k2k-shared","is_archived":true},
					{"id":"CLIVE","name":"k2k-shared","is_archived":false}
				],
				"response_metadata": {"next_cursor":""}
			}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	id, err := c.CreateChannel(context.Background(), "k2k-shared", true)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id != "CLIVE" {
		t.Errorf("channel id = %q, want CLIVE (non-archived match must win over COLD archived match)", id)
	}
}

// TestCreateChannel_NameTaken_RecoveryListFails surfaces the composite
// error when conversations.list fails (auth, scope, transport).
func TestCreateChannel_NameTaken_RecoveryListFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.create":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"name_taken"}`)
		case "/conversations.list":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.CreateChannel(context.Background(), "k2k-x", true)
	if err == nil {
		t.Fatalf("CreateChannel returned nil error")
	}
	if !errors.Is(err, ErrChannelNameTaken) {
		t.Errorf("errors.Is(ErrChannelNameTaken) = false, want true; got %v", err)
	}
	if !errors.Is(err, ErrMissingScope) {
		t.Errorf("errors.Is(ErrMissingScope) = false, want true; got %v", err)
	}
}

// ============================================================
// InviteToChannel
// ============================================================

func TestInviteToChannel_HappyPath_SingleUser(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.invite", http.StatusOK,
		`{"ok":true}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C123", []string{"U1"})
	if err != nil {
		t.Fatalf("InviteToChannel: %v", err)
	}

	var body inviteRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Channel != "C123" {
		t.Errorf("channel = %q, want C123", body.Channel)
	}
	if body.Users != "U1" {
		t.Errorf("users = %q, want U1", body.Users)
	}
}

func TestInviteToChannel_HappyPath_BatchCSV(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.invite", http.StatusOK,
		`{"ok":true}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C123", []string{"U1", "U2", "U3"})
	if err != nil {
		t.Fatalf("InviteToChannel: %v", err)
	}

	var body inviteRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Users != "U1,U2,U3" {
		t.Errorf("users = %q, want U1,U2,U3", body.Users)
	}
}

func TestInviteToChannel_TrimsWhitespaceEntries(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.invite", http.StatusOK,
		`{"ok":true}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C123", []string{"  U1 ", "", "U2"})
	if err != nil {
		t.Fatalf("InviteToChannel: %v", err)
	}

	var body inviteRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Users != "U1,U2" {
		t.Errorf("users = %q, want U1,U2", body.Users)
	}
}

func TestInviteToChannel_EmptyChannel_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "", []string{"U1"})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

func TestInviteToChannel_EmptyUserList_NoOpSuccess(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.InviteToChannel(context.Background(), "C1", nil); err != nil {
		t.Errorf("InviteToChannel(nil) = %v, want nil", err)
	}
	if err := c.InviteToChannel(context.Background(), "C1", []string{}); err != nil {
		t.Errorf("InviteToChannel([]) = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

func TestInviteToChannel_AllWhitespace_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"  ", "\t"})
	if !errors.Is(err, messenger.ErrUserNotFound) {
		t.Errorf("err = %v, want messenger.ErrUserNotFound", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

// TestInviteToChannel_SingleUser_AlreadyInChannel_IdempotentSuccess
// pins that a single-user invite returning already_in_channel
// translates to nil — the all-or-nothing concern raised by iter-1
// codex P1 does not apply when the batch size is 1.
func TestInviteToChannel_SingleUser_AlreadyInChannel_IdempotentSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"already_in_channel"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.InviteToChannel(context.Background(), "C1", []string{"U1"}); err != nil {
		t.Errorf("InviteToChannel: %v; want nil (single-user idempotent already_in_channel)", err)
	}
}

// TestInviteToChannel_MultiUser_AlreadyInChannel_SurfacesSentinel
// pins the iter-1 codex P1 fix: Slack's conversations.invite is
// all-or-nothing per call, so a multi-user batch returning
// already_in_channel means the OTHER users in the batch were NOT
// invited. Translating to nil would hide a real failure mode for
// the K2K Open() fan-out. The sentinel surfaces wrapped so callers
// can retry per-user.
func TestInviteToChannel_MultiUser_AlreadyInChannel_SurfacesSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"already_in_channel"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"U1", "U2"})
	if err == nil {
		t.Fatalf("InviteToChannel returned nil for multi-user already_in_channel; want ErrAlreadyInChannel")
	}
	if !errors.Is(err, ErrAlreadyInChannel) {
		t.Errorf("errors.Is(ErrAlreadyInChannel) = false, want true; got %v", err)
	}
}

// TestInviteToChannel_BatchWithWhitespaceReducesToSingle_AlreadyInChannel_Idempotent
// pins that a post-trim single-user batch (input had whitespace
// entries that joinUserIDs dropped) still benefits from the idempotent
// translation — the trimmed count, not the input slice length, drives
// the safety decision.
func TestInviteToChannel_BatchWithWhitespaceReducesToSingle_AlreadyInChannel_Idempotent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"already_in_channel"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.InviteToChannel(context.Background(), "C1", []string{"U1", "   ", ""}); err != nil {
		t.Errorf("InviteToChannel: %v; want nil (post-trim single-user idempotent already_in_channel)", err)
	}
}

func TestInviteToChannel_ChannelNotFound_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"U1"})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
}

func TestInviteToChannel_IsArchived_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"is_archived"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"U1"})
	if !errors.Is(err, ErrIsArchived) {
		t.Errorf("err = %v, want ErrIsArchived", err)
	}
}

func TestInviteToChannel_CantInviteSelf_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"cant_invite_self"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"BSELF"})
	if !errors.Is(err, ErrCantInviteSelf) {
		t.Errorf("err = %v, want ErrCantInviteSelf", err)
	}
}

func TestInviteToChannel_UserNotFound_LiftsSentinel(t *testing.T) {
	t.Parallel()

	codes := []string{"user_not_found", "users_not_found"}
	for _, code := range codes {
		code := code
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":false,"error":"`+code+`"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			err := c.InviteToChannel(context.Background(), "C1", []string{"UXXX"})
			if !errors.Is(err, messenger.ErrUserNotFound) {
				t.Errorf("err = %v, want messenger.ErrUserNotFound", err)
			}
		})
	}
}

func TestInviteToChannel_RateLimited_PropagatesRetryAfter(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.InviteToChannel(context.Background(), "C1", []string{"U1"})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter = %v, want 5s", apiErr.RetryAfter)
	}
}

func TestInviteToChannel_CtxCancelled_NoNetwork(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.InviteToChannel(ctx, "C1", []string{"U1"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

// ============================================================
// ArchiveChannel
// ============================================================

func TestArchiveChannel_HappyPath(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.archive", http.StatusOK,
		`{"ok":true}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.ArchiveChannel(context.Background(), "C123"); err != nil {
		t.Fatalf("ArchiveChannel: %v", err)
	}

	var body archiveRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Channel != "C123" {
		t.Errorf("channel = %q, want C123", body.Channel)
	}
}

func TestArchiveChannel_AlreadyArchived_IdempotentSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"already_archived"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.ArchiveChannel(context.Background(), "C1"); err != nil {
		t.Errorf("ArchiveChannel: %v; want nil (idempotent already_archived)", err)
	}
}

func TestArchiveChannel_EmptyChannel_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.ArchiveChannel(context.Background(), "")
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

func TestArchiveChannel_ChannelNotFound_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.ArchiveChannel(context.Background(), "CGONE")
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
}

func TestArchiveChannel_MissingScope_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.ArchiveChannel(context.Background(), "C1")
	if !errors.Is(err, ErrMissingScope) {
		t.Errorf("err = %v, want ErrMissingScope", err)
	}
}

func TestArchiveChannel_RateLimited_PropagatesRetryAfter(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.ArchiveChannel(context.Background(), "C1")
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", apiErr.RetryAfter)
	}
}

func TestArchiveChannel_CtxCancelled_NoNetwork(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.ArchiveChannel(ctx, "C1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

// ============================================================
// RevealChannel
// ============================================================

func TestRevealChannel_HappyPath(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/conversations.invite", http.StatusOK,
		`{"ok":true}`,
		&captured,
	)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.RevealChannel(context.Background(), "C123", "U1"); err != nil {
		t.Fatalf("RevealChannel: %v", err)
	}

	var body inviteRequestWire
	if err := json.Unmarshal(captured, &body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if body.Channel != "C123" {
		t.Errorf("channel = %q, want C123", body.Channel)
	}
	if body.Users != "U1" {
		t.Errorf("users = %q, want U1", body.Users)
	}
}

func TestRevealChannel_AlreadyInChannel_IdempotentSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"already_in_channel"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.RevealChannel(context.Background(), "C1", "U1"); err != nil {
		t.Errorf("RevealChannel: %v; want nil (idempotent already_in_channel)", err)
	}
}

func TestRevealChannel_EmptyChannel_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.RevealChannel(context.Background(), "", "U1")
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("server saw %d calls, want 0", got)
	}
}

func TestRevealChannel_EmptyUser_FailsSync(t *testing.T) {
	t.Parallel()

	for _, uid := range []string{"", "   ", "\t"} {
		uid := uid
		t.Run("uid="+uid, func(t *testing.T) {
			t.Parallel()

			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			err := c.RevealChannel(context.Background(), "C1", uid)
			if !errors.Is(err, messenger.ErrUserNotFound) {
				t.Errorf("err = %v, want messenger.ErrUserNotFound", err)
			}
			if got := atomic.LoadInt32(&calls); got != 0 {
				t.Errorf("server saw %d calls, want 0", got)
			}
		})
	}
}

func TestRevealChannel_ChannelNotFound_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.RevealChannel(context.Background(), "CGONE", "U1")
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
}

func TestRevealChannel_IsArchived_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"is_archived"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.RevealChannel(context.Background(), "C1", "U1")
	if !errors.Is(err, ErrIsArchived) {
		t.Errorf("err = %v, want ErrIsArchived", err)
	}
}

// ============================================================
// Concurrency — 16-goroutine source-grep AC mirror from M1.1.a / M7.1.c
// ============================================================

// TestChannels_ConcurrentCallers pins that the four primitives are
// safe for concurrent use. The Slack [Client] is documented as
// goroutine-safe; this test pins the channel-primitive surface
// inherits the contract under concurrent load. No state assertions
// beyond "no panic, no data race" — the -race detector catches the
// invariant.
func TestChannels_ConcurrentCallers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case "/conversations.create":
			_, _ = io.WriteString(w, `{"ok":true,"channel":{"id":"C1"}}`)
		case "/conversations.invite":
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/conversations.archive":
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			switch i % 4 {
			case 0:
				_, _ = c.CreateChannel(ctx, "k2k-load", true)
			case 1:
				_ = c.InviteToChannel(ctx, "C1", []string{"U1", "U2"})
			case 2:
				_ = c.ArchiveChannel(ctx, "C1")
			case 3:
				_ = c.RevealChannel(ctx, "C1", "U1")
			}
		}()
	}
	wg.Wait()
}

// ============================================================
// Source-grep AC — match the M1.1.a / M7.1.c convention: no audit
// emission, no keeperslog calls in this file (the K2K wiring layer
// owns audit + token-budget; M1.1.b is pure adapter surface).
// ============================================================

func TestChannels_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	const path = "channels.go"
	body, err := readSourceFile(t, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Strip /* */ block comments and // line comments before scanning
	// so doc-block references that mention the disallowed callers do
	// not trip the AC. A simple line-comment strip suffices — the
	// implementation file has no block comments.
	stripped := stripGoComments(body)
	for _, banned := range []string{"keeperslog.", ".Append("} {
		if strings.Contains(stripped, banned) {
			t.Errorf("channels.go must not call %q outside comments (M1.1.b is pure adapter surface; audit owned by M1.1.c)", banned)
		}
	}
}

// readSourceFile reads the channels.go from the same directory as the
// test file so the AC stays robust to package relocation. go test sets
// cwd to the package directory.
func readSourceFile(t *testing.T, name string) (string, error) {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// stripGoComments removes // line comments from a Go source string so
// source-grep ACs do not trip on references inside doc-blocks. Block
// comments (/* */) are not stripped because the AC target file does
// not contain any; the helper stays minimal.
func stripGoComments(body string) string {
	var sb strings.Builder
	sb.Grow(len(body))
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}
