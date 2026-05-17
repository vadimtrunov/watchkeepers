/**
 * Watchkeeper TypeScript harness — entry point.
 *
 * Wire format: newline-delimited JSON-RPC 2.0 (NDJSON) over stdin /
 * stdout. The Go core (`core/pkg/runtime`, M5.1) drives this process
 * via a child-process supervisor; future versions add Claude Code
 * integration (M5.3.d), zod-derived tool schemas (M5.3.c), and an
 * `isolated-vm` tool runner (M5.3.b).
 *
 * This module is the stdio shell — it owns line-buffered I/O and
 * lifecycle. All protocol mechanics live in `dispatcher.ts` /
 * `jsonrpc.ts` / `methods.ts` so they can be unit-tested without a
 * subprocess.
 */

import readline from "node:readline";
import { fileURLToPath } from "node:url";

import { handleLine } from "./dispatcher.js";
import { RpcClient, notification, serialize } from "./jsonrpc.js";
import {
  ClaudeAgentProvider,
  type ClaudeAgentProviderOptions,
} from "./llm/claude-agent-provider.js";
import { ClaudeCodeProvider } from "./llm/claude-code-provider.js";
import { LLM_CAPABILITIES } from "./llm/methods.js";
import type { NotificationWriter } from "./llm/notification-writer.js";
import type { LLMProvider } from "./llm/provider.js";
import { HARNESS_VERSION, createDefaultRegistry, type ShutdownSignal } from "./methods.js";
import { EnvSecretSource, getAnthropicApiKey, type SecretSource } from "./secrets/env.js";

/**
 * Env var (read via the {@link SecretSource} adapter) the operator sets
 * to pick the LLM provider for the harness. Recognised values are
 * declared in {@link PROVIDER_KINDS}; unset / empty defaults to
 * `anthropic-api` (backwards-compat with M5.7.a boot path); unrecognised
 * values fall back to the default and emit a WARN line.
 *
 * Phase scope (M5.7.c slice 2): instance-wide toggle only.
 * Per-Watchkeeper selection via a future `Manifest.provider` field is a
 * Phase 2 follow-up — it composes on top of this env-level default
 * without breaking the contract here.
 */
export const PROVIDER_ENV_KEY = "WATCHKEEPER_LLM_PROVIDER";

/**
 * Closed set of accepted {@link PROVIDER_ENV_KEY} values. `anthropic-api`
 * routes through {@link ClaudeCodeProvider} (raw Messages API,
 * API-key-only). `claude-agent` routes through {@link ClaudeAgentProvider}
 * (Claude Agent SDK; subscription auto-detect OR API key).
 */
export const PROVIDER_KINDS = ["anthropic-api", "claude-agent"] as const;
export type ProviderKind = (typeof PROVIDER_KINDS)[number];
const PROVIDER_DEFAULT: ProviderKind = "anthropic-api";

/**
 * Wire stdin → dispatcher → stdout, then resolve when the input stream
 * ends, the shared {@link ShutdownSignal} flips, or a `shutdown`
 * request is observed.
 *
 * Exported so tests can drive the loop with in-memory streams without
 * touching the real `process.stdin` / `process.stdout`. The optional
 * `signal` parameter lets callers (the direct-invocation entry, tests)
 * flip `shouldExit` from outside the dispatch loop — used for SIGTERM /
 * SIGINT handling in the production entry point. The optional
 * `provider` (M5.3.c.c.c.a) is threaded into the registry so the three
 * LLM JSON-RPC methods get wired when supplied; when omitted the
 * harness boots in degraded mode and those methods are absent.
 */
export async function runHarness(
  stdin: NodeJS.ReadableStream,
  stdout: NodeJS.WritableStream,
  signal: ShutdownSignal = { shouldExit: false },
  provider?: LLMProvider,
): Promise<void> {
  // Default writer: serialize each notification envelope to NDJSON and
  // push the bytes to stdout. Constructed only when a provider is wired
  // — degraded mode (no provider) does not advertise readiness, mirrors
  // the LLM-method registration gate in `createDefaultRegistry`. Tests
  // drive the same path by passing a buffer-collecting `stdout`.
  let writer: NotificationWriter | undefined;
  if (provider !== undefined) {
    writer = (n) => {
      stdout.write(serialize(n));
    };
  }

  // M5.5.d.b: shared RpcClient for outbound harness → Go JSON-RPC
  // calls. Threaded into the registry so built-in tools (e.g.
  // `remember`) can dispatch via the bidirectional seam (M5.5.d.a.a).
  // Inbound responses are matched on `id` by the read loop below
  // before the standard request dispatcher runs — see the dispatcher
  // wire-up next to the readline loop.
  const rpc = new RpcClient((line) => {
    stdout.write(line);
  });

  const registry = createDefaultRegistry(signal, provider, writer, rpc);

  // Boot-time readiness signal (M5.3.c.c.c.b.a): one `harness/ready`
  // notification announces the harness identity, version, and the
  // registered LLM capabilities to the Go-core supervisor BEFORE the
  // readline loop starts consuming requests. The capabilities array
  // mirrors the methods registered by `wireLLMMethods` — it MUST be
  // updated in lockstep when M5.3.c.c.c.b.b adds `stream` /
  // `stream/cancel` to the registry.
  if (writer !== undefined) {
    writer(
      notification("harness/ready", {
        harness: "watchkeeper",
        version: HARNESS_VERSION,
        capabilities: [...LLM_CAPABILITIES],
      }),
    );
  }

  const rl = readline.createInterface({ input: stdin, crlfDelay: Infinity });

  try {
    for await (const rawLine of rl) {
      // Check before processing so an out-of-band signal flip (SIGTERM
      // / SIGINT handler in `main`) skips any line that arrived after
      // the supervisor asked us to stop.
      if (signal.shouldExit) {
        break;
      }

      const line = rawLine.trim();
      if (line.length === 0) continue;

      // M5.5.d.b: inbound NDJSON lines may be either incoming
      // requests (Go → harness) OR responses to outbound harness →
      // Go calls (matched by id against the {@link RpcClient}'s
      // pending map). Classify by peeking at top-level keys: a JSON
      // object carrying `result` or `error` is a response; anything
      // else (including parse failures) falls through to the
      // standard request dispatcher, which surfaces ParseError /
      // InvalidRequest envelopes.
      if (looksLikeResponse(line)) {
        rpc.handleResponseLine(line);
        continue;
      }

      const response = await handleLine(registry, line);
      if (response !== undefined) {
        stdout.write(response);
      }
    }
  } finally {
    rl.close();
  }
}

/**
 * Cheap classifier — does the line carry a JSON-RPC response envelope?
 * A response object has `result` or `error` at the top level; a
 * request has `method`. Parse failures and ambiguous objects fall
 * through to the request dispatcher for a structured error envelope.
 *
 * Uses a single `JSON.parse` so the request dispatcher's later parse
 * pays a duplicated cost only on responses (negligible — responses
 * are short-lived and not in a hot loop). Wraps errors so a malformed
 * line still routes to the request dispatcher's spec-compliant
 * ParseError reply.
 */
export function looksLikeResponse(line: string): boolean {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch {
    return false;
  }
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return false;
  if ("method" in raw) return false;
  return "result" in raw || "error" in raw;
}

/**
 * Top-level entry. Booted when `node harness/dist/index.js` is invoked
 * directly. Vitest imports this module without running the loop because
 * the {@link isDirectInvocation} check below fails inside the test
 * runner.
 *
 * Installs SIGTERM / SIGINT handlers so the Go core supervisor can fall
 * back to signal-based teardown when stdin remains open: both signals
 * flip the shared {@link ShutdownSignal}, the dispatch loop drains
 * its current line, and the function resolves cleanly.
 */
/**
 * Boot-time provider construction (M5.7.a).
 *
 * Reads the Claude Code API-key secret via the supplied
 * {@link SecretSource} adapter rather than touching `process.env`
 * directly, then constructs a {@link ClaudeCodeProvider} when the key
 * is present. Missing key is a degraded mode (LLM methods
 * unregistered), NOT a fatal error — the harness still answers
 * `hello` / `shutdown` / `invokeTool` so the supervisor can drive a
 * smoke test without a live provider.
 *
 * Exported for testability: callers pass a stub `SecretSource` and a
 * buffer-collecting `stderr` to assert the boot-path consumes the
 * adapter and emits the WARN log when the key is absent. Production
 * callers (`main` below) wire the concrete {@link EnvSecretSource}.
 *
 * The WARN message intentionally avoids spelling the env-var literal
 * — that string is owned by `harness/src/secrets/env.ts` (see the
 * grep-invariant CI check). The user-visible WARN phrasing changes
 * to "no Anthropic API key …"; existing M5.5.b.b LLMProvider boot
 * tests (per AC7) only assert that A WARN line fires, not its exact
 * spelling.
 */
export function buildProviderFromSecrets(
  secrets: SecretSource,
  stderr: NodeJS.WritableStream = process.stderr,
): LLMProvider | undefined {
  const kind = resolveProviderKind(secrets, stderr);
  const apiKey = getAnthropicApiKey(secrets);

  if (kind === "claude-agent") {
    // Agent SDK auto-detects the local `claude` CLI subscription when
    // no API key is present, so missing-key is NOT a degraded path
    // here — zero-config is the documented subscription mode (Phase 1
    // DoD §7 #1).
    const opts: ClaudeAgentProviderOptions = apiKey !== undefined ? { apiKey } : {};
    return new ClaudeAgentProvider(opts);
  }

  // kind === "anthropic-api": raw Messages API requires an API key;
  // its absence is the degraded mode preserved from M5.7.a.
  if (apiKey !== undefined) {
    return new ClaudeCodeProvider({ apiKey });
  }
  stderr.write("WARN: no Anthropic API key — LLM methods unavailable\n");
  return undefined;
}

/**
 * Resolve the operator-selected provider kind from
 * {@link PROVIDER_ENV_KEY}. Unset / empty / unrecognised values fall
 * back to {@link PROVIDER_DEFAULT}; unrecognised values additionally
 * emit a WARN line so a typo in the env config surfaces in the boot
 * log instead of silently picking the default.
 */
function resolveProviderKind(secrets: SecretSource, stderr: NodeJS.WritableStream): ProviderKind {
  const raw = secrets.get(PROVIDER_ENV_KEY);
  if (raw === undefined || raw === "") return PROVIDER_DEFAULT;
  if ((PROVIDER_KINDS as readonly string[]).includes(raw)) {
    return raw as ProviderKind;
  }
  stderr.write(`WARN: unknown ${PROVIDER_ENV_KEY}=${raw}; falling back to ${PROVIDER_DEFAULT}\n`);
  return PROVIDER_DEFAULT;
}

async function main(): Promise<void> {
  const signal: ShutdownSignal = { shouldExit: false };
  const requestExit = (): void => {
    signal.shouldExit = true;
  };
  process.on("SIGTERM", requestExit);
  process.on("SIGINT", requestExit);

  // M5.7.a: route the credential read through the EnvSecretSource
  // adapter so the boot path no longer carries the key literal.
  const provider = buildProviderFromSecrets(new EnvSecretSource());

  try {
    await runHarness(process.stdin, process.stdout, signal, provider);
  } finally {
    process.off("SIGTERM", requestExit);
    process.off("SIGINT", requestExit);
  }
}

// `import.meta.url` is always a `file://` URL; comparing against
// `process.argv[1]` (a filesystem path) requires URL → path conversion
// via `fileURLToPath` to handle spaces, non-ASCII, and Windows drive
// letters. Naive string templating breaks on those paths.
const isDirectInvocation =
  typeof process.argv[1] === "string" && fileURLToPath(import.meta.url) === process.argv[1];

if (isDirectInvocation) {
  main().catch((err: unknown) => {
    process.stderr.write(`harness: fatal: ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  });
}
