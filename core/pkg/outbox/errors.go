package outbox

import "errors"

// ErrAlreadyStarted is returned by [Consumer.Start] when the consumer is
// already running. Start is idempotent in the sense of "once-only"; a
// second call is reported, not silently absorbed, so a caller bug
// (double-Start in a wiring graph) is visible at the call site.
// Mirrors [cron.ErrAlreadyStarted] / [lifecycle] state-machine
// discipline.
var ErrAlreadyStarted = errors.New("outbox: already started")

// ErrAlreadyStopped is returned by [Consumer.Start] after [Consumer.Stop]
// has been called. The consumer is single-use: once Stopped it cannot be
// restarted; callers build a fresh [Consumer] when they need a new
// lifecycle.
var ErrAlreadyStopped = errors.New("outbox: already stopped")

// ErrNotStarted is returned by [Consumer.Stop] when called on a
// consumer that was never [Consumer.Start]ed. Surfaces the
// programmer-error condition rather than silently no-oping (a Stop on a
// never-Started consumer usually indicates a wiring bug — log it and
// move on, but make it visible at the call site).
var ErrNotStarted = errors.New("outbox: not started")

// ErrPublishExhausted is returned via the optional [Logger] when the
// consumer has exhausted its per-event publish-retry budget for a single
// event without a successful bus emit. The event is dropped from the
// in-flight queue (no ack semantics exist at this layer); on the next
// reconnect it MAY be redelivered by the server, in which case the
// idempotency cache decides whether to re-emit.
//
// Matchable via [errors.Is]; the wrap chain carries the last
// underlying eventbus error.
var ErrPublishExhausted = errors.New("outbox: publish retries exhausted")
