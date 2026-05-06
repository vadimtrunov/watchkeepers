package llm

import "errors"

// ErrInvalidPrompt is returned synchronously by [Provider.Complete] /
// [Provider.Stream] / [Provider.CountTokens] when the supplied request
// has an empty [CompleteRequest.Messages] / [StreamRequest.Messages] /
// [CountTokensRequest.Messages] slice. The underlying provider is NOT
// touched on this path. Matchable via [errors.Is].
var ErrInvalidPrompt = errors.New("llm: invalid prompt")

// ErrModelNotSupported is returned synchronously by [Provider.Complete]
// / [Provider.Stream] / [Provider.CountTokens] when the supplied
// [Model] is empty OR the provider's catalogue does not list it.
// Empty-model rejection happens before contacting the model; catalogue
// rejection MAY consult a static list or a provider-side capability
// probe. Matchable via [errors.Is].
var ErrModelNotSupported = errors.New("llm: model not supported")

// ErrTokenLimitExceeded is returned by [Provider.Complete] /
// [Provider.Stream] when the assembled request would exceed the model's
// context window after token counting OR the provider receives a
// model-side context-overflow signal. Distinct from [ErrInvalidPrompt]
// — the prompt was structurally valid but too large. Matchable via
// [errors.Is].
var ErrTokenLimitExceeded = errors.New("llm: token limit exceeded")

// ErrInvalidHandler is returned synchronously by [Provider.Stream] when
// the supplied [StreamHandler] is nil. A nil handler is a programmer
// error at the call site; surfacing the sentinel rather than panicking
// lets the caller recover and report. Matchable via [errors.Is].
var ErrInvalidHandler = errors.New("llm: invalid handler")

// ErrStreamClosed is returned by [StreamSubscription.Stop] when the
// dispatch loop exited with a transport / handler error before Stop
// was called (the wrapped error rides via the [errors.Is] chain). A
// clean shutdown returns nil. Mirrors [runtime.ErrSubscriptionClosed]
// and [messenger.ErrSubscriptionClosed]. Matchable via [errors.Is].
var ErrStreamClosed = errors.New("llm: stream closed")

// ErrProviderUnavailable is returned by [Provider.Complete] /
// [Provider.Stream] when the provider could not reach its upstream
// service (network error, auth failure, rate-limit exhaustion). The
// caller MAY retry; the wrapped cause rides via the [errors.Is] chain
// for diagnostics. Matchable via [errors.Is].
var ErrProviderUnavailable = errors.New("llm: provider unavailable")

// ErrInvalidManifest is returned synchronously by the manifest-aware
// request builders ([BuildCompleteRequest], [BuildStreamRequest],
// [BuildCountTokensRequest]) when the supplied [runtime.Manifest] fails
// the per-turn projection's validation rules (empty Model or empty
// SystemPrompt). The runtime package emits its own ErrInvalidManifest
// sentinel for [runtime.AgentRuntime.Start]; this one is the llm-layer
// twin so the projection can fail fast without importing runtime's
// error catalogue. Matchable via [errors.Is].
var ErrInvalidManifest = errors.New("llm: invalid manifest")
