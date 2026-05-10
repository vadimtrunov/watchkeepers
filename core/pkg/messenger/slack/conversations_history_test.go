package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// historyRequestWire mirrors the JSON shape ConversationsHistory
// should place on the wire. Tests decode the captured request into
// this struct and assert each field. Mirroring keeps the assertions
// readable and independent of map ordering.
type historyRequestWire struct {
	Channel   string `json:"channel"`
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Oldest    string `json:"oldest,omitempty"`
	Latest    string `json:"latest,omitempty"`
	Inclusive bool   `json:"inclusive,omitempty"`
}

func TestConversationsHistory_HappyPath_ProjectsMessages(t *testing.T) {
	t.Parallel()

	var captured []byte
	const body = `{
		"ok": true,
		"messages": [
			{"ts":"1700000000.000200","user":"U1","text":"newest","team":"T1"},
			{"ts":"1700000000.000100","user":"U2","text":"older","thread_ts":"1700000000.000100","subtype":"","bot_id":"","client_msg_id":"cmid-1"}
		],
		"has_more": false,
		"response_metadata": {"next_cursor": ""}
	}`
	srv := captureServer(t, "/conversations.history", http.StatusOK, body, &captured)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	res, err := c.ConversationsHistory(context.Background(), "D123", HistoryOptions{Limit: 50})
	if err != nil {
		t.Fatalf("ConversationsHistory: %v", err)
	}
	if res.HasMore {
		t.Errorf("HasMore = true, want false")
	}
	if res.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty", res.NextCursor)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(res.Messages))
	}
	if res.Messages[0].TS != "1700000000.000200" || res.Messages[0].UserID != "U1" {
		t.Errorf("Messages[0] = %+v, want TS=1700000000.000200 UserID=U1", res.Messages[0])
	}
	if res.Messages[0].Text != "newest" {
		t.Errorf("Messages[0].Text = %q, want newest", res.Messages[0].Text)
	}
	if res.Messages[0].Metadata["team"] != "T1" {
		t.Errorf("Messages[0].Metadata[team] = %q, want T1", res.Messages[0].Metadata["team"])
	}
	if res.Messages[1].ThreadTS != "1700000000.000100" {
		t.Errorf("Messages[1].ThreadTS = %q, want 1700000000.000100", res.Messages[1].ThreadTS)
	}
	if res.Messages[1].Metadata["client_msg_id"] != "cmid-1" {
		t.Errorf("Messages[1].Metadata[client_msg_id] = %q, want cmid-1", res.Messages[1].Metadata["client_msg_id"])
	}

	var got historyRequestWire
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if got.Channel != "D123" {
		t.Errorf("Channel = %q, want D123", got.Channel)
	}
	if got.Limit != 50 {
		t.Errorf("Limit = %d, want 50", got.Limit)
	}
}

// TestConversationsHistory_ProjectsSubtypeAndMetadataFields pins
// iter-1 critic Missing #2: the wire-shape projection MUST round-trip
// `Subtype` AND every Slack-specific metadata key (`team`, `bot_id`,
// `app_id`, `client_msg_id`) so a downstream filter on subtype (e.g.
// `bot_message` vs human authors) is reliable.
func TestConversationsHistory_ProjectsSubtypeAndMetadataFields(t *testing.T) {
	t.Parallel()

	var captured []byte
	const body = `{
		"ok": true,
		"messages": [
			{
				"ts":"1.1",
				"user":"U1",
				"text":"x",
				"subtype":"bot_message",
				"team":"T-AAA",
				"bot_id":"B-BBB",
				"app_id":"A-CCC",
				"client_msg_id":"cmid-z"
			}
		],
		"has_more": false,
		"response_metadata": {"next_cursor": ""}
	}`
	srv := captureServer(t, "/conversations.history", http.StatusOK, body, &captured)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	res, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{})
	if err != nil {
		t.Fatalf("ConversationsHistory: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(res.Messages))
	}
	m := res.Messages[0]
	if m.Subtype != "bot_message" {
		t.Errorf("Subtype = %q, want bot_message", m.Subtype)
	}
	want := map[string]string{
		"team":          "T-AAA",
		"bot_id":        "B-BBB",
		"app_id":        "A-CCC",
		"client_msg_id": "cmid-z",
	}
	for k, v := range want {
		if got := m.Metadata[k]; got != v {
			t.Errorf("Metadata[%s] = %q, want %q", k, got, v)
		}
	}
}

func TestConversationsHistory_PaginationCursorThreaded(t *testing.T) {
	t.Parallel()

	var captured []byte
	const body = `{
		"ok": true,
		"messages": [{"ts":"1.1","user":"U1","text":"a"}],
		"has_more": true,
		"response_metadata": {"next_cursor": "dXNlcjpDMTIz"}
	}`
	srv := captureServer(t, "/conversations.history", http.StatusOK, body, &captured)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	res, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{
		Limit:     10,
		Cursor:    "previous-cursor",
		Oldest:    "1.0",
		Latest:    "2.0",
		Inclusive: true,
	})
	if err != nil {
		t.Fatalf("ConversationsHistory: %v", err)
	}
	if !res.HasMore {
		t.Errorf("HasMore = false, want true")
	}
	if res.NextCursor != "dXNlcjpDMTIz" {
		t.Errorf("NextCursor = %q, want dXNlcjpDMTIz", res.NextCursor)
	}

	var got historyRequestWire
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if got.Cursor != "previous-cursor" {
		t.Errorf("Cursor = %q, want previous-cursor", got.Cursor)
	}
	if got.Oldest != "1.0" {
		t.Errorf("Oldest = %q, want 1.0", got.Oldest)
	}
	if got.Latest != "2.0" {
		t.Errorf("Latest = %q, want 2.0", got.Latest)
	}
	if !got.Inclusive {
		t.Errorf("Inclusive = false, want true")
	}
}

func TestConversationsHistory_EmptyChannelID_FailsSync(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ConversationsHistory(context.Background(), "", HistoryOptions{})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

func TestConversationsHistory_ChannelNotFound_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ConversationsHistory(context.Background(), "DXXX", HistoryOptions{})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("errors.Is(messenger.ErrChannelNotFound) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("err is not *APIError underneath: %T", err)
	}
}

func TestConversationsHistory_MissingScope_LiftsSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"missing_scope"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{})
	if !errors.Is(err, ErrMissingScope) {
		t.Errorf("errors.Is(ErrMissingScope) = false, want true; got %v", err)
	}
}

func TestConversationsHistory_InvalidAuth_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{})
	if !errors.Is(err, ErrInvalidAuth) {
		t.Errorf("errors.Is(ErrInvalidAuth) = false, want true; got %v", err)
	}
}

func TestConversationsHistory_RateLimited_PropagatesRetryAfter(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(ErrRateLimited) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter = %v, want 5s", apiErr.RetryAfter)
	}
}

func TestConversationsHistory_CtxCancelled_NoNetwork(t *testing.T) {
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
	_, err := c.ConversationsHistory(ctx, "D1", HistoryOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

func TestConversationsHistory_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"messages":[{"ts":"1.1","user":"U1","text":"PII-MESSAGE-BODY-DO-NOT-LOG"}],"has_more":false,"response_metadata":{"next_cursor":""}}`)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN-HIST")),
		WithLogger(logger),
	)
	if _, err := c.ConversationsHistory(context.Background(), "D1", HistoryOptions{}); err != nil {
		t.Fatalf("ConversationsHistory: %v", err)
	}

	entries := logger.snapshot()
	banned := []string{"xoxb-LEAKED-TOKEN-HIST", "PII-MESSAGE-BODY-DO-NOT-LOG", "Bearer "}
	for _, e := range entries {
		entryStr := strings.Join(append([]string{e.Msg}, fmtKV(e.KV)...), " ")
		for _, b := range banned {
			if strings.Contains(entryStr, b) {
				t.Errorf("log entry %q leaks banned substring %q", entryStr, b)
			}
		}
	}
}

func fmtKV(kv []any) []string {
	out := make([]string, 0, len(kv))
	for _, v := range kv {
		out = append(out, fmt2(v))
	}
	return out
}

func fmt2(v any) string {
	switch s := v.(type) {
	case string:
		return s
	default:
		return ""
	}
}
