package auditsubscriber

import "errors"

// ErrAlreadyStarted is returned by [Subscriber.Start] when the
// receiver has already transitioned to the started state via a prior
// successful Start call. Mirrors the lifecycle-discipline of
// [cron.Scheduler] / [lifecycle.Manager].
var ErrAlreadyStarted = errors.New("auditsubscriber: already started")

// ErrStopped is returned by [Subscriber.Start] when [Subscriber.Stop]
// has already been called on the receiver. The Subscriber is single-
// use: once stopped it cannot be restarted; callers spin up a fresh
// [Subscriber] via [NewSubscriber].
var ErrStopped = errors.New("auditsubscriber: stopped")

// ErrSubscribe is the wrapped sentinel for any [Bus.Subscribe]
// failure surfaced through [Subscriber.Start]. The Start error's
// `.Error()` string is DELIBERATELY redacted — it carries the topic
// name and the underlying error's TYPE name, but NEVER the
// underlying error's VALUE. This defends against a future non-
// `*eventbus.Bus` [Bus] implementation whose error strings might
// embed credentials, URLs, or payload-derived data (iter-1 codex m2
// lesson). The original error is preserved via `Unwrap()` so
// callers needing classification can `errors.Is` past the redacted
// wrapper.
var ErrSubscribe = errors.New("auditsubscriber: subscribe failed")

// errUnexpectedPayload is the internal sentinel wrapped by the
// [extractor] helpers when a bus envelope carries neither the
// expected concrete type nor a non-nil pointer to it. The sentinel
// is unexported because no caller path observes it — the dispatcher
// LOGS metadata (binding `ExpectedType` + `%T` of the offending
// event) and DROPS the event. The wrapper is retained so a test
// `errors.Is(err, errUnexpectedPayload)` assertion can verify the
// extractor's chain (iter-1 critic M1 lesson: an unobservable
// exported sentinel is dead API surface).
var errUnexpectedPayload = errors.New("auditsubscriber: unexpected payload type")
