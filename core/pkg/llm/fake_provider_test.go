package llm

import (
	"context"
	"errors"
	"sync"
)

// Compile-time assertion that *FakeProvider satisfies [Provider].
// Pinned in `_test.go` so the production package stays free of
// test-only symbols (mirrors the messenger / runtime / outbox /
// keeperslog hand-rolled-fake pattern documented in `docs/LESSONS.md`).
var _ Provider = (*FakeProvider)(nil)

// FakeProvider is the hand-rolled [Provider] stand-in used by the
// llm test suite. Records every call (request + optional canned
// response) so tests can assert behaviour without standing up a real
// provider. Future provider test suites import the same fake to drive
// Stream-based round-trip checks (M5.10 provider-swap conformance).
// No mocking library.
type FakeProvider struct {
	mu sync.Mutex

	// Models lists the [Model] values the fake's catalogue accepts.
	// Empty means "accept any non-empty model"; populate to exercise
	// the [ErrModelNotSupported] path.
	Models map[Model]struct{}

	// Recorded calls (defensive copies returned via accessor methods).
	completeCalls    []CompleteRequest
	streamCalls      []StreamRequest
	countTokensCalls []CountTokensRequest
	reportCostCalls  []reportCostCall

	// Canned responses / injected errors.
	completeResp    CompleteResponse
	completeErr     error
	streamEvents    []StreamEvent
	streamErr       error
	countTokensResp int
	countTokensErr  error
	reportCostErr   error

	// Active stream subscriptions tracked so tests can assert Stop
	// drains them and idempotent shutdown holds.
	streams []*fakeStreamSubscription
}

// reportCostCall records a single ReportCost invocation.
type reportCostCall struct {
	RuntimeID string
	Usage     Usage
}

// newFakeProvider constructs a FakeProvider. Callers can also use
// `&FakeProvider{}` directly — the fake lazily initialises its models
// map under the lock.
func newFakeProvider() *FakeProvider {
	return &FakeProvider{}
}

// Complete validates the request, records it, and returns the canned
// [FakeProvider.completeResp]. Returns the canned
// [FakeProvider.completeErr] when set (validation errors take
// precedence so error-path tests pin synchronous behaviour).
func (f *FakeProvider) Complete(_ context.Context, req CompleteRequest) (CompleteResponse, error) {
	if err := f.validateModel(req.Model); err != nil {
		return CompleteResponse{}, err
	}
	if len(req.Messages) == 0 {
		return CompleteResponse{}, ErrInvalidPrompt
	}
	for _, td := range req.Tools {
		if td.InputSchema == nil {
			return CompleteResponse{}, ErrInvalidPrompt
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls = append(f.completeCalls, req)
	if f.completeErr != nil {
		return CompleteResponse{}, f.completeErr
	}
	return f.completeResp, nil
}

// Stream validates the request, records it, and dispatches the
// configured [FakeProvider.streamEvents] to the handler synchronously
// (the fake does NOT spawn a goroutine — tests get deterministic
// ordering). Handler errors short-circuit dispatch and surface from a
// future [StreamSubscription.Stop].
func (f *FakeProvider) Stream(ctx context.Context, req StreamRequest, handler StreamHandler) (StreamSubscription, error) {
	if err := f.validateModel(req.Model); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 {
		return nil, ErrInvalidPrompt
	}
	if handler == nil {
		return nil, ErrInvalidHandler
	}
	for _, td := range req.Tools {
		if td.InputSchema == nil {
			return nil, ErrInvalidPrompt
		}
	}

	f.mu.Lock()
	f.streamCalls = append(f.streamCalls, req)
	if f.streamErr != nil {
		err := f.streamErr
		f.mu.Unlock()
		return nil, err
	}
	events := append([]StreamEvent(nil), f.streamEvents...)
	sub := &fakeStreamSubscription{}
	f.streams = append(f.streams, sub)
	f.mu.Unlock()

	// Dispatch synchronously so test assertions see the events before
	// Stream returns. A real provider would dispatch from a worker
	// goroutine.
	for _, ev := range events {
		if sub.isStopped() {
			break
		}
		if err := handler(ctx, ev); err != nil {
			sub.markStopped(err)
			break
		}
	}
	return sub, nil
}

// CountTokens validates the request, records it, and returns the
// configured [FakeProvider.countTokensResp]. The default behaviour
// (when countTokensResp is zero and countTokensErr is nil) computes a
// deterministic token count from message lengths so tests that don't
// rig the response still get a stable value.
func (f *FakeProvider) CountTokens(_ context.Context, req CountTokensRequest) (int, error) {
	if err := f.validateModel(req.Model); err != nil {
		return 0, err
	}
	if len(req.Messages) == 0 {
		return 0, ErrInvalidPrompt
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.countTokensCalls = append(f.countTokensCalls, req)
	if f.countTokensErr != nil {
		return 0, f.countTokensErr
	}
	if f.countTokensResp != 0 {
		return f.countTokensResp, nil
	}
	// Deterministic synthetic count: 1 token per 4 bytes of content,
	// rounded up; system prompt counts the same way.
	total := tokenizeBytes(req.System)
	for _, m := range req.Messages {
		total += tokenizeBytes(m.Content)
	}
	for _, t := range req.Tools {
		total += tokenizeBytes(t.Name) + tokenizeBytes(t.Description)
	}
	return total, nil
}

// ReportCost records the runtimeID + usage tuple in the fake's log.
// Returns the canned [FakeProvider.reportCostErr] when set.
func (f *FakeProvider) ReportCost(_ context.Context, runtimeID string, usage Usage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reportCostCalls = append(f.reportCostCalls, reportCostCall{RuntimeID: runtimeID, Usage: usage})
	return f.reportCostErr
}

// validateModel rejects an empty model unconditionally and an
// off-catalogue model when [FakeProvider.Models] is populated.
func (f *FakeProvider) validateModel(m Model) error {
	if m == "" {
		return ErrModelNotSupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Models) == 0 {
		return nil
	}
	if _, ok := f.Models[m]; !ok {
		return ErrModelNotSupported
	}
	return nil
}

// Recorded accessors return defensive copies so callers cannot mutate
// the fake's internal slices.

func (f *FakeProvider) recordedCompletes() []CompleteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CompleteRequest, len(f.completeCalls))
	copy(out, f.completeCalls)
	return out
}

func (f *FakeProvider) recordedStreams() []StreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]StreamRequest, len(f.streamCalls))
	copy(out, f.streamCalls)
	return out
}

func (f *FakeProvider) recordedCountTokens() []CountTokensRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CountTokensRequest, len(f.countTokensCalls))
	copy(out, f.countTokensCalls)
	return out
}

func (f *FakeProvider) recordedReportCosts() []reportCostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]reportCostCall, len(f.reportCostCalls))
	copy(out, f.reportCostCalls)
	return out
}

// fakeStreamSubscription satisfies [StreamSubscription]. Stop is
// idempotent; a transport-level cause set via markStopped surfaces from
// the FIRST Stop call wrapped in [ErrStreamClosed].
type fakeStreamSubscription struct {
	mu       sync.Mutex
	stopped  bool
	cause    error
	stopOnce sync.Once
}

// isStopped is the lock-protected snapshot the dispatch loop reads.
func (s *fakeStreamSubscription) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// markStopped records `cause` (if non-nil) and flips the stopped flag.
// Idempotent on the cause: only the FIRST cause is preserved.
func (s *fakeStreamSubscription) markStopped(cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.cause = cause
	}
}

// Stop signals the stream to close. Idempotent. Returns nil for clean
// shutdowns; wraps the captured cause in [ErrStreamClosed] when the
// dispatch loop exited with an error.
func (s *fakeStreamSubscription) Stop() error {
	var out error
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		cause := s.cause
		s.mu.Unlock()
		if cause != nil {
			out = wrapStreamClosed(cause)
		}
	})
	return out
}

// wrapStreamClosed wraps `cause` so [errors.Is] matches both the
// sentinel and the original cause.
func wrapStreamClosed(cause error) error {
	return &streamClosedErr{cause: cause}
}

type streamClosedErr struct {
	cause error
}

func (e *streamClosedErr) Error() string {
	if e.cause == nil {
		return ErrStreamClosed.Error()
	}
	return ErrStreamClosed.Error() + ": " + e.cause.Error()
}

func (e *streamClosedErr) Unwrap() []error {
	return []error{ErrStreamClosed, e.cause}
}

// tokenizeBytes returns a deterministic synthetic token count for `s`
// (1 token per 4 bytes, rounded up). Pure helper so CountTokens stays
// stable across runs.
func tokenizeBytes(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// errFakeBoom is a sentinel for tests that need a non-nil non-package
// error (e.g. to surface from a transport-level failure path).
var errFakeBoom = errors.New("fake: boom")
