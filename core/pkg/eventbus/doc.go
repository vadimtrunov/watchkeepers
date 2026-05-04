// Package eventbus is the in-process pub/sub event bus that ROADMAP §M3
// builds on (lifecycle, cron, keeperslog, outbox).
//
// The package exposes a [Bus] type constructed via functional options
// ([Option]) plus three sentinel errors ([ErrClosed], [ErrInvalidTopic],
// [ErrInvalidHandler]). Subscribers register a [Handler] for a named
// string topic; publishers push events of any type onto a topic and the
// bus fans them out to every subscriber sequentially in registration
// order.
//
// # Ordering
//
// Each topic is served by exactly one worker goroutine. Events flow
// through a buffered channel — the topic queue — and the worker pops
// them one at a time and dispatches to a subscriber-list snapshot taken
// per envelope. This guarantees per-topic ordered delivery: events
// published to one topic are delivered to each subscriber in publish
// order. Across topics no order is preserved (each topic is independent).
//
// "Publish order" with concurrent publishers means **enqueue order**, not
// call-time order: when two goroutines call Publish on the same topic
// simultaneously, the bus serialises their sends through the topic's
// channel, and the worker observes whichever entered the channel first.
// Per-publisher ordering (a single goroutine's sequential publishes) is
// always preserved.
//
// # Backpressure
//
// Each per-topic channel is bounded ([WithTopicBufferSize], default 64).
// When the buffer is full, [Bus.Publish] blocks until either a worker
// drains a slot, the caller's context is cancelled, or [Bus.Close] runs.
// On context cancellation Publish returns
// `fmt.Errorf("eventbus: publish: %w", ctx.Err())`; on Close it returns
// [ErrClosed]. The bus never silently drops an event.
//
// # Shutdown
//
// [Bus.Close] is idempotent. The first call flips a `closed` flag (so new
// Publish/Subscribe return [ErrClosed]), wakes any backpressured publishers,
// drains every per-topic queue (each worker finishes dispatching its buffered
// envelopes), and waits for every worker goroutine to exit before
// returning. Under `go test -race` the bus passes a goroutine-baseline
// leak check.
//
// # Out of scope (deferred to later M3 milestones)
//
//   - Correlation-id propagation — M3.6 (`keeperslog`).
//   - Durable replay / persistence — M3.7 (`outbox`).
//   - Cross-process / cross-binary delivery — out of scope (in-process only).
//   - Typed event schemas — M3.6 wraps with typed shapes; the bus itself
//     speaks `any`.
//   - Handler panic recovery — handlers MUST NOT panic in v1; a panicking
//     handler crashes its topic worker. Future revisions may add bounded
//     recovery.
package eventbus
