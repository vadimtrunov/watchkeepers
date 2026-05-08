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
import {
  adjustLanguage,
  adjustPersonality,
  listWatchkeepers,
  promoteToKeep,
  proposeSpawn,
  reportCost,
  reportHealth,
  retireWatchkeeper,
  type AdjustLanguageParams,
  type AdjustPersonalityParams,
  type ListWatchkeepersParams,
  type PromoteToKeepParams,
  type ProposeSpawnParams,
  type ReportCostParams,
  type ReportHealthParams,
  type RetireWatchkeeperParams,
} from "./watchmasterClient.js";

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
 * Zod schema for the `list_watchkeepers` builtin tool input (M6.2.a).
 * Mirrors the Go-side `listWatchkeepersParams` decoder
 * (`core/pkg/harnessrpc/watchmaster_readonly.go`) — both fields
 * optional, snake_case names. `.strict()` rejects unknown wire fields
 * for the same reason {@link SlackAppCreateInputSchema} does.
 *
 * Unlike the M6.1.b `slack_app_create` schema, this one carries NO
 * `agent_id` injection from the manifest: `agent_id` is not part of
 * the request shape. The Go-side claim resolver handles tenant
 * scoping; the harness ACL gate (M5.5.b.a) handles toolset
 * authorisation. The list_watchkeepers tool reads tenant-wide.
 */
const ListWatchkeepersInputSchema = z
  .object({
    status: z.string().optional(),
    limit: z.number().int().min(0).optional(),
  })
  .strict();

/**
 * `list_watchkeepers` built-in (M6.2.a) — forwards to the Go-side
 * `watchmaster.list_watchkeepers` method via {@link listWatchkeepers}.
 * Throws {@link BuiltinAgentIDMissing} when the manifest never set an
 * `agentID` (the read-only tools still require an authenticated
 * caller — the deny-by-default M5.5.b.a posture). Throws
 * {@link BuiltinInvalidInput} on a local zod-shape mismatch BEFORE
 * any outbound RPC.
 */
const listWatchkeepersHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("list_watchkeepers");
  }
  // Empty input is valid — both schema fields are optional.
  const parsed = ListWatchkeepersInputSchema.safeParse(input ?? {});
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "list_watchkeepers",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: ListWatchkeepersParams = {
    ...(parsed.data.status === undefined ? {} : { status: parsed.data.status }),
    ...(parsed.data.limit === undefined ? {} : { limit: parsed.data.limit }),
  };
  const result = await listWatchkeepers(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `report_cost` builtin tool input (M6.2.a). Every
 * field is optional; the Go-side defaults handle the unset case.
 *
 * The `agent_id` field IS in this schema (unlike `slack_app_create`)
 * because it is the TARGET-narrowing field, not the calling agent's
 * identity. The calling agent's identity is handled out-of-band by
 * the Go-side claim resolver and the harness ACL gate.
 */
const ReportCostInputSchema = z
  .object({
    agent_id: z.string().optional(),
    event_type_prefix: z.string().optional(),
    limit: z.number().int().min(0).optional(),
  })
  .strict();

/**
 * `report_cost` built-in (M6.2.a) — forwards to the Go-side
 * `watchmaster.report_cost` method via {@link reportCost}. Throws
 * {@link BuiltinAgentIDMissing} on a missing manifest agent;
 * {@link BuiltinInvalidInput} on a zod-shape mismatch.
 */
const reportCostHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("report_cost");
  }
  const parsed = ReportCostInputSchema.safeParse(input ?? {});
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "report_cost",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: ReportCostParams = {
    ...(parsed.data.agent_id === undefined ? {} : { agent_id: parsed.data.agent_id }),
    ...(parsed.data.event_type_prefix === undefined
      ? {}
      : { event_type_prefix: parsed.data.event_type_prefix }),
    ...(parsed.data.limit === undefined ? {} : { limit: parsed.data.limit }),
  };
  const result = await reportCost(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `report_health` builtin tool input (M6.2.a).
 * The single `agent_id` field is optional — empty means "org-wide
 * aggregation".
 */
const ReportHealthInputSchema = z
  .object({
    agent_id: z.string().optional(),
  })
  .strict();

/**
 * `report_health` built-in (M6.2.a) — forwards to the Go-side
 * `watchmaster.report_health` method via {@link reportHealth}.
 */
const reportHealthHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("report_health");
  }
  const parsed = ReportHealthInputSchema.safeParse(input ?? {});
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "report_health",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: ReportHealthParams =
    parsed.data.agent_id === undefined ? {} : { agent_id: parsed.data.agent_id };
  const result = await reportHealth(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `propose_spawn` builtin tool input (M6.2.b).
 * Mirrors the Go-side `proposeSpawnParams` decoder
 * (`core/pkg/harnessrpc/watchmaster_writes.go`) — snake_case names,
 * required `agent_id` + `system_prompt` + `approval_token`, optional
 * `personality` + `language`. `.strict()` rejects unknown wire fields
 * for the same reason {@link SlackAppCreateInputSchema} does.
 *
 * Unlike the M6.1.b `slack_app_create` schema, this schema's
 * `agent_id` field is part of the wire request shape — it is the
 * TARGET agent the new manifest_version row will be pinned to (a
 * freshly-allocated manifest UUID supplied by the caller). The
 * calling agent's identity is handled out-of-band by the Go-side
 * claim resolver and the harness ACL gate; the dispatcher does NOT
 * inject it here.
 */
const ProposeSpawnInputSchema = z
  .object({
    agent_id: z.string().min(1, "agent_id must be a non-empty string"),
    system_prompt: z.string().min(1, "system_prompt must be a non-empty string"),
    personality: z.string().optional(),
    language: z.string().optional(),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `propose_spawn` built-in (M6.2.b) — forwards to the Go-side
 * `watchmaster.propose_spawn` method via {@link proposeSpawn}. Throws
 * {@link BuiltinAgentIDMissing} when the manifest never delivered an
 * `agentID`; throws {@link BuiltinInvalidInput} on a local zod-shape
 * mismatch BEFORE any outbound RPC.
 */
const proposeSpawnHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("propose_spawn");
  }
  const parsed = ProposeSpawnInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "propose_spawn",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: ProposeSpawnParams = {
    agent_id: parsed.data.agent_id,
    system_prompt: parsed.data.system_prompt,
    ...(parsed.data.personality === undefined ? {} : { personality: parsed.data.personality }),
    ...(parsed.data.language === undefined ? {} : { language: parsed.data.language }),
    approval_token: parsed.data.approval_token,
  };
  const result = await proposeSpawn(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `adjust_personality` builtin tool input (M6.2.b).
 * Mirrors the Go-side `adjustPersonalityParams` decoder. The
 * `new_personality` field allows empty strings — the keepclient
 * preflight only caps the upper bound at 1024 runes; an empty value
 * round-trips as SQL NULL on the server.
 */
const AdjustPersonalityInputSchema = z
  .object({
    agent_id: z.string().min(1, "agent_id must be a non-empty string"),
    new_personality: z.string(),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `adjust_personality` built-in (M6.2.b) — forwards to the Go-side
 * `watchmaster.adjust_personality` method via {@link adjustPersonality}.
 */
const adjustPersonalityHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("adjust_personality");
  }
  const parsed = AdjustPersonalityInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "adjust_personality",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: AdjustPersonalityParams = {
    agent_id: parsed.data.agent_id,
    new_personality: parsed.data.new_personality,
    approval_token: parsed.data.approval_token,
  };
  const result = await adjustPersonality(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `adjust_language` builtin tool input (M6.2.b).
 * Mirrors the Go-side `adjustLanguageParams` decoder.
 */
const AdjustLanguageInputSchema = z
  .object({
    agent_id: z.string().min(1, "agent_id must be a non-empty string"),
    new_language: z.string(),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `adjust_language` built-in (M6.2.b) — forwards to the Go-side
 * `watchmaster.adjust_language` method via {@link adjustLanguage}.
 */
const adjustLanguageHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("adjust_language");
  }
  const parsed = AdjustLanguageInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "adjust_language",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: AdjustLanguageParams = {
    agent_id: parsed.data.agent_id,
    new_language: parsed.data.new_language,
    approval_token: parsed.data.approval_token,
  };
  const result = await adjustLanguage(rpc, params);
  return result as unknown as JsonRpcValue;
};

/**
 * Zod schema for the `retire_watchkeeper` builtin tool input (M6.2.c).
 * Mirrors the Go-side `retireWatchkeeperParams` decoder
 * (`core/pkg/harnessrpc/retire_watchkeeper.go`) — snake_case names,
 * required `agent_id` + `approval_token`. `.strict()` rejects unknown
 * wire fields for the same reason {@link SlackAppCreateInputSchema}
 * does.
 *
 * The `agent_id` field IS in the schema because it is the TARGET
 * watchkeeper row id — the watchkeeper to retire — NOT the calling
 * agent's identity. The calling agent's identity is handled out-of-band
 * by the Go-side claim resolver and the harness ACL gate.
 */
const RetireWatchkeeperInputSchema = z
  .object({
    agent_id: z.string().min(1, "agent_id must be a non-empty string"),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `retire_watchkeeper` built-in (M6.2.c) — forwards to the Go-side
 * `watchmaster.retire_watchkeeper` method via {@link retireWatchkeeper}.
 * Throws {@link BuiltinAgentIDMissing} when the manifest never set an
 * `agentID` (the deny-by-default M5.5.b.a posture). Throws
 * {@link BuiltinInvalidInput} on a local zod-shape mismatch BEFORE
 * any outbound RPC.
 */
const retireWatchkeeperHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("retire_watchkeeper");
  }
  const parsed = RetireWatchkeeperInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "retire_watchkeeper",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: RetireWatchkeeperParams = {
    agent_id: parsed.data.agent_id,
    approval_token: parsed.data.approval_token,
  };
  const result = await retireWatchkeeper(rpc, params);
  // RetireWatchkeeperResult is `Record<string, never>` (an empty object
  // envelope); already assignable to JsonRpcValue without a cast.
  return result;
};

/**
 * Zod schema for the `promote_to_keep` builtin tool input (M6.2.d).
 * Mirrors the Go-side `promoteToKeepParams` decoder
 * (`core/pkg/harnessrpc/promote_to_keep.go`) — snake_case names,
 * required `agent_id` + `notebook_entry_id` + `approval_token`.
 * `.strict()` rejects unknown wire fields for the same reason
 * {@link SlackAppCreateInputSchema} does.
 *
 * The `agent_id` field IS in the schema because it is the calling
 * agent's id (the source notebook owner) — NOT the manifest UUID,
 * NOT a target watchkeeper. The Go-side handler picks the request's
 * `agent_id` for the audit row when present and falls back to the
 * claim's `AgentID` otherwise (`pickAgentForBump`).
 */
const PromoteToKeepInputSchema = z
  .object({
    agent_id: z.string().min(1, "agent_id must be a non-empty string"),
    notebook_entry_id: z.string().min(1, "notebook_entry_id must be a non-empty string"),
    approval_token: z.string().min(1, "approval_token must be a non-empty string"),
  })
  .strict();

/**
 * `promote_to_keep` built-in (M6.2.d) — forwards to the Go-side
 * `watchmaster.promote_to_keep` method via {@link promoteToKeep}.
 * Throws {@link BuiltinAgentIDMissing} when the manifest never set an
 * `agentID` (the deny-by-default M5.5.b.a posture). Throws
 * {@link BuiltinInvalidInput} on a local zod-shape mismatch BEFORE
 * any outbound RPC.
 *
 * The Go-side handler lifts the referenced notebook entry into a
 * fresh `watchkeeper.knowledge_chunk` row via the keep server's
 * POST /v1/knowledge-chunks endpoint. On a notebook-entry-missing
 * the wire surfaces -32011 (ToolNotFound); a TS caller branches on
 * the wire `code` without string-matching.
 */
const promoteToKeepHandler: BuiltinHandler = async (rpc, agentID, input) => {
  if (agentID === undefined) {
    throw new BuiltinAgentIDMissing("promote_to_keep");
  }
  const parsed = PromoteToKeepInputSchema.safeParse(input);
  if (!parsed.success) {
    throw new BuiltinInvalidInput(
      "promote_to_keep",
      parsed.error.issues[0]?.message ?? parsed.error.message,
    );
  }
  const params: PromoteToKeepParams = {
    agent_id: parsed.data.agent_id,
    notebook_entry_id: parsed.data.notebook_entry_id,
    approval_token: parsed.data.approval_token,
  };
  const result = await promoteToKeep(rpc, params);
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
    ["list_watchkeepers", listWatchkeepersHandler],
    ["report_cost", reportCostHandler],
    ["report_health", reportHealthHandler],
    ["propose_spawn", proposeSpawnHandler],
    ["adjust_personality", adjustPersonalityHandler],
    ["adjust_language", adjustLanguageHandler],
    ["retire_watchkeeper", retireWatchkeeperHandler],
    ["promote_to_keep", promoteToKeepHandler],
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
