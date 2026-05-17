// eventbus_memory.go ships the in-memory [EventBus] implementation. The
// M1.3.c roadmap pins an in-memory adapter alongside the Postgres
// LISTEN/NOTIFY adapter so test wiring and dev / smoke loops can
// exercise the Publish / Subscribe surface without a Postgres
// dependency. Production wiring uses [PostgresEventBus] from
// `eventbus_postgres.go`.
//
// Implementation shape:
//
//   - One mutex guards every map / slice. The fast paths (Publish
//     fan-out + Subscribe registration) hold the mutex for the duration
//     of the operation; the slow path (per-subscription delivery
//     goroutine) holds it only across the snapshot-then-deliver step
//     boundary.
//   - Every subscription has an internal `events` queue (unbounded slice
//     under the mutex), a `notify` cond-var equivalent (`signal chan
//     struct{}{}` with a 1-slot buffer), and a `done` channel. A
//     dedicated goroutine per subscription drains the queue onto the
//     caller-facing buffered channel.
//   - The caller-facing channel is bounded (default 16 slots, configurable
//     via [WithMemoryBufferSize]). The delivery goroutine non-blocking-
//     sends onto it; when the channel is full the event is DROPPED and
//     the bus's atomic `dropped` counter increments.
//   - ctx cancellation + CancelFunc invocation both close the
//     caller-facing channel and stop the per-subscription goroutine.
//     The goroutine-stop path is idempotent under the `sync.Once`
//     `closeOnce` guard so a double-cancel does not panic on "close of
//     closed channel".
//
// The shape mirrors the M1.1.c lifecycle in-memory adapter's
// "snapshot-then-act" discipline + the [k2k.MemoryRepository]
// cond-var-on-WaitForReply pattern.

package peer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// defaultMemoryBufferSize is the per-subscription bounded buffer the
// [MemoryEventBus] uses when [WithMemoryBufferSize] is not set. 16 is a
// pragmatic default — large enough to absorb a publish burst within a
// single GC pause, small enough that a wedged consumer cannot dominate
// the heap.
const defaultMemoryBufferSize = 16

// MemoryEventBus is the in-memory [EventBus] adapter. Construct via
// [NewMemoryEventBus]; the zero value is NOT usable.
//
// Concurrency: every exported method is safe for concurrent use across
// goroutines.
type MemoryEventBus struct {
	now func() time.Time

	bufSize int

	mu          sync.Mutex
	nextSubID   uint64
	subscribers map[uint64]*memorySubscription
	closed      bool

	dropped atomic.Uint64
}

// MemoryEventBusOption configures a [MemoryEventBus]. Constructed via
// the `WithXxx` helpers.
type MemoryEventBusOption func(*MemoryEventBus)

// WithMemoryBufferSize overrides the per-subscription bounded buffer
// size. The supplied size must be positive; a non-positive value is
// silently coerced back to [defaultMemoryBufferSize] (mirrors the
// "ignore degenerate option" discipline from [k2k.WithPollInterval]).
func WithMemoryBufferSize(size int) MemoryEventBusOption {
	return func(b *MemoryEventBus) {
		if size > 0 {
			b.bufSize = size
		}
	}
}

// WithMemoryClock overrides the wall-clock the bus stamps onto
// `CreatedAt` for in-memory events whose [Event.CreatedAt] is the zero
// value. The default is [time.Now].
func WithMemoryClock(now func() time.Time) MemoryEventBusOption {
	return func(b *MemoryEventBus) {
		if now != nil {
			b.now = now
		}
	}
}

// NewMemoryEventBus returns a configured [MemoryEventBus] ready for
// Publish / Subscribe traffic.
func NewMemoryEventBus(opts ...MemoryEventBusOption) *MemoryEventBus {
	b := &MemoryEventBus{
		now:         time.Now,
		bufSize:     defaultMemoryBufferSize,
		subscribers: make(map[uint64]*memorySubscription),
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// memorySubscription holds the per-subscription delivery channel, the
// subscriber's filter, and the goroutine-stop machinery.
type memorySubscription struct {
	id     uint64
	bus    *MemoryEventBus
	filter SubscribeFilter

	// outMu serialises sends + closes on `out`. A `Publish` fan-out
	// takes the read-lock for the duration of the non-blocking
	// channel-send; a `close()` takes the write-lock for the duration of
	// the channel-close. This composes correctly under the Go memory
	// model (a send-to-closed panic is impossible) and lets concurrent
	// Publish fan-outs into different subscriptions run in parallel.
	outMu  sync.RWMutex
	out    chan Event
	closed bool

	// closeOnce guards the `out` channel close so ctx-cancel + CancelFunc
	// + a `Close()` race compose under a single close.
	closeOnce sync.Once
	done      chan struct{}
}

// trySend non-blocking-delivers `event` onto the subscription's
// channel. Returns true if delivered, false if the buffer was full OR
// the subscription has been closed (caller bumps the drop counter
// either way). Holds the per-subscription RLock for the duration of the
// send so a concurrent `close()` (which holds the WLock) cannot race.
func (s *memorySubscription) trySend(event Event) bool {
	s.outMu.RLock()
	defer s.outMu.RUnlock()
	if s.closed {
		return false
	}
	select {
	case s.out <- event:
		return true
	default:
		return false
	}
}

// matches reports whether `event` satisfies the subscription's filter.
// Called under [MemoryEventBus.mu] so the held filter slice cannot
// mutate mid-match.
func (s *memorySubscription) matches(event Event) bool {
	if event.OrganizationID != s.filter.OrganizationID {
		return false
	}
	if s.filter.TargetWatchkeeperID != "" && event.WatchkeeperID != s.filter.TargetWatchkeeperID {
		return false
	}
	if len(s.filter.EventTypes) == 0 {
		return true
	}
	for _, et := range s.filter.EventTypes {
		if event.EventType == et {
			return true
		}
	}
	return false
}

// Publish implements [EventBus.Publish]. Validates, defensively
// deep-copies the payload, fan-outs to every matching subscriber under
// the bus mutex.
func (b *MemoryEventBus) Publish(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.ID == uuid.Nil {
		return ErrInvalidEventID
	}
	if event.OrganizationID == uuid.Nil {
		return ErrInvalidOrganizationID
	}
	if strings.TrimSpace(event.WatchkeeperID) == "" {
		return ErrEmptyWatchkeeperID
	}
	if strings.TrimSpace(event.EventType) == "" {
		return ErrEmptyEventType
	}

	// Defensive deep-copy of the payload BEFORE persistence — the bus
	// holds the payload across the fan-out, and a caller mutating the
	// slice after Publish returns must not bleed.
	payload := clonePayload(event.Payload)
	if event.CreatedAt.IsZero() {
		event.CreatedAt = b.now().UTC()
	}
	event.Payload = payload

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("peer: memory event bus closed")
	}
	// Snapshot the slice of subscribers we will deliver to so the
	// non-blocking send below does not hold the mutex across a
	// per-subscriber channel operation (which would serialise the
	// fan-out).
	targets := make([]*memorySubscription, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		if s.matches(event) {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()

	for _, s := range targets {
		// Per-subscriber defensive deep-copy of the payload so a
		// consumer mutating the slice cannot bleed across subscribers.
		// The shared `payload` slice is otherwise immutable on the
		// publish side (the bus does not mutate it), so per-subscriber
		// copy is the only boundary that matters.
		deliver := event
		deliver.Payload = clonePayload(payload)
		if !s.trySend(deliver) {
			// Slow consumer: bounded buffer full OR subscription closed
			// between snapshot and send. Bump the bus-wide counter. The
			// dropped delivery is NOT retried — the consumer's contract
			// is "I read fast enough to drain my buffer, or I lose
			// events".
			b.dropped.Add(1)
		}
	}
	return nil
}

// Subscribe implements [EventBus.Subscribe]. Validates the filter,
// allocates the subscription, registers it under the bus mutex, and
// returns the caller-facing channel + CancelFunc.
func (b *MemoryEventBus) Subscribe(ctx context.Context, filter SubscribeFilter) (<-chan Event, CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if filter.OrganizationID == uuid.Nil {
		return nil, nil, ErrInvalidOrganizationID
	}

	// Defensive deep-copy of the event-type filter so caller-side
	// mutation cannot bleed into the held subscription's matcher.
	copied := SubscribeFilter{
		OrganizationID:      filter.OrganizationID,
		TargetWatchkeeperID: filter.TargetWatchkeeperID,
	}
	if len(filter.EventTypes) > 0 {
		copied.EventTypes = make([]string, len(filter.EventTypes))
		copy(copied.EventTypes, filter.EventTypes)
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("peer: memory event bus closed")
	}
	b.nextSubID++
	sub := &memorySubscription{
		id:     b.nextSubID,
		bus:    b,
		filter: copied,
		out:    make(chan Event, b.bufSize),
		done:   make(chan struct{}),
	}
	b.subscribers[sub.id] = sub
	b.mu.Unlock()

	// Cancel = idempotent shutdown. Closes the caller-facing channel
	// once + removes the subscription from the bus's map.
	cancel := func() {
		sub.close()
	}

	// Spawn a watchdog goroutine that closes the subscription when
	// ctx cancels. The goroutine exits when either ctx cancels OR
	// CancelFunc closes `done` first. Either way the closeOnce inside
	// `close()` guarantees a single channel close.
	go func() {
		select {
		case <-ctx.Done():
			sub.close()
		case <-sub.done:
		}
	}()

	return sub.out, cancel, nil
}

// close is the idempotent subscription teardown. Closes the
// caller-facing channel, removes the subscription from the bus, and
// signals the watchdog goroutine to exit.
func (s *memorySubscription) close() {
	s.closeOnce.Do(func() {
		s.bus.mu.Lock()
		delete(s.bus.subscribers, s.id)
		s.bus.mu.Unlock()
		close(s.done)
		// Take the write-lock so a concurrent Publish fan-out's
		// `trySend` cannot race with the close.
		s.outMu.Lock()
		s.closed = true
		close(s.out)
		s.outMu.Unlock()
	})
}

// DroppedEvents implements [EventBus.DroppedEvents].
func (b *MemoryEventBus) DroppedEvents() uint64 {
	return b.dropped.Load()
}

// Close tears the bus down: closes every active subscription and
// refuses new Publish / Subscribe calls. Idempotent.
func (b *MemoryEventBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	// Snapshot the subscribers slice so we can call close() outside the
	// bus mutex (close() reacquires the mutex to delete from the map).
	subs := make([]*memorySubscription, 0, len(b.subscribers))
	for _, s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
	return nil
}

// clonePayload returns a defensive deep-copy of `in` (nil-safe).
// Hoisted out of the inline Publish path so the helper is shared with
// the Postgres adapter's payload-copy on read.
func clonePayload(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
