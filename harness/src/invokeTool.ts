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

import { MethodError } from "./methods.js";
import { JsonRpcErrorCode, type JsonRpcErrorCodeValue, type JsonRpcValue } from "./types.js";

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
 * Wire shape of `invokeTool` params. Discriminator is `tool.kind` —
 * future M5.3.b.b will add a second variant `worker-process` for the
 * I/O-gated tool path. Today only `isolated-vm` is accepted.
 */
export interface InvokeToolParams {
  readonly tool: {
    readonly kind: "isolated-vm";
    readonly source: string;
  };
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
 * allocating an Isolate (AC6) and delegates to {@link runIsolatedJs} on
 * success. Returns the canonical `{ output }` envelope.
 */
export async function invokeToolHandler(params: JsonRpcValue | undefined): Promise<JsonRpcValue> {
  const validated = validateParams(params);
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
  const tool = obj.tool as { kind?: unknown; source?: unknown };
  if (tool.kind !== "isolated-vm") {
    throw invalidParams('params.tool.kind must be "isolated-vm"');
  }
  if (typeof tool.source !== "string") {
    throw invalidParams("params.tool.source must be a string");
  }
  if (!("input" in obj)) {
    throw invalidParams("params.input is required");
  }
  let limits: InvokeToolParams["limits"];
  if (obj.limits !== undefined) {
    if (typeof obj.limits !== "object" || obj.limits === null || Array.isArray(obj.limits)) {
      throw invalidParams("params.limits must be an object when present");
    }
    const rawLimits = obj.limits as { wallClockMs?: unknown; memoryMb?: unknown };
    const wallClockMs = rawLimits.wallClockMs;
    const memoryMb = rawLimits.memoryMb;
    if (wallClockMs !== undefined && (typeof wallClockMs !== "number" || wallClockMs <= 0)) {
      throw invalidParams("params.limits.wallClockMs must be a positive number");
    }
    if (memoryMb !== undefined && (typeof memoryMb !== "number" || memoryMb <= 0)) {
      throw invalidParams("params.limits.memoryMb must be a positive number");
    }
    limits = {
      ...(wallClockMs === undefined ? {} : { wallClockMs }),
      ...(memoryMb === undefined ? {} : { memoryMb }),
    };
  }
  return limits === undefined
    ? {
        tool: { kind: "isolated-vm", source: tool.source },
        input: obj.input as JsonRpcValue,
      }
    : {
        tool: { kind: "isolated-vm", source: tool.source },
        input: obj.input as JsonRpcValue,
        limits,
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

function toolError(code: ToolErrorCodeValue, message: string): MethodError {
  // Codes intentionally widen into the application slice of the
  // JSON-RPC server-error range; cast preserves the `MethodError`
  // contract without polluting the protocol-level union in
  // `JsonRpcErrorCode`.
  return new MethodError(code as unknown as JsonRpcErrorCodeValue, message);
}
