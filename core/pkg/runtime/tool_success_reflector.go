package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// DefaultSuccessCoolingOff is the active_after window
// [ToolSuccessReflector] adds to `clock()` when [WithSuccessCoolingOff]
// is not supplied. Pinned at 0 (no cooling-off) per the M7.2 design:
// success reflections are observations, not lessons — they are not
// reactive corrections to a failure, so there is no hot-retry-loop
// risk that motivates the 24h cooling-off window used by the
// [ToolErrorReflector]. An operator may still set a cooling-off
// window via [WithSuccessCoolingOff] for noise-throttling.
const DefaultSuccessCoolingOff = 0

// ToolSuccessReflector composes a [notebook.Entry] of category
// `observation` from a tool-invocation SUCCESS tuple (toolName,
// toolVersion, optional callID) and persists it via the supplied
// [Rememberer]. Sampling is delegated to the configured
// [ReflectionSampler] — the production sampler is the deterministic
// FNV-keyed 1-in-N gate ([DeterministicSampler]). When the sampler
// returns false, [Reflect] short-circuits and writes nothing.
//
// The reflector is the success-path counterpart of [ToolErrorReflector]:
// the M5.6.b cycle introduced the error path; M7.2 mirrors the shape
// for the success path so accumulated experience (not just failures)
// can seed future Recall.
//
// Construction discipline: callers supply the [Rememberer]
// positionally, the embedder via [WithSuccessEmbedder], and the
// sampler via [WithSuccessSampler]. Both are required — the
// constructor returns [ErrEmbedderRequired] / [ErrSamplerRequired]
// when omitted (a missing embedder would silently no-op every call;
// a missing sampler would reflect on every call which is rarely the
// intent).
//
// Reflect is best-effort from the wiring layer's perspective: the
// wired runtime ([WithToolSuccessReflector] applied to a
// [WiredRuntime]) logs Reflect failures and returns the original tool
// result to the caller unchanged. The reflector returns Embed /
// Remember errors as-is so the wiring layer can log them with full
// type information.
//
// Concurrency: safe — the struct holds only immutable configuration
// after [NewToolSuccessReflector] returns. The underlying Rememberer
// (typically `*notebook.DB`) MUST itself be safe for concurrent use;
// the M2b.2.a notebook DB is.
type ToolSuccessReflector struct {
	rememberer Rememberer
	embedder   Embedder
	sampler    ReflectionSampler
	clock      func() time.Time
	coolingOff time.Duration
	logger     *slog.Logger
}

// ToolSuccessReflectorOption configures a [ToolSuccessReflector] at
// construction time. Pass options to [NewToolSuccessReflector]; later
// options override earlier ones for the same field.
type ToolSuccessReflectorOption func(*ToolSuccessReflector)

// WithSuccessEmbedder wires the [Embedder] used to compute the dense
// vector stored on the observation [notebook.Entry]. Required: the
// constructor returns [ErrEmbedderRequired] when no embedder is set.
// Mirrors [WithEmbedder] on the error reflector.
func WithSuccessEmbedder(p Embedder) ToolSuccessReflectorOption {
	return func(r *ToolSuccessReflector) {
		r.embedder = p
	}
}

// WithSuccessSampler wires the [ReflectionSampler] the
// [ToolSuccessReflector] consults on every Reflect call. Required:
// the constructor returns [ErrSamplerRequired] when no sampler is
// set. The production wiring supplies [NewDeterministicSampler]
// ([DefaultSuccessSampleRate]); tests substitute a closure-backed
// [SamplerFunc] to force a deterministic decision.
func WithSuccessSampler(s ReflectionSampler) ToolSuccessReflectorOption {
	return func(r *ToolSuccessReflector) {
		r.sampler = s
	}
}

// WithSuccessClock overrides the wall-clock function used to stamp
// [notebook.Entry.CreatedAt] and [notebook.Entry.ActiveAfter].
// Defaults to [time.Now]. A nil function is a no-op so callers can
// always pass through whatever they have. Mirrors [WithClock] on the
// error reflector.
func WithSuccessClock(c func() time.Time) ToolSuccessReflectorOption {
	return func(r *ToolSuccessReflector) {
		if c != nil {
			r.clock = c
		}
	}
}

// WithSuccessCoolingOff overrides the cooling-off window added to
// `clock()` to compute [notebook.Entry.ActiveAfter]. Defaults to
// [DefaultSuccessCoolingOff] (= 0, no cooling-off). Non-positive
// values are treated as zero (no cooling-off) so callers can always
// pass through whatever they have without short-circuiting the
// entry's reactivation.
func WithSuccessCoolingOff(d time.Duration) ToolSuccessReflectorOption {
	return func(r *ToolSuccessReflector) {
		if d > 0 {
			r.coolingOff = d
		}
	}
}

// WithSuccessReflectorLogger wires a structured [*slog.Logger] onto
// the reflector for diagnostic emission. Used by the wiring layer's
// best-effort path: a reflection failure logs the tool name + the
// reflect err_type — never the err value, mirroring the keeperslog
// redaction discipline established by M3.4.b.
//
// A nil logger is a no-op so callers can always pass through whatever
// they have. The default is [slog.Default] (set in the constructor).
func WithSuccessReflectorLogger(l *slog.Logger) ToolSuccessReflectorOption {
	return func(r *ToolSuccessReflector) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewToolSuccessReflector constructs a [ToolSuccessReflector] backed
// by the supplied [Rememberer] (typically `*notebook.DB`). Returns
// [ErrEmbedderRequired] when [WithSuccessEmbedder] is omitted and
// [ErrSamplerRequired] when [WithSuccessSampler] is omitted.
//
// `rememberer` MUST be non-nil; passing a nil rememberer is a
// programmer error and panics with a clear message — matches the
// panic discipline of [NewToolErrorReflector].
//
// Defaults applied by the constructor:
//   - clock      = [time.Now]
//   - coolingOff = [DefaultSuccessCoolingOff] (= 0)
//   - logger     = [slog.Default]
//
// Supplied options override them.
func NewToolSuccessReflector(rememberer Rememberer, opts ...ToolSuccessReflectorOption) (*ToolSuccessReflector, error) {
	if rememberer == nil {
		panic("runtime: NewToolSuccessReflector: rememberer must not be nil")
	}
	r := &ToolSuccessReflector{
		rememberer: rememberer,
		clock:      time.Now,
		coolingOff: DefaultSuccessCoolingOff,
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.embedder == nil {
		return nil, ErrEmbedderRequired
	}
	if r.sampler == nil {
		return nil, ErrSamplerRequired
	}
	return r, nil
}

// Reflect consults the configured [ReflectionSampler] and, on a
// sample-true decision, composes a [notebook.Entry] of category
// `observation` describing the successful tool invocation, embeds
// the textual representation via the configured [Embedder], and
// persists it via the [Rememberer].
//
// Field rules:
//
//   - Subject  = "<toolName>: success" with the first newline (and
//     anything after it) replaced by an ellipsis. Empty subjects are
//     stored as NULL by the storage layer.
//   - Content  = a 3-line block: tool name, version, optional call id.
//   - ToolVersion is stored verbatim from the `toolVersion` arg
//     (empty becomes SQL NULL).
//   - ActiveAfter = `clock().Add(coolingOff).UnixMilli()`. With the
//     default 0 cooling-off, the entry is immediately recallable.
//   - Category = `observation` — the M2b.1 enum value distinct from
//     `lesson`; downstream Recall callers (M5.5.c.d.a / M7.2 weight
//     policy) apply a lower default auto-injection weight to
//     observations than to lessons.
//
// Best-effort contract: Reflect surfaces Embed / Remember errors as
// the returned `error` so the wiring layer (decorated runtime) can
// log them with full type information. The wiring layer is
// responsible for swallowing those errors and returning the ORIGINAL
// tool result to the caller; the reflector itself does NOT log or
// swallow Embed / Remember failures.
//
// Sample-false short-circuit: when the sampler returns false, Reflect
// returns nil without invoking Embed or Remember. This is the
// common case at the default 1-in-50 rate (≈98 % of calls).
func (r *ToolSuccessReflector) Reflect(ctx context.Context, agentID, toolName, toolVersion, toolCallID string) error {
	if !r.sampler.Sample(agentID, toolName, toolCallID) {
		return nil
	}

	subject := composeSuccessSubject(toolName)
	content := composeSuccessContent(toolName, toolVersion, toolCallID)

	embedQuery := subject + reflectionEmbedQuerySeparator + content
	vec, err := r.embedder.Embed(ctx, embedQuery)
	if err != nil {
		return fmt.Errorf("runtime: success reflector embed: %w", err)
	}

	var versionPtr *string
	if toolVersion != "" {
		v := toolVersion
		versionPtr = &v
	}

	now := r.clock()
	entry := notebook.Entry{
		Category:    notebook.CategoryObservation,
		Subject:     subject,
		Content:     content,
		CreatedAt:   now.UnixMilli(),
		ToolVersion: versionPtr,
		ActiveAfter: now.Add(r.coolingOff).UnixMilli(),
		Embedding:   vec,
	}

	if _, err := r.rememberer.Remember(ctx, entry); err != nil {
		return fmt.Errorf("runtime: success reflector remember: %w", err)
	}
	return nil
}

// composeSuccessSubject renders the observation Subject from
// `toolName`. Mirrors [composeSubject] on the error path but pins a
// "success" suffix so subject-keyed UIs distinguish observation rows
// from lesson rows at a glance. Multi-line names collapse to a single
// line (first newline + ellipsis); the storage layer treats empty as
// NULL.
func composeSuccessSubject(toolName string) string {
	subj := toolName + ": success"
	for i := 0; i < len(subj); i++ {
		if subj[i] == '\n' {
			return subj[:i] + "…"
		}
	}
	return subj
}

// composeSuccessContent renders the observation Content as a 3-line
// block: tool name, version, optional call id. Mirrors
// [composeContent] on the error path but omits the error_class /
// error_message slots (no failure to record). When `toolCallID` is
// empty the content omits the line entirely so the body stays
// compact — Recall queries that match on the body do not need a
// placeholder there.
func composeSuccessContent(toolName, toolVersion, toolCallID string) string {
	if toolCallID == "" {
		return fmt.Sprintf("tool: %s\nversion: %s\nresult: success", toolName, toolVersion)
	}
	return fmt.Sprintf(
		"tool: %s\nversion: %s\ncall_id: %s\nresult: success",
		toolName, toolVersion, toolCallID,
	)
}
