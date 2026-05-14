package auditsubscriber

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// errFakeSubscribe is the sentinel a [fakeBus] returns when its
// configured per-topic failure plan triggers a [Bus.Subscribe]
// failure. Used by the rollback test to assert every prior
// subscription's unsubscribe callback fired.
var errFakeSubscribe = errors.New("auditsubscriber: fake subscribe failure")

// errFakeAppend is the sentinel a [fakeWriter] returns when
// configured to fail the next [Writer.Append] call. Used by the
// append-failure test to assert the dispatcher LOGS + DROPS rather
// than crashing or retrying.
var errFakeAppend = errors.New("auditsubscriber: fake append failure")

// fakeBus is a hand-rolled [Bus] for unit tests. Records every
// subscription (in registration order) so the test can deliver
// events to a specific topic AND assert per-topic unsubscribe
// counts. Configurable per-topic failure plan exercises the
// rollback path on [Subscriber.Start].
type fakeBus struct {
	mu sync.Mutex

	// failOnTopic, when non-empty, causes Subscribe to return
	// [errFakeSubscribe] for that exact topic; every prior topic
	// in the same Start cycle has its unsubscribe counted in
	// unsubCount.
	failOnTopic string

	// subs is the ordered list of installed handlers, one entry
	// per successful Subscribe call. Test code looks up by topic
	// via topicHandler.
	subs []subEntry

	// unsubCount counts the number of times an unsubscribe
	// callback has fired (including idempotent re-fires; tests
	// that care assert via the per-entry sub.unsubCount).
	unsubCount int

	// failingUnsubCount counts how many times the no-op closure
	// returned on the error path has been invoked. The
	// [Subscriber.Start] rollback MUST call this on the failing
	// row (iter-1 codex M1).
	failingUnsubCount int
}

// subEntry is a single Subscribe registration record. The handler
// closure captures the dispatcher; tests dispatch via topicHandler.
type subEntry struct {
	topic   string
	handler func(ctx context.Context, event any)
	// unsubCount is incremented every time this entry's
	// unsubscribe callback fires (idempotent re-fires are
	// counted; tests asserting exact-once gate on the first
	// increment).
	unsubCount int
}

// Subscribe records the binding and returns a per-entry
// unsubscribe callback that the [Subscriber.Stop] rollback /
// teardown path consumes.
func (f *fakeBus) Subscribe(topic string, handler func(ctx context.Context, event any)) (func(), error) {
	f.mu.Lock()
	if topic == f.failOnTopic {
		// failingUnsubCount is bumped if the caller invokes the
		// no-op closure we return on the error path. Pins the
		// iter-1 codex M1 defensive-unsub assertion: Start MUST
		// call the failing Subscribe's returned unsubscribe so a
		// non-eventbus.Bus impl that PARTIALLY registered the
		// handler does not leak.
		f.mu.Unlock()
		return func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.failingUnsubCount++
		}, errFakeSubscribe
	}
	idx := len(f.subs)
	f.subs = append(f.subs, subEntry{topic: topic, handler: handler})
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.subs[idx].unsubCount++
		f.unsubCount++
	}, nil
}

// clearFailOnTopic erases the per-topic failure plan, enabling a
// subsequent successful Start on the same Subscriber (iter-1
// critic m6: retry-after-failure pin).
func (f *fakeBus) clearFailOnTopic() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnTopic = ""
}

// perEntryUnsubCounts returns a snapshot of every captured
// subscription's unsubscribe-fire count, in registration order
// (iter-1 critic m4: per-entry exact-once rollback assertion).
func (f *fakeBus) perEntryUnsubCounts() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, 0, len(f.subs))
	for _, s := range f.subs {
		out = append(out, s.unsubCount)
	}
	return out
}

// topicHandler returns the registered handler for `topic` (the
// first one if multiple — production code only registers one per
// topic per Subscriber so the helper is unambiguous in the test
// context). Fatal-fails the test on miss.
func (f *fakeBus) topicHandler(topic string) func(ctx context.Context, event any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		if s.topic == topic {
			return s.handler
		}
	}
	return nil
}

// snapshotTopics returns the ordered topic list installed so far.
func (f *fakeBus) snapshotTopics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.subs))
	for _, s := range f.subs {
		out = append(out, s.topic)
	}
	return out
}

// fakeWriter is a hand-rolled [Writer] capturing every Append call
// for assertion. Supports a one-shot failure mode for the soft-
// failure dispatch test.
type fakeWriter struct {
	mu sync.Mutex

	// failNext, when true, causes the next Append to return
	// [errFakeAppend] AND flip failNext back to false (one-shot).
	failNext bool

	// failAlways, when true, causes EVERY Append to return
	// [errFakeAppend]. Wins over failNext.
	failAlways bool

	// events captures every successful (and failing) Append call's
	// (ctx, event) pair. Tests inspect via snapshotEvents.
	events []writerCall
}

// writerCall is a captured Append invocation.
type writerCall struct {
	ctx           context.Context //nolint:containedctx // captured for assertion of ctx-carried correlation id.
	event         keeperslog.Event
	correlationID string // captured from ctx at Append-call time for ergonomics.
	returnedErr   error
}

// Append captures the call AND optionally fails per the configured
// plan. Always returns an opaque row id on success so the
// dispatcher's drop-the-id behaviour is exercised.
func (f *fakeWriter) Append(ctx context.Context, event keeperslog.Event) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	corr, _ := keeperslog.CorrelationIDFromContext(ctx)
	call := writerCall{ctx: ctx, event: event, correlationID: corr}
	if f.failAlways || f.failNext {
		f.failNext = false
		call.returnedErr = errFakeAppend
		f.events = append(f.events, call)
		return "", errFakeAppend
	}
	f.events = append(f.events, call)
	return "row-" + event.EventType, nil
}

// snapshotEvents returns a copy of the captured Append history.
func (f *fakeWriter) snapshotEvents() []writerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]writerCall, len(f.events))
	copy(out, f.events)
	return out
}

// fakeLogger captures every Log call for assertion. Used to verify
// the soft-failure diagnostic path AND the redaction discipline
// (no payload bodies in log entries).
type fakeLogger struct {
	mu sync.Mutex

	// entries is the ordered list of every captured Log call.
	entries []logEntry
}

// logEntry is a single Log invocation.
type logEntry struct {
	msg string
	kv  []any
}

// Log captures the call.
func (f *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Copy the kv slice so a later caller-side mutation cannot
	// rewrite the captured history (the dispatcher always passes
	// a fresh kv list; this defence is belt-and-braces).
	dup := make([]any, len(kv))
	copy(dup, kv)
	f.entries = append(f.entries, logEntry{msg: msg, kv: dup})
}

// snapshotEntries returns a copy of the captured log history.
func (f *fakeLogger) snapshotEntries() []logEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]logEntry, len(f.entries))
	copy(out, f.entries)
	return out
}

// dump flattens every captured log entry into a single string for
// substring-contain assertions (PII-canary tests). The format is
// intentionally `%v` to surface every kv pair's value verbatim —
// the goal is to catch any path that leaks a payload field into
// the diagnostic surface.
func (f *fakeLogger) dump() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ""
	for _, e := range f.entries {
		out += e.msg + " "
		for _, v := range e.kv {
			out += fmt.Sprintf("%v ", v)
		}
		out += "\n"
	}
	return out
}
