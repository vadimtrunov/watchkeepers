/**
 * Method registry + dispatcher tests — verifies hello / shutdown
 * semantics and the error-lifting behaviour that handlers rely on.
 */

import { describe, expect, it } from "vitest";

import {
  HARNESS_VERSION,
  MethodError,
  createDefaultRegistry,
  dispatch,
  type ShutdownSignal,
} from "../src/methods.js";
import { JsonRpcErrorCode } from "../src/types.js";

describe("createDefaultRegistry", () => {
  it("registers hello and shutdown", () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    expect(registry.has("hello")).toBe(true);
    expect(registry.has("shutdown")).toBe(true);
  });
});

describe("dispatch hello", () => {
  it("returns harness identity", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    const outcome = await dispatch(registry, "hello", undefined);
    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      expect(outcome.result).toEqual({
        harness: "watchkeeper",
        version: HARNESS_VERSION,
      });
    }
  });

  it("does not flip the shutdown signal", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    await dispatch(registry, "hello", undefined);
    expect(signal.shouldExit).toBe(false);
  });
});

describe("dispatch shutdown", () => {
  it("returns accepted=true and flips the signal", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    const outcome = await dispatch(registry, "shutdown", undefined);
    expect(outcome.kind).toBe("ok");
    if (outcome.kind === "ok") {
      expect(outcome.result).toEqual({ accepted: true });
    }
    expect(signal.shouldExit).toBe(true);
  });
});

describe("dispatch unknown method", () => {
  it("returns MethodNotFound", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    const outcome = await dispatch(registry, "no.such.method", undefined);
    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.MethodNotFound);
    }
  });
});

describe("dispatch handler error", () => {
  it("lifts a MethodError verbatim", async () => {
    const registry = new Map([
      [
        "boom",
        () => {
          throw new MethodError(JsonRpcErrorCode.InvalidParams, "bad input", {
            hint: "use number",
          });
        },
      ],
    ]);
    const outcome = await dispatch(registry, "boom", undefined);
    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InvalidParams);
      expect(outcome.message).toBe("bad input");
      expect(outcome.data).toEqual({ hint: "use number" });
    }
  });

  it("lifts an unexpected exception to InternalError", async () => {
    const registry = new Map([
      [
        "boom",
        () => {
          throw new Error("kaboom");
        },
      ],
    ]);
    const outcome = await dispatch(registry, "boom", undefined);
    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InternalError);
      expect(outcome.message).toBe("kaboom");
    }
  });

  it("lifts a non-Error throw to InternalError with a default message", async () => {
    const registry = new Map([
      [
        "boom",
        () => {
          // eslint-disable-next-line @typescript-eslint/only-throw-error
          throw "not an error";
        },
      ],
    ]);
    const outcome = await dispatch(registry, "boom", undefined);
    expect(outcome.kind).toBe("error");
    if (outcome.kind === "error") {
      expect(outcome.code).toBe(JsonRpcErrorCode.InternalError);
      expect(outcome.message).toBe("internal error");
    }
  });
});
