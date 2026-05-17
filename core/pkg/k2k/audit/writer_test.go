package audit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Compile-time assertions: [*keeperslog.Writer] satisfies the
// [audit.Appender] seam, and [*audit.Writer] satisfies the
// [audit.Emitter] consumer interface. Pinned at the package level so
// `go build ./...` rejects a future drift in either surface.
var (
	_ audit.Appender = (*keeperslog.Writer)(nil)
	_ audit.Emitter  = (*audit.Writer)(nil)
)

// recordingAppender is the hand-rolled [audit.Appender] stand-in the
// test suite uses. Records every Append call (a copy of the event, so
// caller-side mutation cannot race the recorded snapshot) and returns
// either a canned response or an injected error.
type recordingAppender struct {
	mu       sync.Mutex
	events   []keeperslog.Event
	resp     string
	respErr  error
	callsLen int
}

func (r *recordingAppender) Append(_ context.Context, evt keeperslog.Event) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Defensive snapshot of the event so a caller that retains and
	// mutates the payload after Append cannot rewrite the recorded value.
	clone := evt
	if m, ok := evt.Payload.(map[string]any); ok {
		mc := make(map[string]any, len(m))
		for k, v := range m {
			if vs, isSlice := v.([]string); isSlice {
				cp := make([]string, len(vs))
				copy(cp, vs)
				mc[k] = cp
			} else {
				mc[k] = v
			}
		}
		clone.Payload = mc
	}
	r.events = append(r.events, clone)
	r.callsLen++
	if r.respErr != nil {
		return "", r.respErr
	}
	if r.resp != "" {
		return r.resp, nil
	}
	return "row-1", nil
}

func (r *recordingAppender) snapshot() []keeperslog.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]keeperslog.Event, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordingAppender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callsLen
}

const (
	fixedConversationID = "11111111-1111-7111-8111-111111111111"
	fixedOrganizationID = "22222222-2222-7222-8222-222222222222"
	fixedCorrelationID  = "33333333-3333-7333-8333-333333333333"
	fixedMessageID      = "44444444-4444-7444-8444-444444444444"
)

var fixedTime = time.Date(2026, time.May, 17, 12, 0, 0, 0, time.UTC)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid.Parse(%q): %v", s, err)
	}
	return id
}

// TestNewWriter_NilAppender_Panics asserts the constructor panics on
// a nil appender — mirrors [keeperslog.New], [k2k.NewLifecycle], and
// [llm/cost.NewLoggingProvider].
func TestNewWriter_NilAppender_Panics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	_ = audit.NewWriter(nil)
}

func TestEventTypes_Closed(t *testing.T) {
	t.Parallel()

	got := audit.EventTypes()
	want := []string{
		audit.EventConversationOpened,
		audit.EventConversationClosed,
		audit.EventMessageSent,
		audit.EventMessageReceived,
		audit.EventOverBudget,
		audit.EventEscalated,
	}
	if len(got) != len(want) {
		t.Fatalf("EventTypes() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("EventTypes()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Defensive copy: mutating the returned slice must not bleed into a
	// subsequent call.
	got[0] = "mutated"
	got2 := audit.EventTypes()
	if got2[0] != audit.EventConversationOpened {
		t.Errorf("EventTypes() second call = %q, want %q (defensive copy broken)", got2[0], audit.EventConversationOpened)
	}
	// Six event types match the M1.4 ROADMAP AC.
	if len(want) != 6 {
		t.Errorf("M1.4 AC pins six event types; got %d", len(want))
	}
}

func TestEventConstants_Snakecase(t *testing.T) {
	t.Parallel()

	// The M1.4 ROADMAP AC pins exact event-type strings. A typo here
	// would silently break a downstream audit subscriber.
	cases := map[string]string{
		"EventConversationOpened": audit.EventConversationOpened,
		"EventConversationClosed": audit.EventConversationClosed,
		"EventMessageSent":        audit.EventMessageSent,
		"EventMessageReceived":    audit.EventMessageReceived,
		"EventOverBudget":         audit.EventOverBudget,
		"EventEscalated":          audit.EventEscalated,
	}
	expected := map[string]string{
		"EventConversationOpened": "k2k_conversation_opened",
		"EventConversationClosed": "k2k_conversation_closed",
		"EventMessageSent":        "k2k_message_sent",
		"EventMessageReceived":    "k2k_message_received",
		"EventOverBudget":         "k2k_over_budget",
		"EventEscalated":          "k2k_escalated",
	}
	for name, got := range cases {
		if got != expected[name] {
			t.Errorf("%s = %q, want %q", name, got, expected[name])
		}
	}
}

func newWriter(t *testing.T) (*audit.Writer, *recordingAppender) {
	t.Helper()
	rec := &recordingAppender{}
	return audit.NewWriter(rec), rec
}

// assertPayloadMatches walks `wants` checking each expected key/value
// pair against `payload`. Hoisted so the per-event happy-path tests
// stay under the gocyclo budget.
func assertPayloadMatches(t *testing.T, payload map[string]any, wants map[string]any) {
	t.Helper()
	for k, want := range wants {
		got, present := payload[k]
		if !present {
			t.Errorf("payload missing key %q (full payload: %+v)", k, payload)
			continue
		}
		if got != want {
			t.Errorf("payload[%q] = %v, want %v", k, got, want)
		}
	}
}

// assertPayloadAbsent fails the test when any of `banned` keys are
// present on `payload`. Hoisted for the PII-discipline asserts shared
// across the happy-path tests.
func assertPayloadAbsent(t *testing.T, payload map[string]any, banned []string) {
	t.Helper()
	for _, k := range banned {
		if _, present := payload[k]; present {
			t.Errorf("payload contains banned key %q: %+v", k, payload)
		}
	}
}

func TestEmitConversationOpened_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)
	corrID := mustUUID(t, fixedCorrelationID)

	id, err := w.EmitConversationOpened(context.Background(), audit.ConversationOpenedEvent{
		ConversationID: convID,
		OrganizationID: orgID,
		Participants:   []string{"wk-a", "wk-b"},
		Subject:        "review PR 123",
		CorrelationID:  corrID,
		SlackChannelID: "C12345",
		OpenedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitConversationOpened: %v", err)
	}
	if id != "row-1" {
		t.Errorf("returned id = %q, want row-1", id)
	}
	evts := rec.snapshot()
	if len(evts) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evts))
	}
	got := evts[0]
	if got.EventType != audit.EventConversationOpened {
		t.Errorf("EventType = %q, want %q", got.EventType, audit.EventConversationOpened)
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got.Payload)
	}
	assertPayloadMatches(t, payload, map[string]any{
		"conversation_id":  convID.String(),
		"organization_id":  orgID.String(),
		"subject":          "review PR 123",
		"correlation_id":   corrID.String(),
		"slack_channel_id": "C12345",
		"opened_at":        fixedTime.Format(time.RFC3339Nano),
	})
	parts, ok := payload["participants"].([]string)
	if !ok || len(parts) != 2 || parts[0] != "wk-a" || parts[1] != "wk-b" {
		t.Errorf("participants = %v, want [wk-a wk-b]", payload["participants"])
	}
	// PII discipline: no message body / participant role / display name.
	assertPayloadAbsent(t, payload, []string{"body", "role", "display_name"})
}

func TestEmitConversationOpened_OmitsCorrelationIDWhenZero(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	_, err := w.EmitConversationOpened(context.Background(), audit.ConversationOpenedEvent{
		ConversationID: mustUUID(t, fixedConversationID),
		OrganizationID: mustUUID(t, fixedOrganizationID),
		SlackChannelID: "C1",
		OpenedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitConversationOpened: %v", err)
	}
	payload := rec.snapshot()[0].Payload.(map[string]any)
	if _, present := payload["correlation_id"]; present {
		t.Errorf("correlation_id present on zero uuid: %+v", payload)
	}
}

func TestEmitConversationOpened_DefensiveCopyOfParticipants(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	parts := []string{"wk-a", "wk-b"}
	_, err := w.EmitConversationOpened(context.Background(), audit.ConversationOpenedEvent{
		ConversationID: mustUUID(t, fixedConversationID),
		OrganizationID: mustUUID(t, fixedOrganizationID),
		Participants:   parts,
		SlackChannelID: "C1",
		OpenedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitConversationOpened: %v", err)
	}
	// Mutate caller's slice; recorded payload must not bleed.
	parts[0] = "wk-mutated"
	recorded := rec.snapshot()[0].Payload.(map[string]any)["participants"].([]string)
	if recorded[0] != "wk-a" {
		t.Errorf("participants[0] = %q, want wk-a (defensive copy broken)", recorded[0])
	}
}

func TestEmitConversationOpened_ValidationFailures(t *testing.T) {
	t.Parallel()

	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	cases := []struct {
		name string
		evt  audit.ConversationOpenedEvent
	}{
		{
			name: "zero conversation id",
			evt: audit.ConversationOpenedEvent{
				OrganizationID: orgID,
				SlackChannelID: "C1",
				OpenedAt:       fixedTime,
			},
		},
		{
			name: "zero organization id",
			evt: audit.ConversationOpenedEvent{
				ConversationID: convID,
				SlackChannelID: "C1",
				OpenedAt:       fixedTime,
			},
		},
		{
			name: "empty slack channel id",
			evt: audit.ConversationOpenedEvent{
				ConversationID: convID,
				OrganizationID: orgID,
				OpenedAt:       fixedTime,
			},
		},
		{
			name: "zero opened_at",
			evt: audit.ConversationOpenedEvent{
				ConversationID: convID,
				OrganizationID: orgID,
				SlackChannelID: "C1",
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, rec := newWriter(t)
			_, err := w.EmitConversationOpened(context.Background(), tc.evt)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Errorf("err = %v, want ErrInvalidEvent", err)
			}
			if rec.count() != 0 {
				t.Errorf("appender called on invalid event: %d times", rec.count())
			}
		})
	}
}

func TestEmitConversationOpened_AppenderError(t *testing.T) {
	t.Parallel()

	rec := &recordingAppender{respErr: errors.New("boom")}
	w := audit.NewWriter(rec)
	_, err := w.EmitConversationOpened(context.Background(), audit.ConversationOpenedEvent{
		ConversationID: mustUUID(t, fixedConversationID),
		OrganizationID: mustUUID(t, fixedOrganizationID),
		SlackChannelID: "C1",
		OpenedAt:       fixedTime,
	})
	if err == nil {
		t.Fatal("expected error from appender")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want underlying err propagated", err)
	}
}

func TestEmitConversationClosed_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	_, err := w.EmitConversationClosed(context.Background(), audit.ConversationClosedEvent{
		ConversationID: convID,
		OrganizationID: orgID,
		CloseReason:    "peer.close",
		ClosedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitConversationClosed: %v", err)
	}
	evt := rec.snapshot()[0]
	if evt.EventType != audit.EventConversationClosed {
		t.Errorf("EventType = %q, want %q", evt.EventType, audit.EventConversationClosed)
	}
	payload := evt.Payload.(map[string]any)
	if payload["conversation_id"] != convID.String() {
		t.Errorf("conversation_id = %v", payload["conversation_id"])
	}
	if payload["close_reason"] != "peer.close" {
		t.Errorf("close_reason = %v", payload["close_reason"])
	}
	if payload["closed_at"] != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("closed_at = %v", payload["closed_at"])
	}
	// close_summary intentionally NOT carried in audit payload.
	if _, present := payload["close_summary"]; present {
		t.Errorf("close_summary leaked to audit payload: %+v", payload)
	}
}

func TestEmitConversationClosed_EmptyReasonAllowed(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	_, err := w.EmitConversationClosed(context.Background(), audit.ConversationClosedEvent{
		ConversationID: mustUUID(t, fixedConversationID),
		OrganizationID: mustUUID(t, fixedOrganizationID),
		ClosedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitConversationClosed: %v", err)
	}
	payload := rec.snapshot()[0].Payload.(map[string]any)
	if payload["close_reason"] != "" {
		t.Errorf("close_reason = %v, want empty", payload["close_reason"])
	}
}

func TestEmitConversationClosed_ValidationFailures(t *testing.T) {
	t.Parallel()

	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)
	cases := []struct {
		name string
		evt  audit.ConversationClosedEvent
	}{
		{"zero conv", audit.ConversationClosedEvent{OrganizationID: orgID, ClosedAt: fixedTime}},
		{"zero org", audit.ConversationClosedEvent{ConversationID: convID, ClosedAt: fixedTime}},
		{"zero closed_at", audit.ConversationClosedEvent{ConversationID: convID, OrganizationID: orgID}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, rec := newWriter(t)
			_, err := w.EmitConversationClosed(context.Background(), tc.evt)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Errorf("err = %v, want ErrInvalidEvent", err)
			}
			if rec.count() != 0 {
				t.Errorf("appender called on invalid event")
			}
		})
	}
}

func TestEmitMessageSent_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	msgID := mustUUID(t, fixedMessageID)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	_, err := w.EmitMessageSent(context.Background(), audit.MessageSentEvent{
		MessageID:           msgID,
		ConversationID:      convID,
		OrganizationID:      orgID,
		SenderWatchkeeperID: "wk-a",
		Direction:           "request",
		CreatedAt:           fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitMessageSent: %v", err)
	}
	evt := rec.snapshot()[0]
	if evt.EventType != audit.EventMessageSent {
		t.Errorf("EventType = %q", evt.EventType)
	}
	payload := evt.Payload.(map[string]any)
	if payload["message_id"] != msgID.String() {
		t.Errorf("message_id = %v", payload["message_id"])
	}
	if payload["sender_watchkeeper_id"] != "wk-a" {
		t.Errorf("sender = %v", payload["sender_watchkeeper_id"])
	}
	if payload["direction"] != "request" {
		t.Errorf("direction = %v", payload["direction"])
	}
	if payload["created_at"] != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("created_at = %v", payload["created_at"])
	}
	// PII discipline: no body / subject leakage on message events.
	for _, banned := range []string{"body", "subject", "payload"} {
		if _, present := payload[banned]; present {
			t.Errorf("payload contains banned key %q: %+v", banned, payload)
		}
	}
}

func TestEmitMessageReceived_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	msgID := mustUUID(t, fixedMessageID)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	_, err := w.EmitMessageReceived(context.Background(), audit.MessageReceivedEvent{
		MessageID:              msgID,
		ConversationID:         convID,
		OrganizationID:         orgID,
		SenderWatchkeeperID:    "wk-a",
		RecipientWatchkeeperID: "wk-b",
		Direction:              "reply",
		CreatedAt:              fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitMessageReceived: %v", err)
	}
	evt := rec.snapshot()[0]
	if evt.EventType != audit.EventMessageReceived {
		t.Errorf("EventType = %q", evt.EventType)
	}
	payload := evt.Payload.(map[string]any)
	if payload["sender_watchkeeper_id"] != "wk-a" {
		t.Errorf("sender = %v", payload["sender_watchkeeper_id"])
	}
	if payload["recipient_watchkeeper_id"] != "wk-b" {
		t.Errorf("recipient = %v", payload["recipient_watchkeeper_id"])
	}
	if payload["direction"] != "reply" {
		t.Errorf("direction = %v", payload["direction"])
	}
}

func TestEmitMessageSent_ValidationFailures(t *testing.T) {
	t.Parallel()

	msg := mustUUID(t, fixedMessageID)
	conv := mustUUID(t, fixedConversationID)
	org := mustUUID(t, fixedOrganizationID)

	cases := []struct {
		name string
		evt  audit.MessageSentEvent
	}{
		{"zero message id", audit.MessageSentEvent{ConversationID: conv, OrganizationID: org, SenderWatchkeeperID: "wk", Direction: "request", CreatedAt: fixedTime}},
		{"zero conv id", audit.MessageSentEvent{MessageID: msg, OrganizationID: org, SenderWatchkeeperID: "wk", Direction: "request", CreatedAt: fixedTime}},
		{"zero org id", audit.MessageSentEvent{MessageID: msg, ConversationID: conv, SenderWatchkeeperID: "wk", Direction: "request", CreatedAt: fixedTime}},
		{"empty sender", audit.MessageSentEvent{MessageID: msg, ConversationID: conv, OrganizationID: org, Direction: "request", CreatedAt: fixedTime}},
		{"empty direction", audit.MessageSentEvent{MessageID: msg, ConversationID: conv, OrganizationID: org, SenderWatchkeeperID: "wk", CreatedAt: fixedTime}},
		{"zero created_at", audit.MessageSentEvent{MessageID: msg, ConversationID: conv, OrganizationID: org, SenderWatchkeeperID: "wk", Direction: "request"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, rec := newWriter(t)
			_, err := w.EmitMessageSent(context.Background(), tc.evt)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Errorf("err = %v, want ErrInvalidEvent", err)
			}
			if rec.count() != 0 {
				t.Errorf("appender called on invalid event")
			}
		})
	}
}

func TestEmitMessageReceived_EmptyRecipientRejected(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	_, err := w.EmitMessageReceived(context.Background(), audit.MessageReceivedEvent{
		MessageID:           mustUUID(t, fixedMessageID),
		ConversationID:      mustUUID(t, fixedConversationID),
		OrganizationID:      mustUUID(t, fixedOrganizationID),
		SenderWatchkeeperID: "wk-a",
		Direction:           "reply",
		CreatedAt:           fixedTime,
	})
	if !errors.Is(err, audit.ErrInvalidEvent) {
		t.Errorf("err = %v, want ErrInvalidEvent on empty recipient", err)
	}
	if rec.count() != 0 {
		t.Errorf("appender called on invalid event")
	}
}

// TestWriter_RejectsWhitespaceOnlyStrings pins the
// `strings.TrimSpace == ""` discipline for every string field the
// validator covers — mirrors the upstream M1.1.a Repository.Open
// participant-trim behavior so an empty-looking value with stray
// whitespace never slips into the audit payload.
func TestWriter_RejectsWhitespaceOnlyStrings(t *testing.T) {
	t.Parallel()

	conv := mustUUID(t, fixedConversationID)
	org := mustUUID(t, fixedOrganizationID)
	msg := mustUUID(t, fixedMessageID)

	t.Run("opened slack channel id whitespace", func(t *testing.T) {
		t.Parallel()
		w, rec := newWriter(t)
		_, err := w.EmitConversationOpened(context.Background(), audit.ConversationOpenedEvent{
			ConversationID: conv,
			OrganizationID: org,
			SlackChannelID: "   ",
			OpenedAt:       fixedTime,
		})
		if !errors.Is(err, audit.ErrInvalidEvent) {
			t.Errorf("err = %v, want ErrInvalidEvent", err)
		}
		if rec.count() != 0 {
			t.Errorf("appender called on whitespace-only slack_channel_id")
		}
	})

	t.Run("sent sender whitespace", func(t *testing.T) {
		t.Parallel()
		w, rec := newWriter(t)
		_, err := w.EmitMessageSent(context.Background(), audit.MessageSentEvent{
			MessageID:           msg,
			ConversationID:      conv,
			OrganizationID:      org,
			SenderWatchkeeperID: "   ",
			Direction:           "request",
			CreatedAt:           fixedTime,
		})
		if !errors.Is(err, audit.ErrInvalidEvent) {
			t.Errorf("err = %v, want ErrInvalidEvent", err)
		}
		if rec.count() != 0 {
			t.Errorf("appender called on whitespace-only sender")
		}
	})

	t.Run("sent direction whitespace", func(t *testing.T) {
		t.Parallel()
		w, rec := newWriter(t)
		_, err := w.EmitMessageSent(context.Background(), audit.MessageSentEvent{
			MessageID:           msg,
			ConversationID:      conv,
			OrganizationID:      org,
			SenderWatchkeeperID: "wk",
			Direction:           "   ",
			CreatedAt:           fixedTime,
		})
		if !errors.Is(err, audit.ErrInvalidEvent) {
			t.Errorf("err = %v, want ErrInvalidEvent", err)
		}
		if rec.count() != 0 {
			t.Errorf("appender called on whitespace-only direction")
		}
	})

	t.Run("received recipient whitespace", func(t *testing.T) {
		t.Parallel()
		w, rec := newWriter(t)
		_, err := w.EmitMessageReceived(context.Background(), audit.MessageReceivedEvent{
			MessageID:              msg,
			ConversationID:         conv,
			OrganizationID:         org,
			SenderWatchkeeperID:    "wk-a",
			RecipientWatchkeeperID: "   ",
			Direction:              "reply",
			CreatedAt:              fixedTime,
		})
		if !errors.Is(err, audit.ErrInvalidEvent) {
			t.Errorf("err = %v, want ErrInvalidEvent", err)
		}
		if rec.count() != 0 {
			t.Errorf("appender called on whitespace-only recipient")
		}
	})
}

func TestEmitOverBudget_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	_, err := w.EmitOverBudget(context.Background(), audit.OverBudgetEvent{
		ConversationID: convID,
		OrganizationID: orgID,
		TokenBudget:    1000,
		TokensUsed:     1200,
		ObservedAt:     fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitOverBudget: %v", err)
	}
	evt := rec.snapshot()[0]
	if evt.EventType != audit.EventOverBudget {
		t.Errorf("EventType = %q, want %q", evt.EventType, audit.EventOverBudget)
	}
	payload := evt.Payload.(map[string]any)
	if payload["conversation_id"] != convID.String() {
		t.Errorf("conversation_id = %v", payload["conversation_id"])
	}
	if payload["token_budget"] != int64(1000) {
		t.Errorf("token_budget = %v, want 1000", payload["token_budget"])
	}
	if payload["tokens_used"] != int64(1200) {
		t.Errorf("tokens_used = %v, want 1200", payload["tokens_used"])
	}
	if payload["observed_at"] != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("observed_at = %v", payload["observed_at"])
	}
}

func TestEmitOverBudget_ValidationFailures(t *testing.T) {
	t.Parallel()

	conv := mustUUID(t, fixedConversationID)
	org := mustUUID(t, fixedOrganizationID)
	cases := []struct {
		name string
		evt  audit.OverBudgetEvent
	}{
		{"zero conv", audit.OverBudgetEvent{OrganizationID: org, ObservedAt: fixedTime}},
		{"zero org", audit.OverBudgetEvent{ConversationID: conv, ObservedAt: fixedTime}},
		{"zero observed_at", audit.OverBudgetEvent{ConversationID: conv, OrganizationID: org}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, rec := newWriter(t)
			_, err := w.EmitOverBudget(context.Background(), tc.evt)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Errorf("err = %v, want ErrInvalidEvent", err)
			}
			if rec.count() != 0 {
				t.Errorf("appender called on invalid event")
			}
		})
	}
}

func TestEmitEscalated_HappyPath(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	convID := mustUUID(t, fixedConversationID)
	orgID := mustUUID(t, fixedOrganizationID)

	_, err := w.EmitEscalated(context.Background(), audit.EscalatedEvent{
		ConversationID:   convID,
		OrganizationID:   orgID,
		EscalatedTo:      "wk-lead",
		EscalationReason: "peer_timeout",
		ObservedAt:       fixedTime,
	})
	if err != nil {
		t.Fatalf("EmitEscalated: %v", err)
	}
	evt := rec.snapshot()[0]
	if evt.EventType != audit.EventEscalated {
		t.Errorf("EventType = %q, want %q", evt.EventType, audit.EventEscalated)
	}
	payload := evt.Payload.(map[string]any)
	if payload["escalated_to"] != "wk-lead" {
		t.Errorf("escalated_to = %v", payload["escalated_to"])
	}
	if payload["escalation_reason"] != "peer_timeout" {
		t.Errorf("escalation_reason = %v", payload["escalation_reason"])
	}
	if payload["observed_at"] != fixedTime.Format(time.RFC3339Nano) {
		t.Errorf("observed_at = %v", payload["observed_at"])
	}
}

func TestEmitEscalated_ValidationFailures(t *testing.T) {
	t.Parallel()

	conv := mustUUID(t, fixedConversationID)
	org := mustUUID(t, fixedOrganizationID)
	cases := []struct {
		name string
		evt  audit.EscalatedEvent
	}{
		{"zero conv", audit.EscalatedEvent{OrganizationID: org, EscalatedTo: "wk", ObservedAt: fixedTime}},
		{"zero org", audit.EscalatedEvent{ConversationID: conv, EscalatedTo: "wk", ObservedAt: fixedTime}},
		{"empty escalated_to", audit.EscalatedEvent{ConversationID: conv, OrganizationID: org, ObservedAt: fixedTime}},
		{"whitespace escalated_to", audit.EscalatedEvent{ConversationID: conv, OrganizationID: org, EscalatedTo: "   ", ObservedAt: fixedTime}},
		{"zero observed_at", audit.EscalatedEvent{ConversationID: conv, OrganizationID: org, EscalatedTo: "wk"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, rec := newWriter(t)
			_, err := w.EmitEscalated(context.Background(), tc.evt)
			if !errors.Is(err, audit.ErrInvalidEvent) {
				t.Errorf("err = %v, want ErrInvalidEvent", err)
			}
			if rec.count() != 0 {
				t.Errorf("appender called on invalid event")
			}
		})
	}
}

func TestWriter_ConcurrentEmits(t *testing.T) {
	t.Parallel()

	w, rec := newWriter(t)
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := w.EmitMessageSent(context.Background(), audit.MessageSentEvent{
				MessageID:           uuid.New(),
				ConversationID:      mustUUID(t, fixedConversationID),
				OrganizationID:      mustUUID(t, fixedOrganizationID),
				SenderWatchkeeperID: "wk-a",
				Direction:           "request",
				CreatedAt:           fixedTime,
			})
			if err != nil {
				t.Errorf("EmitMessageSent: %v", err)
			}
		}()
	}
	wg.Wait()
	if rec.count() != goroutines {
		t.Errorf("recorded %d events, want %d", rec.count(), goroutines)
	}
}
