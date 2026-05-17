package k2k_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
)

// recordingAuditor is the hand-rolled [audit.Emitter] stand-in the
// lifecycle audit-wiring tests inject. Records every emit call (a
// copy of the event so caller-side mutation cannot race the recorded
// snapshot) and returns either a canned id or an injected error.
type recordingAuditor struct {
	mu       sync.Mutex
	opened   []audit.ConversationOpenedEvent
	closed   []audit.ConversationClosedEvent
	openErr  error
	closeErr error
}

func (r *recordingAuditor) EmitConversationOpened(_ context.Context, evt audit.ConversationOpenedEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Defensive copy of participants so a caller mutating the slice
	// after Open() returns cannot rewrite the recorded value.
	cp := evt
	if evt.Participants != nil {
		cp.Participants = make([]string, len(evt.Participants))
		copy(cp.Participants, evt.Participants)
	}
	r.opened = append(r.opened, cp)
	if r.openErr != nil {
		return "", r.openErr
	}
	return "row-open", nil
}

func (r *recordingAuditor) EmitConversationClosed(_ context.Context, evt audit.ConversationClosedEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, evt)
	if r.closeErr != nil {
		return "", r.closeErr
	}
	return "row-close", nil
}

func (r *recordingAuditor) EmitMessageSent(_ context.Context, _ audit.MessageSentEvent) (string, error) {
	return "", errors.New("recordingAuditor: EmitMessageSent not used in lifecycle tests")
}

func (r *recordingAuditor) EmitMessageReceived(_ context.Context, _ audit.MessageReceivedEvent) (string, error) {
	return "", errors.New("recordingAuditor: EmitMessageReceived not used in lifecycle tests")
}

func (r *recordingAuditor) openedSnapshot() []audit.ConversationOpenedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.ConversationOpenedEvent, len(r.opened))
	copy(out, r.opened)
	return out
}

func (r *recordingAuditor) closedSnapshot() []audit.ConversationClosedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.ConversationClosedEvent, len(r.closed))
	copy(out, r.closed)
	return out
}

func newLifecycleWithAuditor(t *testing.T, slack *fakeSlackChannels, auditor audit.Emitter) (*k2k.Lifecycle, *k2k.MemoryRepository) {
	t.Helper()
	repo := newRepo(t)
	lc := k2k.NewLifecycle(k2k.LifecycleDeps{Repo: repo, Slack: slack, Auditor: auditor})
	return lc, repo
}

func TestLifecycle_Open_EmitsConversationOpenedAuditRow(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	orgID := uuid.New()
	corrID := uuid.New()
	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-a", "wk-b"},
		Subject:        "review PR",
		CorrelationID:  corrID,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	emits := auditor.openedSnapshot()
	if len(emits) != 1 {
		t.Fatalf("emitted %d conversation_opened rows, want 1", len(emits))
	}
	got := emits[0]
	if got.ConversationID != conv.ID {
		t.Errorf("ConversationID = %s, want %s", got.ConversationID, conv.ID)
	}
	if got.OrganizationID != orgID {
		t.Errorf("OrganizationID = %s, want %s", got.OrganizationID, orgID)
	}
	if got.SlackChannelID != "C-abc" {
		t.Errorf("SlackChannelID = %q, want C-abc", got.SlackChannelID)
	}
	if got.Subject != "review PR" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.CorrelationID != corrID {
		t.Errorf("CorrelationID = %s, want %s", got.CorrelationID, corrID)
	}
	if len(got.Participants) != 2 || got.Participants[0] != "wk-a" || got.Participants[1] != "wk-b" {
		t.Errorf("Participants = %v", got.Participants)
	}
	if got.OpenedAt.IsZero() {
		t.Errorf("OpenedAt is zero — expected populated from repository row")
	}
}

func TestLifecycle_Open_AuditEmitFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{openErr: errors.New("keep down")}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"wk-a"},
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open returned error on audit failure: %v", err)
	}
	if conv.ID == uuid.Nil {
		t.Errorf("conv.ID is nil — Open should still succeed when audit fails")
	}
	// The auditor was still called.
	if len(auditor.openedSnapshot()) != 1 {
		t.Errorf("auditor not called: %d emits", len(auditor.openedSnapshot()))
	}
}

func TestLifecycle_Open_NilAuditorIsNoop(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	// LifecycleDeps with no Auditor — must not panic + must succeed.
	lc, _ := newLifecycle(t, slack)

	_, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"wk-a"},
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func TestLifecycle_Close_EmitsConversationClosedAuditRow(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"wk-a"},
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Reset openErr to focus on close emit.
	if err := lc.Close(context.Background(), conv.ID, "test-reason"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	emits := auditor.closedSnapshot()
	if len(emits) != 1 {
		t.Fatalf("emitted %d conversation_closed rows, want 1", len(emits))
	}
	got := emits[0]
	if got.ConversationID != conv.ID {
		t.Errorf("ConversationID = %s, want %s", got.ConversationID, conv.ID)
	}
	if got.OrganizationID != conv.OrganizationID {
		t.Errorf("OrganizationID = %s, want %s", got.OrganizationID, conv.OrganizationID)
	}
	if got.CloseReason != "test-reason" {
		t.Errorf("CloseReason = %q, want test-reason", got.CloseReason)
	}
	if got.ClosedAt.IsZero() {
		t.Errorf("ClosedAt is zero — expected populated from repository row")
	}
}

func TestLifecycle_Close_NoEmitOnDoubleClose(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"wk-a"},
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := lc.Close(context.Background(), conv.ID, "first"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	err = lc.Close(context.Background(), conv.ID, "second")
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Fatalf("second Close err = %v, want ErrAlreadyArchived", err)
	}
	// Only the first close emitted an audit row.
	if got := len(auditor.closedSnapshot()); got != 1 {
		t.Errorf("emitted %d conversation_closed rows on double-close, want 1", got)
	}
}

func TestLifecycle_Close_AuditEmitFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{closeErr: errors.New("keep down")}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"wk-a"},
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := lc.Close(context.Background(), conv.ID, "reason"); err != nil {
		t.Fatalf("Close returned error on audit failure: %v", err)
	}
	if len(auditor.closedSnapshot()) != 1 {
		t.Errorf("auditor not called")
	}
}

func TestLifecycle_Open_DefensiveCopyOfParticipantsToAuditPayload(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-abc"}
	auditor := &recordingAuditor{}
	lc, _ := newLifecycleWithAuditor(t, slack, auditor)

	parts := []string{"wk-a", "wk-b"}
	_, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   parts,
		Subject:        "review",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Mutate the caller-side slice; the audit recorded value must not bleed.
	parts[0] = "wk-mutated"
	recorded := auditor.openedSnapshot()[0].Participants
	if recorded[0] != "wk-a" {
		t.Errorf("audit recorded participants[0] = %q after caller mutation, want wk-a (defensive copy broken)", recorded[0])
	}
}
