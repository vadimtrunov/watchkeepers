/**
 * Method registry for the JSON-RPC dispatcher.
 *
 * Each method is a `(params) => result | Promise<result>` callback. The
 * dispatcher catches handler exceptions and lifts them into
 * {@link JsonRpcErrorCode.InternalError} responses; handlers SHOULD
 * raise {@link MethodError} when they want to control the wire-level
 * error code (e.g. InvalidParams from a future zod-validating handler).
 *
 * M5.3.a registered `hello` and `shutdown`. M5.3.b.a adds the pure-JS
 * sandbox path via `invokeTool` (see `invokeTool.ts`). The remaining
 * Claude Code passthrough and zod-derived tool schemas land in
 * M5.3.b.b/c/d as separate sub-tasks.
 */

import { invokeToolHandler } from "./invokeTool.js";
import {
  JsonRpcErrorCode,
  type JsonRpcErrorCodeValue,
  type JsonRpcValue,
  type HelloResult,
  type ShutdownResult,
} from "./types.js";

/**
 * Harness implementation version. Bumped when wire-protocol semantics
 * change; the Go core MAY refuse to drive an unsupported version.
 */
export const HARNESS_VERSION = "0.1.0";

/**
 * Method-handler return: a JSON-RPC value the dispatcher serializes
 * back to the caller. Async handlers are supported but M5.3.a does not
 * use them — kept here so M5.3.b/c can plug in tool calls without
 * widening the registry signature.
 */
export type MethodHandler = (
  params: JsonRpcValue | undefined,
) => JsonRpcValue | Promise<JsonRpcValue>;

/**
 * Error carrying a JSON-RPC error code. Handlers throw this when they
 * want to steer the dispatcher away from the default `InternalError`
 * fallback.
 */
export class MethodError extends Error {
  public readonly code: JsonRpcErrorCodeValue;
  public readonly data: JsonRpcValue | undefined;

  public constructor(code: JsonRpcErrorCodeValue, message: string, data?: JsonRpcValue) {
    super(message);
    this.name = "MethodError";
    this.code = code;
    this.data = data;
  }
}

/**
 * Mutable registry. The harness builds one at boot via
 * {@link createDefaultRegistry} and the dispatcher reads from it.
 * Public so tests can drive a custom registry through the dispatcher
 * without touching stdio.
 */
export type MethodRegistry = ReadonlyMap<string, MethodHandler>;

/**
 * Side-channel signal raised by `shutdown` so the dispatcher can drain
 * its output and exit the loop. The handler still returns a normal
 * JSON-RPC result before this is observed, so the client sees an `accepted`
 * response before the harness closes its streams.
 */
export interface ShutdownSignal {
  shouldExit: boolean;
}

/**
 * Build the default registry — `hello` and `shutdown`. Pure; safe to
 * call once at boot.
 */
export function createDefaultRegistry(signal: ShutdownSignal): MethodRegistry {
  const registry = new Map<string, MethodHandler>();

  registry.set("hello", () => {
    return { harness: "watchkeeper", version: HARNESS_VERSION } satisfies HelloResult;
  });

  registry.set("shutdown", () => {
    signal.shouldExit = true;
    return { accepted: true } satisfies ShutdownResult;
  });

  registry.set("invokeTool", invokeToolHandler);

  return registry;
}

/**
 * Outcome of dispatching a single request through the registry. The
 * dispatcher converts this into a serializable JSON-RPC response.
 *
 * The discriminator is `kind`: `ok` carries a result, `error` carries
 * an error envelope without throwing.
 */
export type DispatchOutcome =
  | { readonly kind: "ok"; readonly result: JsonRpcValue }
  | {
      readonly kind: "error";
      readonly code: JsonRpcErrorCodeValue;
      readonly message: string;
      readonly data?: JsonRpcValue;
    };

/**
 * Run a method against the registry. Catches handler exceptions and
 * returns them as a structured error outcome — the dispatcher never
 * needs a try/catch around this call.
 */
export async function dispatch(
  registry: MethodRegistry,
  method: string,
  params: JsonRpcValue | undefined,
): Promise<DispatchOutcome> {
  const handler = registry.get(method);
  if (handler === undefined) {
    return {
      kind: "error",
      code: JsonRpcErrorCode.MethodNotFound,
      message: `method not found: ${method}`,
    };
  }

  try {
    const result = await handler(params);
    return { kind: "ok", result };
  } catch (err) {
    if (err instanceof MethodError) {
      return err.data === undefined
        ? { kind: "error", code: err.code, message: err.message }
        : { kind: "error", code: err.code, message: err.message, data: err.data };
    }
    return {
      kind: "error",
      code: JsonRpcErrorCode.InternalError,
      message: err instanceof Error ? err.message : "internal error",
    };
  }
}
