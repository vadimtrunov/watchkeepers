package publish

import (
	"context"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// defaultBufferSize is the fallback buffer size NewRegistry applies when
// the caller passes a non-positive bufSize. 64 is the same default the
// config layer exposes via KEEP_SUBSCRIBE_BUFFER — declared here so the
// Registry can be used standalone in unit tests without a real Config.
const defaultBufferSize = 64

// subscription is the Registry's per-client bookkeeping. It owns the
// buffered channel, an unsubscribe seam, and a `done` flag that guards
// the channel against double-close from racing Close()/unsubscribe paths.
//
// The channel is bidirectional inside the package (so Registry can close
// it) but handed to callers as a receive-only `<-chan Event`.
type subscription struct {
	claim auth.Claim
	ch    chan Event

	// done guards ch against a double-close. Flipped under Registry.mu
	// in the single code path that calls close(sub.ch).
	done bool
}

// Registry is the in-process fan-out hub. It holds the set of active
// subscriptions keyed by a monotonic id and delivers events whose
// Scope exactly matches a subscription's Claim.Scope.
//
// Registry is safe for concurrent use. All mutating paths (Subscribe,
// remove, Close, Publish's per-subscriber drop) take mu; Publish holds
// mu for the minimal window needed to iterate the active set and issue
// non-blocking sends.
type Registry struct {
	bufSize int

	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]*subscription
	closed bool
}

// NewRegistry constructs a Registry with the given per-subscriber buffer
// size. The heartbeat argument is reserved for the future outbox-polling
// worker (M2.7.e.b); heartbeats on the wire are currently emitted by each
// SSE handler (one time.Ticker per client), matching the design note in
// the TASK ("each subscriber heartbeats its own connection"). Accepting
// it here keeps the public constructor shape stable once a server-side
// heartbeat source appears.
func NewRegistry(bufSize int, _ /* heartbeat */ time.Duration) *Registry {
	if bufSize <= 0 {
		bufSize = defaultBufferSize
	}
	return &Registry{
		bufSize: bufSize,
		subs:    make(map[uint64]*subscription),
	}
}

// Subscribe registers a subscriber for the given claim. It returns a
// receive-only channel that yields matching events and an unsubscribe
// func the caller should invoke (via defer) when the stream ends.
//
// Subscribe also honours ctx: a cancelled ctx causes the subscription
// to be removed and the channel closed automatically, so SSE handlers
// that derive ctx from the request get free lifecycle cleanup on client
// disconnect. If the Registry is already closed the returned channel is
// pre-closed; the unsubscribe func is a safe no-op in that case too.
func (r *Registry) Subscribe(ctx context.Context, claim auth.Claim) (<-chan Event, func()) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	id := r.nextID
	r.nextID++
	sub := &subscription{
		claim: claim,
		ch:    make(chan Event, r.bufSize),
	}
	r.subs[id] = sub
	r.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() { r.removeAndClose(id) })
	}

	// Per-subscription watchdog: ctx cancellation removes the subscriber
	// without the caller having to call unsub explicitly. Safe even when
	// ctx has no deadline: a background ctx simply never fires.
	go func() {
		<-ctx.Done()
		unsub()
	}()

	return sub.ch, unsub
}

// Publish fans ev out to every subscriber whose Claim.Scope equals
// ev.Scope. Matches the AC3 contract: exact string equality, no scope
// hierarchy widening. The send to each matching subscriber is
// non-blocking (select with a default branch); a full buffer drops the
// subscriber and closes its channel (AC4). Publish always returns nil
// on a happy path, including the post-Close no-op case.
func (r *Registry) Publish(ctx context.Context, ev Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	// Snapshot the ids of subscribers to drop so we can close them after
	// the iteration finishes — closing while iterating the map is safe in
	// Go but collecting the victims first keeps the code easy to reason
	// about against the AC4 "drop-on-full, other subscribers unaffected"
	// contract.
	var drop []uint64
	for id, sub := range r.subs {
		if sub.claim.Scope != ev.Scope {
			continue
		}
		select {
		case sub.ch <- ev:
			// delivered
		default:
			drop = append(drop, id)
		}
	}
	for _, id := range drop {
		r.closeLocked(id)
	}
	return nil
}

// Close broadcasts to all active subscriptions and marks the Registry
// closed so subsequent Subscribe/Publish calls are no-ops. Close is
// idempotent: a second call is a no-op and never panics. Server.Run
// calls Close before httpSrv.Shutdown to make in-flight SSE handlers
// return promptly (AC6).
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return
	}
	r.closed = true
	for id := range r.subs {
		r.closeLocked(id)
	}
}

// closeLocked closes one subscription's channel and removes it from the
// map. Caller must hold r.mu. The sub.done flag guards against the
// ctx-cancel goroutine and an explicit Close racing to call close() on
// the same channel twice.
func (r *Registry) closeLocked(id uint64) {
	sub, ok := r.subs[id]
	if !ok {
		return
	}
	if !sub.done {
		sub.done = true
		close(sub.ch)
	}
	delete(r.subs, id)
}

// removeAndClose is the lock-taking variant invoked by the unsubscribe
// func and the ctx-cancel watchdog. Keeping the two variants separate
// (locked/unlocked) avoids any chance of the public Close path nesting
// locks on itself during shutdown.
func (r *Registry) removeAndClose(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked(id)
}
