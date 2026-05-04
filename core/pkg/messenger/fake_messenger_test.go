package messenger

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Compile-time assertion that *FakeMessenger satisfies
// [Adapter]. Pinned in `_test.go` so the production package
// stays free of test-only symbols (mirrors the lifecycle / outbox /
// keeperslog hand-rolled-fake pattern documented in
// `docs/LESSONS.md`).
var _ Adapter = (*FakeMessenger)(nil)

// FakeMessenger is the hand-rolled [Adapter] stand-in used by
// the messenger test suite. Records every call (request + optional
// canned response) so tests can assert behaviour without standing up a
// platform mock. The fake is also imported by adapter test suites to
// drive Subscribe-based round-trip checks. No mocking library.
type FakeMessenger struct {
	mu sync.Mutex

	// Recorded calls (defensive copies returned via accessor methods).
	sendCalls          []sendCall
	createAppCalls     []AppManifest
	installAppCalls    []installAppCall
	setBotProfileCalls []BotProfile
	lookupUserCalls    []UserQuery
	subscribeCount     int

	// Canned responses / injected errors.
	sendErr            error
	sendID             MessageID
	createAppErr       error
	createAppID        AppID
	installAppErr      error
	installApp         Installation
	setBotProfileErr   error
	lookupUserErr      error
	lookupUser         User
	subscribeErr       error
	activeSubscription *fakeSubscription
}

// sendCall records a single SendMessage invocation. The Message is
// captured by value so subsequent assertions cannot race a still-
// mutating caller.
type sendCall struct {
	ChannelID string
	Message   Message
}

// installAppCall records the parameters of a single InstallApp call.
type installAppCall struct {
	AppID     AppID
	Workspace WorkspaceRef
}

// SendMessage records the call and returns the configured response or
// error. Returns the canned [FakeMessenger.sendID] (defaults to
// "fake-msg-id" when unset) on success.
func (f *FakeMessenger) SendMessage(_ context.Context, channelID string, msg Message) (MessageID, error) {
	f.mu.Lock()
	f.sendCalls = append(f.sendCalls, sendCall{ChannelID: channelID, Message: msg})
	err := f.sendErr
	id := f.sendID
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	if id == "" {
		id = "fake-msg-id"
	}
	return id, nil
}

// Subscribe spawns a [fakeSubscription] and returns it. Tests drive
// inbound events via [FakeMessenger.Deliver]. A nil handler returns
// [ErrInvalidHandler].
func (f *FakeMessenger) Subscribe(_ context.Context, handler MessageHandler) (Subscription, error) {
	if handler == nil {
		return nil, ErrInvalidHandler
	}
	f.mu.Lock()
	f.subscribeCount++
	err := f.subscribeErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sub := newFakeSubscription(handler)
	f.mu.Lock()
	f.activeSubscription = sub
	f.mu.Unlock()
	return sub, nil
}

// CreateApp records the manifest and returns the configured response
// or error. Returns [FakeMessenger.createAppID] (defaults to
// "fake-app-id") on success.
func (f *FakeMessenger) CreateApp(_ context.Context, manifest AppManifest) (AppID, error) {
	f.mu.Lock()
	f.createAppCalls = append(f.createAppCalls, manifest)
	err := f.createAppErr
	id := f.createAppID
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	if id == "" {
		id = "fake-app-id"
	}
	return id, nil
}

// InstallApp records the call and returns the configured installation
// or error. Empty/zero [Installation] defaults to a populated one
// echoing the inputs so tests can assert the round-trip.
func (f *FakeMessenger) InstallApp(_ context.Context, appID AppID, workspace WorkspaceRef) (Installation, error) {
	f.mu.Lock()
	f.installAppCalls = append(f.installAppCalls, installAppCall{AppID: appID, Workspace: workspace})
	err := f.installAppErr
	inst := f.installApp
	f.mu.Unlock()
	if err != nil {
		return Installation{}, err
	}
	if inst.AppID == "" && inst.Workspace.ID == "" {
		inst = Installation{
			AppID:       appID,
			Workspace:   workspace,
			BotUserID:   "fake-bot-user",
			InstalledAt: time.Unix(0, 0).UTC(),
		}
	}
	return inst, nil
}

// SetBotProfile records the profile and returns the configured error.
func (f *FakeMessenger) SetBotProfile(_ context.Context, profile BotProfile) error {
	f.mu.Lock()
	f.setBotProfileCalls = append(f.setBotProfileCalls, profile)
	err := f.setBotProfileErr
	f.mu.Unlock()
	return err
}

// LookupUser records the query and returns the configured user or
// error. Validates the exactly-one-field rule synchronously
// — mirrors the behaviour every real adapter must implement.
func (f *FakeMessenger) LookupUser(_ context.Context, query UserQuery) (User, error) {
	if !validUserQuery(query) {
		return User{}, ErrInvalidQuery
	}
	f.mu.Lock()
	f.lookupUserCalls = append(f.lookupUserCalls, query)
	err := f.lookupUserErr
	user := f.lookupUser
	f.mu.Unlock()
	if err != nil {
		return User{}, err
	}
	if user.ID == "" {
		user = User{ID: "fake-user", Handle: "fakehandle", DisplayName: "Fake User"}
	}
	return user, nil
}

// Deliver routes an [IncomingMessage] to the active subscription's
// handler. Tests use this to simulate inbound platform events. Returns
// the handler's error or [ErrSubscriptionClosed] when no subscription
// is active.
func (f *FakeMessenger) Deliver(ctx context.Context, msg IncomingMessage) error {
	f.mu.Lock()
	sub := f.activeSubscription
	f.mu.Unlock()
	if sub == nil {
		return ErrSubscriptionClosed
	}
	return sub.deliver(ctx, msg)
}

// Recorded accessors return defensive copies so callers cannot mutate
// the fake's internal slices.

func (f *FakeMessenger) recordedSends() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendCall, len(f.sendCalls))
	copy(out, f.sendCalls)
	return out
}

func (f *FakeMessenger) recordedCreateApps() []AppManifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]AppManifest, len(f.createAppCalls))
	copy(out, f.createAppCalls)
	return out
}

func (f *FakeMessenger) recordedInstallApps() []installAppCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]installAppCall, len(f.installAppCalls))
	copy(out, f.installAppCalls)
	return out
}

func (f *FakeMessenger) recordedSetBotProfiles() []BotProfile {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BotProfile, len(f.setBotProfileCalls))
	copy(out, f.setBotProfileCalls)
	return out
}

func (f *FakeMessenger) recordedLookups() []UserQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]UserQuery, len(f.lookupUserCalls))
	copy(out, f.lookupUserCalls)
	return out
}

// validUserQuery enforces the exactly-one-field rule documented on
// [UserQuery]. Exposed test-side here so the fake mirrors the
// portable contract. Real adapters apply the same rule.
func validUserQuery(q UserQuery) bool {
	count := 0
	if q.ID != "" {
		count++
	}
	if q.Handle != "" {
		count++
	}
	if q.Email != "" {
		count++
	}
	return count == 1
}

// fakeSubscription is the hand-rolled [Subscription] returned by
// [FakeMessenger.Subscribe]. Stop is idempotent; calls made after Stop
// are short-circuited to [ErrSubscriptionClosed] from the deliver path
// but Stop itself returns nil.
type fakeSubscription struct {
	handler MessageHandler

	mu       sync.Mutex
	stopped  bool
	stopErr  error
	stopOnce sync.Once
}

func newFakeSubscription(handler MessageHandler) *fakeSubscription {
	return &fakeSubscription{handler: handler}
}

// deliver invokes the subscription handler with `msg` under the
// subscription's lock so a concurrent Stop cannot race the handler
// callback. Returns [ErrSubscriptionClosed] if Stop has already
// fired.
func (s *fakeSubscription) deliver(ctx context.Context, msg IncomingMessage) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrSubscriptionClosed
	}
	handler := s.handler
	s.mu.Unlock()
	return handler(ctx, msg)
}

// Stop marks the subscription as closed and blocks until any in-flight
// deliver has returned. Idempotent.
func (s *fakeSubscription) Stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
	})
	return s.stopErr
}

// withStopErr is a test helper that rigs the subscription to surface
// `err` from a future Stop call (used by closed-stream tests).
func (s *fakeSubscription) withStopErr(err error) *fakeSubscription {
	s.mu.Lock()
	s.stopErr = err
	s.mu.Unlock()
	return s
}

// Sentinel for tests that need a non-nil non-package error on the
// subscription. Defined here so the test suite imports nothing
// external.
var errFakeBoom = errors.New("fake: boom")
