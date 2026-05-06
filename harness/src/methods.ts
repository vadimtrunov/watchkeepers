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
 * sandbox path via `invokeTool` (see `invokeTool.ts`). M5.3.c.c.c.a
 * threads an optional {@link LLMProvider} through {@link createDefaultRegistry}
 * and registers `complete` / `countTokens` / `reportCost` via
 * {@link wireLLMMethods} when a provider is supplied; the streaming
 * `stream` method is deferred to M5.3.c.c.c.b.
 */

import { z } from "zod";

import { invokeToolHandler } from "./invokeTool.js";
import { wireLLMMethods } from "./llm/methods.js";
import type { NotificationWriter } from "./llm/notification-writer.js";
import type { LLMProvider } from "./llm/provider.js";
import { setActiveToolset } from "./manifest.js";
import {
  JsonRpcErrorCode,
  type JsonRpcErrorCodeValue,
  type JsonRpcValue,
  type HelloResult,
  type ShutdownResult,
} from "./types.js";

/**
 * zod schema for `setManifest` params (M5.5.b.a). The Go core projects
 * `manifest_version.tools` jsonb into a `[]string` of tool names and
 * delivers the list once at session boot via this method. The harness
 * stores it via {@link setActiveToolset} for the `invokeTool` ACL gate.
 *
 * `.strict()` rejects future-protocol fields explicitly so a version
 * skew surfaces as a wire error rather than silent acceptance.
 */
const SetManifestParamsSchema = z
  .object({
    toolset: z.array(z.string()),
  })
  .strict();

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
 * Build the default registry — `hello`, `shutdown`, `invokeTool`, plus
 * the LLM methods when `provider` is supplied. Pure; safe to call once
 * at boot.
 *
 * The optional `provider` parameter (M5.3.c.c.c.a) wires the three
 * synchronous LLM JSON-RPC methods (`complete`, `countTokens`,
 * `reportCost`) via {@link wireLLMMethods}. When omitted, the harness
 * runs in degraded mode and clients calling those methods receive
 * `MethodNotFound` from the dispatcher.
 *
 * The optional `writer` parameter (M5.3.c.c.c.b.a) is forwarded to
 * {@link wireLLMMethods} so the streaming `stream` / `stream/cancel`
 * handlers landing in M5.3.c.c.c.b.b can capture it. When `provider`
 * is omitted the writer is ignored — the LLM surface is absent in
 * degraded mode and there is nothing to receive the closure.
 */
export function createDefaultRegistry(
  signal: ShutdownSignal,
  provider?: LLMProvider,
  writer?: NotificationWriter,
): MethodRegistry {
  const registry = new Map<string, MethodHandler>();

  registry.set("hello", () => {
    return { harness: "watchkeeper", version: HARNESS_VERSION } satisfies HelloResult;
  });

  registry.set("shutdown", () => {
    signal.shouldExit = true;
    return { accepted: true } satisfies ShutdownResult;
  });

  registry.set("invokeTool", invokeToolHandler);

  registry.set("setManifest", (params) => {
    const parsed = SetManifestParamsSchema.safeParse(params);
    if (!parsed.success) {
      throw new MethodError(
        JsonRpcErrorCode.InvalidParams,
        `setManifest: invalid params: ${parsed.error.message}`,
      );
    }
    setActiveToolset(parsed.data.toolset);
    return { ok: true };
  });

  if (provider !== undefined) {
    wireLLMMethods(registry, provider, writer);
  }

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
