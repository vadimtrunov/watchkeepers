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
import { ClaudeCodeProvider } from "./llm/claude-code-provider.js";
import type { LLMProvider } from "./llm/provider.js";
import { createDefaultRegistry, type ShutdownSignal } from "./methods.js";

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
  const registry = createDefaultRegistry(signal, provider);

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
async function main(): Promise<void> {
  const signal: ShutdownSignal = { shouldExit: false };
  const requestExit = (): void => {
    signal.shouldExit = true;
  };
  process.on("SIGTERM", requestExit);
  process.on("SIGINT", requestExit);

  // M5.3.c.c.c.a: read the API key from the environment once at boot.
  // Missing key is a degraded mode (LLM methods unregistered), NOT a
  // fatal error — M5.7 will replace this with the secrets-interface
  // plumbing. The harness still answers `hello` / `shutdown` /
  // `invokeTool` so the supervisor can drive a smoke test without a
  // live provider.
  const apiKey = process.env.ANTHROPIC_API_KEY;
  let provider: LLMProvider | undefined;
  if (apiKey !== undefined && apiKey.length > 0) {
    provider = new ClaudeCodeProvider({ apiKey });
  } else {
    process.stderr.write("WARN: no ANTHROPIC_API_KEY — LLM methods unavailable\n");
  }

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
