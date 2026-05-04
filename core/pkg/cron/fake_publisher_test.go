package cron

import (
	"context"
	"sync"
)

// fakeCall is one entry in [fakePublisher]'s call-sequence log. Records
// the topic and the opaque event so per-fire assertions can both count
// and inspect payloads (used by TestScheduler_TopicAndEventDelivered to
// assert the factory's tick value flows through unmodified).
type fakeCall struct {
	Topic string
	Event any
}

// fakePublisher is the hand-rolled [LocalPublisher] stand-in used by the
// cron test suite. Mirrors the M2b.5 / M3.2.b hand-rolled-fake pattern:
// injectable error field, recorded call sequence, no mocking lib.
//
// `injectErr`, when non-nil, is returned from every Publish call. The
// recorded call log still gets the entry — tests that count fires under
// "publish always errors" rely on this.
//
// `injectBlock`, when non-nil, is received from at the start of every
// Publish call so tests can hold Publish in flight while they assert on
// drain semantics. The TestScheduler_StopDrains test sends a single
// value into this channel after Stop is called to release the in-flight
// publish; the channel is buffered to 1 so the goroutine that injects
// can both pre-load the release and observe Stop returning before the
// publisher unblocks.
type fakePublisher struct {
	mu          sync.Mutex
	calls       []fakeCall
	injectErr   error
	injectBlock chan struct{}
}

// Publish records the call and returns injectErr (or nil). When
// injectBlock is non-nil the call additionally receives from the channel
// after recording but before returning, so a test can stage "publish in
// flight" by sending one value into the channel (or by leaving it empty
// to hold the publish indefinitely).
func (f *fakePublisher) Publish(_ context.Context, topic string, event any) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Topic: topic, Event: event})
	block := f.injectBlock
	err := f.injectErr
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

// recordedCalls returns a defensive copy of the call log so a later
// assertion does not race a still-running fire goroutine appending to
// the underlying slice.
func (f *fakePublisher) recordedCalls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// callCount returns the total number of recorded calls.
func (f *fakePublisher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// callsForTopic returns a defensive copy of just the calls whose Topic
// equals `topic`. Used by TestScheduler_MultipleEntriesIndependent to
// assert two topic streams stay independent.
func (f *fakePublisher) callsForTopic(topic string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Topic == topic {
			out = append(out, c)
		}
	}
	return out
}

// fakeLogger is the hand-rolled [Logger] stand-in used by tests that
// need to assert a log call happened (publish-error / panic-recovery
// paths). Records every (msg, kv...) call under a mutex.
type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

type fakeLogEntry struct {
	Msg string
	KV  []any
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, fakeLogEntry{Msg: msg, KV: cp})
}

func (l *fakeLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
