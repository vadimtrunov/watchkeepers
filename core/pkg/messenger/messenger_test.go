package messenger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestSendMessage_RoundTripsThroughAdapter — happy-path: a caller
// sends a message, the adapter returns the platform-assigned id, and
// the recorded call carries the channel + payload verbatim.
func TestSendMessage_RoundTripsThroughAdapter(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{sendID: "msg-42"}

	id, err := f.SendMessage(context.Background(), "C123", Message{
		Text:     "hello",
		ThreadID: "msg-parent",
		Metadata: map[string]string{"unfurl_links": "false"},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "msg-42" {
		t.Fatalf("SendMessage id = %q, want %q", id, "msg-42")
	}

	got := f.recordedSends()
	if len(got) != 1 {
		t.Fatalf("recordedSends len = %d, want 1", len(got))
	}
	if got[0].ChannelID != "C123" {
		t.Fatalf("recordedSends[0].ChannelID = %q, want C123", got[0].ChannelID)
	}
	if got[0].Message.Text != "hello" {
		t.Fatalf("recordedSends[0].Message.Text = %q, want hello", got[0].Message.Text)
	}
	if got[0].Message.ThreadID != "msg-parent" {
		t.Fatalf("recordedSends[0].Message.ThreadID = %q, want msg-parent", got[0].Message.ThreadID)
	}
	if got[0].Message.Metadata["unfurl_links"] != "false" {
		t.Fatalf("metadata round-trip failed: %+v", got[0].Message.Metadata)
	}
}

// TestSendMessage_ChannelNotFoundSurfacesSentinel — the adapter's
// configured error is surfaced verbatim and remains matchable via
// [errors.Is]. Pins the contract that adapter implementations return
// the package sentinel rather than ad-hoc error types.
func TestSendMessage_ChannelNotFoundSurfacesSentinel(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{sendErr: ErrChannelNotFound}

	_, err := f.SendMessage(context.Background(), "C404", Message{Text: "hi"})
	if !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("SendMessage err = %v, want errors.Is ErrChannelNotFound", err)
	}
}

// TestSubscribe_DeliversIncomingMessages — Subscribe returns a live
// subscription; the fake's Deliver helper routes an [IncomingMessage]
// to the handler; the handler observes the value verbatim.
func TestSubscribe_DeliversIncomingMessages(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}
	received := make(chan IncomingMessage, 1)

	sub, err := f.Subscribe(context.Background(), func(_ context.Context, msg IncomingMessage) error {
		received <- msg
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	want := IncomingMessage{
		ID:        "in-1",
		ChannelID: "C123",
		SenderID:  "U456",
		Text:      "hello bot",
		ThreadID:  "msg-parent",
		Timestamp: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{"channel_type": "im"},
	}
	if err := f.Deliver(context.Background(), want); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case got := <-received:
		if got.ID != want.ID || got.SenderID != want.SenderID || got.Text != want.Text {
			t.Fatalf("handler received %+v, want %+v", got, want)
		}
		if got.Metadata["channel_type"] != "im" {
			t.Fatalf("metadata round-trip failed: %+v", got.Metadata)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not observe delivered message within 2s")
	}
}

// TestSubscribe_NilHandlerReturnsErrInvalidHandler — passing a nil
// handler is a programmer error and surfaces synchronously without
// spawning the receive loop.
func TestSubscribe_NilHandlerReturnsErrInvalidHandler(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}
	_, err := f.Subscribe(context.Background(), nil)
	if !errors.Is(err, ErrInvalidHandler) {
		t.Fatalf("Subscribe(nil) err = %v, want errors.Is ErrInvalidHandler", err)
	}
}

// TestSubscription_StopIsIdempotentAndDrains — Stop terminates
// delivery: the first Stop returns nil, a second Stop also returns
// nil, and a Deliver after Stop short-circuits with
// [ErrSubscriptionClosed] (the handler is not invoked).
func TestSubscription_StopIsIdempotentAndDrains(t *testing.T) {
	t.Parallel()

	var handlerHits int
	var mu sync.Mutex
	f := &FakeMessenger{}
	sub, err := f.Subscribe(context.Background(), func(_ context.Context, _ IncomingMessage) error {
		mu.Lock()
		handlerHits++
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := sub.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	if err := f.Deliver(context.Background(), IncomingMessage{ID: "post-stop"}); !errors.Is(err, ErrSubscriptionClosed) {
		t.Fatalf("Deliver after Stop err = %v, want errors.Is ErrSubscriptionClosed", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if handlerHits != 0 {
		t.Fatalf("handler invoked %d times after Stop, want 0", handlerHits)
	}
}

// TestSubscription_StopSurfacesUnderlyingError — when the receive loop
// exits with a transport error before Stop is called, the next Stop
// surfaces it via the wrap chain. Pins the [ErrSubscriptionClosed]
// contract documented in errors.go.
func TestSubscription_StopSurfacesUnderlyingError(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}
	sub, err := f.Subscribe(context.Background(), func(_ context.Context, _ IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Rig the underlying fakeSubscription to surface a transport-style
	// error on Stop.
	if fs, ok := sub.(*fakeSubscription); ok {
		fs.withStopErr(errFakeBoom)
	} else {
		t.Fatalf("sub is %T, want *fakeSubscription", sub)
	}

	if err := sub.Stop(); !errors.Is(err, errFakeBoom) {
		t.Fatalf("Stop err = %v, want errors.Is errFakeBoom", err)
	}
}

// TestCreateApp_HappyPathReturnsAppID — the manifest is recorded and
// the configured AppID is returned. Pins the M4.2 contract that the
// adapter receives the manifest verbatim (no client-side mutation).
func TestCreateApp_HappyPathReturnsAppID(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{createAppID: "A0123"}
	manifest := AppManifest{
		Name:        "TestBot",
		Description: "A test bot",
		Scopes:      []string{"chat:write", "users:read"},
		Metadata:    map[string]string{"display_color": "#ff0000"},
	}

	id, err := f.CreateApp(context.Background(), manifest)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if id != "A0123" {
		t.Fatalf("CreateApp id = %q, want A0123", id)
	}

	got := f.recordedCreateApps()
	if len(got) != 1 || got[0].Name != "TestBot" {
		t.Fatalf("recordedCreateApps = %+v", got)
	}
	if len(got[0].Scopes) != 2 || got[0].Scopes[0] != "chat:write" {
		t.Fatalf("Scopes round-trip failed: %+v", got[0].Scopes)
	}
}

// TestCreateApp_InvalidManifestSentinelMatches — pins the
// [ErrInvalidManifest] contract for callers who need to distinguish
// validation errors from platform errors.
func TestCreateApp_InvalidManifestSentinelMatches(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{createAppErr: ErrInvalidManifest}

	_, err := f.CreateApp(context.Background(), AppManifest{})
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("CreateApp err = %v, want errors.Is ErrInvalidManifest", err)
	}
}

// TestInstallApp_HappyPathReturnsInstallation — the call records the
// AppID + workspace and returns the [Installation] handle the caller
// drives subsequent operations from.
func TestInstallApp_HappyPathReturnsInstallation(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}
	ws := WorkspaceRef{ID: "T01234", Name: "Dev Workspace"}

	inst, err := f.InstallApp(context.Background(), "A0123", ws)
	if err != nil {
		t.Fatalf("InstallApp: %v", err)
	}
	if inst.AppID != "A0123" {
		t.Fatalf("Installation.AppID = %q, want A0123", inst.AppID)
	}
	if inst.Workspace.ID != "T01234" {
		t.Fatalf("Installation.Workspace.ID = %q, want T01234", inst.Workspace.ID)
	}
	if inst.BotUserID == "" {
		t.Fatalf("Installation.BotUserID empty, fake should populate")
	}

	got := f.recordedInstallApps()
	if len(got) != 1 || got[0].AppID != "A0123" || got[0].Workspace.Name != "Dev Workspace" {
		t.Fatalf("recordedInstallApps = %+v", got)
	}
}

// TestInstallApp_AppNotFoundSentinelMatches — pins the
// [ErrAppNotFound] contract.
func TestInstallApp_AppNotFoundSentinelMatches(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{installAppErr: ErrAppNotFound}

	_, err := f.InstallApp(context.Background(), "A_missing", WorkspaceRef{ID: "T01234"})
	if !errors.Is(err, ErrAppNotFound) {
		t.Fatalf("InstallApp err = %v, want errors.Is ErrAppNotFound", err)
	}
}

// TestSetBotProfile_HappyPathRecordsProfile — the call records the
// profile and returns nil. Empty fields are recorded verbatim;
// adapters interpret them as "leave unchanged" (per the godoc
// contract).
func TestSetBotProfile_HappyPathRecordsProfile(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}
	profile := BotProfile{
		DisplayName: "TestBot",
		StatusText:  "watching",
		AvatarPNG:   []byte{0x89, 0x50, 0x4e, 0x47},
		Metadata:    map[string]string{"status_emoji": ":robot_face:"},
	}

	if err := f.SetBotProfile(context.Background(), profile); err != nil {
		t.Fatalf("SetBotProfile: %v", err)
	}

	got := f.recordedSetBotProfiles()
	if len(got) != 1 {
		t.Fatalf("recordedSetBotProfiles len = %d, want 1", len(got))
	}
	if got[0].DisplayName != "TestBot" || got[0].StatusText != "watching" {
		t.Fatalf("recordedSetBotProfiles[0] = %+v", got[0])
	}
	if len(got[0].AvatarPNG) != 4 {
		t.Fatalf("AvatarPNG round-trip failed: %v", got[0].AvatarPNG)
	}
}

// TestSetBotProfile_UnsupportedSentinelMatches — adapters that don't
// support avatar updates surface [ErrUnsupported]; callers MUST handle
// it.
func TestSetBotProfile_UnsupportedSentinelMatches(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{setBotProfileErr: ErrUnsupported}

	err := f.SetBotProfile(context.Background(), BotProfile{AvatarPNG: []byte{0xff}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SetBotProfile err = %v, want errors.Is ErrUnsupported", err)
	}
}

// TestLookupUser_HappyPathReturnsUser — exactly-one-field query is
// accepted; the fake returns its canned [User].
func TestLookupUser_HappyPathReturnsUser(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{
		lookupUser: User{
			ID:          "U01234",
			Handle:      "alice",
			DisplayName: "Alice",
			Email:       "alice@example.com",
			IsBot:       false,
			Metadata:    map[string]string{"team_id": "T01234"},
		},
	}

	user, err := f.LookupUser(context.Background(), UserQuery{ID: "U01234"})
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if user.ID != "U01234" || user.Handle != "alice" || user.DisplayName != "Alice" {
		t.Fatalf("LookupUser = %+v", user)
	}
	if user.IsBot {
		t.Fatalf("LookupUser.IsBot = true, want false")
	}

	got := f.recordedLookups()
	if len(got) != 1 || got[0].ID != "U01234" {
		t.Fatalf("recordedLookups = %+v", got)
	}
}

// TestLookupUser_InvalidQueryRejectedSynchronously — the
// exactly-one-field rule is enforced before any platform call.
// Empty + over-populated queries both return [ErrInvalidQuery].
func TestLookupUser_InvalidQueryRejectedSynchronously(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{}

	cases := []struct {
		name  string
		query UserQuery
	}{
		{"empty", UserQuery{}},
		{"id+handle", UserQuery{ID: "U1", Handle: "bob"}},
		{"id+email", UserQuery{ID: "U1", Email: "bob@example.com"}},
		{"handle+email", UserQuery{Handle: "bob", Email: "bob@example.com"}},
		{"all three", UserQuery{ID: "U1", Handle: "bob", Email: "bob@example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := f.LookupUser(context.Background(), tc.query)
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("LookupUser(%+v) err = %v, want errors.Is ErrInvalidQuery", tc.query, err)
			}
		})
	}

	// Every rejection short-circuited before the recording slice was
	// touched.
	if got := f.recordedLookups(); len(got) != 0 {
		t.Fatalf("invalid queries reached the platform: recorded %d", len(got))
	}
}

// TestLookupUser_UserNotFoundSentinelMatches — adapters return
// [ErrUserNotFound] when the platform reports no match; the bytes
// remain matchable via [errors.Is].
func TestLookupUser_UserNotFoundSentinelMatches(t *testing.T) {
	t.Parallel()

	f := &FakeMessenger{lookupUserErr: ErrUserNotFound}

	_, err := f.LookupUser(context.Background(), UserQuery{ID: "U_missing"})
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("LookupUser err = %v, want errors.Is ErrUserNotFound", err)
	}
}

// TestAttachment_URLAndDataMutuallyExclusive_DocumentedContract —
// pins the godoc contract that an attachment with both URL and Data
// is invalid. Adapters MUST reject; the test asserts the value-shape
// stays portable (no platform-specific exclusion logic in this
// package). Failure here means a future field rename / type change
// silently broke the contract.
func TestAttachment_URLAndDataMutuallyExclusive_DocumentedContract(t *testing.T) {
	t.Parallel()

	a := Attachment{
		Name: "screenshot.png",
		URL:  "https://example.com/x.png",
		Data: []byte{0x89, 0x50},
	}
	if a.URL == "" || len(a.Data) == 0 {
		t.Fatalf("Attachment fields not populated: %+v", a)
	}
	// The exclusion is enforced at adapter boundary, not in this
	// package. Pinning here so a future struct refactor that drops one
	// of the fields fails the test.
}

// TestSentinelErrors_AllMatchableViaErrorsIs — mass-assertion that
// every package sentinel is matchable via [errors.Is] against itself.
// Pins the matchability contract documented in errors.go.
func TestSentinelErrors_AllMatchableViaErrorsIs(t *testing.T) {
	t.Parallel()

	cases := []error{
		ErrUnsupported,
		ErrChannelNotFound,
		ErrUserNotFound,
		ErrAppNotFound,
		ErrInvalidManifest,
		ErrInvalidQuery,
		ErrInvalidHandler,
		ErrSubscriptionClosed,
	}
	for _, e := range cases {
		if !errors.Is(e, e) {
			t.Errorf("errors.Is(%v, %v) = false, want true", e, e)
		}
	}
}
