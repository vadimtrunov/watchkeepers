package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Compile-time assertion that *FakeRuntime satisfies [AgentRuntime].
// Pinned in `_test.go` so the production package stays free of
// test-only symbols (mirrors the messenger / lifecycle / outbox /
// keeperslog hand-rolled-fake pattern documented in `docs/LESSONS.md`).
var _ AgentRuntime = (*FakeRuntime)(nil)

// fakeSession is the per-id bookkeeping the [FakeRuntime] tracks
// for an active or recently-terminated session. The session retains
// state after Terminate so tests can assert the [ErrTerminated] vs
// [ErrRuntimeNotFound] distinction.
type fakeSession struct {
	manifest   Manifest
	toolset    map[string]struct{}
	terminated bool
	subs       []*fakeSubscription
}

// FakeRuntime is the hand-rolled [AgentRuntime] stand-in used by the
// runtime test suite. Records every call (request + optional canned
// response) so tests can assert behaviour without standing up a real
// runtime. The fake is also imported by future runtime test suites to
// drive Subscribe-based round-trip checks. No mocking library.
type FakeRuntime struct {
	mu sync.Mutex

	// idSeq stamps IDs deterministically: "fake-runtime-1",
	// "fake-runtime-2", ... so tests can pin the value.
	idSeq int

	sessions map[ID]*fakeSession

	// Recorded calls (defensive copies returned via accessor methods).
	startCalls      []Manifest
	sendCalls       []sendCall
	invokeToolCalls []invokeToolCall
	terminateCalls  []ID
	subscribeCalls  []ID

	// Canned responses / injected errors.
	startErr     error
	sendErr      error
	invokeErr    error
	invokeResult ToolResult
	terminateErr error
	subscribeErr error
}

// sendCall records a single SendMessage invocation. Captured by value
// so subsequent assertions cannot race a still-mutating caller.
type sendCall struct {
	RuntimeID ID
	Message   Message
}

// invokeToolCall records the parameters of a single InvokeTool call.
type invokeToolCall struct {
	RuntimeID ID
	Call      ToolCall
}

// newFakeRuntime constructs a FakeRuntime with an initialised session
// map. Callers can also use `&FakeRuntime{}` directly — Start lazily
// initialises the map under the lock — but constructing via the helper
// keeps test setups uniform.
func newFakeRuntime() *FakeRuntime {
	return &FakeRuntime{sessions: make(map[ID]*fakeSession)}
}

// Start records the manifest, validates it, and mints a fresh
// [ID]. Returns the canned [FakeRuntime.startErr] when set.
func (f *FakeRuntime) Start(_ context.Context, manifest Manifest, opts ...StartOption) (Runtime, error) {
	if manifest.AgentID == "" || manifest.SystemPrompt == "" || manifest.Model == "" {
		return nil, ErrInvalidManifest
	}
	// Apply options for fidelity even though the fake does not
	// consume them — pins the call site for future StartOption
	// additions.
	var so StartOptions
	for _, opt := range opts {
		opt(&so)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sessions == nil {
		f.sessions = make(map[ID]*fakeSession)
	}
	f.startCalls = append(f.startCalls, manifest)
	if f.startErr != nil {
		return nil, f.startErr
	}

	f.idSeq++
	id := ID(fmt.Sprintf("fake-runtime-%d", f.idSeq))
	// M5.6.e.a: Toolset migrated from []string to []ToolEntry; the ACL
	// gate keys on Name only, so project via .Names() to preserve the
	// existing map shape and lookup semantics.
	names := manifest.Toolset.Names()
	toolset := make(map[string]struct{}, len(names))
	for _, n := range names {
		toolset[n] = struct{}{}
	}
	f.sessions[id] = &fakeSession{manifest: manifest, toolset: toolset}
	return &fakeRuntimeHandle{id: id}, nil
}

// SendMessage validates the runtime id and records the call. Returns
// [ErrInvalidMessage] / [ErrRuntimeNotFound] / [ErrTerminated] per the
// godoc contract; a caller-injected [FakeRuntime.sendErr] short-
// circuits AFTER validation so tests can pin the error path.
func (f *FakeRuntime) SendMessage(_ context.Context, id ID, msg Message) error {
	if msg.Text == "" {
		return ErrInvalidMessage
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	sess, ok := f.sessions[id]
	if !ok {
		return ErrRuntimeNotFound
	}
	if sess.terminated {
		return ErrTerminated
	}
	f.sendCalls = append(f.sendCalls, sendCall{RuntimeID: id, Message: msg})
	return f.sendErr
}

// InvokeTool validates the runtime id and tool name, enforces the
// session manifest's [Manifest.Toolset] ACL, and returns the configured
// canned [FakeRuntime.invokeResult].
func (f *FakeRuntime) InvokeTool(_ context.Context, id ID, call ToolCall) (ToolResult, error) {
	if call.Name == "" {
		return ToolResult{}, ErrInvalidToolCall
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	sess, ok := f.sessions[id]
	if !ok {
		return ToolResult{}, ErrRuntimeNotFound
	}
	if sess.terminated {
		return ToolResult{}, ErrTerminated
	}
	if _, allowed := sess.toolset[call.Name]; !allowed {
		return ToolResult{}, ErrToolUnauthorized
	}
	f.invokeToolCalls = append(f.invokeToolCalls, invokeToolCall{RuntimeID: id, Call: call})
	if f.invokeErr != nil {
		return ToolResult{}, f.invokeErr
	}
	return f.invokeResult, nil
}

// Subscribe spawns a [fakeSubscription] attached to the session and
// returns it. Tests drive event delivery via [FakeRuntime.Deliver]. A
// nil handler returns [ErrInvalidHandler].
func (f *FakeRuntime) Subscribe(_ context.Context, id ID, handler EventHandler) (Subscription, error) {
	if handler == nil {
		return nil, ErrInvalidHandler
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	sess, ok := f.sessions[id]
	if !ok {
		return nil, ErrRuntimeNotFound
	}
	if sess.terminated {
		return nil, ErrTerminated
	}
	f.subscribeCalls = append(f.subscribeCalls, id)
	if f.subscribeErr != nil {
		return nil, f.subscribeErr
	}
	sub := newFakeSubscription(handler)
	sess.subs = append(sess.subs, sub)
	return sub, nil
}

// Terminate marks the session as terminated. Idempotent: a second call
// on the same id returns nil without re-running the shutdown.
func (f *FakeRuntime) Terminate(_ context.Context, id ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.terminateCalls = append(f.terminateCalls, id)
	if f.terminateErr != nil {
		return f.terminateErr
	}
	sess, ok := f.sessions[id]
	if !ok {
		// Idempotent on an unknown id; mirrors the godoc contract.
		return nil
	}
	sess.terminated = true
	return nil
}

// Deliver routes a [Event] to every active subscription on the
// session identified by `id`. Tests use this to simulate runtime-side
// streaming. Returns the FIRST handler error encountered (per-handler
// errors after the first are dropped, matching at-most-once semantics)
// or [ErrSubscriptionClosed] when no live subscription exists.
func (f *FakeRuntime) Deliver(ctx context.Context, id ID, ev Event) error {
	f.mu.Lock()
	sess, ok := f.sessions[id]
	if !ok {
		f.mu.Unlock()
		return ErrRuntimeNotFound
	}
	if sess.terminated {
		f.mu.Unlock()
		return ErrTerminated
	}
	subs := append([]*fakeSubscription(nil), sess.subs...)
	f.mu.Unlock()

	if len(subs) == 0 {
		return ErrSubscriptionClosed
	}
	var firstErr error
	delivered := false
	for _, s := range subs {
		err := s.deliver(ctx, ev)
		if errors.Is(err, ErrSubscriptionClosed) {
			continue
		}
		delivered = true
		if firstErr == nil && err != nil {
			firstErr = err
		}
	}
	if !delivered {
		return ErrSubscriptionClosed
	}
	return firstErr
}

// Recorded accessors return defensive copies so callers cannot mutate
// the fake's internal slices.

func (f *FakeRuntime) recordedStarts() []Manifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Manifest, len(f.startCalls))
	copy(out, f.startCalls)
	return out
}

func (f *FakeRuntime) recordedSends() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendCall, len(f.sendCalls))
	copy(out, f.sendCalls)
	return out
}

func (f *FakeRuntime) recordedInvokes() []invokeToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]invokeToolCall, len(f.invokeToolCalls))
	copy(out, f.invokeToolCalls)
	return out
}

func (f *FakeRuntime) recordedTerminates() []ID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ID, len(f.terminateCalls))
	copy(out, f.terminateCalls)
	return out
}

// fakeRuntimeHandle satisfies [Runtime].
type fakeRuntimeHandle struct {
	id ID
}

// ID returns the [ID] the fake assigned at Start.
func (h *fakeRuntimeHandle) ID() ID {
	return h.id
}

// fakeSubscription is the hand-rolled [Subscription] returned by
// [FakeRuntime.Subscribe]. Stop is idempotent; calls made after Stop
// are short-circuited to [ErrSubscriptionClosed] from the deliver path
// but Stop itself returns nil.
type fakeSubscription struct {
	handler EventHandler

	mu       sync.Mutex
	stopped  bool
	stopErr  error
	stopOnce sync.Once
}

func newFakeSubscription(handler EventHandler) *fakeSubscription {
	return &fakeSubscription{handler: handler}
}

// deliver invokes the subscription handler with `ev` while the
// subscription is still alive. Returns [ErrSubscriptionClosed] if Stop
// has fired.
func (s *fakeSubscription) deliver(ctx context.Context, ev Event) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrSubscriptionClosed
	}
	handler := s.handler
	s.mu.Unlock()
	return handler(ctx, ev)
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

// errFakeBoom is a sentinel for tests that need a non-nil non-package
// error (e.g. to surface from a transport-level failure path).
var errFakeBoom = errors.New("fake: boom")
