package runtime

import "errors"

// ErrInvalidManifest is returned synchronously by [AgentRuntime.Start]
// when the supplied [Manifest] fails validation (empty AgentID, empty
// SystemPrompt, empty Model). Distinguishes "the manifest is malformed
// and the runtime never spawned a session" from "the runtime tried but
// failed". Matchable via [errors.Is].
var ErrInvalidManifest = errors.New("runtime: invalid manifest")

// ErrInvalidMessage is returned synchronously by
// [AgentRuntime.SendMessage] when [Message.Text] is empty. The
// underlying runtime is NOT touched on this path. Matchable via
// [errors.Is].
var ErrInvalidMessage = errors.New("runtime: invalid message")

// ErrInvalidToolCall is returned synchronously by
// [AgentRuntime.InvokeTool] when [ToolCall.Name] is empty. The
// underlying runtime / tool is NOT touched on this path. Matchable
// via [errors.Is].
var ErrInvalidToolCall = errors.New("runtime: invalid tool call")

// ErrInvalidHandler is returned synchronously by
// [AgentRuntime.Subscribe] when the supplied [EventHandler] is nil. A
// nil handler is a programmer error at the call site; surfacing the
// sentinel rather than panicking lets the caller recover and report.
// Matchable via [errors.Is].
var ErrInvalidHandler = errors.New("runtime: invalid handler")

// ErrRuntimeNotFound is returned by [AgentRuntime.SendMessage] /
// [AgentRuntime.InvokeTool] / [AgentRuntime.Subscribe] when the
// supplied [ID] does not match a live session. Distinct from
// [ErrTerminated] — the former means the id was never minted, the
// latter means the id was minted but the session has since been
// terminated. Matchable via [errors.Is].
var ErrRuntimeNotFound = errors.New("runtime: runtime not found")

// ErrTerminated is returned by [AgentRuntime.SendMessage] /
// [AgentRuntime.InvokeTool] / [AgentRuntime.Subscribe] after the
// session was terminated via [AgentRuntime.Terminate]. The handle id
// remains a valid lookup key for the lifetime of the runtime
// implementation's bookkeeping; reusing it surfaces this sentinel
// rather than [ErrRuntimeNotFound] so callers can distinguish "stale
// pointer" from "wrong pointer". Matchable via [errors.Is].
var ErrTerminated = errors.New("runtime: terminated")

// ErrToolUnauthorized is returned by [AgentRuntime.InvokeTool] when the
// requested tool name is absent from the session manifest's
// [Manifest.Toolset]. This is the runtime-side enforcement of the
// capability ACL the manifest declares; it is NOT a substitute for the
// capability-token validation that happens upstream in
// [capability.Broker.Validate]. Matchable via [errors.Is].
var ErrToolUnauthorized = errors.New("runtime: tool unauthorized")

// ErrSandboxKilled is returned by [SandboxRunner.Run] when the
// sandboxed process was terminated by one of the guardrail paths
// (wall-clock timeout, output-byte cap, or context cancellation).
// Natural exits — including non-zero exit codes — return a nil error;
// callers inspect [RunResult.ExitCode] to react to those. The wrapping
// chain preserves the underlying termination cause: context-cancel
// kills wrap [context.Canceled] / [context.DeadlineExceeded] alongside
// this sentinel so the call site can `errors.Is` either signal.
// Matchable via [errors.Is].
var ErrSandboxKilled = errors.New("runtime: sandbox killed")

// ErrSubscriptionClosed is returned by [Subscription.Stop] when the
// dispatch loop exited with a transport error before Stop was called
// (the wrapped error rides via the [errors.Is] chain). A clean
// shutdown returns nil. Mirrors [messenger.ErrSubscriptionClosed].
// Matchable via [errors.Is].
var ErrSubscriptionClosed = errors.New("runtime: subscription closed")

// ErrEmbedderRequired is returned by [NewToolErrorReflector] when no
// [llm.EmbeddingProvider] was supplied via [WithEmbedder]. The
// reflector composes a [notebook.Entry] of category `lesson` and
// MUST embed it before calling Remember; there is no sane default
// embedder to fall back to (silently no-op'ing every call would mask
// the bug). Matches the existing runtime sentinel idioms; matchable
// via [errors.Is].
var ErrEmbedderRequired = errors.New("runtime: embedder required")

// ErrUnsupportedPlatform is returned by [SandboxRunner.Run] (M5.4.b)
// when the caller asked for a non-zero CPU-time or memory-ceiling
// rlimit on a platform whose `applyRlimits` shim does not implement
// kernel enforcement. Linux returns nil from the shim; Darwin returns
// nil and silently does not enforce; every other platform surfaces
// this sentinel only when at least one rlimit field is non-zero. A
// fully zeroed [SandboxConfig] never trips this error. Matchable via
// [errors.Is].
var ErrUnsupportedPlatform = errors.New("runtime: rlimits unsupported on this platform")
