/**
 * `invokeTool` — pure-JS sandbox runner exposed as a JSON-RPC method.
 *
 * Each call allocates a fresh `isolated-vm` Isolate, compiles the
 * caller-supplied source as the body of a `function(input){ … }`, calls
 * it with the JSON-RPC `input` deep-copied across the isolate boundary,
 * and copies the return value back. The Isolate is disposed in a
 * `finally` so a thrown handler / OOM / timeout never leaks a V8 heap.
 *
 * Capability surface inside the isolate is intentionally empty: no
 * `require`, no `process`, no `fetch`, no host globals. The pure-JS
 * tool path is a sandboxed compute kernel — host I/O lands separately
 * on the worker-process path (M5.3.b.b).
 *
 * Error translation:
 *   - timeout            → MethodError(ToolErrorCode.ToolTimeout)
 *   - memory limit       → MethodError(ToolErrorCode.ToolMemoryExceeded)
 *   - any other throw    → MethodError(ToolErrorCode.ToolExecutionError, message)
 *   - shape mismatch     → MethodError(JsonRpcErrorCode.InvalidParams) BEFORE
 *                          allocating the Isolate
 */

import ivm from "isolated-vm";

import { CapabilityDeclarationSchema, type CapabilityDeclaration } from "./capabilities.js";
import { MethodError } from "./methods.js";
import { JsonRpcErrorCode, type JsonRpcErrorCodeValue, type JsonRpcValue } from "./types.js";
import { gateToolInvocation, type ToolOperation } from "./worker/broker.js";
import {
  spawnWorker,
  type SpawnWorkerOptions,
  type WorkerCrashEvent,
  type WorkerHandle,
} from "./worker/spawn.js";
import { JsonRpcRemoteError } from "./worker/transport.js";

/**
 * Application-range JSON-RPC error codes for the tool runner. Codes
 * sit at the top of the JSON-RPC server-error band (-32000 down) per
 * the spec's reservation: -32768..-32000 is reserved for the protocol,
 * the implementation-defined slice (-32099..-32000) is available for
 * application-level errors.
 */
export const ToolErrorCode = {
  /** Tool source threw, returned a non-transferable value, or hit a runtime ReferenceError. */
  ToolExecutionError: -32000,
  /** Wall-clock budget was exceeded inside the isolate. */
  ToolTimeout: -32001,
  /** Isolate breached the configured memory ceiling. */
  ToolMemoryExceeded: -32002,
  /** Worker rejected the call: requested I/O is outside the frozen capability declaration (ADR §0001). */
  ToolCapabilityDenied: -32003,
  /** Worker process exited unexpectedly mid-session (ADR §0001). */
  ToolWorkerCrashed: -32004,
} as const;

/**
 * Numeric value of one of the {@link ToolErrorCode} entries. Widens
 * cleanly into {@link JsonRpcErrorCodeValue} at the {@link MethodError}
 * call site — the codes deliberately live in the application range,
 * outside the protocol band.
 */
export type ToolErrorCodeValue = (typeof ToolErrorCode)[keyof typeof ToolErrorCode];

/**
 * Default wall-clock budget for an `invokeTool` call when the caller
 * omits `limits.wallClockMs`. One second is generous for pure compute
 * and small enough that a runaway test never hangs the dispatcher.
 */
export const DEFAULT_WALL_CLOCK_MS = 1000;

/**
 * Default memory ceiling for the isolate (MB). 16 MB is the smallest
 * value isolated-vm accepts in practice and matches the framing of
 * "tool body, no heavy allocations".
 */
export const DEFAULT_MEMORY_MB = 16;

/**
 * Pure-JS sandbox tool — see {@link runIsolatedJs}. The runner compiles
 * `source` as a function body and calls it with the JSON-RPC `input`.
 */
export interface IsolatedVmTool {
  readonly kind: "isolated-vm";
  readonly source: string;
}

/**
 * I/O-capable worker tool (ADR §0001). The dispatcher forks a Node
 * child with `capabilities` frozen at spawn, then sends a single
 * JSON-RPC request `method`(args=`input`) over the IPC channel.
 *
 * `requiredOps` lists statically-deniable operations the dispatcher
 * MUST gate via {@link gateToolInvocation} BEFORE spawning. A single
 * deny aborts the call with {@link ToolErrorCode.ToolCapabilityDenied}
 * and never pays the fork cost.
 */
export interface WorkerTool {
  readonly kind: "worker";
  readonly method: string;
  readonly capabilities: CapabilityDeclaration;
  readonly requiredOps?: readonly ToolOperation[];
  /** Test-only escape hatch forwarded to {@link spawnWorker}. */
  readonly spawnOptions?: SpawnWorkerOptions;
}

/**
 * Wire shape of `invokeTool` params. Discriminator is `tool.kind`. The
 * `'isolated-vm'` variant runs an in-process sandbox; the `'worker'`
 * variant forks an OS-isolated child for I/O-gated tool calls.
 */
export interface InvokeToolParams {
  readonly tool: IsolatedVmTool | WorkerTool;
  readonly input: JsonRpcValue;
  readonly limits?: {
    readonly wallClockMs?: number;
    readonly memoryMb?: number;
  };
}

/**
 * Wire shape of `invokeTool` success result. The runner deep-copies the
 * isolate-side return value, so `output` is always a plain JSON-RPC
 * value safe to serialize. An `undefined` return inside the isolate is
 * normalized to `null` for JSON compatibility.
 */
export interface InvokeToolResult {
  readonly output: JsonRpcValue;
}

/**
 * Inputs to {@link runIsolatedJs}. All fields are pre-validated by the
 * caller; the runner trusts the shape and only translates isolated-vm
 * errors into typed {@link MethodError}s.
 */
export interface RunIsolatedJsArgs {
  readonly source: string;
  readonly input: JsonRpcValue;
  readonly wallClockMs: number;
  readonly memoryMb: number;
}

/**
 * Allocate a fresh isolate, run the supplied function-body source with
 * `input` injected as the lone parameter, and return the deep-copied
 * result. Always disposes the isolate.
 *
 * Throws a {@link MethodError} carrying a {@link ToolErrorCode} for
 * timeout / OOM / generic execution failure so the dispatcher can lift
 * the wire-level code without inspecting the underlying error.
 */
export async function runIsolatedJs(args: RunIsolatedJsArgs): Promise<JsonRpcValue> {
  const { source, input, wallClockMs, memoryMb } = args;
  const isolate = new ivm.Isolate({ memoryLimit: memoryMb });
  try {
    const context = await isolate.createContext();
    // Wrap the user source in an IIFE-style closure so `return` works
    // and so the source body cannot accidentally clobber `globalThis`.
    // `evalClosure` injects positional arguments as `$0`, `$1`, ... and
    // honors the wall-clock budget on the C++ side.
    const code = `return (function(input){\n${source}\n})($0);`;
    const result: unknown = await context.evalClosure(code, [input], {
      arguments: { copy: true },
      result: { copy: true, promise: true },
      timeout: wallClockMs,
    });
    // JSON-RPC has no `undefined`; normalize so the wire shape stays
    // valid even when the source body falls off the end without
    // returning.
    return (result ?? null) as JsonRpcValue;
  } catch (err) {
    throw translateIsolateError(err);
  } finally {
    isolate.dispose();
  }
}

/**
 * `invokeTool` JSON-RPC handler. Validates the params shape WITHOUT
 * allocating an Isolate / spawning a worker (AC6) and dispatches to the
 * matching backend on success. Returns the canonical `{ output }`
 * envelope.
 */
export async function invokeToolHandler(params: JsonRpcValue | undefined): Promise<JsonRpcValue> {
  const validated = validateParams(params);
  if (validated.tool.kind === "isolated-vm") {
    const wallClockMs = validated.limits?.wallClockMs ?? DEFAULT_WALL_CLOCK_MS;
    const memoryMb = validated.limits?.memoryMb ?? DEFAULT_MEMORY_MB;
    const output = await runIsolatedJs({
      source: validated.tool.source,
      input: validated.input,
      wallClockMs,
      memoryMb,
    });
    return { output } satisfies InvokeToolResult;
  }
  const output = await runWorkerTool(validated.tool, validated.input);
  return { output } satisfies InvokeToolResult;
}

/**
 * Worker-process tool path (ADR §0001). Per-invocation spawn (no pool
 * yet): pre-gates `requiredOps`, forks the worker with the frozen
 * capabilities, sends the JSON-RPC request, and always terminates the
 * worker — including on sync deny, crash, and remote-error paths
 * (AC5).
 *
 * Error translation:
 *   - sync deny           → MethodError(ToolCapabilityDenied, reason)
 *   - worker crash        → MethodError(ToolWorkerCrashed)
 *   - remote -32003       → MethodError(ToolCapabilityDenied) (preserve data)
 *   - remote -32004       → MethodError(ToolWorkerCrashed)
 *   - other remote err    → MethodError(ToolExecutionError, message)
 */
async function runWorkerTool(tool: WorkerTool, input: JsonRpcValue): Promise<JsonRpcValue> {
  // 1. Pre-gate every requested op against the frozen declaration.
  //    Pure / synchronous — never pays the fork cost on a deny (AC3).
  for (const op of tool.requiredOps ?? []) {
    const decision = gateToolInvocation(tool.capabilities, op);
    if (!decision.allow) {
      throw toolError(
        ToolErrorCode.ToolCapabilityDenied,
        decision.reason ?? "tool capability denied",
        { reason: decision.reason ?? "tool capability denied" },
      );
    }
  }

  // 2. Spawn the worker. A spawn failure surfaces as ToolExecutionError —
  //    the worker never came up, so there is nothing to terminate.
  let worker: WorkerHandle;
  try {
    worker = await spawnWorker(tool.capabilities, tool.spawnOptions);
  } catch (err) {
    throw toolError(
      ToolErrorCode.ToolExecutionError,
      err instanceof Error ? err.message : String(err),
    );
  }

  // 3. Capture the crash event in a closure flag and await the request.
  //    spawn.ts's exit handler calls transport.dispose() BEFORE notifying
  //    crash listeners, so a pending request rejects with
  //    "transport disposed" instead of a typed crash error. The catch
  //    arm consults `crashEvent` to surface the correct -32004 wire code
  //    regardless of which rejection happened to fire first.
  let crashEvent: WorkerCrashEvent | undefined;
  const crashHandler = (event: WorkerCrashEvent): void => {
    crashEvent = event;
  };
  worker.on("crash", crashHandler);
  try {
    return await worker.request(tool.method, input);
  } catch (err) {
    if (crashEvent !== undefined) {
      throw toolError(
        ToolErrorCode.ToolWorkerCrashed,
        buildCrashMessage(crashEvent),
        crashEventData(crashEvent),
      );
    }
    if (err instanceof MethodError) throw err;
    if (err instanceof JsonRpcRemoteError) throw translateRemoteError(err);
    throw toolError(
      ToolErrorCode.ToolExecutionError,
      err instanceof Error ? err.message : String(err),
    );
  } finally {
    // AC5: terminate on every code path. `off` first so a crash event
    // arriving during terminate() does not retrigger the handler after
    // the surrounding promise has already settled.
    worker.off("crash", crashHandler);
    // Swallow terminate() failures: if a future spawn.ts change makes
    // terminate reject, we MUST NOT let that rejection mask the original
    // crash / deny / remote error already in flight (Phase 4 I3).
    try {
      await worker.terminate();
    } catch {
      /* terminate failures are non-fatal; the original error wins. */
    }
  }
}

function buildCrashMessage(event: WorkerCrashEvent): string {
  if (event.exitCode !== undefined && event.signal !== undefined) {
    return `worker crashed: exitCode=${String(event.exitCode)} signal=${event.signal}`;
  }
  if (event.exitCode !== undefined) return `worker crashed: exitCode=${String(event.exitCode)}`;
  if (event.signal !== undefined) return `worker crashed: signal=${event.signal}`;
  return "worker crashed";
}

function crashEventData(event: WorkerCrashEvent): JsonRpcValue {
  const out: { exitCode?: number; signal?: string } = {};
  if (event.exitCode !== undefined) out.exitCode = event.exitCode;
  if (event.signal !== undefined) out.signal = event.signal;
  return out;
}

function translateRemoteError(err: JsonRpcRemoteError): MethodError {
  if (err.code === ToolErrorCode.ToolCapabilityDenied) {
    return toolError(ToolErrorCode.ToolCapabilityDenied, err.message, err.data);
  }
  if (err.code === ToolErrorCode.ToolWorkerCrashed) {
    return toolError(ToolErrorCode.ToolWorkerCrashed, err.message, err.data);
  }
  return toolError(ToolErrorCode.ToolExecutionError, err.message, err.data);
}

/**
 * Narrow `params` to {@link InvokeToolParams}. Throws a
 * {@link MethodError} with {@link JsonRpcErrorCode.InvalidParams} on
 * any shape mismatch — never allocates an Isolate, so callers can rely
 * on this to fail cheap before paying the V8 startup cost.
 */
function validateParams(params: JsonRpcValue | undefined): InvokeToolParams {
  if (typeof params !== "object" || params === null || Array.isArray(params)) {
    throw invalidParams("params must be an object");
  }
  const obj = params as { tool?: unknown; input?: unknown; limits?: unknown };
  if (typeof obj.tool !== "object" || obj.tool === null || Array.isArray(obj.tool)) {
    throw invalidParams("params.tool must be an object");
  }
  const toolRaw = obj.tool as { kind?: unknown };
  if (toolRaw.kind !== "isolated-vm" && toolRaw.kind !== "worker") {
    throw invalidParams('params.tool.kind must be "isolated-vm" or "worker"');
  }
  if (!("input" in obj)) {
    throw invalidParams("params.input is required");
  }
  const tool = validateTool(toolRaw);
  const limits = validateLimits(obj.limits);
  return limits === undefined
    ? { tool, input: obj.input as JsonRpcValue }
    : { tool, input: obj.input as JsonRpcValue, limits };
}

function validateTool(raw: { kind?: unknown }): IsolatedVmTool | WorkerTool {
  if (raw.kind === "isolated-vm") {
    const t = raw as { kind: "isolated-vm"; source?: unknown };
    if (typeof t.source !== "string") {
      throw invalidParams("params.tool.source must be a string");
    }
    return { kind: "isolated-vm", source: t.source };
  }
  // raw.kind === "worker" (narrowed by caller).
  const t = raw as {
    kind: "worker";
    method?: unknown;
    capabilities?: unknown;
    requiredOps?: unknown;
    spawnOptions?: unknown;
  };
  if (typeof t.method !== "string" || t.method.length === 0) {
    throw invalidParams("params.tool.method must be a non-empty string");
  }
  const capsParsed = CapabilityDeclarationSchema.strict().safeParse(t.capabilities);
  if (!capsParsed.success) {
    throw invalidParams(`params.tool.capabilities invalid: ${capsParsed.error.message}`);
  }
  let requiredOps: readonly ToolOperation[] | undefined;
  if (t.requiredOps !== undefined) {
    if (!Array.isArray(t.requiredOps)) {
      throw invalidParams("params.tool.requiredOps must be an array when present");
    }
    requiredOps = t.requiredOps.map((op, i) => validateOp(op, i));
  }
  // `spawnOptions` is a test-only escape hatch; we only allow it through
  // the in-process API so it is preserved verbatim from `raw` rather than
  // re-validated. Wire callers cannot reach this branch because the JSON
  // values that survive structured-clone don't carry through the
  // typed-API surface.
  return requiredOps === undefined
    ? {
        kind: "worker",
        method: t.method,
        capabilities: capsParsed.data,
        ...(t.spawnOptions === undefined
          ? {}
          : { spawnOptions: t.spawnOptions as SpawnWorkerOptions }),
      }
    : {
        kind: "worker",
        method: t.method,
        capabilities: capsParsed.data,
        requiredOps,
        ...(t.spawnOptions === undefined
          ? {}
          : { spawnOptions: t.spawnOptions as SpawnWorkerOptions }),
      };
}

function validateOp(raw: unknown, index: number): ToolOperation {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw invalidParams(`params.tool.requiredOps[${String(index)}] must be an object`);
  }
  const op = raw as {
    kind?: unknown;
    path?: unknown;
    host?: unknown;
    port?: unknown;
    name?: unknown;
  };
  switch (op.kind) {
    case "fs.read":
    case "fs.write":
      if (typeof op.path !== "string") {
        throw invalidParams(`params.tool.requiredOps[${String(index)}].path must be a string`);
      }
      return { kind: op.kind, path: op.path };
    case "net.connect":
      if (typeof op.host !== "string") {
        throw invalidParams(`params.tool.requiredOps[${String(index)}].host must be a string`);
      }
      if (op.port !== undefined && typeof op.port !== "number") {
        throw invalidParams(
          `params.tool.requiredOps[${String(index)}].port must be a number when present`,
        );
      }
      return op.port === undefined
        ? { kind: "net.connect", host: op.host }
        : { kind: "net.connect", host: op.host, port: op.port };
    case "env.get":
      if (typeof op.name !== "string") {
        throw invalidParams(`params.tool.requiredOps[${String(index)}].name must be a string`);
      }
      return { kind: "env.get", name: op.name };
    case "proc.spawn":
      return { kind: "proc.spawn" };
    default:
      throw invalidParams(
        `params.tool.requiredOps[${String(index)}].kind is not a known operation`,
      );
  }
}

function validateLimits(raw: unknown): InvokeToolParams["limits"] {
  if (raw === undefined) return undefined;
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw invalidParams("params.limits must be an object when present");
  }
  const rawLimits = raw as { wallClockMs?: unknown; memoryMb?: unknown };
  const wallClockMs = rawLimits.wallClockMs;
  const memoryMb = rawLimits.memoryMb;
  if (wallClockMs !== undefined && (typeof wallClockMs !== "number" || wallClockMs <= 0)) {
    throw invalidParams("params.limits.wallClockMs must be a positive number");
  }
  if (memoryMb !== undefined && (typeof memoryMb !== "number" || memoryMb <= 0)) {
    throw invalidParams("params.limits.memoryMb must be a positive number");
  }
  return {
    ...(wallClockMs === undefined ? {} : { wallClockMs }),
    ...(memoryMb === undefined ? {} : { memoryMb }),
  };
}

function invalidParams(message: string): MethodError {
  return new MethodError(JsonRpcErrorCode.InvalidParams, message);
}

/**
 * Translate an isolated-vm thrown value into a typed
 * {@link MethodError}. The library reports timeouts and memory
 * exhaustion via Error.message substrings rather than dedicated
 * subclasses, so we pattern-match on the message and fall back to
 * {@link ToolErrorCode.ToolExecutionError}.
 */
function translateIsolateError(err: unknown): MethodError {
  if (err instanceof MethodError) {
    return err;
  }
  const message = err instanceof Error ? err.message : String(err);
  if (/Script execution timed out/i.test(message)) {
    return toolError(ToolErrorCode.ToolTimeout, "tool execution timed out");
  }
  if (
    /exceeded its memory limit/i.test(message) ||
    /Array buffer allocation failed/i.test(message)
  ) {
    return toolError(ToolErrorCode.ToolMemoryExceeded, "tool exceeded memory limit");
  }
  return toolError(ToolErrorCode.ToolExecutionError, message);
}

function toolError(code: ToolErrorCodeValue, message: string, data?: JsonRpcValue): MethodError {
  // Codes intentionally widen into the application slice of the
  // JSON-RPC server-error range; cast preserves the `MethodError`
  // contract without polluting the protocol-level union in
  // `JsonRpcErrorCode`.
  return new MethodError(code as unknown as JsonRpcErrorCodeValue, message, data);
}
