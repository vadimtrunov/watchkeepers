/**
 * JSON-RPC bridge for the synchronous {@link LLMProvider} surface
 * (M5.3.c.c.c.a). Wires `complete`, `countTokens`, and `reportCost`
 * into the harness method registry so the Go core can drive an LLM
 * provider over NDJSON / stdio without owning the SDK directly.
 *
 * The streaming `stream` method is intentionally NOT registered here —
 * it lands in M5.3.c.c.c.b together with the multi-event server-to-
 * client notification protocol it requires.
 *
 * Validation happens BEFORE the provider call: malformed wire payloads
 * surface as {@link MethodError}(InvalidParams) without burning any
 * upstream API quota. {@link LLMError} thrown from the provider is
 * funneled through {@link mapLLMErrorToMethodError} so the discriminator
 * code rides on `MethodError.data.code` for the caller.
 */

import { MethodError, type MethodHandler } from "../methods.js";
import { JsonRpcErrorCode, type JsonRpcValue } from "../types.js";

import { LLMError, type LLMErrorCode } from "./errors.js";
import type { LLMProvider } from "./provider.js";
import {
  ROLES,
  model as toModel,
  type CompleteRequest,
  type CompleteResponse,
  type CountTokensRequest,
  type FinishReason,
  type Message,
  type Model,
  type Role,
  type ToolCall,
  type ToolDefinition,
  type Usage,
} from "./types.js";

/**
 * Wire shape of a `complete` JSON-RPC request's `params`. Mirrors
 * {@link CompleteRequest} field-for-field with the brand on `model`
 * relaxed to `string` because branded types do not survive JSON.
 */
export interface CompleteParams {
  readonly model: string;
  readonly system?: string;
  readonly messages: readonly Message[];
  readonly maxTokens?: number;
  readonly temperature?: number;
  readonly tools?: readonly ToolDefinition[];
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Wire shape of a `countTokens` JSON-RPC request's `params`. Mirrors
 * {@link CountTokensRequest}.
 */
export interface CountTokensParams {
  readonly model: string;
  readonly system?: string;
  readonly messages: readonly Message[];
  readonly tools?: readonly ToolDefinition[];
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Wire shape of a `reportCost` JSON-RPC request's `params`.
 */
export interface ReportCostParams {
  readonly runtimeID: string;
  readonly usage: Usage;
}

/**
 * Register the three synchronous LLM methods on `registry` against
 * `provider`. Idempotent in spirit but a re-registration overwrites the
 * prior handler (Map semantics). Stream is intentionally absent —
 * deferred to M5.3.c.c.c.b.
 */
export function wireLLMMethods(registry: Map<string, MethodHandler>, provider: LLMProvider): void {
  registry.set("complete", makeCompleteHandler(provider));
  registry.set("countTokens", makeCountTokensHandler(provider));
  registry.set("reportCost", makeReportCostHandler(provider));
}

function makeCompleteHandler(provider: LLMProvider): MethodHandler {
  return async (params: JsonRpcValue | undefined): Promise<JsonRpcValue> => {
    const req = parseCompleteParams(params);
    let resp: CompleteResponse;
    try {
      resp = await provider.complete(req);
    } catch (e) {
      throw liftProviderError(e);
    }
    return completeResponseToWire(resp);
  };
}

function makeCountTokensHandler(provider: LLMProvider): MethodHandler {
  return async (params: JsonRpcValue | undefined): Promise<JsonRpcValue> => {
    const req = parseCountTokensParams(params);
    let count: number;
    try {
      count = await provider.countTokens(req);
    } catch (e) {
      throw liftProviderError(e);
    }
    return { inputTokens: count };
  };
}

function makeReportCostHandler(provider: LLMProvider): MethodHandler {
  return async (params: JsonRpcValue | undefined): Promise<JsonRpcValue> => {
    const { runtimeID, usage } = parseReportCostParams(params);
    try {
      await provider.reportCost(runtimeID, usage);
    } catch (e) {
      throw liftProviderError(e);
    }
    return { accepted: true };
  };
}

/**
 * Re-throw `e` as a {@link MethodError} when it is an {@link LLMError};
 * otherwise rethrow verbatim so the dispatcher's default `InternalError`
 * fallback handles it.
 */
function liftProviderError(e: unknown): unknown {
  if (e instanceof LLMError) {
    return mapLLMErrorToMethodError(e);
  }
  return e;
}

/**
 * Centralised {@link LLMError} → {@link MethodError} mapping. The
 * discriminator code rides on `MethodError.data.code` so the wire-level
 * caller can pattern-match without re-parsing the message.
 *
 * Mapping table (per TASK AC5):
 *
 *   - `invalid_prompt`       → InvalidParams
 *   - `model_not_supported`  → InvalidParams
 *   - `token_limit_exceeded` → InvalidParams
 *   - `invalid_manifest`     → InvalidParams
 *   - `provider_unavailable` → InternalError
 *   - `invalid_handler`      → InternalError (defensive — not reachable
 *                              from the three sync methods)
 *   - `stream_closed`        → InternalError (defensive — see above)
 */
export function mapLLMErrorToMethodError(e: LLMError): MethodError {
  const code = e.code;
  const data: JsonRpcValue = { code };
  return new MethodError(jsonRpcCodeFor(code), e.message, data);
}

function jsonRpcCodeFor(
  code: LLMErrorCode,
): typeof JsonRpcErrorCode.InvalidParams | typeof JsonRpcErrorCode.InternalError {
  switch (code) {
    case "invalid_prompt":
    case "model_not_supported":
    case "token_limit_exceeded":
    case "invalid_manifest":
      return JsonRpcErrorCode.InvalidParams;
    case "provider_unavailable":
    case "invalid_handler":
    case "stream_closed":
      return JsonRpcErrorCode.InternalError;
  }
}

/* -----------------------------------------------------------------------
 * Param parsing — tight inline validators (no zod dependency).
 * --------------------------------------------------------------------- */

function parseCompleteParams(params: JsonRpcValue | undefined): CompleteRequest {
  const obj = requireObject(params, "params");
  const model = parseModel(obj.model);
  const messages = parseMessages(obj.messages);
  const tools = parseTools(obj.tools);
  const system = parseOptionalString(obj.system, "system");
  const maxTokens = parseOptionalNonNegativeNumber(obj.maxTokens, "maxTokens");
  const temperature = parseOptionalNumber(obj.temperature, "temperature");
  const metadata = parseOptionalStringRecord(obj.metadata, "metadata");

  const req: CompleteRequest = {
    model,
    messages,
    ...(system !== undefined ? { system } : {}),
    ...(maxTokens !== undefined ? { maxTokens } : {}),
    ...(temperature !== undefined ? { temperature } : {}),
    ...(tools !== undefined ? { tools } : {}),
    ...(metadata !== undefined ? { metadata } : {}),
  };
  return req;
}

function parseCountTokensParams(params: JsonRpcValue | undefined): CountTokensRequest {
  const obj = requireObject(params, "params");
  const model = parseModel(obj.model);
  const messages = parseMessages(obj.messages);
  const tools = parseTools(obj.tools);
  const system = parseOptionalString(obj.system, "system");
  const metadata = parseOptionalStringRecord(obj.metadata, "metadata");

  const req: CountTokensRequest = {
    model,
    messages,
    ...(system !== undefined ? { system } : {}),
    ...(tools !== undefined ? { tools } : {}),
    ...(metadata !== undefined ? { metadata } : {}),
  };
  return req;
}

function parseReportCostParams(params: JsonRpcValue | undefined): {
  runtimeID: string;
  usage: Usage;
} {
  const obj = requireObject(params, "params");
  const runtimeID = obj.runtimeID;
  if (typeof runtimeID !== "string" || runtimeID.length === 0) {
    throw invalidParams("runtimeID must be a non-empty string");
  }
  const usage = parseUsage(obj.usage);
  return { runtimeID, usage };
}

function parseModel(v: JsonRpcValue | undefined): Model {
  if (typeof v !== "string" || v.length === 0) {
    throw invalidParams("model must be a non-empty string");
  }
  return toModel(v);
}

function parseMessages(v: JsonRpcValue | undefined): readonly Message[] {
  if (!Array.isArray(v)) {
    throw invalidParams("messages must be a non-empty array");
  }
  if (v.length === 0) {
    throw invalidParams("messages must be a non-empty array");
  }
  const out: Message[] = [];
  for (let i = 0; i < v.length; i++) {
    out.push(parseMessage(v[i] as JsonRpcValue, i));
  }
  return out;
}

function parseMessage(v: JsonRpcValue, idx: number): Message {
  if (!isPlainObject(v)) {
    throw invalidParams(`messages[${String(idx)}] must be an object`);
  }
  const role = v.role;
  if (typeof role !== "string" || !isRole(role)) {
    throw invalidParams(`messages[${String(idx)}].role must be one of ${ROLES.join(",")}`);
  }
  const content = v.content;
  if (typeof content !== "string") {
    throw invalidParams(`messages[${String(idx)}].content must be a string`);
  }
  const metadata = parseOptionalStringRecord(v.metadata, `messages[${String(idx)}].metadata`);
  return metadata === undefined ? { role, content } : { role, content, metadata };
}

function isRole(v: string): v is Role {
  return (ROLES as readonly string[]).includes(v);
}

function parseTools(v: JsonRpcValue | undefined): readonly ToolDefinition[] | undefined {
  if (v === undefined) return undefined;
  if (!Array.isArray(v)) {
    throw invalidParams("tools must be an array when present");
  }
  const out: ToolDefinition[] = [];
  for (let i = 0; i < v.length; i++) {
    out.push(parseTool(v[i] as JsonRpcValue, i));
  }
  return out;
}

function parseTool(v: JsonRpcValue, idx: number): ToolDefinition {
  if (!isPlainObject(v)) {
    throw invalidParams(`tools[${String(idx)}] must be an object`);
  }
  const name = v.name;
  if (typeof name !== "string" || name.length === 0) {
    throw invalidParams(`tools[${String(idx)}].name must be a non-empty string`);
  }
  const description = v.description;
  if (typeof description !== "string") {
    throw invalidParams(`tools[${String(idx)}].description must be a string`);
  }
  const schemaRaw = v.inputSchema;
  if (schemaRaw === null) {
    throw invalidParams(`tools[${String(idx)}].inputSchema must not be null`);
  }
  if (schemaRaw === undefined || !isPlainObject(schemaRaw)) {
    throw invalidParams(`tools[${String(idx)}].inputSchema must be an object`);
  }
  return {
    name,
    description,
    inputSchema: schemaRaw,
  };
}

function parseUsage(v: JsonRpcValue | undefined): Usage {
  if (!isPlainObject(v)) {
    throw invalidParams("usage must be an object");
  }
  const m = v.model;
  if (typeof m !== "string" || m.length === 0) {
    throw invalidParams("usage.model must be a non-empty string");
  }
  const inputTokens = parseNonNegativeNumber(v.inputTokens, "usage.inputTokens");
  const outputTokens = parseNonNegativeNumber(v.outputTokens, "usage.outputTokens");
  const costCents = parseNonNegativeNumber(v.costCents, "usage.costCents");
  const metadata = parseOptionalStringRecord(v.metadata, "usage.metadata");
  const usage: Usage = {
    model: toModel(m),
    inputTokens,
    outputTokens,
    costCents,
    ...(metadata !== undefined ? { metadata } : {}),
  };
  return usage;
}

function parseOptionalString(v: JsonRpcValue | undefined, label: string): string | undefined {
  if (v === undefined) return undefined;
  if (typeof v !== "string") {
    throw invalidParams(`${label} must be a string when present`);
  }
  return v;
}

function parseOptionalNumber(v: JsonRpcValue | undefined, label: string): number | undefined {
  if (v === undefined) return undefined;
  if (typeof v !== "number" || !Number.isFinite(v)) {
    throw invalidParams(`${label} must be a finite number when present`);
  }
  return v;
}

function parseOptionalNonNegativeNumber(
  v: JsonRpcValue | undefined,
  label: string,
): number | undefined {
  const n = parseOptionalNumber(v, label);
  if (n === undefined) return undefined;
  if (n < 0) {
    throw invalidParams(`${label} must be non-negative`);
  }
  return n;
}

function parseNonNegativeNumber(v: JsonRpcValue | undefined, label: string): number {
  if (typeof v !== "number" || !Number.isFinite(v)) {
    throw invalidParams(`${label} must be a finite number`);
  }
  if (v < 0) {
    throw invalidParams(`${label} must be non-negative`);
  }
  return v;
}

function parseOptionalStringRecord(
  v: JsonRpcValue | undefined,
  label: string,
): Readonly<Record<string, string>> | undefined {
  if (v === undefined) return undefined;
  if (!isPlainObject(v)) {
    throw invalidParams(`${label} must be an object when present`);
  }
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v)) {
    if (typeof val !== "string") {
      throw invalidParams(`${label}.${k} must be a string`);
    }
    out[k] = val;
  }
  return out;
}

function requireObject(
  v: JsonRpcValue | undefined,
  label: string,
): Readonly<Record<string, JsonRpcValue>> {
  if (!isPlainObject(v)) {
    throw invalidParams(`${label} must be an object`);
  }
  return v;
}

function isPlainObject(v: JsonRpcValue | undefined): v is Readonly<Record<string, JsonRpcValue>> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function invalidParams(message: string): MethodError {
  return new MethodError(JsonRpcErrorCode.InvalidParams, message);
}

/* -----------------------------------------------------------------------
 * Response translation.
 * --------------------------------------------------------------------- */

function completeResponseToWire(resp: CompleteResponse): JsonRpcValue {
  const out: Record<string, JsonRpcValue> = {
    content: resp.content,
    toolCalls: resp.toolCalls.map(toolCallToWire),
    finishReason: resp.finishReason satisfies FinishReason,
    usage: usageToWire(resp.usage),
  };
  // Forwarded from CompleteResponse.errorMessage per the Go counterpart
  // (core/pkg/llm/provider.go CompleteResponse.ErrorMessage). Conditionally
  // included because it is only set by the provider when finishReason === "error",
  // preserving provider-reported diagnostics for the Go-core caller without
  // forcing them to parse metadata.
  if (resp.errorMessage !== undefined) {
    out.errorMessage = resp.errorMessage;
  }
  if (resp.metadata !== undefined) {
    out.metadata = { ...resp.metadata };
  }
  return out;
}

function toolCallToWire(tc: ToolCall): JsonRpcValue {
  return {
    id: tc.id,
    name: tc.name,
    arguments: tc.arguments as JsonRpcValue,
  };
}

function usageToWire(u: Usage): JsonRpcValue {
  const out: Record<string, JsonRpcValue> = {
    model: u.model,
    inputTokens: u.inputTokens,
    outputTokens: u.outputTokens,
    costCents: u.costCents,
  };
  if (u.metadata !== undefined) {
    out.metadata = { ...u.metadata };
  }
  return out;
}
