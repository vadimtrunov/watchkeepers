package auditsubscriber

import (
	"context"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Bus is the minimal subset of the [eventbus.Bus] surface the
// [Subscriber] consumes. `*eventbus.Bus` satisfies this interface
// directly: its
// `Subscribe(topic string, handler eventbus.Handler) (func(), error)`
// signature matches by underlying-func-signature equality —
// `eventbus.Handler` is currently a type alias
// (`type Handler = func(ctx context.Context, event any)`), so the
// signatures are interchangeable at the type-system level. The
// compile-time assertion in `ports_test.go` pins satisfaction; if a
// future eventbus refactor promoted `Handler` to a defined type
// (`type Handler func(...)`), the assertion would fail at build
// time before any runtime surprise.
//
// Per-call seam shape so tests can substitute a hand-rolled fake
// without standing up a real bus.
type Bus interface {
	// Subscribe registers `handler` for `topic` and returns a single-
	// shot `unsubscribe` callback. Subsequent topic publishes invoke
	// the handler in registration order. The callback is idempotent.
	// On error the returned unsubscribe is a no-op closure so callers
	// can `defer unsubscribe()` without an explicit nil check (mirror
	// [eventbus.Bus.Subscribe]'s contract).
	Subscribe(topic string, handler func(ctx context.Context, event any)) (func(), error)
}

// Writer is the minimal subset of the [keeperslog.Writer] surface
// the [Subscriber] consumes. `*keeperslog.Writer` satisfies this
// interface directly. The compile-time assertion lives in
// `ports_test.go`.
//
// Mirrors the `LocalKeepClient` / `LocalPublisher` one-way import-
// cycle-break pattern from M3-M9: keeperslog is concrete in
// production wiring; the subscriber holds the seam.
type Writer interface {
	// Append composes the structured envelope and ships it to the
	// underlying [keeperslog.LocalKeepClient]. Returns the persisted
	// row id on success. Errors are wrapped with the `keeperslog:`
	// prefix; the underlying keepclient sentinels remain matchable
	// via [errors.Is]. The subscriber treats every error as a soft
	// failure: it LOGS metadata (no payload, no err value) via the
	// optional [Logger] and DROPS the event.
	Append(ctx context.Context, event keeperslog.Event) (string, error)
}

// Logger is the optional diagnostic sink the [Subscriber] uses to
// report soft failures (unexpected payload type, [Writer.Append]
// failure, [Bus.Subscribe] retry / rollback). Shape mirrors
// [keeperslog.Logger] so callers can reuse one structured logger
// across the audit subsystem.
//
// IMPORTANT (redaction discipline): implementations MUST NOT log
// payload bodies. The subscriber never passes the payload through
// the logger; only topic name, event-type label, expected /
// actual Go type names, and error TYPE (never error VALUE — error
// strings may carry credentials, see [toolregistry.SourceFailed]
// doc-block) appear in log entries.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}
