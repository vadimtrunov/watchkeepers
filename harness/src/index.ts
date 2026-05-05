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

import { handleLine } from "./dispatcher.js";
import { createDefaultRegistry, type ShutdownSignal } from "./methods.js";

/**
 * Wire stdin → dispatcher → stdout, then resolve when the input stream
 * ends or a `shutdown` request is observed.
 *
 * Exported so tests can drive the loop with in-memory streams without
 * touching the real `process.stdin` / `process.stdout`.
 */
export async function runHarness(
  stdin: NodeJS.ReadableStream,
  stdout: NodeJS.WritableStream,
): Promise<void> {
  const signal: ShutdownSignal = { shouldExit: false };
  const registry = createDefaultRegistry(signal);

  const rl = readline.createInterface({ input: stdin, crlfDelay: Infinity });

  try {
    for await (const rawLine of rl) {
      const line = rawLine.trim();
      if (line.length === 0) continue;

      const response = await handleLine(registry, line);
      if (response !== undefined) {
        stdout.write(response);
      }

      if (signal.shouldExit) {
        break;
      }
    }
  } finally {
    rl.close();
  }
}

/**
 * Top-level entry. Booted when `node harness/dist/index.js` is invoked
 * directly. Vitest imports this module without running the loop because
 * the file URL check below fails inside the test runner.
 */
async function main(): Promise<void> {
  await runHarness(process.stdin, process.stdout);
}

const isDirectInvocation =
  typeof process.argv[1] === "string" && import.meta.url === `file://${process.argv[1]}`;

if (isDirectInvocation) {
  main().catch((err: unknown) => {
    process.stderr.write(`harness: fatal: ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  });
}
