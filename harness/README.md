# `@watchkeepers/harness`

TypeScript harness process the Go core (`core/pkg/runtime`, M5.1) drives
to host a Watchkeeper agent. The harness owns:

- Claude Code integration (M5.3.d) via the `LLMProvider` wrapper
  (`core/pkg/llm`, M5.2).
- JSON-RPC 2.0 over stdio with the Go core.
- Tool invocation in `isolated-vm` (pure-JS tools) or a worker process
  (I/O-capable tools), once M5.3.b lands.
- `zod`-derived tool schemas auto-derived from the Tool Manifest, once
  M5.3.c lands.

This package corresponds to ROADMAP §M5.3
(`docs/ROADMAP-phase1.md`).

## Current scope — M5.3.a (foundation)

| Feature                      | Status |
| ---------------------------- | ------ |
| JSON-RPC 2.0 envelope        | yes    |
| NDJSON stdio framing         | yes    |
| `hello` method               | yes    |
| `shutdown` method            | yes    |
| Streaming-notification shape | yes    |
| Tool runner (`isolated-vm`)  | M5.3.b |
| `zod` tool schemas           | M5.3.c |
| Claude Code integration      | M5.3.d |
| Manifest loader              | M5.5   |
| Notebook auto-recall         | M5.6   |

The harness boots, accepts a single JSON-RPC `hello`, and replies with
`{harness: "watchkeeper", version: "0.1.0"}`. `shutdown` cleanly drains
the loop. Everything else is explicitly deferred to subsequent slices.

## Wire protocol

### Framing

**Newline-delimited JSON (NDJSON).** One JSON value per line, UTF-8
encoded, LF (`\n`) separator. Trailing CR is tolerated on input
(Windows-friendly) but never emitted. Empty / whitespace-only lines are
skipped.

The Go core controls both ends of the pipe so we never need to recover
from a truncated payload — a partial line means the producer crashed
and the supervisor restarts the harness anyway. NDJSON keeps the parser
~30 lines instead of ~120 vs. LSP-style Content-Length headers, and
degrades gracefully under `cat | harness` smoke tests. If a future
bidirectional embedding requires Content-Length framing the swap is
local to `src/jsonrpc.ts` — the method registry does not depend on the
framing.

### Envelope

Standard JSON-RPC 2.0
([spec](https://www.jsonrpc.org/specification)) — request, response,
notification. The harness honors:

| Code     | Meaning                                                 |
| -------- | ------------------------------------------------------- |
| `-32700` | Parse error — line was not valid JSON                   |
| `-32600` | Invalid Request — wrong `jsonrpc` value, missing method |
| `-32601` | Method not found                                        |
| `-32602` | Invalid params (reserved; first user is M5.3.b/c)       |
| `-32603` | Internal error (unhandled handler exception)            |

Application-level error codes (capability denied, tool timeout, …) live
outside the `-32768`..`-32000` reserved range and are introduced as the
tool runner lands.

### Methods (M5.3.a)

#### `hello` — handshake

Request:

```json
{ "jsonrpc": "2.0", "id": 1, "method": "hello" }
```

Response:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": { "harness": "watchkeeper", "version": "0.1.0" }
}
```

#### `shutdown` — clean exit

Request:

```json
{ "jsonrpc": "2.0", "id": 2, "method": "shutdown" }
```

Response:

```json
{ "jsonrpc": "2.0", "id": 2, "result": { "accepted": true } }
```

The harness writes the response, drains stdout, then exits the dispatch
loop and lets the process terminate.

### Streaming notifications (server → client)

Reserved shape — no methods registered in M5.3.a. The harness emits
JSON-RPC notifications (no `id`) for streaming events once M5.3.b/c/d
land. Example (illustrative; not implemented yet):

```json
{
  "jsonrpc": "2.0",
  "method": "stream.text_delta",
  "params": { "turn_id": "t1", "delta": "Hello" }
}
```

The Go core never replies; matching is by `method` name.

## Running locally

```bash
pnpm --filter @watchkeepers/harness build
echo '{"jsonrpc":"2.0","id":1,"method":"hello"}' \
  | node harness/dist/index.js
```

Expected output:

```text
{"jsonrpc":"2.0","id":1,"result":{"harness":"watchkeeper","version":"0.1.0"}}
```

Pipe a `shutdown` request after `hello` to exercise clean teardown.

## Layout

```text
harness/
├── README.md            ← this file
├── package.json         ← workspace member, scripts
├── tsconfig.json        ← strict mode, ES2023, NodeNext (covers src + test)
├── tsconfig.build.json  ← src-only emit config used by `pnpm build`
└── src/
    ├── index.ts         ← stdio entry, owns I/O lifecycle
    ├── dispatcher.ts    ← parse → dispatch → serialize line handler
    ├── jsonrpc.ts       ← envelope + framing helpers
    ├── methods.ts       ← method registry (hello, shutdown)
    └── types.ts         ← JSON-RPC 2.0 wire types
└── test/
    ├── jsonrpc.test.ts
    ├── methods.test.ts
    ├── dispatcher.test.ts
    └── runHarness.test.ts
```

## Dev scripts

| Command                                         | What it does                 |
| ----------------------------------------------- | ---------------------------- |
| `pnpm --filter @watchkeepers/harness build`     | Compile to `dist/`           |
| `pnpm --filter @watchkeepers/harness typecheck` | Strict tsc with src + test   |
| `pnpm --filter @watchkeepers/harness lint`      | ESLint (strict + type-aware) |
| `pnpm --filter @watchkeepers/harness test`      | Vitest with v8 coverage      |

Or from the workspace root:

```bash
pnpm typecheck
pnpm lint
pnpm test
pnpm build
```

## Extending the registry

```typescript
// src/methods.ts
registry.set("notebook.remember", async (params) => {
  // 1. validate params (zod arrives in M5.3.c)
  // 2. execute (capability check arrives in M5.3.b)
  // 3. return JSON-shaped result
});
```

Per-method input validation will move to a `zod`-derived schema once the
Tool Manifest loader lands (M5.3.c). Until then, methods MAY raise
`MethodError(JsonRpcErrorCode.InvalidParams, ...)` from `methods.ts` to
surface a typed error envelope.

## Out of scope for this slice

- Tool execution (M5.3.b — `isolated-vm`).
- Tool manifests + `zod` schemas (M5.3.c).
- Claude Code integration (M5.3.d).
- Per-tool resource limits (M5.4).
- Manifest loader (M5.5).
- Notebook linking + auto-recall (M5.6).

Each subsequent slice extends the method registry and adds a fresh
chapter to this README.
