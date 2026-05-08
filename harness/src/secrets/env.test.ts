/**
 * `EnvSecretSource` vitest suite (M5.7.a).
 *
 * Pins the SecretSource contract: env-backed read returns the value,
 * unset names return `undefined`, and empty-string values are returned
 * verbatim (NOT collapsed to `undefined` — this is the documented TS
 * deviation from the Go `EnvSource`, see env.ts module doc-comment).
 *
 * The literal `ANTHROPIC_API_KEY` MAY appear in this test file because
 * the M5.7.a grep-invariant excludes `_test.ts` files from its allowlist
 * check. Production code outside `harness/src/secrets/env.ts` MUST NOT
 * carry the literal.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { EnvSecretSource, type SecretSource } from "./env.js";

describe("EnvSecretSource", () => {
  const TEST_NAME = "ANTHROPIC_API_KEY";
  let savedValue: string | undefined;

  beforeEach(() => {
    savedValue = process.env[TEST_NAME];
    // Dynamic delete is the only way to remove an env var: assigning
    // `undefined` is rejected by the typed `ProcessEnv` interface, and
    // assigning `""` would be observably different from "unset" — the
    // adapter MUST distinguish them. Disable the lint rule narrowly.
    // eslint-disable-next-line @typescript-eslint/no-dynamic-delete
    delete process.env[TEST_NAME];
  });

  afterEach(() => {
    if (savedValue === undefined) {
      // eslint-disable-next-line @typescript-eslint/no-dynamic-delete
      delete process.env[TEST_NAME];
    } else {
      process.env[TEST_NAME] = savedValue;
    }
  });

  it("returns the env value when set", () => {
    process.env[TEST_NAME] = "sk-test-12345";
    const source: SecretSource = new EnvSecretSource();

    expect(source.get(TEST_NAME)).toBe("sk-test-12345");
  });

  it("returns undefined for an unset name", () => {
    const source = new EnvSecretSource();

    expect(source.get("WATCHKEEPER_DEFINITELY_UNSET_VAR_M5_7_A")).toBeUndefined();
  });

  it("returns the empty string when the env var is set to empty (NOT undefined)", () => {
    // Documented TS deviation from the Go EnvSource: process.env exposes
    // `""` distinct from `undefined`; the harness boot path uses a
    // length check to collapse the two. The adapter itself preserves
    // the underlying `process.env` shape so callers can distinguish if
    // they need to.
    process.env[TEST_NAME] = "";
    const source = new EnvSecretSource();

    expect(source.get(TEST_NAME)).toBe("");
  });

  it("satisfies the SecretSource interface (structural conformance)", () => {
    // Compile-time + runtime confirmation that EnvSecretSource is
    // assignable to SecretSource — pins the contract for future
    // implementations (Vault, harnessrpc-bridged source).
    const source: SecretSource = new EnvSecretSource();

    expect(typeof source.get).toBe("function");
    // Idempotent reads: calling get twice with no mutation between
    // returns the same value.
    process.env[TEST_NAME] = "stable";
    expect(source.get(TEST_NAME)).toBe(source.get(TEST_NAME));
  });
});
