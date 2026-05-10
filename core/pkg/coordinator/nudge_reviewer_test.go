package coordinator

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeSlackMessenger stubs [SlackMessenger] for tests. Mirrors the
// M8.2.b fake pattern (no mocking lib).
type fakeSlackMessenger struct {
	calls      int32
	mu         sync.Mutex
	gotChannel []string
	gotMessage []messenger.Message
	returnID   messenger.MessageID
	returnErr  error
}

func (f *fakeSlackMessenger) SendMessage(
	_ context.Context, channelID string, msg messenger.Message,
) (messenger.MessageID, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.gotChannel = append(f.gotChannel, channelID)
	f.gotMessage = append(f.gotMessage, msg)
	f.mu.Unlock()
	if f.returnErr != nil {
		return "", f.returnErr
	}
	if f.returnID == "" {
		return "1700000000.000999", nil
	}
	return f.returnID, nil
}

func validNudgeArgs() map[string]any {
	return map[string]any{
		ToolArgReviewerUserID: "U123ABC",
		ToolArgText:           "PR #42 has been waiting for review for 3 days.",
	}
}

func TestNewNudgeReviewerHandler_PanicsOnNilSender(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil sender; got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "sender must not be nil") {
			t.Errorf("panic msg %q missing nil-sender discipline", msg)
		}
	}()
	NewNudgeReviewerHandler(nil)
}

//nolint:staticcheck // ST1023: explicit type asserts the factory return shape.
func TestNewNudgeReviewerHandler_SatisfiesToolHandlerSignature(t *testing.T) {
	t.Parallel()
	var h agentruntime.ToolHandler = NewNudgeReviewerHandler(&fakeSlackMessenger{})
	if h == nil {
		t.Fatal("factory returned nil handler")
	}
}

func TestNudgeReviewerHandler_HappyPath(t *testing.T) {
	t.Parallel()

	sender := &fakeSlackMessenger{returnID: "1700000123.000456"}
	h := NewNudgeReviewerHandler(sender)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validNudgeArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("ToolResult.Error = %q, want empty", res.Error)
	}
	if res.Output["message_ts"] != "1700000123.000456" {
		t.Errorf("message_ts = %v, want 1700000123.000456", res.Output["message_ts"])
	}
	if _, ok := res.Output["chars_sent"].(int); !ok {
		t.Errorf("chars_sent shape = %T, want int", res.Output["chars_sent"])
	}
	scope, _ := res.Output["scope"].(map[string]any)
	if scope == nil {
		t.Fatalf("Output[scope] missing or wrong shape: %#v", res.Output["scope"])
	}
	if _, present := scope["reviewer_user_id"]; present {
		t.Errorf("scope.reviewer_user_id must be absent — PII reach")
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.gotChannel) != 1 || sender.gotChannel[0] != "U123ABC" {
		t.Errorf("sender saw channels %v; want [U123ABC]", sender.gotChannel)
	}
}

func TestNudgeReviewerHandler_PreCheckContextCancelled(t *testing.T) {
	t.Parallel()
	sender := &fakeSlackMessenger{}
	h := NewNudgeReviewerHandler(sender)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h(ctx, agentruntime.ToolCall{Arguments: validNudgeArgs()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if atomic.LoadInt32(&sender.calls) != 0 {
		t.Errorf("sender called despite cancelled ctx")
	}
}

func TestNudgeReviewerHandler_ArgValidationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(args map[string]any)
		wantErrIn string
	}{
		{"missing reviewer_user_id", func(a map[string]any) { delete(a, ToolArgReviewerUserID) }, "reviewer_user_id"},
		{"non-string reviewer_user_id", func(a map[string]any) { a[ToolArgReviewerUserID] = 42 }, "reviewer_user_id must be a string"},
		{"empty reviewer_user_id", func(a map[string]any) { a[ToolArgReviewerUserID] = "" }, "reviewer_user_id must be non-empty"},
		{"missing text", func(a map[string]any) { delete(a, ToolArgText) }, "text"},
		{"non-string text", func(a map[string]any) { a[ToolArgText] = 42 }, "text must be a string"},
		{"empty text", func(a map[string]any) { a[ToolArgText] = "" }, "text must be non-empty"},
		{"over-cap text", func(a map[string]any) {
			a[ToolArgText] = strings.Repeat("x", maxNudgeTextChars+1)
		}, "characters (rune count)"},
		{"non-string title", func(a map[string]any) { a[ToolArgTitle] = 42 }, "title must be a string"},
		{"over-cap title", func(a map[string]any) {
			a[ToolArgTitle] = strings.Repeat("x", maxNudgeTitleChars+1)
		}, "characters (rune count)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sender := &fakeSlackMessenger{}
			h := NewNudgeReviewerHandler(sender)
			args := validNudgeArgs()
			tc.mutate(args)
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
			if err != nil {
				t.Fatalf("handler Go err = %v; want nil err with ToolResult.Error", err)
			}
			if !strings.Contains(res.Error, tc.wantErrIn) {
				t.Errorf("ToolResult.Error = %q; want substring %q", res.Error, tc.wantErrIn)
			}
			if atomic.LoadInt32(&sender.calls) != 0 {
				t.Errorf("sender called despite validation refusal")
			}
		})
	}
}

func TestNudgeReviewerHandler_RefusesMalformedReviewerUserID_NoEcho(t *testing.T) {
	t.Parallel()
	const tokenShaped = "redaction-probe-token-m82c-nudge-do-not-log" //nolint:gosec // G101: synthetic redaction-harness canary.
	sender := &fakeSlackMessenger{}
	h := NewNudgeReviewerHandler(sender)
	args := validNudgeArgs()
	args[ToolArgReviewerUserID] = tokenShaped
	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "Slack user-id shape") {
		t.Errorf("ToolResult.Error = %q; want shape refusal", res.Error)
	}
	if strings.Contains(res.Error, tokenShaped) {
		t.Errorf("ToolResult.Error echoed the raw user-id (PII leak): %q", res.Error)
	}
}

func TestNudgeReviewerHandler_RefusesTokenShapedUserID_NoEcho(t *testing.T) {
	t.Parallel()
	const tokenShaped = "THE_API_KEY_VALUE_DO_NOT_LOG_NUDGE" //nolint:gosec // G101: synthetic redaction-harness canary.
	sender := &fakeSlackMessenger{}
	h := NewNudgeReviewerHandler(sender)
	args := validNudgeArgs()
	args[ToolArgReviewerUserID] = tokenShaped
	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "Slack user-id shape") {
		t.Errorf("ToolResult.Error = %q; want shape refusal", res.Error)
	}
	if strings.Contains(res.Error, tokenShaped) {
		t.Errorf("ToolResult.Error echoed the raw token (PII leak): %q", res.Error)
	}
}

func TestNudgeReviewerHandler_RefusalNeverEchoesTextOrTitle(t *testing.T) {
	t.Parallel()
	const piiText = "API_KEY=xoxb-CANARY-DO-NOT-LOG-NUDGE" //nolint:gosec // G101: synthetic redaction-harness canary.
	const piiTitle = "TITLE_PII_DO_NOT_LOG_NUDGE"          //nolint:gosec // G101: synthetic redaction-harness canary.

	sender := &fakeSlackMessenger{}
	h := NewNudgeReviewerHandler(sender)
	args := map[string]any{
		ToolArgReviewerUserID: "Unot-shape-valid", // refuse via reviewer
		ToolArgText:           piiText,
		ToolArgTitle:          piiTitle,
	}
	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if strings.Contains(res.Error, piiText) {
		t.Errorf("refusal echoed text PII: %q", res.Error)
	}
	if strings.Contains(res.Error, piiTitle) {
		t.Errorf("refusal echoed title PII: %q", res.Error)
	}
}

func TestNudgeReviewerHandler_SendError_Wrapped(t *testing.T) {
	t.Parallel()
	inner := errors.New("slack: chat.postMessage: 503")
	sender := &fakeSlackMessenger{returnErr: inner}
	h := NewNudgeReviewerHandler(sender)
	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: validNudgeArgs()})
	if !errors.Is(err, inner) {
		t.Fatalf("handler did not wrap inner; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: nudge_reviewer:") {
		t.Errorf("handler err = %q; want prefix coordinator: nudge_reviewer:", err.Error())
	}
}

func TestNudgeReviewerHandler_ConcurrentInvocationSafety(t *testing.T) {
	t.Parallel()
	sender := &fakeSlackMessenger{}
	h := NewNudgeReviewerHandler(sender)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := h(context.Background(), agentruntime.ToolCall{Arguments: validNudgeArgs()}); err != nil {
				t.Errorf("goroutine: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&sender.calls); got != goroutines {
		t.Errorf("sender called %d times; want %d", got, goroutines)
	}
}

// TestNudgeReviewerHandler_ValidationPrecedesSendCall_SourceGrep pins
// the gate ordering. Anchored to the helper CALL pattern.
func TestNudgeReviewerHandler_ValidationPrecedesSendCall_SourceGrep(t *testing.T) {
	t.Parallel()
	srcPath := repoRelative(t, "core/pkg/coordinator/nudge_reviewer.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))

	sendIdx := strings.Index(body, "sender.SendMessage(")
	if sendIdx < 0 {
		t.Fatal("source missing sender.SendMessage( call site")
	}
	for _, guard := range []string{"readReviewerUserIDArg(", "readNudgeTextArg(", "readNudgeTitleArg("} {
		idx := strings.Index(body, guard)
		if idx < 0 {
			t.Errorf("source missing validation call %q", guard)
			continue
		}
		if idx >= sendIdx {
			t.Errorf("validation call %q must precede SendMessage; got call@%d send@%d", guard, idx, sendIdx)
		}
	}
	if !strings.Contains(body, "slackUserIDPattern.MatchString") {
		t.Error("source missing slackUserIDPattern.MatchString reference")
	}
}

func TestNudgeReviewerHandler_NoAuditOrLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()
	srcPath := repoRelative(t, "core/pkg/coordinator/nudge_reviewer.go")
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
