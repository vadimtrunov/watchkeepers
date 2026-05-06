/**
 * `wireLLMMethods` vitest suite — covers the JSON-RPC bridge for the
 * three synchronous LLMProvider methods (`complete`, `countTokens`,
 * `reportCost`) introduced in M5.3.c.c.c.a.
 *
 * Uses {@link FakeProvider} as the underlying provider so each test
 * exercises the wire-layer translation + validation without needing
 * `vi.mock` of `@anthropic-ai/sdk` (ClaudeCodeProvider has its own
 * dedicated suite). Negative cases pin every reachable {@link LLMError}
 * code through the provider and confirm `mapLLMErrorToMethodError`
 * surfaces the matching {@link MethodError}.
 */

import { describe, expect, it } from "vitest";

import {
  FakeProvider,
  LLMError,
  model,
  type CompleteResponse,
  type Usage,
} from "../src/llm/index.js";
import { wireLLMMethods } from "../src/llm/methods.js";
import {
  MethodError,
  createDefaultRegistry,
  dispatch,
  type MethodHandler,
  type ShutdownSignal,
} from "../src/methods.js";
import { JsonRpcErrorCode, type JsonRpcValue } from "../src/types.js";

const MODEL = model("claude-sonnet-4-6");

const ZERO_USAGE: Usage = {
  model: MODEL,
  inputTokens: 0,
  outputTokens: 0,
  costCents: 0,
};

function freshRegistry(provider: FakeProvider): {
  registry: Map<string, MethodHandler>;
  signal: ShutdownSignal;
} {
  const signal: ShutdownSignal = { shouldExit: false };
  const registry = new Map<string, MethodHandler>(createDefaultRegistry(signal, provider));
  return { registry, signal };
}

describe("wireLLMMethods — complete (happy)", () => {
  it("dispatches a basic complete request and returns the canned wire-shape response", async () => {
    const fake = new FakeProvider();
    const canned: CompleteResponse = {
      content: "pong",
      toolCalls: [],
      finishReason: "stop",
      usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 1 },
    };
    fake.completeResp = canned;
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      expect(outcome.result).toEqual({
        content: "pong",
        toolCalls: [],
        finishReason: "stop",
        usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 1 },
      });
    }
    expect(fake.recordedCompletes()).toHaveLength(1);
    const recorded = fake.recordedCompletes()[0];
    expect(recorded?.model).toBe(MODEL);
    expect(recorded?.messages).toHaveLength(1);
  });

  it("forwards tools to the provider and surfaces tool-call responses", async () => {
    const fake = new FakeProvider();
    const canned: CompleteResponse = {
      content: "",
      toolCalls: [{ id: "t1", name: "lookup", arguments: { q: "hi" } }],
      finishReason: "tool_use",
      usage: { ...ZERO_USAGE, inputTokens: 5, outputTokens: 3 },
    };
    fake.completeResp = canned;
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "find" }],
      tools: [
        {
          name: "lookup",
          description: "lookup a thing",
          inputSchema: { type: "object", properties: {} },
        },
      ],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      const result = outcome.result as { toolCalls: readonly unknown[]; finishReason: string };
      expect(result.toolCalls).toHaveLength(1);
      expect(result.finishReason).toBe("tool_use");
    }
    const recorded = fake.recordedCompletes()[0];
    expect(recorded?.tools).toHaveLength(1);
    expect(recorded?.tools?.[0]?.name).toBe("lookup");
  });
});

describe("wireLLMMethods — countTokens (happy)", () => {
  it("dispatches a basic countTokens request and returns the wire shape", async () => {
    const fake = new FakeProvider();
    fake.countTokensResp = 42;
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hello" }],
    };
    const outcome = await dispatch(registry, "countTokens", params);

    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      expect(outcome.result).toEqual({ inputTokens: 42 });
    }
  });
});

describe("wireLLMMethods — reportCost (happy)", () => {
  it("dispatches a reportCost request and returns accepted=true", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      runtimeID: "agent-1",
      usage: {
        model: MODEL,
        inputTokens: 10,
        outputTokens: 20,
        costCents: 7,
      },
    };
    const outcome = await dispatch(registry, "reportCost", params);

    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      expect(outcome.result).toEqual({ accepted: true });
    }
    const recorded = fake.recordedReportCosts();
    expect(recorded).toHaveLength(1);
    expect(recorded[0]?.runtimeID).toBe("agent-1");
    expect(recorded[0]?.usage.inputTokens).toBe(10);
  });
});

describe("wireLLMMethods — complete validation", () => {
  it("rejects missing model with InvalidParams and does NOT call provider", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedCompletes()).toHaveLength(0);
  });

  it("rejects empty messages array with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedCompletes()).toHaveLength(0);
  });

  it("rejects messages with an out-of-set role", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "wrong-role", content: "x" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
  });

  it("rejects tools entries with null inputSchema", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
      tools: [{ name: "t", description: "d", inputSchema: null }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
  });
});

describe("wireLLMMethods — countTokens validation", () => {
  it("rejects missing messages with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = { model: MODEL };
    const outcome = await dispatch(registry, "countTokens", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedCountTokens()).toHaveLength(0);
  });
});

describe("wireLLMMethods — reportCost validation", () => {
  it("rejects missing runtimeID with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      usage: {
        model: MODEL,
        inputTokens: 1,
        outputTokens: 1,
        costCents: 0,
      },
    };
    const outcome = await dispatch(registry, "reportCost", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedReportCosts()).toHaveLength(0);
  });

  it("rejects negative inputTokens with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      runtimeID: "agent-1",
      usage: {
        model: MODEL,
        inputTokens: -5,
        outputTokens: 1,
        costCents: 0,
      },
    };
    const outcome = await dispatch(registry, "reportCost", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
  });
});

describe("wireLLMMethods — LLMError mapping", () => {
  it("maps modelNotSupported to InvalidParams with code data", async () => {
    const fake = new FakeProvider();
    fake.completeErr = LLMError.modelNotSupported();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
      expect(outcome.data).toEqual({ code: "model_not_supported" });
    }
  });

  it("maps invalidPrompt to InvalidParams with code data", async () => {
    const fake = new FakeProvider();
    fake.completeErr = LLMError.invalidPrompt();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
      expect(outcome.data).toEqual({ code: "invalid_prompt" });
    }
  });

  it("maps tokenLimitExceeded to InvalidParams with code data", async () => {
    const fake = new FakeProvider();
    fake.completeErr = LLMError.tokenLimitExceeded();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
      expect(outcome.data).toEqual({ code: "token_limit_exceeded" });
    }
  });

  it("maps providerUnavailable to InternalError with code data", async () => {
    const fake = new FakeProvider();
    fake.completeErr = LLMError.providerUnavailable();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InternalError);
      expect(outcome.data).toEqual({ code: "provider_unavailable" });
    }
  });

  it("maps invalidManifest to InvalidParams with code data", async () => {
    const fake = new FakeProvider();
    fake.completeErr = LLMError.invalidManifest();
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
      expect(outcome.data).toEqual({ code: "invalid_manifest" });
    }
  });

  it("lets non-LLMError throws bubble to dispatcher's default InternalError", async () => {
    const fake = new FakeProvider();
    fake.completeErr = new Error("kaboom");
    const { registry } = freshRegistry(fake);

    const params: JsonRpcValue = {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    };
    const outcome = await dispatch(registry, "complete", params);

    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InternalError);
      expect(outcome.message).toBe("kaboom");
      // Plain Error: should NOT carry the structured `{ code: ... }` data
      // bag the helper attaches for LLMError instances.
      expect(outcome.data).toBeUndefined();
    }
  });
});

describe("wireLLMMethods — registry shape", () => {
  it("createDefaultRegistry without provider exposes only hello/shutdown/invokeTool (LLM methods → MethodNotFound)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    expect(registry.has("hello")).toBe(true);
    expect(registry.has("shutdown")).toBe(true);
    expect(registry.has("invokeTool")).toBe(true);
    expect(registry.has("complete")).toBe(false);
    expect(registry.has("countTokens")).toBe(false);
    expect(registry.has("reportCost")).toBe(false);

    const completeOutcome = await dispatch(registry, "complete", null);
    expect(completeOutcome.kind).toBe("error");
    if (completeOutcome.kind === "error") {
      expect(completeOutcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
    const countOutcome = await dispatch(registry, "countTokens", null);
    expect(countOutcome.kind).toBe("error");
    if (countOutcome.kind === "error") {
      expect(countOutcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
    const reportOutcome = await dispatch(registry, "reportCost", null);
    expect(reportOutcome.kind).toBe("error");
    if (reportOutcome.kind === "error") {
      expect(reportOutcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
  });

  it("createDefaultRegistry with provider includes the three LLM methods alongside existing ones", () => {
    const fake = new FakeProvider();
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal, fake);

    expect(registry.has("hello")).toBe(true);
    expect(registry.has("shutdown")).toBe(true);
    expect(registry.has("invokeTool")).toBe(true);
    expect(registry.has("complete")).toBe(true);
    expect(registry.has("countTokens")).toBe(true);
    expect(registry.has("reportCost")).toBe(true);
  });

  it("does NOT register the deferred `stream` method when provider is supplied", async () => {
    const fake = new FakeProvider();
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal, fake);

    expect(registry.has("stream")).toBe(false);
    const outcome = await dispatch(registry, "stream", null);
    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
  });
});

describe("wireLLMMethods — direct API", () => {
  it("registers exactly the three sync method names (no `stream`)", () => {
    const fake = new FakeProvider();
    const registry = new Map<string, MethodHandler>();
    wireLLMMethods(registry, fake);

    expect(registry.has("complete")).toBe(true);
    expect(registry.has("countTokens")).toBe(true);
    expect(registry.has("reportCost")).toBe(true);
    expect(registry.has("stream")).toBe(false);
    expect(registry.size).toBe(3);
  });

  it("MethodError thrown by validation has the documented data shape", async () => {
    const fake = new FakeProvider();
    const registry = new Map<string, MethodHandler>();
    wireLLMMethods(registry, fake);

    const handler = registry.get("complete");
    expect(handler).toBeDefined();
    if (handler === undefined) return;

    let caught: unknown;
    try {
      await handler({});
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
  });
});
