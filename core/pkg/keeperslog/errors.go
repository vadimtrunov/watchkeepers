package keeperslog

import "errors"

// ErrInvalidEvent is returned synchronously by [Writer.Append] when the
// supplied [Event] is malformed (currently: empty EventType). The
// keepclient is NOT touched when this sentinel is returned. Matchable
// via [errors.Is].
//
// Mirrors the synchronous-validation pattern from M3.2.b's
// [lifecycle.ErrInvalidParams] and M3.5's [capability.ErrInvalidScope]:
// fail fast at the call site so the underlying transport never sees an
// obviously bad request.
var ErrInvalidEvent = errors.New("keeperslog: invalid event")
