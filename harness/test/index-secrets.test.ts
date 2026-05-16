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

import { PROVIDER_ENV_KEY, buildProviderFromSecrets } from "../src/index.js";
import { ClaudeAgentProvider } from "../src/llm/claude-agent-provider.js";
import { ClaudeCodeProvider } from "../src/llm/claude-code-provider.js";
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

/* -----------------------------------------------------------------------
 * M5.7.c slice 2 — WATCHKEEPER_LLM_PROVIDER toggle
 *
 * Pins the contract that the env var selects which concrete provider
 * the boot path constructs. Default + unrecognised → ClaudeCodeProvider
 * (backwards-compat with M5.7.a). `claude-agent` → ClaudeAgentProvider,
 * and missing API key on that path is NOT degraded — the Agent SDK
 * auto-detects subscription auth when no key is supplied (Phase 1 DoD
 * §7 #1 "operator runs Claude Code they already have").
 * --------------------------------------------------------------------- */

describe("buildProviderFromSecrets — provider toggle (M5.7.c)", () => {
  it("defaults to ClaudeCodeProvider when WATCHKEEPER_LLM_PROVIDER is unset", () => {
    const secrets = new StubSecretSource({ ANTHROPIC_API_KEY: "sk-test-abc" });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeInstanceOf(ClaudeCodeProvider);
    expect(secrets.calls).toContain(PROVIDER_ENV_KEY);
    expect(stderr.output()).toBe("");
  });

  it("constructs ClaudeCodeProvider when WATCHKEEPER_LLM_PROVIDER=anthropic-api + key present", () => {
    const secrets = new StubSecretSource({
      [PROVIDER_ENV_KEY]: "anthropic-api",
      ANTHROPIC_API_KEY: "sk-test-abc",
    });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeInstanceOf(ClaudeCodeProvider);
    expect(stderr.output()).toBe("");
  });

  it("constructs ClaudeAgentProvider when WATCHKEEPER_LLM_PROVIDER=claude-agent + key present", () => {
    const secrets = new StubSecretSource({
      [PROVIDER_ENV_KEY]: "claude-agent",
      ANTHROPIC_API_KEY: "sk-test-abc",
    });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeInstanceOf(ClaudeAgentProvider);
    expect(stderr.output()).toBe("");
  });

  it("constructs ClaudeAgentProvider zero-config when claude-agent + NO key (subscription path)", () => {
    const secrets = new StubSecretSource({ [PROVIDER_ENV_KEY]: "claude-agent" });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    // Subscription mode: zero-config is the intended path, NOT degraded.
    expect(provider).toBeInstanceOf(ClaudeAgentProvider);
    // No "missing API key" WARN on the subscription path — that warning
    // is specific to the anthropic-api kind.
    expect(stderr.output()).toBe("");
  });

  it("falls back to ClaudeCodeProvider + warns when WATCHKEEPER_LLM_PROVIDER is unrecognised", () => {
    const secrets = new StubSecretSource({
      [PROVIDER_ENV_KEY]: "openai-chat", // typo / unsupported
      ANTHROPIC_API_KEY: "sk-test-abc",
    });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeInstanceOf(ClaudeCodeProvider);
    const out = stderr.output();
    expect(out).toContain("WARN:");
    expect(out).toContain(PROVIDER_ENV_KEY);
    expect(out).toContain("openai-chat");
    expect(out).toContain("anthropic-api"); // names the fallback
  });

  it("still warns about missing API key when anthropic-api is selected without one", () => {
    const secrets = new StubSecretSource({ [PROVIDER_ENV_KEY]: "anthropic-api" });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeUndefined();
    expect(stderr.output().toLowerCase()).toContain("api key");
    expect(stderr.output()).toContain("LLM methods unavailable");
  });

  it("empty WATCHKEEPER_LLM_PROVIDER value collapses to default (no WARN)", () => {
    const secrets = new StubSecretSource({
      [PROVIDER_ENV_KEY]: "",
      ANTHROPIC_API_KEY: "sk-test-abc",
    });
    const stderr = new StringWritable();

    const provider = buildProviderFromSecrets(secrets, stderr);

    expect(provider).toBeInstanceOf(ClaudeCodeProvider);
    expect(stderr.output()).toBe("");
  });
});
