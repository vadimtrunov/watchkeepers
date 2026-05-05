/**
 * Integration test for the stdio loop. Drives {@link runHarness} with
 * in-memory streams (Readable / Writable) and asserts that NDJSON
 * round-trips end-to-end including shutdown teardown.
 */

import { Readable, Writable } from "node:stream";

import { describe, expect, it } from "vitest";

import { runHarness } from "../src/index.js";
import type { ShutdownSignal } from "../src/methods.js";

function readableFromLines(lines: readonly string[]): Readable {
  return Readable.from(lines.map((line) => line + "\n"));
}

class CollectingWritable extends Writable {
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

  public lines(): string[] {
    return this.output()
      .split("\n")
      .filter((line) => line.length > 0);
  }
}

describe("runHarness", () => {
  it("answers hello then exits on shutdown", async () => {
    const stdin = readableFromLines([
      '{"jsonrpc":"2.0","id":1,"method":"hello"}',
      '{"jsonrpc":"2.0","id":2,"method":"shutdown"}',
    ]);
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout);

    const lines = stdout.lines();
    expect(lines).toHaveLength(2);
    const [helloLine, shutdownLine] = lines;
    if (helloLine === undefined || shutdownLine === undefined) {
      throw new Error("expected two response lines");
    }

    const first = JSON.parse(helloLine) as { id: number; result: { harness: string } };
    expect(first.id).toBe(1);
    expect(first.result.harness).toBe("watchkeeper");

    const second = JSON.parse(shutdownLine) as { id: number; result: { accepted: boolean } };
    expect(second.id).toBe(2);
    expect(second.result.accepted).toBe(true);
  });

  it("ignores blank lines between requests", async () => {
    const stdin = Readable.from([
      "\n",
      '{"jsonrpc":"2.0","id":1,"method":"hello"}\n',
      "   \n",
      '{"jsonrpc":"2.0","id":2,"method":"shutdown"}\n',
    ]);
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout);

    expect(stdout.lines()).toHaveLength(2);
  });

  it("returns ParseError without aborting the loop", async () => {
    const stdin = readableFromLines(["garbage", '{"jsonrpc":"2.0","id":1,"method":"shutdown"}']);
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout);

    const lines = stdout.lines();
    expect(lines).toHaveLength(2);
    const [errLine, okLine] = lines;
    if (errLine === undefined || okLine === undefined) {
      throw new Error("expected two response lines");
    }

    const parseErr = JSON.parse(errLine) as {
      id: null;
      error: { code: number };
    };
    expect(parseErr.id).toBeNull();
    expect(parseErr.error.code).toBe(-32700);

    const shutdownOk = JSON.parse(okLine) as { result: { accepted: boolean } };
    expect(shutdownOk.result.accepted).toBe(true);
  });

  it("exits cleanly on EOF without shutdown", async () => {
    const stdin = readableFromLines(['{"jsonrpc":"2.0","id":1,"method":"hello"}']);
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout);

    expect(stdout.lines()).toHaveLength(1);
  });

  it("exits cleanly when the shared shutdown signal is flipped (SIGTERM/SIGINT path)", async () => {
    // Simulates the SIGTERM/SIGINT handler path: a long-running stdin
    // would normally keep the loop alive, but flipping `shouldExit`
    // before the next line drains makes the loop break out cleanly.
    // The async generator awaits between yields so the dispatch loop
    // has a chance to process line #1 before the flag flips.
    const signal: ShutdownSignal = { shouldExit: false };
    const wait = (ms: number): Promise<void> =>
      new Promise((resolve) => {
        setTimeout(resolve, ms);
      });
    const stdin = Readable.from(
      (async function* () {
        yield '{"jsonrpc":"2.0","id":1,"method":"hello"}\n';
        // Give the dispatch loop a turn so it processes line #1 before
        // the SIGTERM-equivalent flag flip.
        await wait(20);
        signal.shouldExit = true;
        // Yield another line; the loop must NOT process it because the
        // signal already requested teardown.
        yield '{"jsonrpc":"2.0","id":2,"method":"hello"}\n';
      })(),
    );
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout, signal);

    // First line was processed before the flag flipped; second line is
    // dropped because the loop checked the flag at the top of the next
    // iteration.
    expect(stdout.lines()).toHaveLength(1);
    expect(signal.shouldExit).toBe(true);
  });
});
