package eventbus

import "errors"

// ErrClosed is returned by [Bus.Publish] and [Bus.Subscribe] after [Bus.Close]
// has been called. Subsequent [Bus.Close] calls return nil (the close is
// idempotent), but Publish/Subscribe never resurrect — the bus is single-use.
// Callers can `errors.Is(err, ErrClosed)` to distinguish a closed-bus refusal
// from a transient publish error.
var ErrClosed = errors.New("eventbus: closed")

// ErrInvalidTopic is returned synchronously by [Bus.Publish] and
// [Bus.Subscribe] when the supplied topic is the empty string. The empty
// topic is reserved as a sentinel in the internal map and using it would
// also be a programmer mistake — every topic name is a deliberate identifier
// shared between publishers and subscribers.
var ErrInvalidTopic = errors.New("eventbus: invalid topic")

// ErrInvalidHandler is returned synchronously by [Bus.Subscribe] when the
// supplied handler is nil. The bus dispatches by calling the handler value
// directly; a nil handler would panic on the worker goroutine and bring down
// the whole topic.
var ErrInvalidHandler = errors.New("eventbus: invalid handler")
