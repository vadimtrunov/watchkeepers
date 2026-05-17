package audit

import "errors"

// ErrInvalidEvent is returned synchronously by the [Writer.Emit*]
// methods when a supplied event struct is malformed (zero-valued
// required field). The underlying [Appender] is NOT touched when this
// sentinel is returned. Matchable via [errors.Is].
//
// Mirrors the synchronous-validation pattern from
// [keeperslog.ErrInvalidEvent], [k2k.ErrEmptyOrganization], and
// [capability.ErrInvalidScope]: fail fast at the call site so the
// downstream transport never sees an obviously bad request.
var ErrInvalidEvent = errors.New("audit: invalid event")
