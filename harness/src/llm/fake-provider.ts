/**
 * {@link FakeProvider} — hand-rolled {@link LLMProvider} stand-in used
 * by the harness vitest suite (M5.3.c.c.a). TypeScript twin of the Go
 * `*FakeProvider` defined in `core/pkg/llm/fake_provider_test.go`.
 *
 * Records every call (request + optional canned response) so tests can
 * assert behaviour without standing up a real provider. Future provider
 * test suites import the same fake to drive Stream-based round-trip
 * checks (M5.10 provider-swap conformance in the Go core has the same
 * pattern).
 *
 * Mirroring intent: the Go `_test.go` lives next to the production
 * package because Go's `*_test.go` exclusion pattern keeps test-only
 * symbols out of the production binary. TS lacks an equivalent build
 * filter, so the fake ships in `src/llm/` and is gated only by the
 * developer convention "do not import from `fake-provider.ts` in
 * production code paths". The vitest suite is the sole consumer in
 * Phase 1.
 */

import { LLMError } from "./errors.js";
import type { LLMProvider } from "./provider.js";
import type {
  CompleteRequest,
  CompleteResponse,
  CountTokensRequest,
  Message,
  Model,
  StreamEvent,
  StreamHandler,
  StreamRequest,
  StreamSubscription,
  ToolDefinition,
  Usage,
} from "./types.js";

/**
 * Single recorded `reportCost` call. Mirrors the Go `reportCostCall`
 * struct in `fake_provider_test.go`.
 */
export interface ReportCostCall {
  readonly runtimeID: string;
  readonly usage: Usage;
}

/**
 * `FakeProvider` is constructed via `new FakeProvider()` (or
 * `new FakeProvider({ models: ['claude-sonnet-4'] })` to exercise the
 * catalogue-rejection path). Mutate the `completeResp` / `streamEvents`
 * / `countTokensResp` / `*Err` fields between calls to steer behaviour
 * per test.
 */
export interface FakeProviderOptions {
  /**
   * Catalogue of accepted {@link Model} values. Empty / undefined means
   * "accept any non-empty model"; populate to exercise the
   * `model_not_supported` path.
   */
  readonly models?: readonly Model[];
}

/**
 * Hand-rolled in-process {@link LLMProvider}. See module doc comment.
 *
 * Public mutable fields steer per-test behaviour; record accessors
 * (`recordedCompletes()` etc.) return defensive copies so test code
 * mutating returned arrays does not corrupt the fake's internal state.
 */
export class FakeProvider implements LLMProvider {
  /**
   * Catalogue of accepted models. Empty means "accept any non-empty".
   * Mutable so tests can swap catalogues mid-suite.
   */
  public models: Set<Model>;

  /** Canned response for `complete`. */
  public completeResp: CompleteResponse | undefined;
  /** Injected error for `complete`. Validation errors take precedence. */
  public completeErr: LLMError | Error | undefined;
  /** Canned event sequence dispatched to the handler by `stream`. */
  public streamEvents: readonly StreamEvent[] = [];
  /** Injected error for `stream`. Validation errors take precedence. */
  public streamErr: LLMError | Error | undefined;
  /** Canned response for `countTokens`. Zero means "use synthetic count". */
  public countTokensResp = 0;
  /** Injected error for `countTokens`. Validation errors take precedence. */
  public countTokensErr: LLMError | Error | undefined;
  /** Injected error for `reportCost`. */
  public reportCostErr: LLMError | Error | undefined;

  // Recorded calls (private; exposed via defensive-copy accessors).
  private readonly _completeCalls: CompleteRequest[] = [];
  private readonly _streamCalls: StreamRequest[] = [];
  private readonly _countTokensCalls: CountTokensRequest[] = [];
  private readonly _reportCostCalls: ReportCostCall[] = [];

  // Active stream subscriptions tracked so tests can assert idempotency.
  private readonly _streams: FakeStreamSubscription[] = [];

  public constructor(options?: FakeProviderOptions) {
    this.models = new Set(options?.models ?? []);
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; body is synchronous by design (validation precedes any I/O the real provider would perform).
  public async complete(req: CompleteRequest): Promise<CompleteResponse> {
    this.validateModel(req.model);
    this.validateMessages(req.messages);
    this.validateTools(req.tools);

    this._completeCalls.push(req);
    if (this.completeErr !== undefined) {
      throw this.completeErr;
    }
    return (
      this.completeResp ?? {
        content: "",
        toolCalls: [],
        finishReason: "stop",
        usage: zeroUsage(req.model),
      }
    );
  }

  public async stream(req: StreamRequest, handler: StreamHandler): Promise<StreamSubscription> {
    this.validateModel(req.model);
    this.validateMessages(req.messages);
    // Handler check explicitly comes BEFORE tool check to mirror the Go
    // ordering: a nil handler is the most fundamental shape error.
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass null; the sentinel mirrors Go's `ErrInvalidHandler` for those paths.
    if (handler === null || handler === undefined) {
      throw LLMError.invalidHandler();
    }
    this.validateTools(req.tools);

    this._streamCalls.push(req);
    if (this.streamErr !== undefined) {
      throw this.streamErr;
    }

    const sub = new FakeStreamSubscription();
    this._streams.push(sub);

    // Snapshot the configured events so a test mutating `streamEvents`
    // mid-dispatch cannot rewrite history.
    const events = [...this.streamEvents];

    // Dispatch synchronously (await each handler) so test assertions see
    // the events before `stream` resolves. A real provider would
    // dispatch from a worker task.
    for (const ev of events) {
      if (sub.isStopped) {
        break;
      }
      try {
        await handler(ev);
      } catch (handlerErr: unknown) {
        sub.markStopped(handlerErr);
        break;
      }
    }
    return sub;
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; body is synchronous by design.
  public async countTokens(req: CountTokensRequest): Promise<number> {
    this.validateModel(req.model);
    this.validateMessages(req.messages);

    this._countTokensCalls.push(req);
    if (this.countTokensErr !== undefined) {
      throw this.countTokensErr;
    }
    if (this.countTokensResp !== 0) {
      return this.countTokensResp;
    }
    return syntheticTokenCount(req.system, req.messages, req.tools);
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; body is synchronous by design.
  public async reportCost(runtimeID: string, usage: Usage): Promise<void> {
    this._reportCostCalls.push({ runtimeID, usage });
    if (this.reportCostErr !== undefined) {
      throw this.reportCostErr;
    }
  }

  /** Defensive-copy accessor for recorded `complete` calls. */
  public recordedCompletes(): CompleteRequest[] {
    return [...this._completeCalls];
  }

  /** Defensive-copy accessor for recorded `stream` calls. */
  public recordedStreams(): StreamRequest[] {
    return [...this._streamCalls];
  }

  /** Defensive-copy accessor for recorded `countTokens` calls. */
  public recordedCountTokens(): CountTokensRequest[] {
    return [...this._countTokensCalls];
  }

  /** Defensive-copy accessor for recorded `reportCost` calls. */
  public recordedReportCosts(): ReportCostCall[] {
    return [...this._reportCostCalls];
  }

  // -- internals ----------------------------------------------------------

  private validateModel(m: Model): void {
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null/undefined at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass them; the sentinel mirrors Go's `ErrModelNotSupported` for those paths.
    if (m === undefined || m === null || m === "") {
      throw LLMError.modelNotSupported();
    }
    if (this.models.size === 0) {
      return;
    }
    if (!this.models.has(m)) {
      throw LLMError.modelNotSupported();
    }
  }

  private validateMessages(messages: readonly Message[]): void {
    if (messages.length === 0) {
      throw LLMError.invalidPrompt();
    }
  }

  private validateTools(tools: readonly ToolDefinition[] | undefined): void {
    if (tools === undefined) return;
    for (const t of tools) {
      if (t.inputSchema === null) {
        throw LLMError.invalidPrompt();
      }
    }
  }
}

/**
 * Internal {@link StreamSubscription} implementation backing
 * {@link FakeProvider.stream}. Stop is idempotent via a one-shot guard;
 * a transport-level cause set via {@link markStopped} surfaces from the
 * FIRST `stop()` call wrapped in `LLMError(stream_closed, cause)`.
 */
class FakeStreamSubscription implements StreamSubscription {
  private _stopped = false;
  private _cause: unknown = undefined;
  private _stopRan = false;
  private _stopResult: LLMError | undefined;

  public get isStopped(): boolean {
    return this._stopped;
  }

  /** Record `cause` (if non-nil) and flip the stopped flag. */
  public markStopped(cause: unknown): void {
    if (this._stopped) return;
    this._stopped = true;
    this._cause = cause;
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; the fake's shutdown is synchronous (a real provider would await transport drain).
  public async stop(): Promise<void> {
    if (this._stopRan) {
      // Idempotent: re-raise the captured terminal error if any, else
      // resolve cleanly. Preserves the contract documented on
      // `StreamSubscription.stop` in `provider.ts`.
      if (this._stopResult !== undefined) {
        throw this._stopResult;
      }
      return;
    }
    this._stopRan = true;
    this._stopped = true;
    if (this._cause !== undefined) {
      this._stopResult = LLMError.streamClosed(undefined, this._cause);
      throw this._stopResult;
    }
  }
}

/**
 * Synthetic token counter — 1 token per 4 bytes (UTF-16 code units in
 * JS, matching the Go fake's `len(s)` over a UTF-8 byte slice closely
 * enough for the test contract). Mirrors the Go `tokenizeBytes` helper.
 */
function tokenizeBytes(s: string | undefined): number {
  if (s === undefined || s.length === 0) return 0;
  return Math.ceil(s.length / 4);
}

function syntheticTokenCount(
  system: string | undefined,
  messages: readonly Message[],
  tools: readonly ToolDefinition[] | undefined,
): number {
  let total = tokenizeBytes(system);
  for (const m of messages) {
    total += tokenizeBytes(m.content);
  }
  if (tools !== undefined) {
    for (const t of tools) {
      total += tokenizeBytes(t.name) + tokenizeBytes(t.description);
    }
  }
  return total;
}

function zeroUsage(model: Model): Usage {
  return {
    model,
    inputTokens: 0,
    outputTokens: 0,
    costCents: 0,
  };
}
