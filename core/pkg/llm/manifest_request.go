package llm

import (
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// metadataKeyLanguage is the reserved [CompleteRequest.Metadata] key
// the manifest-aware builders populate when [runtime.Manifest.Language]
// is non-empty. Concrete providers MAY consume the hint when their
// upstream API exposes a language knob; otherwise it is informational.
// Pinned as a constant so callers reading the metadata bag downstream
// can match the same key without hard-coding the string at every site.
const metadataKeyLanguage = "language"

// requestParams is the fully-projected, option-merged shape every
// builder folds before stamping the concrete CompleteRequest /
// StreamRequest / CountTokensRequest. Keeping the projection in one
// struct lets the three builders share validation and mapping rules
// (AC6) without duplicating the field-by-field assembly.
type requestParams struct {
	model       Model
	system      string
	messages    []Message
	maxTokens   int
	temperature float64
	tools       []ToolDefinition
	metadata    map[string]string
}

// RequestOption mutates a [requestParams] in place. Implementations
// apply left-to-right so later options override earlier ones for the
// same field (last-write-wins, AC4). Mirrors the [runtime.StartOption]
// surface so callers wiring both sides share a mental model.
type RequestOption func(*requestParams)

// WithMaxTokens overrides the default [CompleteRequest.MaxTokens] /
// [StreamRequest.MaxTokens] (zero, "provider default") with `n`. A
// negative `n` is forwarded verbatim — the provider's own validation
// catches it; this layer does not second-guess the contract.
func WithMaxTokens(n int) RequestOption {
	return func(p *requestParams) {
		p.maxTokens = n
	}
}

// WithTemperature overrides the default [CompleteRequest.Temperature]
// (zero, "provider default") with `t`. The provider validates the
// numeric range against its own catalogue; this layer forwards the
// value untouched.
func WithTemperature(t float64) RequestOption {
	return func(p *requestParams) {
		p.temperature = t
	}
}

// WithTools populates [CompleteRequest.Tools] / [StreamRequest.Tools] /
// [CountTokensRequest.Tools] with `td` verbatim. Default is nil (NOT
// an empty slice, AC5) — preserves the documented contract on the
// request types so concrete providers distinguish "no tools at all"
// from "tools list intentionally empty".
func WithTools(td []ToolDefinition) RequestOption {
	return func(p *requestParams) {
		p.tools = td
	}
}

// WithMetadata sets `metadata[k] = v` after the manifest-derived
// metadata has been folded. Last-write-wins on duplicate keys means a
// caller-supplied option overrides any value the manifest seeded for
// the same key (AC4). Successive WithMetadata calls compose: the LAST
// value for a given key wins.
func WithMetadata(k, v string) RequestOption {
	return func(p *requestParams) {
		if p.metadata == nil {
			p.metadata = make(map[string]string, 1)
		}
		p.metadata[k] = v
	}
}

// composeBaseFields validates `m` / `msgs` and returns the populated
// [requestParams] before option application. Shared by the three
// builders so validation and mapping rules stay in one place (AC6).
//
// Validation order pins behaviour observable to callers:
//  1. Empty Manifest.Model returns [ErrInvalidManifest].
//  2. Empty Manifest.SystemPrompt returns [ErrInvalidManifest].
//  3. Empty / nil msgs returns [ErrInvalidPrompt].
//
// Manifest.Metadata is copied verbatim into the projected metadata bag
// so callers can mutate the returned slice without touching the
// caller-supplied manifest. A non-empty Manifest.Language is mapped to
// the reserved [metadataKeyLanguage] entry; nil [Manifest.AuthorityMatrix]
// is a non-event for this layer (this projection does not consult it).
func composeBaseFields(m runtime.Manifest, msgs []Message) (requestParams, error) {
	if m.Model == "" {
		return requestParams{}, ErrInvalidManifest
	}
	if m.SystemPrompt == "" {
		return requestParams{}, ErrInvalidManifest
	}
	if len(msgs) == 0 {
		return requestParams{}, ErrInvalidPrompt
	}

	var meta map[string]string
	if len(m.Metadata) > 0 || m.Language != "" {
		meta = make(map[string]string, len(m.Metadata)+1)
		for k, v := range m.Metadata {
			meta[k] = v
		}
		if m.Language != "" {
			meta[metadataKeyLanguage] = m.Language
		}
	}

	return requestParams{
		model:    Model(m.Model),
		system:   m.SystemPrompt,
		messages: msgs,
		metadata: meta,
	}, nil
}

// applyOptions folds `opts` into `p` left-to-right. Centralised so the
// three builders share the iteration semantics (last-write-wins).
func applyOptions(p *requestParams, opts []RequestOption) {
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(p)
	}
}

// BuildCompleteRequest projects `m` plus the caller-supplied `msgs`
// into a [CompleteRequest] suitable for [Provider.Complete]. Returns
// [ErrInvalidManifest] when [runtime.Manifest.Model] or
// [runtime.Manifest.SystemPrompt] is empty; returns [ErrInvalidPrompt]
// when `msgs` is empty / nil.
//
// Mapping rules (AC2):
//
//   - Model     ← Manifest.Model
//   - System    ← Manifest.SystemPrompt
//   - Messages  ← msgs verbatim
//   - Metadata  ← Manifest.Metadata + ("language" → Manifest.Language)
//     when non-empty, then overridden by [WithMetadata] options
//
// MaxTokens, Temperature, Tools default to the request's zero value;
// [WithMaxTokens], [WithTemperature], [WithTools] override.
func BuildCompleteRequest(m runtime.Manifest, msgs []Message, opts ...RequestOption) (CompleteRequest, error) {
	p, err := composeBaseFields(m, msgs)
	if err != nil {
		return CompleteRequest{}, err
	}
	applyOptions(&p, opts)
	return CompleteRequest{
		Model:       p.model,
		System:      p.system,
		Messages:    p.messages,
		MaxTokens:   p.maxTokens,
		Temperature: p.temperature,
		Tools:       p.tools,
		Metadata:    p.metadata,
	}, nil
}

// BuildStreamRequest mirrors [BuildCompleteRequest] for streaming. The
// validation surface and mapping rules are identical (AC6); the only
// shape difference is the absence of synchronous-only knobs (none in
// scope at M5.3.c.b).
func BuildStreamRequest(m runtime.Manifest, msgs []Message, opts ...RequestOption) (StreamRequest, error) {
	p, err := composeBaseFields(m, msgs)
	if err != nil {
		return StreamRequest{}, err
	}
	applyOptions(&p, opts)
	return StreamRequest{
		Model:       p.model,
		System:      p.system,
		Messages:    p.messages,
		MaxTokens:   p.maxTokens,
		Temperature: p.temperature,
		Tools:       p.tools,
		Metadata:    p.metadata,
	}, nil
}

// BuildCountTokensRequest mirrors [BuildCompleteRequest] for the
// pre-flight token-count surface. Validation and mapping rules are
// identical (AC6). [CountTokensRequest] has no MaxTokens / Temperature
// fields (a token count is deterministic w.r.t. the prompt only); the
// matching options are silently inert when the caller passes them
// here, which keeps the option set uniform across the three builders.
func BuildCountTokensRequest(m runtime.Manifest, msgs []Message, opts ...RequestOption) (CountTokensRequest, error) {
	p, err := composeBaseFields(m, msgs)
	if err != nil {
		return CountTokensRequest{}, err
	}
	applyOptions(&p, opts)
	return CountTokensRequest{
		Model:    p.model,
		System:   p.system,
		Messages: p.messages,
		Tools:    p.tools,
		Metadata: p.metadata,
	}, nil
}
