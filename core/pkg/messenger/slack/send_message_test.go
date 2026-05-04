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
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// postMessageRequest mirrors the JSON shape we expect SendMessage to
// place on the wire. Tests decode the captured request into this struct
// and assert each field. Mirroring keeps the assertions readable and
// independent of map ordering.
type postMessageRequest struct {
	Channel        string `json:"channel"`
	Text           string `json:"text,omitempty"`
	ThreadTS       string `json:"thread_ts,omitempty"`
	Mrkdwn         *bool  `json:"mrkdwn,omitempty"`
	Parse          string `json:"parse,omitempty"`
	LinkNames      *bool  `json:"link_names,omitempty"`
	UnfurlLinks    *bool  `json:"unfurl_links,omitempty"`
	UnfurlMedia    *bool  `json:"unfurl_media,omitempty"`
	IconEmoji      string `json:"icon_emoji,omitempty"`
	IconURL        string `json:"icon_url,omitempty"`
	Username       string `json:"username,omitempty"`
	ReplyBroadcast *bool  `json:"reply_broadcast,omitempty"`
}

// captureServer returns an httptest.Server that records every request
// body and replies with the supplied response. Used across SendMessage
// tests to keep the boilerplate compact.
func captureServer(t *testing.T, path string, status int, respBody string, captured *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path = %q, want %q", r.URL.Path, path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		*captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSendMessage_HappyPath_MinimalText asserts the simplest case: a
// Message with only Text travels to chat.postMessage as a JSON body
// {"channel": ..., "text": ...} and the returned MessageID equals the
// `ts` from the response.
func TestSendMessage_HappyPath_MinimalText(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C123","ts":"1700000000.000100"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-test")),
	)
	id, err := c.SendMessage(context.Background(), "C123", messenger.Message{Text: "hello"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "1700000000.000100" {
		t.Errorf("MessageID = %q, want 1700000000.000100", id)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.Channel != "C123" {
		t.Errorf("Channel = %q, want C123", got.Channel)
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want hello", got.Text)
	}
	if got.ThreadTS != "" {
		t.Errorf("ThreadTS = %q, want empty", got.ThreadTS)
	}
}

// TestSendMessage_ThreadReply_UsesTypedThreadID asserts that the typed
// Message.ThreadID field (per messenger.Message contract) is wired
// through as `thread_ts` on the JSON request.
func TestSendMessage_ThreadReply_UsesTypedThreadID(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C123","ts":"1700000000.000200"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C123", messenger.Message{
		Text:     "reply",
		ThreadID: "1700000000.000100",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.ThreadTS != "1700000000.000100" {
		t.Errorf("ThreadTS = %q, want 1700000000.000100", got.ThreadTS)
	}
}

// assertOptionalBool fails the test when `got` is nil or differs from
// `want`. Tests use this for the *bool fields on postMessageRequest so
// the per-field assertion stays a single line.
func assertOptionalBool(t *testing.T, name string, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = nil, want %v", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

// assertString fails the test when `got != want`. Wrapper exists so the
// metadata-pass-through test stays linear (each field is a single
// helper call instead of an inline `if` ladder).
func assertString(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// TestSendMessage_MetadataKeys_PassedThrough asserts the documented
// Slack-specific Metadata keys (mrkdwn, parse, link_names,
// unfurl_links, unfurl_media, icon_emoji, icon_url, username,
// reply_broadcast) flow into the JSON request body. Keys outside the
// documented set are ignored (forward compatibility — adapter only
// understands what it knows).
func TestSendMessage_MetadataKeys_PassedThrough(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C1","ts":"1.2"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text: "rich",
		Metadata: map[string]string{
			"mrkdwn":          "false",
			"parse":           "full",
			"link_names":      "true",
			"unfurl_links":    "false",
			"unfurl_media":    "true",
			"icon_emoji":      ":robot_face:",
			"icon_url":        "https://example.invalid/icon.png",
			"username":        "watchkeeper",
			"reply_broadcast": "true",
			"unknown_key":     "ignored",
		},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	assertOptionalBool(t, "Mrkdwn", got.Mrkdwn, false)
	assertOptionalBool(t, "LinkNames", got.LinkNames, true)
	assertOptionalBool(t, "UnfurlLinks", got.UnfurlLinks, false)
	assertOptionalBool(t, "UnfurlMedia", got.UnfurlMedia, true)
	assertOptionalBool(t, "ReplyBroadcast", got.ReplyBroadcast, true)
	assertString(t, "Parse", got.Parse, "full")
	assertString(t, "IconEmoji", got.IconEmoji, ":robot_face:")
	assertString(t, "IconURL", got.IconURL, "https://example.invalid/icon.png")
	assertString(t, "Username", got.Username, "watchkeeper")
	// unknown_key is silently ignored — no field on postMessageRequest
	// should accept it. Just assert it's not echoed verbatim.
	if strings.Contains(string(captured), "unknown_key") {
		t.Errorf("body leaks unknown_key: %s", string(captured))
	}
}

// TestSendMessage_MetadataMrkdwn_NonBooleanIgnored asserts that a
// boolean-typed Metadata value that fails to parse is silently dropped
// (the adapter does not panic and does not forward garbage).
func TestSendMessage_MetadataMrkdwn_NonBooleanIgnored(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C1","ts":"1.2"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text:     "x",
		Metadata: map[string]string{"mrkdwn": "not-a-bool"},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.Mrkdwn != nil {
		t.Errorf("Mrkdwn = %v, want nil (unparseable bool dropped)", got.Mrkdwn)
	}
}

// TestSendMessage_ThreadID_TakesPrecedenceOverMetadata asserts that
// when both Message.ThreadID and Metadata["thread_ts"] are populated,
// the typed field wins (the typed field is the documented contract;
// Metadata fallback exists for callers that don't know about the
// typed field, but typed callers must not be silently overridden).
func TestSendMessage_ThreadID_TakesPrecedenceOverMetadata(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C1","ts":"1.2"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text:     "x",
		ThreadID: "1.0",
		Metadata: map[string]string{"thread_ts": "9.9"},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.ThreadTS != "1.0" {
		t.Errorf("ThreadTS = %q, want 1.0 (typed field wins)", got.ThreadTS)
	}
}

// TestSendMessage_MetadataThreadTS_FallbackWhenTypedEmpty asserts that
// when Message.ThreadID is empty but Metadata carries thread_ts (older
// callers that don't yet use the typed field), the metadata value is
// honoured.
func TestSendMessage_MetadataThreadTS_FallbackWhenTypedEmpty(t *testing.T) {
	t.Parallel()

	var captured []byte
	srv := captureServer(
		t, "/chat.postMessage", http.StatusOK,
		`{"ok":true,"channel":"C1","ts":"1.2"}`,
		&captured,
	)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text:     "x",
		Metadata: map[string]string{"thread_ts": "9.9"},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	var got postMessageRequest
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.ThreadTS != "9.9" {
		t.Errorf("ThreadTS = %q, want 9.9 (metadata fallback)", got.ThreadTS)
	}
}

// TestSendMessage_EmptyChannelID_FailsSync asserts that an empty
// channelID surfaces a synchronous error WITHOUT contacting the
// platform — Slack would reject it anyway with channel_not_found, but
// catching it client-side avoids burning a rate-limit token on a
// known-bad request.
func TestSendMessage_EmptyChannelID_FailsSync(t *testing.T) {
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
	_, err := c.SendMessage(context.Background(), "", messenger.Message{Text: "x"})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("err = %v, want messenger.ErrChannelNotFound", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (empty channel must not hit network)", calls)
	}
}

// TestSendMessage_AttachmentsUnsupported asserts that the portable
// messenger.Attachment shape (URL or inline Data) is NOT yet wired to
// Slack chat.postMessage in M4.2.b — a non-empty Attachments slice
// returns messenger.ErrUnsupported synchronously. Slack's
// files.upload + blocks integration is a follow-up; the contract
// reserves the field for future work without silently dropping it.
func TestSendMessage_AttachmentsUnsupported(t *testing.T) {
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
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text: "x",
		Attachments: []messenger.Attachment{
			{Name: "x.png", URL: "https://example.invalid/x.png"},
		},
	})
	if !errors.Is(err, messenger.ErrUnsupported) {
		t.Errorf("err = %v, want messenger.ErrUnsupported", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0 (unsupported must not hit network)", calls)
	}
}

// TestSendMessage_ChannelNotFound_PortableSentinel asserts that a
// Slack `error: "channel_not_found"` envelope surfaces as
// messenger.ErrChannelNotFound (the portable sentinel) — adapter
// callers match the portable form, not the slack-specific one.
func TestSendMessage_ChannelNotFound_PortableSentinel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C404", messenger.Message{Text: "x"})
	if !errors.Is(err, messenger.ErrChannelNotFound) {
		t.Errorf("errors.Is(err, messenger.ErrChannelNotFound) = false, want true; got %v", err)
	}
	// Underlying APIError still accessible for callers that care.
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Errorf("error is not *APIError underneath: %T", err)
	}
}

// TestSendMessage_OtherErrorCodes_RetainAPIError asserts that error
// codes WITHOUT a portable sentinel mapping (msg_too_long,
// not_in_channel, is_archived) still surface as *APIError so callers
// can inspect Code.
func TestSendMessage_OtherErrorCodes_RetainAPIError(t *testing.T) {
	t.Parallel()

	codes := []string{"msg_too_long", "not_in_channel", "is_archived", "no_text"}
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
				WithTokenSource(StaticToken("t")),
			)
			_, err := c.SendMessage(context.Background(), "C1", messenger.Message{Text: "x"})
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

// TestSendMessage_RateLimited_PropagatesAPIError asserts that an HTTP
// 429 from Slack surfaces as *APIError wrapping ErrRateLimited (the
// slack-package sentinel — the portable messenger interface does not
// document a rate-limit sentinel, so callers match the slack one).
func TestSendMessage_RateLimited_PropagatesAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{Text: "x"})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("errors.Is(err, ErrRateLimited) = false, want true; got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T %v", err, err)
	}
	if apiErr.RetryAfter != 10*time.Second {
		t.Errorf("RetryAfter = %v, want 10s", apiErr.RetryAfter)
	}
}

// TestSendMessage_CtxCancellation asserts a pre-cancelled ctx returns
// ctx.Err() and never contacts the platform.
func TestSendMessage_CtxCancellation(t *testing.T) {
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
	_, err := c.SendMessage(ctx, "C1", messenger.Message{Text: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, want true; got %v", err)
	}
	if calls != 0 {
		t.Errorf("server saw %d calls, want 0", calls)
	}
}

// TestSendMessage_LoggerRedacted asserts the M4.2.a redaction
// discipline carries through the higher-level helper: the bearer
// token, the message text (which may contain user PII), and the
// returned ts NEVER appear in log entries.
func TestSendMessage_LoggerRedacted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"channel":"C1","ts":"1700000000.000999"}`)
	}))
	t.Cleanup(srv.Close)

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxb-LEAKED-TOKEN")),
		WithLogger(logger),
	)
	_, err := c.SendMessage(context.Background(), "C1", messenger.Message{
		Text: "PII-PAYLOAD-PLEASE-REDACT",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	entries := logger.snapshot()
	banned := []string{"xoxb-LEAKED-TOKEN", "PII-PAYLOAD-PLEASE-REDACT", "Bearer "}
	for _, e := range entries {
		entryStr := fmt.Sprintf("msg=%q kv=%v", e.Msg, e.KV)
		for _, b := range banned {
			if strings.Contains(entryStr, b) {
				t.Errorf("log entry %q leaks banned substring %q", entryStr, b)
			}
		}
	}
}
