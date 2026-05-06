/**
 * Public barrel for the harness LLM provider surface (M5.3.c.c.a).
 *
 * Consumers `import { LLMProvider, FakeProvider, LLMError } from
 * "../llm/index.js"`. Concrete provider adapters (Claude Code in
 * M5.3.c.c.b, future Anthropic / OpenAI backends) re-export the same
 * surface from their own modules; this barrel is the single place every
 * downstream caller pulls types and the fake from.
 */

export type {
  CompleteRequest,
  CompleteResponse,
  CountTokensRequest,
  FinishReason,
  Message,
  Model,
  Role,
  StreamEvent,
  StreamEventKind,
  StreamHandler,
  StreamRequest,
  StreamSubscription,
  ToolCall,
  ToolDefinition,
  Usage,
} from "./types.js";
export { FINISH_REASONS, ROLES, STREAM_EVENT_KINDS, model } from "./types.js";

export { LLMError, LLM_ERROR_CODES, type LLMErrorCode } from "./errors.js";

export type { LLMProvider } from "./provider.js";

export { FakeProvider, type FakeProviderOptions, type ReportCostCall } from "./fake-provider.js";

export {
  ClaudeCodeProvider,
  type ClaudeCodeProviderOptions,
  mapAnthropicError,
} from "./claude-code-provider.js";
