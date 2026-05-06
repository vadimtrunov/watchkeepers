/**
 * {@link LLMError} — discriminated error class shared by every
 * {@link LLMProvider} implementation. TypeScript twin of the
 * `errors.go` sentinel set in `core/pkg/llm/errors.go`.
 *
 * The Go contract uses sentinel `errors.New` values matched via
 * `errors.Is`. JavaScript has no equivalent, so we surface the same set
 * via a single class with a string-literal `code` discriminator. Callers
 * pattern-match either with `instanceof LLMError` plus
 * `error.code === 'foo'` or via the static factory helpers
 * (`LLMError.invalidPrompt(...)`).
 *
 * Wrapped causes ride on `error.cause` — the standard ES2022
 * `cause: unknown` convention — so the equivalent of Go's
 * `errors.Is(err, ErrStreamClosed)` is
 * `err instanceof LLMError && err.code === 'stream_closed'` and the
 * underlying transport error is reachable via `err.cause`.
 */

/**
 * Discriminator code corresponding to a Go sentinel in
 * `core/pkg/llm/errors.go`. The seven values cover the full Go set:
 *
 * | TS code                | Go sentinel                  |
 * |------------------------|------------------------------|
 * | `invalid_prompt`       | `ErrInvalidPrompt`           |
 * | `model_not_supported`  | `ErrModelNotSupported`       |
 * | `token_limit_exceeded` | `ErrTokenLimitExceeded`      |
 * | `invalid_handler`      | `ErrInvalidHandler`          |
 * | `stream_closed`        | `ErrStreamClosed`            |
 * | `provider_unavailable` | `ErrProviderUnavailable`     |
 * | `invalid_manifest`     | `ErrInvalidManifest`         |
 */
export const LLM_ERROR_CODES = [
  "invalid_prompt",
  "model_not_supported",
  "token_limit_exceeded",
  "invalid_handler",
  "stream_closed",
  "provider_unavailable",
  "invalid_manifest",
] as const;
export type LLMErrorCode = (typeof LLM_ERROR_CODES)[number];

/**
 * Default human-readable messages, mirroring the Go sentinel strings
 * (each Go value is `errors.New("llm: …")`). Callers may override per
 * call site by passing an explicit `message` to the static factory.
 */
const DEFAULT_MESSAGES: Readonly<Record<LLMErrorCode, string>> = {
  invalid_prompt: "llm: invalid prompt",
  model_not_supported: "llm: model not supported",
  token_limit_exceeded: "llm: token limit exceeded",
  invalid_handler: "llm: invalid handler",
  stream_closed: "llm: stream closed",
  provider_unavailable: "llm: provider unavailable",
  invalid_manifest: "llm: invalid manifest",
};

/**
 * Single error class carrying every {@link LLMErrorCode}. The
 * discriminated `code` field plus `instanceof LLMError` form the
 * matching surface that mirrors Go's `errors.Is`.
 *
 * `cause` is intentionally typed as `unknown` (not `Error`) so callers
 * can wrap rejection values that are not `Error` instances — a
 * pragmatic concession to JavaScript's `throw "anything"` legacy.
 */
export class LLMError extends Error {
  public readonly code: LLMErrorCode;
  public override readonly cause: unknown;

  public constructor(code: LLMErrorCode, message?: string, cause?: unknown) {
    const finalMessage = message ?? DEFAULT_MESSAGES[code];
    // The ES2022 Error options bag carries `cause` natively; we still
    // assign to the readonly field via `Object.defineProperty` because
    // `super({ cause })` on subclasses is finicky across runtimes.
    super(finalMessage);
    this.name = "LLMError";
    this.code = code;
    this.cause = cause;
  }

  /** Factory mirroring Go's `ErrInvalidPrompt`. */
  public static invalidPrompt(message?: string, cause?: unknown): LLMError {
    return new LLMError("invalid_prompt", message, cause);
  }

  /** Factory mirroring Go's `ErrModelNotSupported`. */
  public static modelNotSupported(message?: string, cause?: unknown): LLMError {
    return new LLMError("model_not_supported", message, cause);
  }

  /** Factory mirroring Go's `ErrTokenLimitExceeded`. */
  public static tokenLimitExceeded(message?: string, cause?: unknown): LLMError {
    return new LLMError("token_limit_exceeded", message, cause);
  }

  /** Factory mirroring Go's `ErrInvalidHandler`. */
  public static invalidHandler(message?: string, cause?: unknown): LLMError {
    return new LLMError("invalid_handler", message, cause);
  }

  /** Factory mirroring Go's `ErrStreamClosed`. The `cause` field carries
   *  the underlying transport / handler error so callers can pattern-match
   *  on it (`err.cause instanceof FooError`). */
  public static streamClosed(message?: string, cause?: unknown): LLMError {
    return new LLMError("stream_closed", message, cause);
  }

  /** Factory mirroring Go's `ErrProviderUnavailable`. */
  public static providerUnavailable(message?: string, cause?: unknown): LLMError {
    return new LLMError("provider_unavailable", message, cause);
  }

  /** Factory mirroring Go's `ErrInvalidManifest`. */
  public static invalidManifest(message?: string, cause?: unknown): LLMError {
    return new LLMError("invalid_manifest", message, cause);
  }
}
