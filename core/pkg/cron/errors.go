package cron

import "errors"

// ErrInvalidSpec is returned synchronously by [Scheduler.Schedule] when the
// supplied cron `spec` is the empty string. Mismatched-but-non-empty specs
// flow through robfig/cron v3's parser and surface as a wrapped parse error
// (`fmt.Errorf("cron: parse spec %q: %w", spec, err)`) rather than this
// sentinel — `errors.Is(err, ErrInvalidSpec)` is therefore the right check
// only for the synchronous-validation "empty spec" case, not for
// arbitrary parser failures.
var ErrInvalidSpec = errors.New("cron: invalid spec")

// ErrInvalidTopic is returned synchronously by [Scheduler.Schedule] when the
// supplied `topic` is the empty string. The empty topic would land on the
// downstream [LocalPublisher] which itself rejects it; cron rejects it
// up-front so the entry is never registered.
var ErrInvalidTopic = errors.New("cron: invalid topic")

// ErrInvalidFactory is returned synchronously by [Scheduler.Schedule] when
// the supplied [EventFactory] is nil. The scheduler invokes the factory on
// every fire to mint a fresh per-fire event (typically with a fresh
// correlation id); a nil factory would panic on first fire.
var ErrInvalidFactory = errors.New("cron: invalid factory")

// ErrAlreadyStarted is returned by [Scheduler.Start] when the scheduler is
// already running. Start is idempotent in the sense of "once-only"; a
// second call is reported, not silently absorbed, so a caller bug
// (double-Start in a wiring graph) is visible at the call site.
var ErrAlreadyStarted = errors.New("cron: already started")

// ErrAlreadyStopped is returned by [Scheduler.Schedule] and
// [Scheduler.Unschedule] after [Scheduler.Stop] has been called. The
// scheduler is single-use: once Stopped it cannot be restarted; callers
// build a fresh [Scheduler] when they need a new lifecycle.
var ErrAlreadyStopped = errors.New("cron: already stopped")
