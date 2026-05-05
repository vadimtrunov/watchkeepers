# runtime — portable agent-runtime interface

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/runtime`

This package defines the **portable `AgentRuntime` interface** every
concrete agent runtime implementation satisfies: provision a session
from a `Manifest`, feed it messages, invoke tools on its behalf, stream
events back to the orchestrator, and tear it down. ROADMAP §M5 → M5.1.

The interface and its value types live here; concrete runtime
implementations (Claude-Code-via-TS-harness, in-process Go, embedded
SDK, fakes) live in sibling packages — the M5.3 TS-harness runtime is
the first one to land. Higher-level callers depend only on the
interface — they never import a concrete runtime package directly.

## Public API

```go
type AgentRuntime interface {
    Start(ctx context.Context, manifest Manifest, opts ...StartOption) (Runtime, error)
    SendMessage(ctx context.Context, runtimeID ID, msg Message) error
    InvokeTool(ctx context.Context, runtimeID ID, call ToolCall) (ToolResult, error)
    Subscribe(ctx context.Context, runtimeID ID, handler EventHandler) (Subscription, error)
    Terminate(ctx context.Context, runtimeID ID) error
}

type Runtime interface {
    ID() ID
}

type Subscription interface {
    Stop() error
}

type EventHandler func(ctx context.Context, ev Event) error
```

Value types: `Manifest`, `Message`, `ToolCall`, `ToolResult`,
`Event`, `StartOptions`.

ID alias: `ID string`.

Enums: `AutonomyLevel` (`manual`, `supervised`, `autonomous`),
`EventKind` (`message`, `tool_call`, `tool_result`, `error`).

Functional options: `WithStartMetadata` (extensible — future
`StartOption` constructors land here without breaking callers).

## Sentinel errors

All matchable via `errors.Is`:

- `ErrInvalidManifest` — `Start` got a `Manifest` with empty
  `AgentID`, `SystemPrompt`, or `Model`.
- `ErrInvalidMessage` — `SendMessage` got an empty `Message.Text`.
- `ErrInvalidToolCall` — `InvokeTool` got an empty `ToolCall.Name`.
- `ErrInvalidHandler` — `Subscribe` got a nil handler.
- `ErrRuntimeNotFound` — the supplied `ID` was never minted.
- `ErrTerminated` — the supplied `ID` was minted but the
  session has been terminated.
- `ErrToolUnauthorized` — `InvokeTool` got a tool name absent from
  the session manifest's `Toolset`.
- `ErrSubscriptionClosed` — the dispatch loop exited (transport error
  or post-Stop delivery attempt).

## Runtime contract (Phase 1)

1. **Method-set fidelity**: every runtime implements all five methods.
   When the runtime genuinely cannot satisfy one (a fake runtime that
   does not stream events, an embedded runtime that has no out-of-band
   tool invocation), return `ErrUnsupported`-style sentinels via the
   wrap chain — but the surface stays the same.
2. **Synchronous validation first**: `Start` rejects an empty
   `AgentID` / `SystemPrompt` / `Model` before contacting any
   subprocess; `SendMessage` rejects empty `Text`; `InvokeTool`
   rejects empty `Name`; `Subscribe` rejects a nil handler.
3. **Sentinel discipline**: when an error has a portable meaning,
   return the package sentinel (wrapped via `fmt.Errorf` if a runtime
   reason adds value). When the meaning is runtime-specific, surface
   the runtime error directly — but document it.
4. **`ID` opacity**: ids are runtime-defined byte sequences.
   Callers do NOT parse or reconstruct them; they pass the value
   verbatim back to the runtime's other methods.
5. **`Manifest.Toolset` is the runtime-side ACL**: `InvokeTool` MUST
   reject names absent from `Toolset` with `ErrToolUnauthorized`
   BEFORE touching the tool. This is in addition to (not instead of)
   the upstream capability-token validation in M3.5.
6. **Subscriptions are owned by the runtime**: the handler runs in a
   goroutine the runtime spawned. `Subscription.Stop` blocks until
   the in-flight handler returns; idempotent.
7. **Terminate is permanent**: a terminated `ID` cannot be
   resurrected — subsequent `SendMessage` / `InvokeTool` / `Subscribe`
   return `ErrTerminated`. To start a new session the caller calls
   `Start` again and gets a fresh `ID`.
8. **No infrastructure metadata in payloads** (M2 cross-cutting
   constraint): `Manifest.Metadata`, `Message.Metadata`,
   `ToolCall.Metadata`, `ToolResult.Metadata`, `Event.Metadata`
   carry runtime-specific fields, never `deployment_id`,
   `environment`, `host`, `pod`, etc.

## Quick start (consumer)

```go
import (
    "context"

    "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

func drive(ctx context.Context, ar runtime.AgentRuntime, manifest runtime.Manifest) error {
    rt, err := ar.Start(ctx, manifest)
    if err != nil {
        return err
    }
    defer func() { _ = ar.Terminate(ctx, rt.ID()) }()

    sub, err := ar.Subscribe(ctx, rt.ID(), func(ctx context.Context, ev runtime.Event) error {
        // observe agent messages / tool calls / tool results
        return nil
    })
    if err != nil {
        return err
    }
    defer func() { _ = sub.Stop() }()

    return ar.SendMessage(ctx, rt.ID(), runtime.Message{Text: "hello"})
}
```

## Manifest mapping

The runtime-facing `Manifest` here is NOT the wire-format
`keepclient.ManifestVersion` returned by `GET /v1/manifests/{id}`. M5.5
owns a loader that projects the wire shape into this typed `Manifest`:

- `keepclient.ManifestVersion.SystemPrompt` + `.Personality` +
  `.Language` → `Manifest.SystemPrompt` (the loader composes them via a
  templater; the runtime does NOT re-template).
- `keepclient.ManifestVersion.Personality` → `Manifest.Personality`
  (preserved verbatim for meta-tool introspection).
- `keepclient.ManifestVersion.Language` → `Manifest.Language`.
- The wire `tools` jsonb → `Manifest.Toolset` (the loader decodes the
  list of tool names).
- The wire `authority_matrix` jsonb → `Manifest.AuthorityMatrix` (the
  loader projects to a portable `map[string]string`).

Defining the runtime-facing `Manifest` locally (rather than aliasing
`keepclient.ManifestVersion`) keeps the runtime decoupled from the
wire schema's evolution. The mapping is one-way: loader →
`runtime.Manifest`; the runtime never round-trips a `Manifest` back to
the wire shape.

## Out of scope (deferred)

- Concrete runtime implementations — the Claude Code TS harness lands
  in M5.3.
- LLM provider abstraction — see M5.2 (`LLMProvider`); the runtime
  wraps a provider, but the two surfaces are distinct.
- Per-tool resource limits — wall-clock / CPU / memory / output-byte
  caps live in M5.4.
- Manifest loader — M5.5 projects `keepclient.ManifestVersion` into
  `Manifest`.
- Notebook auto-recall, auto-reflection, tool-version awareness — M5.6
  / M5.7 / M5.8 layer on top of this surface.
- Provider-swap conformance test — M5.10 exercises the contract a
  `FakeProvider` and the Claude Code provider both must pass.
- Capability-token enforcement on `InvokeTool` — token issuance is
  M3.5; runtime-side wiring is deferred to M5.3 where call sites are
  concrete.

## Test fake

`FakeRuntime` (in `fake_runtime_test.go`) is a hand-rolled
`AgentRuntime` stand-in available to in-package tests. Higher-level
test suites that want a portable harness can copy the pattern; the
fake is intentionally test-only (not exported from the package) to
keep production builds free of test scaffolding — mirrors the
messenger / lifecycle / outbox / keeperslog hand-rolled-fake pattern
documented in `docs/LESSONS.md`.

## References

- ROADMAP `docs/ROADMAP-phase1.md` § M5 → M5.1
- Pattern siblings: `core/pkg/messenger`, `core/pkg/keeperslog`,
  `core/pkg/outbox`, `core/pkg/capability`
- LESSONS: M4.1 metadata-maps, M3.5.a.1 sibling-methods,
  M4.2.b wire-format byte-level
