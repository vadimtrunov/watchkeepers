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

// ErrSubscriptionClosed is returned by [Subscription.Stop] when the
// dispatch loop exited with a transport error before Stop was called
// (the wrapped error rides via the [errors.Is] chain). A clean
// shutdown returns nil. Mirrors [messenger.ErrSubscriptionClosed].
// Matchable via [errors.Is].
var ErrSubscriptionClosed = errors.New("runtime: subscription closed")
