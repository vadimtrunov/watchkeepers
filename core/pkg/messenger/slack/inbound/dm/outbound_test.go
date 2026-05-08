package dm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/dm"
)

// fakeSlackSender records SendMessage calls for the
// [dm.SlackOutbound] smoke tests; substitutes a canned error.
type fakeSlackSender struct {
	calls []fakeSlackSendCall
	err   error
}

type fakeSlackSendCall struct {
	ChannelID string
	Msg       messenger.Message
}

func (f *fakeSlackSender) SendMessage(_ context.Context, channelID string, msg messenger.Message) (messenger.MessageID, error) {
	f.calls = append(f.calls, fakeSlackSendCall{ChannelID: channelID, Msg: msg})
	if f.err != nil {
		return "", f.err
	}
	return "ts.0001", nil
}

// TestSlackOutbound_PostHappy pins the production-wiring smoke: the
// adapter forwards `channelID` + the fallback text onto the underlying
// [dm.SlackSender] surface.
func TestSlackOutbound_PostHappy(t *testing.T) {
	t.Parallel()

	fake := &fakeSlackSender{}
	out := dm.NewSlackOutbound(fake)

	err := out.Post(context.Background(), "D-CHAN", []cards.Block{{Type: "section"}}, "hello there")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("SendMessage calls = %d, want 1", len(fake.calls))
	}
	if fake.calls[0].ChannelID != "D-CHAN" {
		t.Errorf("channel id = %q, want D-CHAN", fake.calls[0].ChannelID)
	}
	if fake.calls[0].Msg.Text != "hello there" {
		t.Errorf("text = %q, want %q", fake.calls[0].Msg.Text, "hello there")
	}
}

// TestSlackOutbound_PostSurfacesError pins the wrap chain: a sender
// error round-trips wrapped through the adapter.
func TestSlackOutbound_PostSurfacesError(t *testing.T) {
	t.Parallel()

	canned := errors.New("slack 500")
	fake := &fakeSlackSender{err: canned}
	out := dm.NewSlackOutbound(fake)

	err := out.Post(context.Background(), "D-CHAN", nil, "hi")
	if err == nil {
		t.Fatal("Post returned nil, want non-nil")
	}
	if !errors.Is(err, canned) {
		t.Errorf("err = %v, want wrapping %v", err, canned)
	}
}

// TestNewSlackOutbound_PanicsOnNilSender pins the panic discipline.
func TestNewSlackOutbound_PanicsOnNilSender(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewSlackOutbound(nil) did not panic")
		}
	}()
	_ = dm.NewSlackOutbound(nil)
}
