/**
 * `gateToolInvocation` unit tests (M5.3.b.b.d).
 *
 * Pure function — no spawn, no async. Tests cover the full deny-matrix
 * across all five {@link ToolOperation} kinds and both allow / deny
 * outcomes per kind.
 */

import { describe, expect, it } from "vitest";

import type { CapabilityDeclaration } from "../src/capabilities.js";
import { gateToolInvocation, type ToolOperation } from "../src/worker/broker.js";

const EMPTY_CAPS: CapabilityDeclaration = {
  fs: { read: [], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

const FULL_CAPS: CapabilityDeclaration = {
  fs: { read: ["/tmp/r.txt"], write: ["/tmp/w.txt"] },
  net: { allow: ["api.example.com:443", "raw.example.com"] },
  env: { allow: ["HOME", "PATH"] },
  proc: { spawn: true },
};

describe("gateToolInvocation — fs.read", () => {
  it("allows when path is in fs.read", () => {
    expect(gateToolInvocation(FULL_CAPS, { kind: "fs.read", path: "/tmp/r.txt" })).toEqual({
      allow: true,
    });
  });

  it("denies when path is not in fs.read with a useful reason", () => {
    const decision = gateToolInvocation(FULL_CAPS, { kind: "fs.read", path: "/etc/passwd" });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("/etc/passwd");
    expect(decision.reason).toContain("fs.read");
  });

  it("denies under empty allowlist", () => {
    const decision = gateToolInvocation(EMPTY_CAPS, { kind: "fs.read", path: "/tmp/r.txt" });
    expect(decision.allow).toBe(false);
  });
});

describe("gateToolInvocation — fs.write", () => {
  it("allows when path is in fs.write", () => {
    expect(gateToolInvocation(FULL_CAPS, { kind: "fs.write", path: "/tmp/w.txt" })).toEqual({
      allow: true,
    });
  });

  it("denies when path is not in fs.write", () => {
    const decision = gateToolInvocation(FULL_CAPS, { kind: "fs.write", path: "/tmp/r.txt" });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("fs.write");
  });
});

describe("gateToolInvocation — net.connect", () => {
  it("allows when host:port matches verbatim", () => {
    expect(
      gateToolInvocation(FULL_CAPS, { kind: "net.connect", host: "api.example.com", port: 443 }),
    ).toEqual({ allow: true });
  });

  it("allows when only host matches and a port is requested (port-open declaration)", () => {
    expect(
      gateToolInvocation(FULL_CAPS, { kind: "net.connect", host: "raw.example.com", port: 8080 }),
    ).toEqual({ allow: true });
  });

  it("allows a port-less request when host matches in declaration", () => {
    expect(gateToolInvocation(FULL_CAPS, { kind: "net.connect", host: "raw.example.com" })).toEqual(
      { allow: true },
    );
  });

  it("denies when host:port is not in net.allow", () => {
    const decision = gateToolInvocation(FULL_CAPS, {
      kind: "net.connect",
      host: "evil.example.com",
      port: 443,
    });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("evil.example.com:443");
  });

  it("denies a port-less request when host is not in net.allow", () => {
    const decision = gateToolInvocation(FULL_CAPS, {
      kind: "net.connect",
      host: "nope.example.com",
    });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("nope.example.com");
  });

  it("denies when port differs and only the verbatim host:port is in declaration", () => {
    const decision = gateToolInvocation(FULL_CAPS, {
      kind: "net.connect",
      host: "api.example.com",
      port: 80,
    });
    expect(decision.allow).toBe(false);
  });
});

describe("gateToolInvocation — env.get", () => {
  it("allows when env name is in env.allow", () => {
    expect(gateToolInvocation(FULL_CAPS, { kind: "env.get", name: "HOME" })).toEqual({
      allow: true,
    });
  });

  it("denies when env name is not in env.allow", () => {
    const decision = gateToolInvocation(FULL_CAPS, { kind: "env.get", name: "AWS_SECRET" });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("AWS_SECRET");
  });
});

describe("gateToolInvocation — proc.spawn", () => {
  it("allows when proc.spawn is true", () => {
    expect(gateToolInvocation(FULL_CAPS, { kind: "proc.spawn" })).toEqual({ allow: true });
  });

  it("denies when proc.spawn is false", () => {
    const decision = gateToolInvocation(EMPTY_CAPS, { kind: "proc.spawn" });
    expect(decision.allow).toBe(false);
    expect(decision.reason).toContain("proc.spawn");
  });
});

describe("gateToolInvocation — exhaustive routing", () => {
  it.each<[string, ToolOperation]>([
    ["fs.read", { kind: "fs.read", path: "/tmp/r.txt" }],
    ["fs.write", { kind: "fs.write", path: "/tmp/w.txt" }],
    ["net.connect", { kind: "net.connect", host: "api.example.com", port: 443 }],
    ["env.get", { kind: "env.get", name: "HOME" }],
    ["proc.spawn", { kind: "proc.spawn" }],
  ])("dispatches %s to a defined branch (no fallthrough)", (_label, op) => {
    const decision = gateToolInvocation(FULL_CAPS, op);
    expect(decision.allow).toBe(true);
  });
});
