package auditsubscriber

import (
	"context"
	"fmt"
	"sync"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Subscriber bridges the [eventbus.Bus] M9 audit topics to the
// [keeperslog.Writer]. Construct via [NewSubscriber]; the zero value
// is not usable. Methods are safe for concurrent use after
// [Subscriber.Start] returns nil — the receiver holds only immutable
// configuration plus a mutex-guarded lifecycle state.
//
// The Subscriber is single-use: [Subscriber.Stop] is a one-way
// transition. Callers needing to restart the bridge construct a
// fresh Subscriber via [NewSubscriber]. (A previously-failed Start
// CAN be retried on the same receiver — the rollback leaves
// `started=false`; only a successful Stop closes the door.)
type Subscriber struct {
	bus    Bus
	writer Writer
	logger Logger // optional

	mu      sync.Mutex
	started bool
	stopped bool
	unsubs  []func()
}

// SubscriberDeps is the typed input bag for [NewSubscriber]. Mirrors
// the `*Deps` constructor pattern across M9 (e.g.
// [approval.ProposerDeps], [localpatch.InstallerDeps]).
type SubscriberDeps struct {
	// Bus is the in-process [eventbus.Bus] (or a hand-rolled fake
	// in tests) the [Subscriber] subscribes against. Required;
	// nil panics in [NewSubscriber].
	Bus Bus

	// Writer is the [keeperslog.Writer] (or a hand-rolled fake in
	// tests) every successful dispatch writes to. Required; nil
	// panics in [NewSubscriber].
	Writer Writer

	// Logger is the optional diagnostic sink for soft failures
	// (unexpected payload type, [Writer.Append] failure,
	// [Bus.Subscribe] rollback during [Subscriber.Start]). Nil
	// is allowed — diagnostics are silently dropped.
	Logger Logger
}

// NewSubscriber constructs a [Subscriber] from `deps`. Panics on
// nil `Bus` / `Writer` (programmer error; a Subscriber without
// either is silently a no-op which would mask a wiring bug).
// Mirrors the panic-on-nil-required-dep discipline of
// [localpatch.NewInstaller] / [approval.NewProposer] /
// [toolshare.NewSharer] / [hostedexport.NewExporter].
func NewSubscriber(deps SubscriberDeps) *Subscriber {
	if deps.Bus == nil {
		panic("auditsubscriber: NewSubscriber: deps.Bus must not be nil")
	}
	if deps.Writer == nil {
		panic("auditsubscriber: NewSubscriber: deps.Writer must not be nil")
	}
	return &Subscriber{
		bus:    deps.Bus,
		writer: deps.Writer,
		logger: deps.Logger,
	}
}

// Start subscribes one handler per binding in [allBindings] against
// the configured [Bus]. Subscriptions are installed in declaration
// order; on any [Bus.Subscribe] failure every prior subscription is
// unsubscribed AND the failing call's returned unsubscribe is also
// invoked, then Start returns the wrapped error (atomic install).
//
// Calling the failing-Subscribe's own unsubscribe callback is
// belt-and-braces: the [Bus.Subscribe] godoc documents the error-
// path return as a no-op closure, but the [Bus] seam is a public
// interface — a future non-`*eventbus.Bus` implementation that
// PARTIALLY registers the handler before returning an error would
// otherwise leave a live subscription behind (iter-1 codex M1
// lesson). The atomic-install guarantee is load-bearing for
// operators triaging audit gaps.
//
// The returned error wraps the underlying [Bus.Subscribe] failure
// via a redacted-`Error()` wrapper ([ErrSubscribe]) so a caller
// that logs `err.Error()` does NOT surface the underlying error's
// VALUE — only the topic name and the underlying error's Go TYPE
// name (iter-1 codex m2 lesson: a future non-`*eventbus.Bus` impl
// may embed credentials / URLs / payload data in its error
// strings). [errors.Is] / [errors.As] against the original
// sentinel still work through `Unwrap`.
//
// Returns [ErrAlreadyStarted] if Start has already returned nil on
// this receiver, [ErrStopped] if [Subscriber.Stop] has been
// called.
//
// Start does NOT block — handlers run on the [eventbus.Bus] per-
// topic worker goroutines. Callers control shutdown via
// [Subscriber.Stop] (and indirectly via [eventbus.Bus.Close]).
func (s *Subscriber) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	if s.started {
		return ErrAlreadyStarted
	}
	unsubs := make([]func(), 0, len(allBindings))
	for _, b := range allBindings {
		unsub, err := s.bus.Subscribe(b.Topic, s.dispatch(b))
		if err != nil {
			// Defensive: invoke the failing call's returned
			// unsub. For *eventbus.Bus this is a no-op closure
			// (documented); for non-eventbus.Bus impls it closes
			// the partial-install hole.
			if unsub != nil {
				unsub()
			}
			// Rollback every prior subscription before returning.
			for _, u := range unsubs {
				u()
			}
			s.logf(
				context.Background(), "auditsubscriber: subscribe failed; rolling back",
				"topic", b.Topic,
				"err_type", fmt.Sprintf("%T", err),
			)
			return &subscribeError{topic: b.Topic, err: err}
		}
		unsubs = append(unsubs, unsub)
	}
	s.unsubs = unsubs
	s.started = true
	return nil
}

// Stop unsubscribes every handler installed by [Subscriber.Start].
// Idempotent: subsequent calls return nil without re-running the
// unsubscribe loop. A Stop call AFTER an unsuccessful Start (which
// has already rolled back its partial subscriptions) is a no-op.
// A Stop call on a never-Started Subscriber transitions the
// receiver to the stopped state (subsequent Start returns
// [ErrStopped]).
//
// Stop does NOT close the [Bus] itself — that is the operator's
// responsibility via [eventbus.Bus.Close] at process-shutdown
// time. Stopping only the Subscriber leaves the bus available for
// other subscribers + publishers.
//
// The `error` return is decorative: the implementation always
// returns nil. The signature is preserved for symmetry with the
// other M9 lifecycle types ([cron.Scheduler.Stop],
// [lifecycle.Manager.Stop]) so a caller that wraps the lifecycle
// generically does not need a one-off helper.
func (s *Subscriber) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	unsubs := s.unsubs
	s.unsubs = nil
	s.stopped = true
	s.mu.Unlock()

	// Run the unsubscribe callbacks OUTSIDE the mutex so a slow
	// [Bus] implementation cannot deadlock against a concurrent
	// `Start` blocked on `s.mu`.
	for _, u := range unsubs {
		u()
	}
	return nil
}

// dispatch wraps a binding into the [Bus.Subscribe] handler shape.
// The closure captures the binding by value so a future mutation
// of `allBindings` after Start would not corrupt in-flight
// dispatches (today the slice is package-level read-only; the
// belt-and-braces value capture is cheap and removes a reasoning
// burden for future readers).
//
// On unexpected payload type: log metadata + return — best-effort
// audit; do NOT panic the topic worker (a panicking handler kills
// the worker goroutine and silently drops every subsequent event
// on that topic per [eventbus.Handler] godoc).
//
// On [Writer.Append] failure: log metadata + return — same best-
// effort discipline. The optional [Logger] surfaces the failure
// to operators; the topic worker keeps draining.
//
// The append-failure log uses `appendCtx` (which carries the
// payload's `CorrelationID`), not the raw bus ctx, so a [Logger]
// implementation that derives trace / correlation metadata from
// ctx records the join key on the exact path operators need to
// debug (iter-1 codex m1 lesson).
//
// The type-mismatch log carries `expected_type` (from the
// binding) and `got_type` (the offending event's `%T`) but
// deliberately omits `err_type` — `typeMismatch` produces a
// `fmt.Errorf("%w: ...")` wrapped sentinel, whose `%T` is always
// `*fmt.wrapError` and carries zero diagnostic signal (iter-1
// critic m10 lesson). The wrapped sentinel exists so a test
// `errors.Is(..., errUnexpectedPayload)` matches; the dispatcher
// does not surface the err value.
func (s *Subscriber) dispatch(b binding) func(ctx context.Context, event any) {
	return func(ctx context.Context, event any) {
		payload, correlationID, err := b.Extract(event)
		if err != nil {
			s.logf(
				ctx, "auditsubscriber: unexpected payload",
				"topic", b.Topic,
				"event_type", b.EventType,
				"expected_type", b.ExpectedType,
				"got_type", fmt.Sprintf("%T", event),
			)
			return
		}
		appendCtx := keeperslog.ContextWithCorrelationID(ctx, correlationID)
		if _, err := s.writer.Append(appendCtx, keeperslog.Event{
			EventType: b.EventType,
			Payload:   payload,
		}); err != nil {
			s.logf(
				appendCtx, "auditsubscriber: append failed",
				"topic", b.Topic,
				"event_type", b.EventType,
				"err_type", fmt.Sprintf("%T", err),
			)
			return
		}
	}
}

// logf forwards a diagnostic entry to the optional [Logger]. Nil-
// logger safe: a Subscriber constructed without [Logger] silently
// drops the call.
func (s *Subscriber) logf(ctx context.Context, msg string, kv ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Log(ctx, msg, kv...)
}

// subscribeError is the redacted-`Error()` wrapper returned by
// [Subscriber.Start] when [Bus.Subscribe] fails. The exported
// `.Error()` string carries the topic name and the underlying
// error's Go TYPE name, NEVER the underlying error's VALUE
// (defensive PII boundary against non-`*eventbus.Bus` [Bus]
// implementations whose error strings may carry credentials,
// URLs, or payload-derived data — iter-1 codex m2 lesson).
//
// `Unwrap()` returns the original error so a caller needing
// classification can `errors.Is` past the redaction.
type subscribeError struct {
	topic string
	err   error
}

// Error implements [error]. Carries topic + err Go TYPE name only.
func (e *subscribeError) Error() string {
	return fmt.Sprintf("%s: topic %q (err type %T)", ErrSubscribe.Error(), e.topic, e.err)
}

// Unwrap preserves the original error for [errors.Is] / [errors.As]
// classification through the redacted wrapper. The chain length is
// kept short (one layer) so a future
// `errors.Is(err, eventbus.ErrInvalidTopic)` still matches.
func (e *subscribeError) Unwrap() error { return e.err }

// Is reports whether `target` is [ErrSubscribe] so callers can
// classify "any subscribe failure" without reaching for the
// concrete `*subscribeError` type assertion.
func (e *subscribeError) Is(target error) bool {
	return target == ErrSubscribe
}
