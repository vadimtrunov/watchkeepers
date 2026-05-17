package k2k_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// appendValidParams returns a fully-populated [k2k.AppendMessageParams]
// keyed off the supplied conversation. Mirrors the [validParams] helper
// style used by the existing Open tests so the message-side tests
// stay scannable next to their lifecycle-side siblings.
func appendValidParams(conv k2k.Conversation, dir k2k.MessageDirection) k2k.AppendMessageParams {
	return k2k.AppendMessageParams{
		ConversationID:      conv.ID,
		OrganizationID:      conv.OrganizationID,
		SenderWatchkeeperID: "bot-a",
		Body:                []byte("hello"),
		Direction:           dir,
	}
}

func TestMemoryRepository_AppendMessage_HappyPath(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	msg, err := r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionRequest))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if msg.ID == uuid.Nil {
		t.Errorf("Message.ID = uuid.Nil, want non-zero (minted by repository)")
	}
	if msg.ConversationID != conv.ID {
		t.Errorf("Message.ConversationID = %v, want %v", msg.ConversationID, conv.ID)
	}
	if msg.OrganizationID != conv.OrganizationID {
		t.Errorf("Message.OrganizationID = %v, want %v", msg.OrganizationID, conv.OrganizationID)
	}
	if msg.SenderWatchkeeperID != "bot-a" {
		t.Errorf("Message.SenderWatchkeeperID = %q, want %q", msg.SenderWatchkeeperID, "bot-a")
	}
	if string(msg.Body) != "hello" {
		t.Errorf("Message.Body = %q, want %q", string(msg.Body), "hello")
	}
	if msg.Direction != k2k.MessageDirectionRequest {
		t.Errorf("Message.Direction = %q, want %q", msg.Direction, k2k.MessageDirectionRequest)
	}
	if msg.CreatedAt.IsZero() {
		t.Error("Message.CreatedAt is zero; want stamped")
	}
}

func TestMemoryRepository_AppendMessage_ValidationFailures(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(p *k2k.AppendMessageParams)
		want   error
	}{
		{
			name:   "empty conversation id",
			mutate: func(p *k2k.AppendMessageParams) { p.ConversationID = uuid.Nil },
			want:   k2k.ErrEmptyConversationID,
		},
		{
			name:   "empty organization id",
			mutate: func(p *k2k.AppendMessageParams) { p.OrganizationID = uuid.Nil },
			want:   k2k.ErrEmptyOrganization,
		},
		{
			name:   "whitespace sender",
			mutate: func(p *k2k.AppendMessageParams) { p.SenderWatchkeeperID = "   " },
			want:   k2k.ErrEmptySenderWatchkeeperID,
		},
		{
			name:   "empty body",
			mutate: func(p *k2k.AppendMessageParams) { p.Body = nil },
			want:   k2k.ErrEmptyMessageBody,
		},
		{
			name:   "invalid direction",
			mutate: func(p *k2k.AppendMessageParams) { p.Direction = "bogus" },
			want:   k2k.ErrInvalidMessageDirection,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := appendValidParams(conv, k2k.MessageDirectionRequest)
			tc.mutate(&p)
			_, err := r.AppendMessage(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Errorf("AppendMessage err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestMemoryRepository_AppendMessage_CtxCancelled(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.AppendMessage(ctx, appendValidParams(conv, k2k.MessageDirectionRequest))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("AppendMessage err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_AppendMessage_UnknownConversation(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	p := k2k.AppendMessageParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		SenderWatchkeeperID: "bot-a",
		Body:                []byte("hi"),
		Direction:           k2k.MessageDirectionRequest,
	}
	_, err := r.AppendMessage(context.Background(), p)
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("AppendMessage err = %v, want errors.Is ErrConversationNotFound", err)
	}
}

func TestMemoryRepository_AppendMessage_ArchivedConversation(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionRequest))
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Errorf("AppendMessage err = %v, want errors.Is ErrAlreadyArchived", err)
	}
}

func TestMemoryRepository_AppendMessage_DefensiveCopyOfBody(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body := []byte("original")
	msg, err := r.AppendMessage(context.Background(), k2k.AppendMessageParams{
		ConversationID:      conv.ID,
		OrganizationID:      conv.OrganizationID,
		SenderWatchkeeperID: "bot-a",
		Body:                body,
		Direction:           k2k.MessageDirectionRequest,
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// Caller-side mutation of the input slice must not bleed into
	// the returned message body or any subsequent WaitForReply
	// result.
	for i := range body {
		body[i] = 'X'
	}
	if string(msg.Body) != "original" {
		t.Errorf("returned Body = %q, want %q (defensive copy regressed)", string(msg.Body), "original")
	}
}

func TestMemoryRepository_WaitForReply_AlreadyPresent(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before := time.Date(2026, 5, 17, 11, 59, 0, 0, time.UTC)
	if _, err := r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionReply)); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	msg, err := r.WaitForReply(context.Background(), conv.ID, before, time.Second)
	if err != nil {
		t.Fatalf("WaitForReply: %v", err)
	}
	if msg.Direction != k2k.MessageDirectionReply {
		t.Errorf("Direction = %q, want %q", msg.Direction, k2k.MessageDirectionReply)
	}
}

func TestMemoryRepository_WaitForReply_SignalledOnAppend(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	since := time.Now().UTC()

	done := make(chan struct{})
	var (
		gotMsg k2k.Message
		gotErr error
	)
	go func() {
		gotMsg, gotErr = r.WaitForReply(context.Background(), conv.ID, since, 2*time.Second)
		close(done)
	}()

	// Yield to give the waiter a chance to park on the cond-var
	// before we append. Without the yield the append might land
	// strictly before the wait runs, and we would exercise the
	// "already present" path rather than the "signalled on append"
	// path. Mirrors the M1.1.a concurrent-Open test's discipline.
	time.Sleep(10 * time.Millisecond)

	if _, err := r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionReply)); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForReply did not return within timeout after a matching reply")
	}
	if gotErr != nil {
		t.Errorf("WaitForReply err = %v, want nil", gotErr)
	}
	if gotMsg.Direction != k2k.MessageDirectionReply {
		t.Errorf("Direction = %q, want %q", gotMsg.Direction, k2k.MessageDirectionReply)
	}
}

func TestMemoryRepository_WaitForReply_TimeoutFires(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	start := time.Now()
	_, err = r.WaitForReply(context.Background(), conv.ID, time.Now().UTC(), 50*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, k2k.ErrWaitForReplyTimeout) {
		t.Fatalf("WaitForReply err = %v, want errors.Is ErrWaitForReplyTimeout", err)
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, want >= 40ms (timeout fired prematurely)", elapsed)
	}
}

func TestMemoryRepository_WaitForReply_CtxCancelled(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = r.WaitForReply(ctx, conv.ID, time.Now().UTC(), 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitForReply err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_WaitForReply_IgnoresRequestDirection(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionRequest)); err != nil {
		t.Fatalf("AppendMessage(request): %v", err)
	}
	// Should time out because the only message is request-direction.
	_, err = r.WaitForReply(context.Background(), conv.ID, time.Now().UTC().Add(-time.Hour), 50*time.Millisecond)
	if !errors.Is(err, k2k.ErrWaitForReplyTimeout) {
		t.Errorf("WaitForReply err = %v, want ErrWaitForReplyTimeout", err)
	}
}

func TestMemoryRepository_WaitForReply_ValidationFailures(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)

	cases := []struct {
		name    string
		convID  uuid.UUID
		timeout time.Duration
		want    error
	}{
		{name: "empty conversation id", convID: uuid.Nil, timeout: time.Second, want: k2k.ErrEmptyConversationID},
		{name: "zero timeout", convID: uuid.New(), timeout: 0, want: k2k.ErrInvalidWaitTimeout},
		{name: "negative timeout", convID: uuid.New(), timeout: -time.Second, want: k2k.ErrInvalidWaitTimeout},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.WaitForReply(context.Background(), tc.convID, time.Now().UTC(), tc.timeout)
			if !errors.Is(err, tc.want) {
				t.Errorf("WaitForReply err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestMemoryRepository_WaitForReply_PrecancelledCtx(t *testing.T) {
	t.Parallel()

	r := k2k.NewMemoryRepository(time.Now, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.WaitForReply(ctx, uuid.New(), time.Now().UTC(), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitForReply err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_WaitForReply_ConcurrentAskReply(t *testing.T) {
	t.Parallel()

	// 16 concurrent ask-reply pairs on 16 distinct conversations:
	// each ask blocks on WaitForReply, each reply appends. The test
	// pins that no ask is dropped (every WaitForReply must return
	// the matching reply within the timeout).
	const pairs = 16

	r := k2k.NewMemoryRepository(time.Now, nil)
	convs := make([]k2k.Conversation, pairs)
	for i := 0; i < pairs; i++ {
		c, err := r.Open(context.Background(), validParams())
		if err != nil {
			t.Fatalf("Open[%d]: %v", i, err)
		}
		convs[i] = c
	}

	var (
		wg       sync.WaitGroup
		gotCount int64
	)
	wg.Add(pairs * 2)
	for i := 0; i < pairs; i++ {
		i := i
		go func() {
			defer wg.Done()
			since := time.Now().UTC()
			msg, err := r.WaitForReply(context.Background(), convs[i].ID, since, 10*time.Second)
			if err != nil {
				t.Errorf("WaitForReply[%d]: %v", i, err)
				return
			}
			if string(msg.Body) != fmt.Sprintf("reply-%d", i) {
				t.Errorf("Body[%d] = %q, want %q", i, string(msg.Body), fmt.Sprintf("reply-%d", i))
				return
			}
			atomic.AddInt64(&gotCount, 1)
		}()
		go func() {
			defer wg.Done()
			// Spread the appends across the window so some land
			// before WaitForReply parks and some land after.
			time.Sleep(time.Duration(i) * time.Millisecond)
			_, err := r.AppendMessage(context.Background(), k2k.AppendMessageParams{
				ConversationID:      convs[i].ID,
				OrganizationID:      convs[i].OrganizationID,
				SenderWatchkeeperID: "bot-replier",
				Body:                []byte(fmt.Sprintf("reply-%d", i)),
				Direction:           k2k.MessageDirectionReply,
			})
			if err != nil {
				t.Errorf("AppendMessage[%d]: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt64(&gotCount) != pairs {
		t.Errorf("gotCount = %d, want %d (every WaitForReply must return a matching reply)", gotCount, pairs)
	}
}

func TestMemoryRepository_WaitForReply_SinceCursorIsExclusive(t *testing.T) {
	t.Parallel()

	// A reply appended at exactly `since` must NOT satisfy the wait.
	// The repository's cond-var-driven scan uses `m.CreatedAt.After(since)`,
	// which is strictly-greater-than.
	r := k2k.NewMemoryRepository(fixedClock(), nil)
	conv, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	msg, err := r.AppendMessage(context.Background(), appendValidParams(conv, k2k.MessageDirectionReply))
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	// fixedClock returns the same instant every call; the appended
	// reply's CreatedAt equals what we'll pass as `since`.
	_, err = r.WaitForReply(context.Background(), conv.ID, msg.CreatedAt, 30*time.Millisecond)
	if !errors.Is(err, k2k.ErrWaitForReplyTimeout) {
		t.Errorf("WaitForReply err = %v, want ErrWaitForReplyTimeout (since must be strictly exclusive)", err)
	}
}

func TestMessageDirection_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dir  k2k.MessageDirection
		want error
	}{
		{dir: k2k.MessageDirectionRequest, want: nil},
		{dir: k2k.MessageDirectionReply, want: nil},
		{dir: "", want: k2k.ErrInvalidMessageDirection},
		{dir: "bogus", want: k2k.ErrInvalidMessageDirection},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.dir), func(t *testing.T) {
			t.Parallel()
			got := tc.dir.Validate()
			if tc.want == nil {
				if got != nil {
					t.Errorf("Validate(%q) = %v, want nil", tc.dir, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("Validate(%q) = %v, want errors.Is %v", tc.dir, got, tc.want)
			}
		})
	}
}
