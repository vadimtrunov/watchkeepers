/**
 * `capabilities` schema tests — covers the worker-process capability
 * declaration and the gating policy decision shape introduced in
 * M5.3.b.b.b. Schemas are pure validators (no I/O), so tests assert on
 * `safeParse` outcomes and the zod issue paths surfaced on rejection.
 *
 * Wiring of these schemas into the dispatcher arrives in M5.3.b.b.d;
 * this file therefore exercises only the schema/type boundary.
 */

import { describe, expect, it } from "vitest";

import {
  CapabilityDeclarationSchema,
  GatingPolicyDecisionSchema,
  WORKER_CAPABILITY_ERROR_CODE,
  type CapabilityDeclaration,
  type GatingPolicyDecision,
} from "../src/capabilities.js";

describe("CapabilityDeclarationSchema — happy paths", () => {
  it("parses a fully-populated declaration with non-empty allowlists and proc.spawn=true", () => {
    const decl: CapabilityDeclaration = {
      fs: { read: ["/etc/hosts", "/tmp"], write: ["/tmp/out"] },
      net: { allow: ["api.example.com:443"] },
      env: { allow: ["HOME", "PATH"] },
      proc: { spawn: true },
    };
    const parsed = CapabilityDeclarationSchema.parse(decl);
    expect(parsed).toEqual(decl);
  });
});

describe("CapabilityDeclarationSchema — edge cases", () => {
  it("parses empty allowlists across every axis", () => {
    const decl: CapabilityDeclaration = {
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: false },
    };
    const parsed = CapabilityDeclarationSchema.parse(decl);
    expect(parsed).toEqual(decl);
  });
});

describe("CapabilityDeclarationSchema — negative cases", () => {
  it("rejects an unknown top-level field via .strict()", () => {
    const result = CapabilityDeclarationSchema.safeParse({
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: false },
      extra: "nope",
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((i) => i.code);
      expect(codes).toContain("unrecognized_keys");
    }
  });

  it("rejects proc.spawn as a string with a useful issue path", () => {
    const result = CapabilityDeclarationSchema.safeParse({
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: "yes" },
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const issue = result.error.issues.find((i) => i.path[0] === "proc" && i.path[1] === "spawn");
      expect(issue).toBeDefined();
      expect(issue?.code).toBe("invalid_type");
    }
  });

  it("rejects fs.read as a string instead of an array with a useful issue path", () => {
    const result = CapabilityDeclarationSchema.safeParse({
      fs: { read: "/tmp", write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: false },
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const issue = result.error.issues.find((i) => i.path[0] === "fs" && i.path[1] === "read");
      expect(issue).toBeDefined();
      expect(issue?.code).toBe("invalid_type");
    }
  });
});

describe("GatingPolicyDecisionSchema — happy paths", () => {
  it("parses a deny decision with a reason string", () => {
    const decision: GatingPolicyDecision = {
      allow: false,
      reason: "fs read outside allowlist",
    };
    const parsed = GatingPolicyDecisionSchema.parse(decision);
    expect(parsed).toEqual(decision);
  });
});

describe("GatingPolicyDecisionSchema — edge cases", () => {
  it("parses an allow decision with no reason field", () => {
    const decision: GatingPolicyDecision = { allow: true };
    const parsed = GatingPolicyDecisionSchema.parse(decision);
    expect(parsed).toEqual(decision);
  });
});

describe("GatingPolicyDecisionSchema — negative cases", () => {
  it("rejects an unknown top-level field via .strict()", () => {
    const result = GatingPolicyDecisionSchema.safeParse({
      allow: true,
      extra: "nope",
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((i) => i.code);
      expect(codes).toContain("unrecognized_keys");
    }
  });
});

describe("WORKER_CAPABILITY_ERROR_CODE", () => {
  it("matches the JSON-RPC code reserved by ADR §0001", () => {
    expect(WORKER_CAPABILITY_ERROR_CODE).toBe(-32003);
  });
});
