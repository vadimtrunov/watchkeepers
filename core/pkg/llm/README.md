# llm — portable LLM-provider interface

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/llm`

This package defines the **portable `Provider` interface** every
concrete LLM-provider implementation satisfies: complete a turn
synchronously, stream a turn as it generates, count tokens
deterministically for budgeting, and report post-turn usage for cost
accounting. ROADMAP §M5 → M5.2.

The interface and its value types live here; concrete provider
implementations (Claude Code, future Anthropic-direct, OpenAI, fakes)
live in sibling sub-packages — the M5.2.b Claude Code default impl is
the first one to land. Higher-level callers depend only on the
interface — they never import a concrete provider package directly.

## Public API

```go
type Provider interface {
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
    Stream(ctx context.Context, req StreamRequest, handler StreamHandler) (StreamSubscription, error)
    CountTokens(ctx context.Context, req CountTokensRequest) (int, error)
    ReportCost(ctx context.Context, runtimeID string, usage Usage) error
}

type StreamSubscription interface {
    Stop() error
}

type StreamHandler func(ctx context.Context, ev StreamEvent) error
```

Value types: `CompleteRequest`, `CompleteResponse`, `StreamRequest`,
`StreamEvent`, `CountTokensRequest`, `Usage`, `Message`,
`ToolDefinition`, `ToolCall`.

ID alias: `Model string`.

Enums: `Role` (`system`, `user`, `assistant`, `tool`), `FinishReason`
(`stop`, `max_tokens`, `tool_use`, `error`), `StreamEventKind`
(`text_delta`, `tool_call_start`, `tool_call_delta`, `message_stop`,
`error`).

## Sentinel errors

All matchable via `errors.Is`:

- `ErrInvalidPrompt` — `Complete` / `Stream` / `CountTokens` got an
  empty `Messages` slice OR a `ToolDefinition` with a nil
  `InputSchema`.
- `ErrModelNotSupported` — the supplied `Model` is empty OR the
  provider's catalogue does not list it.
- `ErrTokenLimitExceeded` — the assembled request exceeds the model's
  context window.
- `ErrInvalidHandler` — `Stream` got a nil `StreamHandler`.
- `ErrStreamClosed` — `StreamSubscription.Stop` exited with a
  transport / handler-error cause (the wrapped cause rides via the
  `errors.Is` chain).
- `ErrProviderUnavailable` — the upstream service was unreachable
  (network error, auth failure, rate-limit exhaustion).

## Provider contract (Phase 1)

1. **Method-set fidelity**: every provider implements all four
   methods. When the provider genuinely cannot satisfy one (a
   no-streaming provider, a provider without local tokeniser),
   surface a wrapped sentinel — but the surface stays the same.
2. **Synchronous validation first**: `Complete` / `Stream` /
   `CountTokens` reject empty `Model` (with `ErrModelNotSupported`),
   empty `Messages` (with `ErrInvalidPrompt`), and (for `Stream`) a
   nil handler (with `ErrInvalidHandler`) before contacting the
   upstream service.
3. **Sentinel discipline**: when an error has a portable meaning,
   return the package sentinel (wrapped via `fmt.Errorf` if a
   provider reason adds value). When the meaning is provider-specific,
   surface the provider error directly — but document it.
4. **`Model` opacity**: model ids are provider-defined byte sequences.
   Callers do NOT parse or reconstruct them; they pass the value
   verbatim from `runtime.Manifest.Model` to the provider's
   `CompleteRequest.Model` / `StreamRequest.Model`.
5. **Streams are owned by the provider**: the handler runs in a
   goroutine the provider spawned. `StreamSubscription.Stop` blocks
   until the in-flight handler returns; idempotent. A handler error
   terminates the stream and surfaces from the next `Stop` via
   `ErrStreamClosed` wrapping the original cause.
6. **`ReportCost` is fire-and-forget**: callers MUST call exactly
   once per completed turn (synchronous Complete OR streaming
   stop event). Duplicates produce duplicate accounting; missing
   calls leak cost data. The provider's bookkeeping is the source of
   truth for the M6.3 cost tracker.
7. **Tool-call lifecycle**: tool definitions go INTO the provider via
   `CompleteRequest.Tools` / `StreamRequest.Tools` (`ToolDefinition`).
   Model-emitted tool requests come OUT via
   `CompleteResponse.ToolCalls` (synchronous) or
   `StreamEventKindToolCallStart` + `StreamEventKindToolCallDelta`
   events (streaming). Execution belongs to the runtime
   (`runtime.AgentRuntime.InvokeTool`); the provider does not invoke
   tools.
8. **No infrastructure metadata in payloads** (M2 cross-cutting
   constraint): `CompleteRequest.Metadata`, `CompleteResponse.Metadata`,
   `StreamEvent.Metadata`, `Message.Metadata`, `Usage.Metadata` carry
   provider-specific fields, never `deployment_id`, `environment`,
   `host`, `pod`, etc.

## Quick start (consumer)

```go
import (
    "context"

    "github.com/vadimtrunov/watchkeepers/core/pkg/llm"
)

func turn(ctx context.Context, p llm.Provider, runtimeID string, prompt string) (string, error) {
    req := llm.CompleteRequest{
        Model:  "claude-sonnet-4",
        System: "You are a Watchkeeper.",
        Messages: []llm.Message{
            {Role: llm.RoleUser, Content: prompt},
        },
        MaxTokens: 1024,
    }
    resp, err := p.Complete(ctx, req)
    if err != nil {
        return "", err
    }
    if err := p.ReportCost(ctx, runtimeID, resp.Usage); err != nil {
        return "", err
    }
    return resp.Content, nil
}
```

Streaming variant:

```go
func stream(ctx context.Context, p llm.Provider, runtimeID string, prompt string) error {
    req := llm.StreamRequest{
        Model:    "claude-sonnet-4",
        System:   "You are a Watchkeeper.",
        Messages: []llm.Message{{Role: llm.RoleUser, Content: prompt}},
    }
    var finalUsage llm.Usage
    sub, err := p.Stream(ctx, req, func(ctx context.Context, ev llm.StreamEvent) error {
        switch ev.Kind {
        case llm.StreamEventKindTextDelta:
            // append ev.TextDelta to the assistant turn buffer
        case llm.StreamEventKindMessageStop:
            finalUsage = ev.Usage
        }
        return nil
    })
    if err != nil {
        return err
    }
    if err := sub.Stop(); err != nil {
        return err
    }
    return p.ReportCost(ctx, runtimeID, finalUsage)
}
```

## Distinction from the `runtime` package

`runtime.AgentRuntime` (M5.1) drives an agent SESSION — boot from a
manifest, feed it user messages, execute tool calls, stream session
events, terminate. `llm.Provider` (this package) drives a single
LLM TURN — one request, one response (or stream of chunks).

A concrete `runtime.AgentRuntime` implementation typically WRAPS a
`llm.Provider`: the runtime owns the session loop, the manifest,
and the tool-execution path; the provider owns the model invocation.
The two surfaces are intentionally distinct so a runtime can swap
providers without touching its session machinery, and a provider can
be reused across runtimes (in-process, TS-harness, embedded SDK).

`runtime.Message` (user-input shape into the runtime) is distinct
from `llm.Message` (conversational-turn shape into the model). The
runtime converts its message into a `llm.Message` sequence when
driving the LLM — the conversion shape is the runtime's concern, not
the provider's.

## Out of scope (deferred)

- Concrete provider implementations — the Claude-Code-backed default
  lands in M5.2.b (proposed sub-task). This package is types +
  interface only.
- Tool execution — owned by `runtime.AgentRuntime.InvokeTool` (M5.1).
  The provider only surfaces the tool-call requests the model emits.
- Provider credentials — M5.9 routes Claude Code credentials via the
  secrets interface; this package never names a credential.
- Provider-swap conformance test — M5.10 exercises the contract
  `FakeProvider` (test-only, in `fake_provider_test.go`) and the
  Claude Code provider both must pass.
- Cost catalogue / aggregation — concrete cost-per-token rates and
  per-Watchkeeper roll-ups (M6.3) live downstream; this package's
  `ReportCost` is the ingest point.

## Test fake

`FakeProvider` (in `fake_provider_test.go`) is a hand-rolled
`Provider` stand-in available to in-package tests. Higher-level test
suites (M5.10 conformance) can copy the pattern; the fake is
intentionally test-only (not exported from the package) to keep
production builds free of test scaffolding — mirrors the messenger /
runtime / outbox / keeperslog hand-rolled-fake pattern documented in
`docs/LESSONS.md`.

## References

- ROADMAP `docs/ROADMAP-phase1.md` § M5 → M5.2
- Pattern siblings: `core/pkg/runtime` (M5.1), `core/pkg/messenger`
  (M4.1), `core/pkg/keeperslog`, `core/pkg/outbox`,
  `core/pkg/capability`
- LESSONS: M5.1 wire-vs-runtime decoupling, M4.1 metadata-maps,
  M3.5.a.1 sibling-methods
