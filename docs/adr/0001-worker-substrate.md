# 0001 — Worker substrate for the I/O-capable tool path

```text
Status:   Accepted
Date:     2026-05-06
Deciders: Vadym Trunov
```

## Context

M5.1's `AgentRuntime` already supervises the harness as a child of the Go core, so isolation between the LLM-facing harness and the OS belongs to the Go side. Inside the harness, M5.3.b.a landed `invokeTool` on `isolated-vm`: a pure-JS sandbox with no host globals, suitable for compute kernels but unable to host I/O-capable tools (fs, net, child commands). M5.3.b.b is the I/O-capable counterpart, and it needs an actual sandbox boundary — not a shared V8 context. The isolated-vm README explicitly cautions that `running untrusted code is an extraordinarily difficult problem` and recommends keeping isolate instances `in a different Node.js process` (see References). The CodeRabbit thread on PR #57 echoed the same point: same-process execution of capability-bearing code is a footgun even with isolated-vm in front of it.

Forces:

- I/O tools must be killable independently of the harness; a runaway tool cannot take the JSON-RPC dispatcher down.
- Capability gating (fs scope, net allow-list) is easier to enforce at a process boundary than inside the same V8 instance.
- M5.3.b.b.b–e need a stable transport surface for the JSON-RPC sub-channel; reinventing framing inflates the test surface.

## Decision

**Decision**: `child_process.fork`.

The harness will spawn each I/O-capable tool as a forked Node child process. `fork` gives true OS isolation (separate PID, killable, OS-level resource accounting) and a built-in IPC channel that round-trips JSON-RPC messages via structured clone — no second framing layer to maintain. This aligns with the isolated-vm upstream guidance to keep capability-bearing code in a different Node.js process and matches the JSON-RPC envelope the harness already speaks to the Go core.

## Considered alternatives

### `worker_threads.Worker`

Pros:

- Lowest spawn overhead (≈10–20 ms on the host's Node 24) — ideal for many short tool calls.
- Structured-clone over `MessagePort` is built in; no serialization layer to author.

Cons:

- Same OS process as the harness. A native crash (e.g. a buggy `node-gyp` dependency a tool transitively pulls) takes the dispatcher down with it. Disqualifying for the M5.3.b.b blast-radius requirement.
- OS-level capabilities (open fds, sockets, env) are shared with the parent. A capability-denied tool that nonetheless calls `fs.openSync` succeeds; gating must be done in JS on every host API. This contradicts the isolated-vm upstream guidance cited in Context.

### `child_process.fork`

Pros:

- Separate OS process: SIGKILL-able, accountable in `ps`, isolated from the dispatcher's event loop. A segfaulting native dep crashes only the tool worker.
- Built-in `process.send` / `message` IPC channel. JSON serialization by default; structured-clone semantics (so `Map`, `Date`, typed arrays, and `undefined`-valued keys survive) require opting in via `serialization: 'advanced'` on the `fork()` call (see Node `child_process` docs). The harness MUST set this option. The JSON-RPC envelope rides natively without a second NDJSON framer.

Cons:

- Spawn cost ≈70–90 ms on the host's Node 24 — non-trivial vs. `worker_threads`. Mitigated by tools being long-lived per-call (LLM-paced), not hot-loop.
- IPC channel inherits Node's structured-clone limits; functions, class instances with private fields, and `WeakMap` instances do not transfer. Tools must return JSON-RPC-shaped values, which the harness already enforces.

### `child_process.spawn` with the `node` binary

Pros:

- Maximum control over stdio framing — could share the harness's existing NDJSON framer (`src/jsonrpc.ts`) symmetrically.
- Trivially substitutable with non-Node executables later (e.g. a `deno run` worker for Deno-permission tools), since the contract is just stdio bytes.

Cons:

- Reinvents the IPC channel that `fork` provides for free: framing, back-pressure, half-close handling all become harness code. Doubles the framing surface vs. `fork` and grows the test matrix in M5.3.b.b.e.
- Stdio JSON-RPC framing means every value is JSON-stringified on the wire (no structured clone, no `serialization: 'advanced'` escape hatch as on `child_process.fork`). `Date`, typed arrays, and `undefined`-valued keys are lost.

## Consequences

- **Transport surface**: the JSON-RPC sub-channel between harness and tool worker rides `process.send` / `process.on("message")`. M5.3.b.b.c MUST pass `serialization: 'advanced'` on the `fork()` call so that the wire shape uses structured-clone semantics (a superset of the harness↔Go core JSON-RPC envelope) rather than plain JSON. M5.3.b.b.c will codify the sub-channel as the same envelope shape (`jsonrpc`, `id`, `method`, `params` / `result` / `error`).
- **Capability declaration ergonomics**: capabilities are declared at spawn time and frozen in the child's bootstrap module. Per-call capability mutation is rejected. M5.3.b.b.b's zod schema will model this as a single `capabilities` field on the spawn request, not as a per-`invokeTool` argument.
- **Error-model alignment**: the existing `ToolErrorCode` band in `harness/src/invokeTool.ts` (`ToolExecutionError: -32000`, `ToolTimeout: -32001`, `ToolMemoryExceeded: -32002`) carries over unchanged. The worker path reserves `ToolCapabilityDenied: -32003` for capability-gating denials and `ToolWorkerCrashed: -32004` for unexpected child exits / signals. Both stay inside the JSON-RPC application-error band (`-32099..-32000`).
- **Test ergonomics**: vitest tests will use the spawn-and-wait integration shape for happy paths (real `fork` of a fixture worker script under `harness/test/fixtures/`) and a thin `ChildProcess`-shaped fake for unit tests of the host-side dispatcher. `worker_threads` would have allowed in-process unit tests; that ergonomic loss is the price of the OS boundary and is accepted.

## References

- Node `worker_threads` API — <https://nodejs.org/api/worker_threads.html>
- Node `child_process` API — <https://nodejs.org/api/child_process.html>
- `isolated-vm` README, security caveat on running untrusted code — <https://github.com/laverdet/isolated-vm/blob/main/README.md#security>
- CodeRabbit review thread on PR #57 (same-process execution risk) — <https://github.com/vadimtrunov/watchkeepers/pull/57#discussion_r3190823845>
