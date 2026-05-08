/**
 * Boot-path secrets-injection tests (M5.7.a, AC2 + AC7).
 *
 * Pins the contract that `buildProviderFromSecrets` consumes a
 * `SecretSource` (NOT `process.env` directly) and that the missing-key
 * path still emits a WARN log on stderr — the absence-handling
 * preserved verbatim from the M5.3.c.c.c.a boot path, only the SOURCE
 * of the key changed.
 */

import { Writable } from "node:stream";

import { describe, expect, it } from "vitest";

import { buildProviderFromSecrets } from "../src/index.js";
import type { SecretSource } from "../src/secrets/env.js";

class StubSecretSource implements SecretSource {
  public readonly calls: string[] = [];
  public constructor(private readonly values: Readonly<Record<string, string | undefined>>) {}

  public get(name: string): string | undefined {
    this.calls.push(name);
    return this.values[name];
  }
}

class StringWritable extends Writable {
  public chunks: string[] = [];
  public override _write(
    chunk: Buffer | string,
    _encoding: BufferEncoding,
    callback: (error?: Error | null) => void,
  ): void {
    this.chunks.push(chunk.toString("utf-8"));
    callback();
  }
  public output(): string {
    return this.chunks.join("");
  }
}

describe("buildProviderFromSecrets", () => {
  it("constructs a ClaudeCodeProvider when the SecretSource yields a key", () => {
    const secrets = new StubSecretSource({ ANTHROPIC_API_KEY: "sk-test-abc" });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeDefined();
    // The boot path must consult the adapter (NOT process.env), pin
    // by asserting the stub recorded the lookup.
    expect(secrets.calls).toContain("ANTHROPIC_API_KEY");
    // No WARN line on the happy path.
    expect(stderr.output()).toBe("");
  });

  it("returns undefined and emits the WARN log when the key is absent (AC7)", () => {
    const secrets = new StubSecretSource({});
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeUndefined();
    // Existing M5.5.b.b boot-test contract: a WARN line fires when
    // the key is missing. The exact phrasing changed (the literal
    // moved into the adapter file under M5.7.a), but the WARN
    // semantics are preserved.
    const out = stderr.output();
    expect(out.startsWith("WARN:")).toBe(true);
    expect(out.toLowerCase()).toContain("api key");
    expect(out).toContain("LLM methods unavailable");
  });

  it("returns undefined and warns when the env value is the empty string (edge case)", () => {
    // process.env exposes "FOO=" as `""` (set but empty); the harness
    // collapse-to-undefined behaviour is asserted at the boot layer.
    const secrets = new StubSecretSource({ ANTHROPIC_API_KEY: "" });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeUndefined();
    expect(stderr.output().startsWith("WARN:")).toBe(true);
  });
});
