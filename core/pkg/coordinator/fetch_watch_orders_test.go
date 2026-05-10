package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeIMOpener stubs [SlackIMOpener] for tests. `gotUserID` /
// `keyHistory` guarded by mu; `calls` atomic for cheap assertion.
type fakeIMOpener struct {
	calls      int32
	mu         sync.Mutex
	gotUserID  string
	keyHistory []string
	returnID   string
	returnErr  error
}

func (f *fakeIMOpener) OpenIMChannel(_ context.Context, userID string) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.gotUserID = userID
	f.keyHistory = append(f.keyHistory, userID)
	f.mu.Unlock()
	if f.returnErr != nil {
		return "", f.returnErr
	}
	if f.returnID == "" {
		return "D9999", nil
	}
	return f.returnID, nil
}

// fakeHistoryReader stubs [SlackHistoryReader] for tests. `pages` is
// a queue: each ConversationsHistory call pops the head and returns
// it. Per-call options captured in `gotOpts` for assertion. Mirrors
// the M8.2.b [fakeJiraSearcher] pattern.
type fakeHistoryReader struct {
	calls     int32
	mu        sync.Mutex
	gotChan   []string
	gotOpts   []slack.HistoryOptions
	pages     []slack.HistoryResult
	returnErr error
	afterCall func(callIdx int)
}

func (f *fakeHistoryReader) ConversationsHistory(
	_ context.Context, channelID string, opts slack.HistoryOptions,
) (slack.HistoryResult, error) {
	idx := atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotChan = append(f.gotChan, channelID)
	f.gotOpts = append(f.gotOpts, opts)
	if f.afterCall != nil {
		f.afterCall(int(idx))
	}
	if f.returnErr != nil {
		return slack.HistoryResult{}, f.returnErr
	}
	if len(f.pages) == 0 {
		return slack.HistoryResult{HasMore: false}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

func validFetchArgs() map[string]any {
	return map[string]any{
		ToolArgLeadUserID:      "U123ABC",
		ToolArgLookbackMinutes: float64(60),
	}
}

func newFetchHandlerWithFixedNow(t *testing.T, opener SlackIMOpener, reader SlackHistoryReader, at time.Time) agentruntime.ToolHandler {
	t.Helper()
	return newFetchWatchOrdersHandlerWithClock(opener, reader, func() time.Time { return at })
}

func TestNewFetchWatchOrdersHandler_PanicsOnNilOpener(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil opener; got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "opener must not be nil") {
			t.Errorf("panic msg %q missing opener-nil discipline", msg)
		}
	}()
	NewFetchWatchOrdersHandler(nil, &fakeHistoryReader{})
}

func TestNewFetchWatchOrdersHandler_PanicsOnNilReader(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil reader; got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "reader must not be nil") {
			t.Errorf("panic msg %q missing reader-nil discipline", msg)
		}
	}()
	NewFetchWatchOrdersHandler(&fakeIMOpener{}, nil)
}

func TestNewFetchWatchOrdersHandler_PanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil clock; got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "clock must not be nil") {
			t.Errorf("panic msg %q missing clock-nil discipline", msg)
		}
	}()
	newFetchWatchOrdersHandlerWithClock(&fakeIMOpener{}, &fakeHistoryReader{}, nil)
}

//nolint:staticcheck // ST1023: explicit type asserts the factory return shape; without the annotation the assignment is a tautology.
func TestNewFetchWatchOrdersHandler_SatisfiesToolHandlerSignature(t *testing.T) {
	t.Parallel()
	var h agentruntime.ToolHandler = NewFetchWatchOrdersHandler(&fakeIMOpener{}, &fakeHistoryReader{})
	if h == nil {
		t.Fatal("factory returned nil handler")
	}
}

func TestFetchWatchOrdersHandler_HappyPath(t *testing.T) {
	t.Parallel()

	opener := &fakeIMOpener{returnID: "D-LEAD"}
	reader := &fakeHistoryReader{
		pages: []slack.HistoryResult{
			{
				HasMore: false,
				Messages: []slack.HistoryMessage{
					{TS: "1700000200.000100", UserID: "ULEAD", Text: "ship the M8.2.c PR today"},
					{TS: "1700000100.000100", UserID: "ULEAD", Text: "also check stale PRs"},
				},
			},
		},
	}
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	h := newFetchHandlerWithFixedNow(t, opener, reader, now)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	wantOldest := slackTSFromTime(now.Add(-60 * time.Minute))

	assertHappyPathTopLevel(t, res)
	assertHappyPathScope(t, res, wantOldest)
	assertHappyPathMessages(t, res)
	assertHappyPathSideEffects(t, opener, reader, wantOldest)
}

func assertHappyPathTopLevel(t *testing.T, res agentruntime.ToolResult) {
	t.Helper()
	if res.Error != "" {
		t.Errorf("ToolResult.Error = %q, want empty on happy path", res.Error)
	}
	if res.Output["total_returned"] != 2 {
		t.Errorf("total_returned = %v, want 2", res.Output["total_returned"])
	}
	if res.Output["truncated"] != false {
		t.Errorf("truncated = %v, want false", res.Output["truncated"])
	}
}

func assertHappyPathScope(t *testing.T, res agentruntime.ToolResult, wantOldest string) {
	t.Helper()
	scope, _ := res.Output["scope"].(map[string]any)
	if scope == nil {
		t.Fatalf("Output[scope] missing or wrong shape: %#v", res.Output["scope"])
	}
	if scope["lookback_minutes"] != 60 {
		t.Errorf("scope.lookback_minutes = %v, want 60", scope["lookback_minutes"])
	}
	if _, present := scope["lead_user_id"]; present {
		t.Errorf("scope.lead_user_id must be absent — Slack user id is identifier-PII; should not echo back")
	}
	if _, present := scope["im_channel_id"]; present {
		t.Errorf("scope.im_channel_id must be absent — leaks the lead-bot relationship inside the workspace")
	}
	if scope["oldest_ts"] != wantOldest {
		t.Errorf("scope.oldest_ts = %v, want %q", scope["oldest_ts"], wantOldest)
	}
}

func assertHappyPathMessages(t *testing.T, res agentruntime.ToolResult) {
	t.Helper()
	messages, ok := res.Output["messages"].([]map[string]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("Output[messages] shape unexpected: %#v", res.Output["messages"])
	}
	if messages[0]["text"] != "ship the M8.2.c PR today" {
		t.Errorf("messages[0].text = %v, want 'ship the M8.2.c PR today'", messages[0]["text"])
	}
	if messages[0]["ts"] != "1700000200.000100" {
		t.Errorf("messages[0].ts = %v, want 1700000200.000100", messages[0]["ts"])
	}
}

func assertHappyPathSideEffects(t *testing.T, opener *fakeIMOpener, reader *fakeHistoryReader, wantOldest string) {
	t.Helper()
	if atomic.LoadInt32(&opener.calls) != 1 {
		t.Errorf("opener called %d times; want 1", opener.calls)
	}
	opener.mu.Lock()
	gotUserID := opener.gotUserID
	opener.mu.Unlock()
	if gotUserID != "U123ABC" {
		t.Errorf("opener got userID %q; want U123ABC", gotUserID)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.gotChan) != 1 || reader.gotChan[0] != "D-LEAD" {
		t.Errorf("reader saw channels %v; want [D-LEAD]", reader.gotChan)
	}
	if reader.gotOpts[0].Oldest != wantOldest {
		t.Errorf("reader.Oldest = %q; want %q", reader.gotOpts[0].Oldest, wantOldest)
	}
	if reader.gotOpts[0].Limit != fetchPageSize {
		t.Errorf("reader.Limit = %d; want %d", reader.gotOpts[0].Limit, fetchPageSize)
	}
}

func TestFetchWatchOrdersHandler_PreCheckContextCancelled(t *testing.T) {
	t.Parallel()

	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h(ctx, agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler err = %v; want context.Canceled", err)
	}
	if atomic.LoadInt32(&opener.calls) != 0 {
		t.Errorf("opener called despite cancelled ctx; calls=%d", opener.calls)
	}
	if atomic.LoadInt32(&reader.calls) != 0 {
		t.Errorf("reader called despite cancelled ctx; calls=%d", reader.calls)
	}
}

func TestFetchWatchOrdersHandler_ArgValidationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(args map[string]any)
		wantErrIn string
	}{
		{"missing lead_user_id", func(a map[string]any) { delete(a, ToolArgLeadUserID) }, "lead_user_id"},
		{"non-string lead_user_id", func(a map[string]any) { a[ToolArgLeadUserID] = 42 }, "lead_user_id must be a string"},
		{"empty lead_user_id", func(a map[string]any) { a[ToolArgLeadUserID] = "" }, "lead_user_id must be non-empty"},
		{"missing lookback_minutes", func(a map[string]any) { delete(a, ToolArgLookbackMinutes) }, "lookback_minutes"},
		{"non-number lookback_minutes", func(a map[string]any) { a[ToolArgLookbackMinutes] = "60" }, "lookback_minutes must be a number"},
		{"non-integer lookback", func(a map[string]any) { a[ToolArgLookbackMinutes] = 7.5 }, "lookback_minutes must be an integer"},
		{"zero lookback", func(a map[string]any) { a[ToolArgLookbackMinutes] = float64(0) }, "lookback_minutes must be ≥ 1"},
		{"negative lookback", func(a map[string]any) { a[ToolArgLookbackMinutes] = float64(-1) }, "lookback_minutes must be ≥ 1"},
		{"over-cap lookback", func(a map[string]any) { a[ToolArgLookbackMinutes] = float64(maxLookbackMinutes + 1) }, "lookback_minutes must be ≤ 1440"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opener := &fakeIMOpener{}
			reader := &fakeHistoryReader{}
			h := NewFetchWatchOrdersHandler(opener, reader)

			args := validFetchArgs()
			tc.mutate(args)
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
			if err != nil {
				t.Fatalf("handler Go err = %v; want nil err with ToolResult.Error", err)
			}
			if !strings.Contains(res.Error, tc.wantErrIn) {
				t.Errorf("ToolResult.Error = %q; want substring %q", res.Error, tc.wantErrIn)
			}
			if atomic.LoadInt32(&opener.calls) != 0 {
				t.Errorf("opener called despite validation refusal")
			}
		})
	}
}

func TestFetchWatchOrdersHandler_RefusesMalformedLeadUserID_NoEcho(t *testing.T) {
	t.Parallel()

	// Token-shaped canary: lowercase + dashes; rejected by slackUserIDPattern
	// (which requires UWB-prefix + uppercase alnum).
	const tokenShaped = "redaction-probe-token-m82c-leaduser-do-not-log" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.

	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	args := validFetchArgs()
	args[ToolArgLeadUserID] = tokenShaped

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "Slack user-id shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'Slack user-id shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShaped) {
		t.Errorf("ToolResult.Error echoed the raw user-id (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&opener.calls) != 0 {
		t.Errorf("opener called despite malformed lead user-id")
	}
}

func TestFetchWatchOrdersHandler_RefusesTokenShapedLeadUserID_NoEcho(t *testing.T) {
	t.Parallel()

	// Character-class-valid (all-caps, alnum) but missing the U/W/B
	// discriminant prefix — synthetic env-var leak shape. Iter-1-style
	// canary; the discriminant gate is what catches this.
	const tokenShaped = "THE_API_KEY_VALUE_DO_NOT_LOG_M82C" //nolint:gosec // G101: synthetic redaction-harness canary.

	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	args := validFetchArgs()
	args[ToolArgLeadUserID] = tokenShaped

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "Slack user-id shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'Slack user-id shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShaped) {
		t.Errorf("ToolResult.Error echoed the raw token (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&opener.calls) != 0 {
		t.Errorf("opener called despite token-shaped lead user-id; discriminant gate broken")
	}
}

func TestFetchWatchOrdersHandler_OpenIM_UserNotFound_RefusesNoLeak(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("slack: conversations.open: %w", messenger.ErrUserNotFound)
	opener := &fakeIMOpener{returnErr: wrapped}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler Go err = %v; want nil err with ToolResult.Error (user_not_found is PII-classified)", err)
	}
	if !strings.Contains(res.Error, "did not resolve to a Slack user") {
		t.Errorf("ToolResult.Error = %q; want substring 'did not resolve to a Slack user'", res.Error)
	}
	if strings.Contains(res.Error, "U123ABC") {
		t.Errorf("refusal text echoed the raw user-id (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&reader.calls) != 0 {
		t.Errorf("reader called despite IM-open refusal")
	}
}

func TestFetchWatchOrdersHandler_OpenIM_CannotDMBot_Refuses(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("slack: conversations.open: %w", slack.ErrCannotDMBot)
	opener := &fakeIMOpener{returnErr: wrapped}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !strings.Contains(res.Error, "bot account") {
		t.Errorf("ToolResult.Error = %q; want substring 'bot account'", res.Error)
	}
}

func TestFetchWatchOrdersHandler_OpenIM_MissingScope_Refuses(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("slack: conversations.open: %w", slack.ErrMissingScope)
	opener := &fakeIMOpener{returnErr: wrapped}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !strings.Contains(res.Error, "OAuth scope missing") {
		t.Errorf("ToolResult.Error = %q; want substring 'OAuth scope missing'", res.Error)
	}
}

func TestFetchWatchOrdersHandler_OpenIM_TransportError_Wrapped(t *testing.T) {
	t.Parallel()

	inner := errors.New("slack: conversations.open: 503 service unavailable")
	opener := &fakeIMOpener{returnErr: inner}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !errors.Is(err, inner) {
		t.Fatalf("handler did not wrap inner error; got %v, want %v in chain", err, inner)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: fetch_watch_orders:") {
		t.Errorf("handler err = %q; want prefix coordinator: fetch_watch_orders:", err.Error())
	}
}

func TestFetchWatchOrdersHandler_HistoryError_Wrapped(t *testing.T) {
	t.Parallel()

	inner := errors.New("slack: conversations.history: 500 internal")
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{returnErr: inner}
	h := NewFetchWatchOrdersHandler(opener, reader)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !errors.Is(err, inner) {
		t.Fatalf("handler did not wrap inner err; got %v", err)
	}
}

func TestFetchWatchOrdersHandler_PaginationCollectsMultiplePages(t *testing.T) {
	t.Parallel()

	opener := &fakeIMOpener{}
	page1 := slack.HistoryResult{
		HasMore:    true,
		NextCursor: "cursor-2",
		Messages: []slack.HistoryMessage{
			{TS: "1.1", UserID: "U1", Text: "a"},
			{TS: "1.0", UserID: "U1", Text: "b"},
		},
	}
	page2 := slack.HistoryResult{
		HasMore: false,
		Messages: []slack.HistoryMessage{
			{TS: "0.9", UserID: "U1", Text: "c"},
		},
	}
	reader := &fakeHistoryReader{pages: []slack.HistoryResult{page1, page2}}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != 3 {
		t.Errorf("total_returned = %v, want 3", res.Output["total_returned"])
	}
	if res.Output["truncated"] != false {
		t.Errorf("truncated = %v, want false (HasMore reached)", res.Output["truncated"])
	}
	if got := atomic.LoadInt32(&reader.calls); got != 2 {
		t.Errorf("ConversationsHistory called %d times; want 2", got)
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.gotOpts[0].Cursor != "" {
		t.Errorf("first call cursor = %q; want empty", reader.gotOpts[0].Cursor)
	}
	if reader.gotOpts[1].Cursor != "cursor-2" {
		t.Errorf("second call cursor = %q; want cursor-2", reader.gotOpts[1].Cursor)
	}
}

func TestFetchWatchOrdersHandler_TruncatesAtMaxMessages(t *testing.T) {
	t.Parallel()

	bigPage := slack.HistoryResult{HasMore: true, NextCursor: "x"}
	for i := 0; i < maxFetchMessages+5; i++ {
		bigPage.Messages = append(bigPage.Messages, slack.HistoryMessage{
			TS: fmt.Sprintf("%d.000100", i+1), UserID: "U1", Text: "m",
		})
	}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{pages: []slack.HistoryResult{bigPage}}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != maxFetchMessages {
		t.Errorf("total_returned = %v, want %d", res.Output["total_returned"], maxFetchMessages)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (message cap fired)", res.Output["truncated"])
	}
}

func TestFetchWatchOrdersHandler_TruncatesAtMaxMessages_OnLastPageWithUnread(t *testing.T) {
	t.Parallel()

	// HasMore=false but page carries MORE than maxFetchMessages messages —
	// server overshot the limit, unread tail must surface as truncated.
	bigPage := slack.HistoryResult{HasMore: false, NextCursor: ""}
	for i := 0; i < maxFetchMessages+3; i++ {
		bigPage.Messages = append(bigPage.Messages, slack.HistoryMessage{
			TS: fmt.Sprintf("%d.0", i+1), Text: "m",
		})
	}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{pages: []slack.HistoryResult{bigPage}}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != maxFetchMessages {
		t.Errorf("total_returned = %v, want %d", res.Output["total_returned"], maxFetchMessages)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (message cap + unread tail on last page)", res.Output["truncated"])
	}
}

func TestFetchWatchOrdersHandler_TruncatesOnHasMoreFalseWithNonEmptyCursor(t *testing.T) {
	t.Parallel()

	const staleCursor = "stale-cursor" //nolint:gosec // G101: synthetic test cursor (Slack next_cursor is an opaque pagination handle, not a credential).
	page := slack.HistoryResult{
		HasMore:    false,
		NextCursor: staleCursor,
		Messages: []slack.HistoryMessage{
			{TS: "1.1", Text: "x"},
		},
	}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{pages: []slack.HistoryResult{page}}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true on HasMore=false + non-empty cursor (server-contract violation)", res.Output["truncated"])
	}
}

func TestFetchWatchOrdersHandler_TruncatesAtMaxPages(t *testing.T) {
	t.Parallel()

	pages := make([]slack.HistoryResult, maxFetchPages+2)
	for i := range pages {
		pages[i] = slack.HistoryResult{
			HasMore:    true,
			NextCursor: fmt.Sprintf("cursor-%d", i+1),
			Messages: []slack.HistoryMessage{
				{TS: fmt.Sprintf("%d.0", i+1), Text: "m"},
			},
		}
	}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{pages: pages}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := atomic.LoadInt32(&reader.calls); got != maxFetchPages {
		t.Errorf("ConversationsHistory called %d times; want %d", got, maxFetchPages)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (page cap fired)", res.Output["truncated"])
	}
}

func TestFetchWatchOrdersHandler_PaginatorChecksContextBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	page1 := slack.HistoryResult{HasMore: true, NextCursor: "c2", Messages: []slack.HistoryMessage{{TS: "1.0"}}}
	page2 := slack.HistoryResult{HasMore: false, Messages: []slack.HistoryMessage{{TS: "0.9"}}}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{
		pages: []slack.HistoryResult{page1, page2},
		afterCall: func(idx int) {
			if idx == 1 {
				cancel()
			}
		},
	}
	h := NewFetchWatchOrdersHandler(opener, reader)

	_, err := h(ctx, agentruntime.ToolCall{Arguments: validFetchArgs()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler err = %v; want context.Canceled wrapped", err)
	}
	if got := atomic.LoadInt32(&reader.calls); got != 1 {
		t.Errorf("ConversationsHistory called %d times; want exactly 1", got)
	}
}

func TestFetchWatchOrdersHandler_AcceptsLookbackFromJSONDecode(t *testing.T) {
	t.Parallel()

	const payload = `{"lead_user_id": "U123ABC", "lookback_minutes": 60}`
	var args map[string]any
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("expected acceptance of JSON-decoded args; got refusal %q", res.Error)
	}
}

func TestFetchWatchOrdersHandler_AcceptsLookbackAsInt(t *testing.T) {
	t.Parallel()

	args := validFetchArgs()
	args[ToolArgLookbackMinutes] = int(30)
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("expected acceptance of int lookback; got refusal %q", res.Error)
	}
}

func TestFetchWatchOrdersHandler_ConcurrentInvocationSafety(t *testing.T) {
	t.Parallel()

	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{}
	h := NewFetchWatchOrdersHandler(opener, reader)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()}); err != nil {
				t.Errorf("goroutine: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&opener.calls); got != goroutines {
		t.Errorf("opener called %d times; want %d", got, goroutines)
	}
	if got := atomic.LoadInt32(&reader.calls); got != goroutines {
		t.Errorf("reader called %d times; want %d", got, goroutines)
	}
}

// TestFetchWatchOrdersHandler_ValidationPrecedesNetworkCalls_SourceGrep
// pins the gate ordering at the source-file level. Anchored to the
// helper CALL pattern, NOT the bare symbol — the consts at the top of
// the file would otherwise satisfy a symbol-only search (M8.2.a iter-1
// codex Major #1 lesson).
func TestFetchWatchOrdersHandler_ValidationPrecedesNetworkCalls_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/fetch_watch_orders.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))

	openIdx := strings.Index(body, "opener.OpenIMChannel(")
	if openIdx < 0 {
		t.Fatal("source missing opener.OpenIMChannel( call site")
	}
	for _, guard := range []string{"readLeadUserIDArg(", "readLookbackMinutesArg("} {
		idx := strings.Index(body, guard)
		if idx < 0 {
			t.Errorf("source missing validation call %q; PII gate removed?", guard)
			continue
		}
		if idx >= openIdx {
			t.Errorf("validation call %q must precede opener.OpenIMChannel; got call@%d open@%d",
				guard, idx, openIdx)
		}
	}
	if !strings.Contains(body, "slackUserIDPattern.MatchString") {
		t.Error("source missing slackUserIDPattern.MatchString; shape regex check removed?")
	}
}

// TestFetchWatchOrdersHandler_NoAuditOrLogAppend_SourceGrep pins the
// audit discipline: the handler MUST NOT call into keeperslog (the
// audit boundary lives at the runtime's tool-result reflection layer,
// M5.6.b).
func TestFetchWatchOrdersHandler_NoAuditOrLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/fetch_watch_orders.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))
	for _, forbidden := range []string{"keeperslog.", ".Append("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("source contains forbidden audit shape %q outside comments", forbidden)
		}
	}
}

// TestFetchWatchOrdersHandler_PaginateSendsInclusiveOldest pins
// iter-1 codex Major #1: the handler MUST send Inclusive=true on
// every ConversationsHistory call so a message with ts==oldest_ts is
// included in the result. Slack's default for the `oldest` bound is
// exclusive (`ts > oldest`); without Inclusive=true the boundary
// message is silently dropped.
func TestFetchWatchOrdersHandler_PaginateSendsInclusiveOldest(t *testing.T) {
	t.Parallel()

	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{
		pages: []slack.HistoryResult{
			{HasMore: false, Messages: []slack.HistoryMessage{{TS: "1.0"}}},
		},
	}
	h := NewFetchWatchOrdersHandler(opener, reader)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.gotOpts) != 1 {
		t.Fatalf("reader.gotOpts len = %d, want 1", len(reader.gotOpts))
	}
	if !reader.gotOpts[0].Inclusive {
		t.Errorf("Inclusive = false; want true (iter-1 codex Major #1 — boundary message at ts==oldest_ts MUST be included)")
	}
}

// TestFetchWatchOrdersHandler_TruncatesOnHasMoreTrueWithEmptyCursor
// pins iter-1 codex Major #2: when Slack returns has_more=true with
// an empty next_cursor (the symmetric server-contract violation), the
// handler MUST treat the empty cursor as a truncation signal rather
// than re-requesting page 1 indefinitely.
func TestFetchWatchOrdersHandler_TruncatesOnHasMoreTrueWithEmptyCursor(t *testing.T) {
	t.Parallel()

	page := slack.HistoryResult{
		HasMore:    true,
		NextCursor: "", // server-contract violation
		Messages: []slack.HistoryMessage{
			{TS: "1.1", Text: "a"},
		},
	}
	// Provide a SECOND page so a regression that re-requested page 1
	// would consume it and not just hang against an empty fake.
	page2 := slack.HistoryResult{HasMore: false, Messages: []slack.HistoryMessage{{TS: "1.0"}}}
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{pages: []slack.HistoryResult{page, page2}}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true on has_more=true + empty cursor (server-contract violation)",
			res.Output["truncated"])
	}
	if got := atomic.LoadInt32(&reader.calls); got != 1 {
		t.Errorf("ConversationsHistory called %d times; want exactly 1 (must NOT re-request page 1 on empty cursor)", got)
	}
}

// TestFetchWatchOrdersHandler_DoesNotEchoUserIDInOutput pins iter-1
// codex Major #3: the per-message `user_id` field MUST NOT appear in
// the projected success Output — every message in a 1:1 lead-DM
// carries the same lead user id, so per-message echoes multiply the
// identifier surface (omitting only `lead_user_id` from `scope` is
// insufficient).
func TestFetchWatchOrdersHandler_DoesNotEchoUserIDInOutput(t *testing.T) {
	t.Parallel()

	const probeUserID = "U_PII_PROBE_LEAD_DO_NOT_LOG_M82C" //nolint:gosec // G101: synthetic redaction-harness canary.
	opener := &fakeIMOpener{}
	reader := &fakeHistoryReader{
		pages: []slack.HistoryResult{
			{
				HasMore: false,
				Messages: []slack.HistoryMessage{
					{TS: "1.1", UserID: probeUserID, Text: "watch order"},
					{TS: "1.0", UserID: probeUserID, Text: "follow up"},
				},
			},
		},
	}
	h := NewFetchWatchOrdersHandler(opener, reader)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validFetchArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	messages, ok := res.Output["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("Output[messages] shape unexpected: %#v", res.Output["messages"])
	}
	for i, m := range messages {
		if _, present := m["user_id"]; present {
			t.Errorf("messages[%d] contains user_id; PII per-message leak (iter-1 codex Major #3)", i)
		}
		// Defence-in-depth: walk every value in the map and ensure the
		// probe substring did not survive in any other field (e.g.
		// metadata leak via Subtype, or Text containing the id).
		for k, v := range m {
			if s, ok := v.(string); ok && strings.Contains(s, probeUserID) {
				t.Errorf("messages[%d][%s] echoes raw user_id %q (PII leak)", i, k, probeUserID)
			}
		}
	}
}

func TestSlackTSFromTime_RendersUnixSecondsWithMicroSuffix(t *testing.T) {
	t.Parallel()

	got := slackTSFromTime(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))
	const want = "1768478400.000000" // 2026-01-15 12:00:00 UTC
	if got != want {
		t.Errorf("slackTSFromTime = %q, want %q", got, want)
	}
}
