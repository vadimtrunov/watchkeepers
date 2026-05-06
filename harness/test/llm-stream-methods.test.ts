/**
 * `wireLLMMethods` streaming-protocol vitest suite (M5.3.c.c.c.b.b).
 *
 * Pins the wire-shape contract for `stream` and `stream/cancel` plus the
 * ancillary `streamEventToWire` translator. Uses {@link FakeProvider} for
 * deterministic event sequences and a small array-backed
 * {@link NotificationWriter} so test assertions can read the exact
 * envelope ordering observed by a real client.
 */

import { describe, expect, it, vi } from "vitest";

import {
  FakeProvider,
  LLMError,
  model,
  type StreamEvent,
  type StreamHandler,
  type StreamSubscription,
  type Usage,
} from "../src/llm/index.js";
import { LLM_CAPABILITIES, streamEventToWire, wireLLMMethods } from "../src/llm/methods.js";
import type { NotificationWriter } from "../src/llm/notification-writer.js";
import {
  MethodError,
  createDefaultRegistry,
  dispatch,
  type MethodHandler,
  type ShutdownSignal,
} from "../src/methods.js";
import { JsonRpcErrorCode, type JsonRpcNotification, type JsonRpcValue } from "../src/types.js";

const MODEL = model("claude-sonnet-4-6");

const ZERO_USAGE: Usage = {
  model: MODEL,
  inputTokens: 0,
  outputTokens: 0,
  costCents: 0,
};

interface BufferingWriter {
  notifications: JsonRpcNotification[];
  writer: NotificationWriter;
}

function buffering(): BufferingWriter {
  const notifications: JsonRpcNotification[] = [];
  const writer: NotificationWriter = (n) => {
    notifications.push(n);
  };
  return { notifications, writer };
}

interface WiredHarness {
  registry: Map<string, MethodHandler>;
  buffer: BufferingWriter;
}

function wired(provider: FakeProvider): WiredHarness {
  const registry = new Map<string, MethodHandler>();
  const buffer = buffering();
  wireLLMMethods(registry, provider, buffer.writer);
  return { registry, buffer };
}

function streamEventNotifications(buf: BufferingWriter): JsonRpcNotification[] {
  return buf.notifications.filter((n) => n.method === "stream/event");
}

function streamIDOf(result: JsonRpcValue | Promise<JsonRpcValue>): string {
  if (typeof result !== "object" || result === null || Array.isArray(result) || "then" in result) {
    throw new Error("expected stream result to be a resolved object");
  }
  const sid = (result as { streamID?: JsonRpcValue }).streamID;
  if (typeof sid !== "string") {
    throw new Error("expected streamID to be a string");
  }
  return sid;
}

describe("LLM_CAPABILITIES", () => {
  it("advertises the full 5-method surface as a frozen tuple", () => {
    expect(LLM_CAPABILITIES).toEqual([
      "complete",
      "countTokens",
      "reportCost",
      "stream",
      "stream/cancel",
    ]);
  });
});

describe("streamEventToWire", () => {
  it("maps text_delta to {kind, textDelta}", () => {
    const ev: StreamEvent = { kind: "text_delta", textDelta: "hello" };
    expect(streamEventToWire(ev)).toEqual({ kind: "text_delta", textDelta: "hello" });
  });

  it("maps tool_call_start to {kind, id, name}", () => {
    const ev: StreamEvent = {
      kind: "tool_call_start",
      toolCall: { id: "t1", name: "lookup", arguments: {} },
    };
    expect(streamEventToWire(ev)).toEqual({
      kind: "tool_call_start",
      id: "t1",
      name: "lookup",
    });
  });

  it("maps tool_call_delta to {kind, id, argumentsDelta} (renamed toolCallID -> id)", () => {
    const ev: StreamEvent = {
      kind: "tool_call_delta",
      toolCall: { id: "t1", name: "", arguments: {} },
      textDelta: '{"q":',
    };
    expect(streamEventToWire(ev)).toEqual({
      kind: "tool_call_delta",
      id: "t1",
      argumentsDelta: '{"q":',
    });
  });

  it("maps message_stop to {kind, finishReason, usage}", () => {
    const ev: StreamEvent = {
      kind: "message_stop",
      finishReason: "stop",
      usage: ZERO_USAGE,
    };
    expect(streamEventToWire(ev)).toEqual({
      kind: "message_stop",
      finishReason: "stop",
      usage: {
        model: MODEL,
        inputTokens: 0,
        outputTokens: 0,
        costCents: 0,
      },
    });
  });

  it("maps error to {kind, message}", () => {
    const ev: StreamEvent = { kind: "error", errorMessage: "boom" };
    expect(streamEventToWire(ev)).toEqual({ kind: "error", message: "boom" });
  });
});

describe("wireLLMMethods — stream registration gating", () => {
  it("registers stream + stream/cancel ONLY when writer is supplied", () => {
    const fake = new FakeProvider();
    const registryWithWriter = new Map<string, MethodHandler>();
    wireLLMMethods(registryWithWriter, fake, () => {
      // noop
    });
    expect(registryWithWriter.has("stream")).toBe(true);
    expect(registryWithWriter.has("stream/cancel")).toBe(true);
    expect(registryWithWriter.size).toBe(5);

    const registryNoWriter = new Map<string, MethodHandler>();
    wireLLMMethods(registryNoWriter, fake);
    expect(registryNoWriter.has("stream")).toBe(false);
    expect(registryNoWriter.has("stream/cancel")).toBe(false);
    expect(registryNoWriter.size).toBe(3);
  });

  it("createDefaultRegistry without writer returns MethodNotFound for stream methods", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const fake = new FakeProvider();
    const registry = createDefaultRegistry(signal, fake);

    const streamOutcome = await dispatch(registry, "stream", {
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    expect(streamOutcome.kind).toBe("error");
    if (streamOutcome.kind === "error") {
      expect(streamOutcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }

    const cancelOutcome = await dispatch(registry, "stream/cancel", { streamID: "x" });
    expect(cancelOutcome.kind).toBe("error");
    if (cancelOutcome.kind === "error") {
      expect(cancelOutcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
  });
});

describe("wireLLMMethods — stream happy paths", () => {
  it("emits text_delta×3 + message_stop notifications and returns a streamID", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [
      { kind: "text_delta", textDelta: "he" },
      { kind: "text_delta", textDelta: "ll" },
      { kind: "text_delta", textDelta: "o" },
      {
        kind: "message_stop",
        finishReason: "stop",
        usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 3 },
      },
    ];
    const { registry, buffer } = wired(fake);
    const handler = registry.get("stream");
    expect(handler).toBeDefined();
    if (handler === undefined) return;

    const result = await handler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);
    expect(sid.length).toBeGreaterThan(0);

    const events = streamEventNotifications(buffer);
    expect(events).toHaveLength(4);
    expect(events.map((n) => (n.params as { event: { kind: string } }).event.kind)).toEqual([
      "text_delta",
      "text_delta",
      "text_delta",
      "message_stop",
    ]);
    // Every notification carries the same streamID and the right envelope.
    for (const n of events) {
      expect(n.jsonrpc).toBe("2.0");
      expect(n.method).toBe("stream/event");
      const params = n.params as { streamID: string; event: { kind: string } };
      expect(params.streamID).toBe(sid);
    }
    const last = events[events.length - 1];
    if (last === undefined) throw new Error("missing last event");
    const finalEv = (
      last.params as unknown as {
        event: { kind: string; finishReason?: string; usage?: Usage };
      }
    ).event;
    expect(finalEv.kind).toBe("message_stop");
    expect(finalEv.finishReason).toBe("stop");
    expect(finalEv.usage?.inputTokens).toBe(1);

    // Registry hygiene: cancelling the now-completed streamID is a no-op.
    const cancelHandler = registry.get("stream/cancel");
    if (cancelHandler === undefined) throw new Error("stream/cancel missing");
    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: false });
  });

  it("emits tool_call_start + tool_call_delta + message_stop with correlated id", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [
      {
        kind: "tool_call_start",
        toolCall: { id: "tc-1", name: "lookup", arguments: {} },
      },
      {
        kind: "tool_call_delta",
        toolCall: { id: "tc-1", name: "", arguments: {} },
        textDelta: '{"q":"hi"}',
      },
      {
        kind: "message_stop",
        finishReason: "tool_use",
        usage: ZERO_USAGE,
      },
    ];
    const { registry, buffer } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    const result = await handler({
      model: MODEL,
      messages: [{ role: "user", content: "find" }],
    });
    streamIDOf(result);

    const events = streamEventNotifications(buffer);
    expect(events).toHaveLength(3);
    const kinds = events.map((n) => (n.params as { event: { kind: string } }).event.kind);
    expect(kinds).toEqual(["tool_call_start", "tool_call_delta", "message_stop"]);

    const startNotif = events[0];
    const deltaNotif = events[1];
    if (startNotif === undefined || deltaNotif === undefined) {
      throw new Error("expected start + delta notifications");
    }
    const startEv = (startNotif.params as { event: { id: string; name: string } }).event;
    expect(startEv.id).toBe("tc-1");
    expect(startEv.name).toBe("lookup");

    const deltaEv = (deltaNotif.params as { event: { id: string; argumentsDelta: string } }).event;
    expect(deltaEv.id).toBe("tc-1");
    expect(deltaEv.argumentsDelta).toBe('{"q":"hi"}');
  });
});

describe("wireLLMMethods — stream/cancel paths", () => {
  it("cancel-success: stop() invoked, registry entry deleted, accepted=true", async () => {
    const fake = new FakeProvider();
    let stopCount = 0;
    // Override FakeProvider.stream to delay terminal event so cancel can race.
    const originalStream = fake.stream.bind(fake);
    fake.stream = async (req, h) => {
      const sub = await originalStream(req, h);
      const wrappedStop = sub.stop.bind(sub);
      const wrapper = {
        stop: async (): Promise<void> => {
          stopCount += 1;
          await wrappedStop();
        },
      };
      return wrapper;
    };
    // Empty events: provider.stream resolves immediately with no events,
    // so the registry entry persists until cancelled.
    fake.streamEvents = [];

    const { registry } = wired(fake);
    const streamHandler = registry.get("stream");
    const cancelHandler = registry.get("stream/cancel");
    if (streamHandler === undefined || cancelHandler === undefined) {
      throw new Error("missing handlers");
    }

    const result = await streamHandler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);

    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: true });
    expect(stopCount).toBe(1);

    // Registry entry deleted: a second cancel returns false.
    const second = await cancelHandler({ streamID: sid });
    expect(second).toEqual({ accepted: false });
  });

  it("cancel-unknown: streamID never registered → accepted=false (no error)", async () => {
    const fake = new FakeProvider();
    const { registry } = wired(fake);
    const cancelHandler = registry.get("stream/cancel");
    if (cancelHandler === undefined) throw new Error("stream/cancel missing");

    const result = await cancelHandler({ streamID: "does-not-exist" });
    expect(result).toEqual({ accepted: false });
  });

  it("cancel-after-completion: streamID was removed by terminal event → accepted=false", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [
      { kind: "text_delta", textDelta: "x" },
      { kind: "message_stop", finishReason: "stop", usage: ZERO_USAGE },
    ];
    const { registry } = wired(fake);
    const streamHandler = registry.get("stream");
    const cancelHandler = registry.get("stream/cancel");
    if (streamHandler === undefined || cancelHandler === undefined) {
      throw new Error("missing handlers");
    }

    const result = await streamHandler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);

    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: false });
  });

  it("late-event-after-cancel: event dispatched after stream/cancel is silently dropped", async () => {
    // Capture the handler callback passed to provider.stream so we can
    // invoke it manually after cancel, simulating a provider in-flight
    // delivery that races the stop() call.
    const fake = new FakeProvider();
    // StreamHandler allows void | Promise<void> return; match that type so
    // no-misused-promises does not fire when we assign `h` into this variable.
    let capturedHandler: StreamHandler | undefined;
    const originalStream = fake.stream.bind(fake);
    fake.stream = (req, h) => {
      capturedHandler = h;
      // Use empty events so no terminal event fires synchronously —
      // the registry entry persists until we cancel manually.
      return originalStream(req, h);
    };
    fake.streamEvents = [];

    const { registry, buffer } = wired(fake);
    const streamHandler = registry.get("stream");
    const cancelHandler = registry.get("stream/cancel");
    if (streamHandler === undefined || cancelHandler === undefined) {
      throw new Error("missing handlers");
    }

    const result = await streamHandler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);
    if (capturedHandler === undefined) throw new Error("handler not captured");

    // Cancel the stream — sets state.cancelled = true, returns accepted:true.
    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: true });

    // Snapshot notification count after cancel.
    const notifCountAfterCancel = buffer.notifications.length;

    // Simulate a late in-flight event delivered by the provider after stop().
    // `void` suppresses no-floating-promises: the handler is synchronous in
    // practice; StreamHandler's Promise<void> return is a type-level allowance.
    void capturedHandler({ kind: "text_delta", textDelta: "late!" });

    // No new stream/event notification must have been written.
    expect(buffer.notifications.length).toBe(notifCountAfterCancel);
  });
});

describe("wireLLMMethods — dispatch-loop exception clears registry (AC8)", () => {
  it("writer that throws on first call removes registry entry; stream/cancel returns accepted=false", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [
      { kind: "text_delta", textDelta: "x" },
      { kind: "message_stop", finishReason: "stop", usage: ZERO_USAGE },
    ];

    // Build a writer that throws on its first invocation, simulating an
    // EPIPE / stdout-closed condition during dispatch.
    let callCount = 0;
    const registry = new Map<string, MethodHandler>();
    wireLLMMethods(registry, fake, (notification) => {
      void notification; // consumed to satisfy no-unused-vars; only callCount matters here.
      callCount += 1;
      if (callCount === 1) {
        throw new Error("simulated EPIPE");
      }
    });

    const streamHandler = registry.get("stream");
    const cancelHandler = registry.get("stream/cancel");
    if (streamHandler === undefined || cancelHandler === undefined) {
      throw new Error("missing handlers");
    }

    // The stream handler starts, the spy dispatches text_delta, writer
    // throws → FakeProvider's loop catches the re-thrown error and stops.
    // The streamID is returned because the subscription was created before
    // the event loop ran.
    const result = await streamHandler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);

    // Registry must be empty: the dispatch-loop exception handler deleted
    // the entry. stream/cancel should report accepted=false (not found).
    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: false });
  });
});

describe("wireLLMMethods — async error notification", () => {
  it("kind:error event surfaces as a stream/event notification and clears the registry", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [
      { kind: "text_delta", textDelta: "x" },
      { kind: "error", errorMessage: "upstream blew up" },
    ];
    const { registry, buffer } = wired(fake);
    const streamHandler = registry.get("stream");
    const cancelHandler = registry.get("stream/cancel");
    if (streamHandler === undefined || cancelHandler === undefined) {
      throw new Error("missing handlers");
    }

    const result = await streamHandler({
      model: MODEL,
      messages: [{ role: "user", content: "hi" }],
    });
    const sid = streamIDOf(result);

    const events = streamEventNotifications(buffer);
    expect(events).toHaveLength(2);
    const errNotif = events[1];
    if (errNotif === undefined) throw new Error("missing error notification");
    const errEv = (errNotif.params as { event: { kind: string; message: string } }).event;
    expect(errEv.kind).toBe("error");
    expect(errEv.message).toBe("upstream blew up");

    // Registry entry cleared by the terminal error event.
    const cancelResult = await cancelHandler({ streamID: sid });
    expect(cancelResult).toEqual({ accepted: false });
  });
});

describe("wireLLMMethods — stream validation", () => {
  it("rejects empty model with InvalidParams; provider never called", async () => {
    const fake = new FakeProvider();
    const { registry, buffer } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    let caught: unknown;
    try {
      await handler({
        model: "",
        messages: [{ role: "user", content: "hi" }],
      });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedStreams()).toHaveLength(0);
    expect(buffer.notifications).toHaveLength(0);
  });

  it("rejects empty messages array with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    let caught: unknown;
    try {
      await handler({ model: MODEL, messages: [] });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
    expect(fake.recordedStreams()).toHaveLength(0);
  });

  it("rejects malformed message entry (out-of-set role) with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    let caught: unknown;
    try {
      await handler({
        model: MODEL,
        messages: [{ role: "wrong-role", content: "x" }],
      });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).toBe(JsonRpcErrorCode.InvalidParams);
    }
  });

  it("rejects stream/cancel missing streamID with InvalidParams", async () => {
    const fake = new FakeProvider();
    const { registry } = wired(fake);
    const handler = registry.get("stream/cancel");
    if (handler === undefined) throw new Error("stream/cancel missing");

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

    let caught2: unknown;
    try {
      await handler({ streamID: 42 });
    } catch (e) {
      caught2 = e;
    }
    expect(caught2).toBeInstanceOf(MethodError);
  });
});

describe("wireLLMMethods — stream sync error mapping", () => {
  it("provider.stream throws LLMError(provider_unavailable) → MethodError(InternalError); no streamID", async () => {
    const fake = new FakeProvider();
    fake.streamErr = LLMError.providerUnavailable();
    const { registry, buffer } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    let caught: unknown;
    try {
      await handler({
        model: MODEL,
        messages: [{ role: "user", content: "hi" }],
      });
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).toBe(JsonRpcErrorCode.InternalError);
      expect(caught.data).toEqual({ code: "provider_unavailable" });
    }
    expect(buffer.notifications).toHaveLength(0);
    // Cancel of any id is no-op since registry was never populated.
    const cancelHandler = registry.get("stream/cancel");
    if (cancelHandler === undefined) throw new Error("stream/cancel missing");
    const cancelResult = await cancelHandler({ streamID: "anything" });
    expect(cancelResult).toEqual({ accepted: false });
  });
});

describe("wireLLMMethods — concurrency", () => {
  it("two streams interleave correctly; each event tagged with its own streamID", async () => {
    // AC9: both streams must be simultaneously in-flight while interleaved
    // events are dispatched. We spy on fake.stream to capture each call's
    // handler, then drive both handlers manually in interleaved order:
    //   stream1.event1, stream2.event1, stream1.event2, stream2.event2
    // This proves the registry correctly isolates per-stream state and
    // tags every notification with the right streamID despite interleaving.

    const fake = new FakeProvider();

    // Captured per-call handlers: index 0 = stream1, index 1 = stream2.
    // StreamHandler may return void | Promise<void>; use that exact type.
    const capturedHandlers: ((ev: StreamEvent) => void | Promise<void>)[] = [];

    vi.spyOn(fake, "stream").mockImplementation((_req, handler) => {
      capturedHandlers.push(handler);
      // Return a minimal subscription; stop is a no-op for this test.
      const sub: StreamSubscription = { stop: () => Promise.resolve() };
      return Promise.resolve(sub);
    });

    const { registry, buffer } = wired(fake);
    const handler = registry.get("stream");
    if (handler === undefined) throw new Error("stream missing");

    // Start both streams. Because our spy never dispatches events, both
    // handler() calls resolve immediately while the streams remain live
    // (no terminal event seen → both entries still in the registry).
    const p1 = handler({ model: MODEL, messages: [{ role: "user", content: "first" }] });
    const p2 = handler({ model: MODEL, messages: [{ role: "user", content: "second" }] });
    const [r1, r2] = await Promise.all([p1, p2]);
    const sid1 = streamIDOf(r1);
    const sid2 = streamIDOf(r2);
    expect(sid1).not.toBe(sid2);

    // At this point both streams are simultaneously in-flight (registry
    // holds both entries). Verify that before any events are dispatched.
    const cancelHandler = registry.get("stream/cancel");
    if (cancelHandler === undefined) throw new Error("stream/cancel missing");

    // Both entries live — cancel each and confirm accepted=true.
    // (We restore after so we can drive events manually below.)
    // Actually: drive events first, then verify per-streamID tagging.
    // Reset and re-start so we can see the interleaved notification sequence.
    // Instead: drive events NOW in interleaved order, then assert.

    const [h1, h2] = capturedHandlers;
    if (h1 === undefined || h2 === undefined) {
      throw new Error("expected two captured handlers");
    }

    // Interleaved dispatch: stream1.text_delta, stream2.text_delta,
    //                       stream1.message_stop, stream2.message_stop.
    await h1({ kind: "text_delta", textDelta: "a" });
    await h2({ kind: "text_delta", textDelta: "b" });
    await h1({ kind: "message_stop", finishReason: "stop", usage: ZERO_USAGE });
    await h2({ kind: "message_stop", finishReason: "stop", usage: ZERO_USAGE });

    const events = streamEventNotifications(buffer);
    // 4 events total: 2 per stream.
    expect(events).toHaveLength(4);

    // Assert interleaved order: s1.text_delta, s2.text_delta, s1.message_stop, s2.message_stop.
    const tags = events.map((n) => {
      const p = n.params as { streamID: string; event: { kind: string } };
      return { sid: p.streamID, kind: p.event.kind };
    });
    expect(tags[0]).toEqual({ sid: sid1, kind: "text_delta" });
    expect(tags[1]).toEqual({ sid: sid2, kind: "text_delta" });
    expect(tags[2]).toEqual({ sid: sid1, kind: "message_stop" });
    expect(tags[3]).toEqual({ sid: sid2, kind: "message_stop" });

    // Registry hygiene: terminal events cleaned both entries.
    expect(await cancelHandler({ streamID: sid1 })).toEqual({ accepted: false });
    expect(await cancelHandler({ streamID: sid2 })).toEqual({ accepted: false });
  });
});
