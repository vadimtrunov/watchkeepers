package wklog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// EnvLevelPrefix is the per-subsystem env-var prefix. The full env-var
// name is EnvLevelPrefix + UPPERCASE(strings.ReplaceAll(subsystem,".","_")).
const EnvLevelPrefix = "WK_LOG_LEVEL_"

// EnvLevelDefault is the env-var consulted when no per-subsystem
// override is set.
const EnvLevelDefault = "WK_LOG_LEVEL"

// AttrSubsystem is the JSON key for the subsystem name attribute.
const AttrSubsystem = "subsystem"

// AttrCorrelationID is the JSON key for the correlation-id attribute.
const AttrCorrelationID = "correlation_id"

// correlationKey is the unexported context-key type. Using a typed key
// prevents accidental collisions with other packages that might stash
// string values under the same key.
type correlationKey struct{}

// WithCorrelationID returns a child context carrying id. An empty id is
// not stored — the resulting context is returned unchanged. Use
// [CorrelationIDFromContext] to retrieve the value downstream.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationKey{}, id)
}

// CorrelationIDFromContext returns the correlation id previously attached
// via [WithCorrelationID]. The second return value is false when no id
// is present so callers can distinguish "absent" from "empty".
func CorrelationIDFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(correlationKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// Options configures a logger built by [New] or [NewWithWriter]. The
// zero value is valid: it resolves the level from the environment and
// writes JSON to stderr.
type Options struct {
	// Level, when non-nil, overrides any environment lookup. Useful for
	// tests and for parents that want to clone with a fixed level.
	Level *slog.Level

	// LevelLookup, when non-nil, replaces os.LookupEnv as the source of
	// level strings. Tests inject a deterministic map; production
	// always uses os.LookupEnv (the zero-value default).
	LevelLookup func(key string) (string, bool)

	// WarnSink is where the one-shot "unrecognised level value" warning
	// is written when a misconfigured env var is encountered. Defaults
	// to os.Stderr; tests inject a buffer.
	WarnSink io.Writer
}

// New returns a [*slog.Logger] configured for the named subsystem. The
// subsystem name is baked into every record as the AttrSubsystem
// attribute; the level is resolved per [Options].
//
// New is safe for concurrent use; the returned logger is goroutine-safe
// per slog's own guarantees.
func New(subsystem string) *slog.Logger {
	return NewWithWriter(subsystem, os.Stderr, Options{})
}

// NewWithWriter is the tests-and-composition variant of [New]: callers
// supply both the io.Writer destination and an Options struct. Production
// code uses [New].
func NewWithWriter(subsystem string, w io.Writer, opts Options) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	lookup := opts.LevelLookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	warnSink := opts.WarnSink
	if warnSink == nil {
		warnSink = os.Stderr
	}

	var level slog.Level
	if opts.Level != nil {
		level = *opts.Level
	} else {
		level = resolveLevel(subsystem, lookup, warnSink)
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	base := slog.New(handler).With(slog.String(AttrSubsystem, subsystem))
	return slog.New(&correlationHandler{inner: base.Handler()})
}

// resolveLevel implements the per-subsystem → global → INFO precedence
// described in the package doc. It also emits a single warning to
// warnSink when an env var is set to a value that does not parse, so
// misconfigurations are loud at boot rather than silently downgraded.
func resolveLevel(subsystem string, lookup func(string) (string, bool), warnSink io.Writer) slog.Level {
	subKey := EnvLevelPrefix + strings.ToUpper(strings.ReplaceAll(subsystem, ".", "_"))
	if raw, ok := lookup(subKey); ok {
		lvl, perr := parseLevel(raw)
		if perr == nil {
			return lvl
		}
		fmt.Fprintf(warnSink,
			"wklog: ignoring %s=%q (subsystem=%q): %v; falling back\n",
			subKey, raw, subsystem, perr)
	}
	if raw, ok := lookup(EnvLevelDefault); ok {
		lvl, perr := parseLevel(raw)
		if perr == nil {
			return lvl
		}
		fmt.Fprintf(warnSink,
			"wklog: ignoring %s=%q: %v; defaulting to INFO\n",
			EnvLevelDefault, raw, perr)
	}
	return slog.LevelInfo
}

// parseLevel maps the case-insensitive level strings the package
// recognises onto slog.Level. The set is intentionally narrow: any
// other input is rejected so a typo surfaces at boot.
func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unrecognised log level %q (want debug|info|warn|error)", raw)
	}
}

// correlationHandler wraps an inner slog.Handler and, on Handle, adds
// the AttrCorrelationID attribute pulled from the record's context if
// present. Implemented as a wrapper rather than a handler-Group attr so
// the id appears as a top-level field in the JSON output (operators
// grepping by correlation_id should not have to know about group
// nesting).
type correlationHandler struct {
	inner slog.Handler
}

// Enabled delegates verbatim — gating happens in the inner JSON handler
// using the level resolved at construction time.
func (h *correlationHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds AttrCorrelationID to the record when present in ctx, then
// forwards. The record is shallow-copied via AddAttrs to honour slog's
// "do not mutate the Record passed in" contract.
func (h *correlationHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := CorrelationIDFromContext(ctx); ok {
		r.AddAttrs(slog.String(AttrCorrelationID, id))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a wrapper around the inner handler's WithAttrs so
// the correlation-id step continues to apply.
func (h *correlationHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &correlationHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup likewise wraps the inner handler.
func (h *correlationHandler) WithGroup(name string) slog.Handler {
	return &correlationHandler{inner: h.inner.WithGroup(name)}
}

// SetAsDefault installs logger as slog's process-wide default. The
// caller MUST own that decision — wklog does not call SetAsDefault on
// its own.
//
// Returns a restore func that puts the previous default back; tests use
// this for hermetic setup.
func SetAsDefault(logger *slog.Logger) (restore func()) {
	prev := slog.Default()
	slog.SetDefault(logger)
	return func() {
		slog.SetDefault(prev)
	}
}
