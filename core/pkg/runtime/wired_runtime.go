package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// WiredRuntimeOption configures a [WiredRuntime] at construction time.
// Pass options to [NewWiredRuntime]; later options override earlier ones
// for the same field.
type WiredRuntimeOption func(*WiredRuntime)

// WithToolErrorReflector wires a [ToolErrorReflector] onto the
// returned [*WiredRuntime]. When set, every [WiredRuntime.InvokeTool]
// error return path calls the reflector synchronously BEFORE returning
// the error to the caller. The reflector is best-effort: any error it
// returns is logged via the wired [WithLogger] sink and does NOT
// replace the original tool error returned to the caller.
//
// A nil reflector is a no-op so callers can always pass through
// whatever they have. Without this option the runtime behaves
// identically to the underlying [AgentRuntime] — pinning the
// regression guard documented in the M5.6.b test plan.
func WithToolErrorReflector(r *ToolErrorReflector) WiredRuntimeOption {
	return func(w *WiredRuntime) {
		if r != nil {
			w.reflector = r
		}
	}
}

// WithLogger wires a structured [*slog.Logger] onto the returned
// [*WiredRuntime]. The wired logger is only consulted by the
// reflector best-effort path: a Reflect failure emits one
// `runtime: tool-error reflector failed` entry carrying the tool
// name, error class, and the reflector's err_type. Callers passing
// nil get the package default ([slog.Default]).
func WithLogger(l *slog.Logger) WiredRuntimeOption {
	return func(w *WiredRuntime) {
		if l != nil {
			w.logger = l
		}
	}
}

// WithAgentID wires the agent identifier the [WiredRuntime] forwards
// to [ToolErrorReflector.Reflect] on every tool-error path. Phase 1
// callers populate it from the manifest the underlying [AgentRuntime]
// was started with — there is no portable way to pull it back out of
// an opaque [Runtime] handle. M6's Watchmaster session-aware
// dispatch will add a more structured plumbing path; for M5.6.b a
// single agent-id per WiredRuntime is sufficient.
func WithAgentID(agentID string) WiredRuntimeOption {
	return func(w *WiredRuntime) {
		w.agentID = agentID
	}
}

// WiredRuntime is a thin decorator over an [AgentRuntime] that adds
// the M5.6.b auto-reflection hook to the [AgentRuntime.InvokeTool]
// error path. It satisfies [AgentRuntime] itself and forwards the
// other four lifecycle methods (Start / SendMessage / Subscribe /
// Terminate) to the inner runtime verbatim.
//
// Why a decorator: the runtime package's [AgentRuntime] interface is
// satisfied today only by test fakes (the production
// Claude-Code TS-harness runtime lands later in M5.3+); a decorator
// lets the M5.6.b reflection cycle ride on top of any existing or
// future [AgentRuntime] without modifying its source. The same
// pattern lets a concrete runtime opt out by simply not wrapping
// itself in a WiredRuntime.
//
// Concurrency: WiredRuntime is safe for concurrent use as long as the
// underlying [AgentRuntime] is. The decorator itself holds only
// immutable configuration after [NewWiredRuntime] returns.
type WiredRuntime struct {
	inner     AgentRuntime
	reflector *ToolErrorReflector
	logger    *slog.Logger
	agentID   string
}

// NewWiredRuntime returns a [*WiredRuntime] decorating `inner`. Pass
// options ([WithToolErrorReflector], [WithLogger], [WithAgentID]) to
// configure the M5.6.b auto-reflection seam. Without any options the
// returned WiredRuntime is a transparent forwarder — callers SHOULD
// drop the wrapper rather than carry a no-op decorator, but doing so
// is not enforced.
//
// Panics on a nil `inner`; matches the panic discipline of
// [keeperslog.New], [lifecycle.New], and [NewToolErrorReflector].
func NewWiredRuntime(inner AgentRuntime, opts ...WiredRuntimeOption) *WiredRuntime {
	if inner == nil {
		panic("runtime: NewWiredRuntime: inner runtime must not be nil")
	}
	w := &WiredRuntime{
		inner:  inner,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Start forwards to the inner [AgentRuntime] verbatim. The decorator
// has no Start-time hook in the M5.6.b cycle.
func (w *WiredRuntime) Start(ctx context.Context, manifest Manifest, opts ...StartOption) (Runtime, error) {
	return w.inner.Start(ctx, manifest, opts...)
}

// SendMessage forwards to the inner [AgentRuntime] verbatim.
func (w *WiredRuntime) SendMessage(ctx context.Context, runtimeID ID, msg Message) error {
	return w.inner.SendMessage(ctx, runtimeID, msg)
}

// InvokeTool forwards the call to the inner [AgentRuntime] and, on
// the error return path, synchronously calls
// [ToolErrorReflector.Reflect] to compose + persist a `lesson` row
// describing the failure. The reflector is best-effort (AC5):
//
//   - The original `error` from the inner runtime is ALWAYS returned
//     to the caller unchanged, regardless of whether Reflect
//     succeeded, failed, or was skipped (no reflector configured).
//   - A reflector error is logged via the wired [*slog.Logger]
//     carrying the tool name, error class, and the err_type — never
//     the err_type's value, mirroring the keeperslog redaction
//     discipline documented in M3.4.b.
//   - The runtime sentinels [ErrInvalidToolCall], [ErrRuntimeNotFound],
//     [ErrTerminated], and [ErrToolUnauthorized] short-circuit the
//     reflection: they are pre-tool-execution guards (no real tool
//     ran) so there is no useful lesson to learn. The wiring tests
//     pin this branch.
//
// errClass extraction: the wiring matches the inner runtime's error
// against the package sentinels via [errors.Is] in priority order
// (sandbox-killed -> capability-denied / sentinel -> unsupported-
// platform). Errors that match no sentinel fall back to the type
// name via fmt.Sprintf("%T", err); this is good enough for a phase-1
// classifier and gives downstream Recall queries something
// deterministic to match on. Future cycles (M5.6.f e2e
// verification) may tighten the mapping.
func (w *WiredRuntime) InvokeTool(ctx context.Context, runtimeID ID, call ToolCall) (ToolResult, error) {
	res, err := w.inner.InvokeTool(ctx, runtimeID, call)
	if err == nil {
		return res, nil
	}
	if w.reflector == nil {
		return res, err
	}
	if shouldSkipReflection(err) {
		return res, err
	}

	errClass := classifyToolError(err)
	if reflectErr := w.reflector.Reflect(
		ctx,
		w.agentID,
		call.Name,
		call.ToolVersion,
		errClass,
		err.Error(),
	); reflectErr != nil {
		w.logger.LogAttrs(
			ctx, slog.LevelWarn,
			"runtime: tool-error reflector failed",
			slog.String("tool", call.Name),
			slog.String("err_class", errClass),
			slog.String("reflect_err_type", typeName(reflectErr)),
		)
	}
	return res, err
}

// Subscribe forwards to the inner [AgentRuntime] verbatim.
func (w *WiredRuntime) Subscribe(ctx context.Context, runtimeID ID, handler EventHandler) (Subscription, error) {
	return w.inner.Subscribe(ctx, runtimeID, handler)
}

// Terminate forwards to the inner [AgentRuntime] verbatim.
func (w *WiredRuntime) Terminate(ctx context.Context, runtimeID ID) error {
	return w.inner.Terminate(ctx, runtimeID)
}

// shouldSkipReflection short-circuits the reflection path for
// pre-tool-execution guards: ErrInvalidToolCall, ErrRuntimeNotFound,
// ErrTerminated, and ErrToolUnauthorized. None of these mean a tool
// actually ran and failed — they mean the runtime refused to dispatch
// at all — so writing a `lesson` row would be noise.
func shouldSkipReflection(err error) bool {
	switch {
	case errors.Is(err, ErrInvalidToolCall),
		errors.Is(err, ErrRuntimeNotFound),
		errors.Is(err, ErrTerminated),
		errors.Is(err, ErrToolUnauthorized):
		return true
	}
	return false
}

// classifyToolError maps a tool-invocation error into a stable string
// suitable for the reflector's `errClass` slot. The mapping is
// intentionally narrow in Phase 1 — only the sandbox / platform
// sentinels are recognised by name. Unknown errors fall through to
// the type name (fmt.Sprintf("%T", err)) so the resulting `lesson`
// row is at least groupable by Go type even when the underlying
// failure has no sentinel.
//
// Adding a new sentinel: declare it in errors.go, add an errors.Is
// branch here, and pin the mapping in
// classifyToolError_test.go. The order of branches does not matter
// for correctness because each errors.Is check is independent.
func classifyToolError(err error) string {
	switch {
	case errors.Is(err, ErrSandboxKilled):
		return "sandbox_killed"
	case errors.Is(err, ErrUnsupportedPlatform):
		return "unsupported_platform"
	}
	return typeName(err)
}

// typeName returns the Go type name of `err` formatted via
// fmt.Sprintf("%T", err). Pulled into a helper so the wiring's
// error-classifying branches share a single fallback path with the
// reflector-failure logger.
func typeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
