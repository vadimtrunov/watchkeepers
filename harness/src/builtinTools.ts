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

import { z } from "zod";

import { type RpcClient } from "./jsonrpc.js";
import { rememberEntry, type RememberEntryParams } from "./notebookClient.js";
import { createSlackApp, type SlackAppCreateParams } from "./slackClient.js";
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
 * Typed error a built-in handler throws when the wire-level `input`
 * fails the handler's local zod schema (e.g. missing
 * `approval_token` for `slack_app_create`). The dispatcher in
 * `invokeTool.ts` lets it propagate; callers see a generic
 * {@link ToolErrorCode.ToolExecutionError} with the zod issue message
 * attached. Carrying a dedicated class lets future dispatch wiring
 * (M6.2) tell "shape mismatch" apart from "remote RPC error" without
 * string-matching, and keeps the registry signature stable.
 */
export class BuiltinInvalidInput extends Error {
  public constructor(toolName: string, message: string) {
    super(`builtin tool ${toolName}: invalid input: ${message}`);
    this.name = "BuiltinInvalidInput";
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
 * Zod schema for the `slack_app_create` builtin tool input. Mirrors
 * the wire shape the Go-side handler decodes
 * (`slackAppCreateParams` in `core/pkg/harnessrpc/slack_app_create.go`)
 * — snake_case field names, required `app_name` + `approval_token`
 * + `scopes` array of strings, optional `app_description`.
 *
 * The `agent_id` field is NOT in the schema because the dispatcher
 * supplies it from the active manifest (the same way the `remember`
 * builtin does). A wire-level `agent_id` would let a tool caller
 * spoof the audit row's `agent_id` field, defeating the M6.1.b
 * privileged-action audit contract.
 *
 * `.strict()` so a future protocol field on the wire surfaces as a
 * validation error rather than being silently dropped — matches the
 * conservative posture of {@link CapabilityDeclarationSchema} and
 * {@link ToolInputSpec}.
 */
const SlackAppCreateInputSchema = z
  .object({
    app_name: z.string().min(1, "app_name must be a non-empty string"),
    app_description: z.string().optional(),
    scopes: z.array(z.string()).default([]),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `slack_app_create` built-in (M6.1.b) — forwards to the Go-side
 * `slack.app_create` method via {@link createSlackApp}. Throws
 * {@link BuiltinAgentIDMissing} when the manifest never delivered an
 * `agentID`; throws {@link BuiltinInvalidInput} on a local zod-shape
 * mismatch BEFORE any outbound RPC. Downstream Go-side validation
 * (claim authority, secrets read, audit chain) surfaces malformed
 * payloads as JSON-RPC ToolUnauthorized / ApprovalRequired /
 * InvalidParams / InternalError per the M6.1.b sentinel mapping.
 */
const slackAppCreateHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("slack_app_create");
  }
  const parsed = SlackAppCreateInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "slack_app_create",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  // The dispatcher injects the manifest-provided `agent_id`; the
  // wire-level input MUST NOT carry it. See SlackAppCreateInputSchema
  // docblock for the audit-spoofing rationale.
  const params: SlackAppCreateParams = {
    agent_id: agentID,
    app_name: parsed.data.app_name,
    ...(parsed.data.app_description === undefined
      ? {}
      : { app_description: parsed.data.app_description }),
    scopes: parsed.data.scopes,
    approval_token: parsed.data.approval_token,
  };
  const result = await createSlackApp(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Read-only registry of built-in tools. Indexed by the wire-level
 * `tool.name` from `invokeTool.params.tool`. Adding a new built-in is
 * a single-line edit; no dispatch-branch change is needed.
 */
export const builtinHandlers: ReadonlyMap<string, BuiltinHandler> = new Map<string, BuiltinHandler>(
  [
    ["remember", rememberHandler],
    ["slack_app_create", slackAppCreateHandler],
  ],
);

/**
 * Lookup helper symmetric with {@link builtinHandlers.get}. Wraps the
 * Map access so future migrations (e.g. lazy-loaded built-ins) can
 * change the storage without rippling through call sites.
 */
export function getBuiltinHandler(name: string): BuiltinHandler | undefined {
  return builtinHandlers.get(name);
}
