/**
 * {@link NotificationWriter} — sink for server-to-client JSON-RPC 2.0
 * notifications (M5.3.c.c.c.b.a).
 *
 * JSON-RPC 2.0 §4.1 defines a Notification as a Request without an `id`
 * member: the server MUST NOT reply, and the writer therefore returns
 * `void`. The harness uses this single-arity sink to emit boot lifecycle
 * signals (`harness/ready`) today and will use it to emit token-delta /
 * tool-call events from the streaming `stream` method that lands in
 * **M5.3.c.c.c.b.b** — that leaf is the primary consumer of the writer
 * captured by {@link wireLLMMethods}.
 *
 * The runtime-default writer constructed by `runHarness` serializes the
 * envelope to NDJSON via `serialize(...)` from `jsonrpc.ts` and writes
 * the bytes to `stdout`. The writer is synchronous because Node's
 * writable streams buffer internally — a normal `stdout.write` returns
 * before the kernel completes the syscall, so the producer never blocks
 * on a healthy pipe. If a future writer needs to apply back-pressure
 * (rate-limited transports, queued retries) the type can widen to
 * `(n: JsonRpcNotification) => Promise<void>` without breaking
 * synchronous callers.
 *
 * Test callers swap in a buffer-collecting stdout to assert the wire
 * shape; production uses the closure built inside `runHarness`.
 */
import type { JsonRpcNotification } from "../types.js";

/**
 * Function alias for the notification sink. Accepts any JSON-RPC 2.0
 * notification envelope; returns nothing because notifications never
 * carry a response per JSON-RPC 2.0 §4.1.
 */
export type NotificationWriter = (n: JsonRpcNotification) => void;
