/**
 * Built-in tool registry (M5.5.d.b).
 *
 * "Built-in" tools are first-party harness operations that route to a
 * Go-side JSON-RPC method via the bidirectional {@link RpcClient} (see
 * `jsonrpc.ts`, M5.5.d.a.a). They are dispatched alongside the
 * `isolated-vm` (in-process sandbox) and `worker` (forked child) kinds
 * by {@link invokeToolHandler} (`invokeTool.ts`).
 *
 * Today only `"remember"` is registered: it forwards to
 * {@link rememberEntry} which calls the Go host's `notebook.remember`
 * (M5.5.d.a.b). New built-ins land here without touching the dispatch
 * branch in `invokeTool.ts` — register a {@link BuiltinHandler} in
 * {@link builtinHandlers} and the dispatcher picks it up on the next
 * call.
 *
 * All built-in handlers share the same signature: they receive the
 * shared {@link RpcClient} for outbound calls, the manifest-declared
 * `agentID` (or `undefined` when no manifest has been delivered with
 * an identity), and the wire-level `input` payload from
 * `invokeTool.params.input`.
 *
 * Error contract: handlers throw {@link BuiltinAgentIDMissing} when
 * the call requires an agent identity but the manifest never set one;
 * the dispatcher in `invokeTool.ts` catches this and maps it to
 * {@link ToolErrorCode.ToolUnauthorized} on the wire. Any other thrown
 * error propagates to the dispatcher as-is, where it is translated by
 * the standard {@link MethodError} → JSON-RPC error path.
 */

import { type RpcClient } from "./jsonrpc.js";
import { rememberEntry, type RememberEntryParams } from "./notebookClient.js";
import type { JsonRpcValue } from "./types.js";

/**
 * Typed error a built-in handler throws when the call requires an
 * agent identity but the manifest never delivered one. The dispatcher
 * in `invokeTool.ts` catches this and surfaces
 * {@link ToolErrorCode.ToolUnauthorized} on the wire. Carrying a
 * dedicated class lets the dispatcher tell "missing identity" apart
 * from "remote RPC error" without string-matching the message.
 */
export class BuiltinAgentIDMissing extends Error {
  public constructor(toolName: string) {
    super(`builtin tool ${toolName} requires an agentID but the manifest has not set one`);
    this.name = "BuiltinAgentIDMissing";
  }
}

/**
 * Signature every built-in tool handler implements.
 *
 * @param rpc The shared {@link RpcClient} used to invoke Go-side
 *   JSON-RPC methods. Per-call rather than module-state so tests can
 *   inject a stub without driving a real bidirectional pipe.
 * @param agentID The manifest-declared agent identifier, or
 *   `undefined` when no identity was set. Handlers that need an
 *   identity MUST throw {@link BuiltinAgentIDMissing} on a missing
 *   value rather than letting the call succeed with a synthetic id.
 * @param input The wire-level `params.input` payload from
 *   `invokeTool`. Handlers narrow this with their own runtime shape
 *   check; widening to `unknown` here keeps the registry signature
 *   stable as new built-ins land.
 */
export type BuiltinHandler = (
  rpc: RpcClient,
  agentID: string | undefined,
  input: JsonRpcValue,
) => Promise<JsonRpcValue>;

/**
 * `remember` built-in — forwards to the Go-side `notebook.remember`
 * method via {@link rememberEntry} (M5.5.d.a.b). Throws
 * {@link BuiltinAgentIDMissing} when the manifest never delivered an
 * `agentID`. The remaining input fields (`category`, `subject`,
 * `content`) are forwarded verbatim; downstream Go-side validation
 * surfaces malformed payloads as JSON-RPC InvalidParams.
 */
const rememberHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("remember");
  }
  // Narrow the wire-level payload to the fields `notebook.remember`
  // requires. The Go host re-validates each field; this cast only
  // shapes the call into the {@link RememberEntryParams} contract for
  // {@link rememberEntry}.
  const obj = (input ?? {}) as Record<string, unknown>;
  const params: RememberEntryParams = {
    agentID,
    category: typeof obj.category === "string" ? obj.category : "",
    subject: typeof obj.subject === "string" ? obj.subject : "",
    content: typeof obj.content === "string" ? obj.content : "",
  };
  const result = await rememberEntry(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Read-only registry of built-in tools. Indexed by the wire-level
 * `tool.name` from `invokeTool.params.tool`. Adding a new built-in is
 * a single-line edit; no dispatch-branch change is needed.
 */
export const builtinHandlers: ReadonlyMap<string, BuiltinHandler> = new Map<string, BuiltinHandler>(
  [["remember", rememberHandler]],
);

/**
 * Lookup helper symmetric with {@link builtinHandlers.get}. Wraps the
 * Map access so future migrations (e.g. lazy-loaded built-ins) can
 * change the storage without rippling through call sites.
 */
export function getBuiltinHandler(name: string): BuiltinHandler | undefined {
  return builtinHandlers.get(name);
}
