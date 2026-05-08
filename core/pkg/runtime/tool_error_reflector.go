package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// defaultCoolingOff is the active_after window the reflector adds to
// `clock()` when [WithCoolingOff] is not supplied. Pinned at 24h per
// the M5.6.b acceptance criteria — long enough that a single tool
// failure cannot dominate auto-recall in a tight retry loop, short
// enough that genuinely durable failures resurface within a day.
const defaultCoolingOff = 24 * time.Hour

// reflectionEmbedQuerySeparator is the byte the reflector uses to join
// Subject and Content into a single embedding source. Picked as a
// newline so the embedded text reads naturally; downstream Recall
// queries that match either field can still hit the row because the
// fake / real embedder folds both into one vector.
const reflectionEmbedQuerySeparator = "\n"

// Embedder is the minimal subset of the llm.EmbeddingProvider surface
// the [ToolErrorReflector] consumes — only the Embed method. Defined
// as a local interface in this package so the runtime package does
// NOT import the llm package (which itself imports runtime, which
// would create a cycle). The concrete llm.EmbeddingProvider
// implementations (*llm.FakeEmbeddingProvider, the production
// HTTP-backed provider in M5.5.c.d.b) satisfy this interface
// structurally; tests assert this via a compile-time assertion in
// `tool_error_reflector_test.go`. Mirrors the keeperslog.LocalKeepClient
// / lifecycle.LocalKeepClient one-way import-cycle-break pattern
// documented in `docs/LESSONS.md`.
type Embedder interface {
	Embed(ctx context.Context, query string) ([]float32, error)
}

// Rememberer is the minimal subset of the [notebook.DB] surface the
// [ToolErrorReflector] consumes — only [notebook.DB.Remember]. Defined
// as an interface in this package so tests can substitute a tiny stub
// (the failure-path test for AC5 needs a Remember that returns a
// sentinel error) without standing up a malformed DB. The concrete
// `*notebook.DB` satisfies it as-is; the compile-time assertion lives
// in `tool_error_reflector_test.go` to keep production code free of
// test-only symbols. Mirrors the keeperslog.LocalKeepClient /
// lifecycle.LocalKeepClient one-way import-cycle-break pattern
// documented in `docs/LESSONS.md`.
type Rememberer interface {
	Remember(ctx context.Context, e notebook.Entry) (string, error)
}

// ToolErrorReflector composes a [notebook.Entry] of category `lesson`
// from a tool-error tuple (toolName, toolVersion, errClass, errMsg)
// and persists it via the supplied [Rememberer]. The reflector is the
// single hook the runtime calls on a tool-invocation error so future
// turns (M5.6.d auto-injection) can recall the lesson and adapt.
//
// Construction discipline: callers supply the [Rememberer] positionally
// and the embedder via [WithEmbedder]. The constructor returns
// [ErrEmbedderRequired] when no embedder is supplied — there is no
// sane default (silently no-op'ing every call would mask the bug).
//
// Reflect itself is best-effort from the wiring layer's perspective:
// the wired runtime ([WithToolErrorReflector] applied to a
// [DecoratedRuntime]) logs Reflect failures and returns the original
// tool error to the caller. The reflector returns Embed / Remember
// errors as-is so the wiring layer can log them with full type
// information.
type ToolErrorReflector struct {
	rememberer Rememberer
	embedder   Embedder
	clock      func() time.Time
	logRefFunc func() string
	coolingOff time.Duration
}

// ToolErrorReflectorOption configures a [ToolErrorReflector] at
// construction time. Pass options to [NewToolErrorReflector]; later
// options override earlier ones for the same field.
type ToolErrorReflectorOption func(*ToolErrorReflector)

// WithEmbedder wires the [Embedder] used to compute the dense vector
// stored on the lesson [notebook.Entry]. Required: the constructor
// returns [ErrEmbedderRequired] when no embedder is set. Picking an
// embedder is a deliberate caller decision (production: the
// HTTP-backed `llm.EmbeddingProvider`; tests:
// `llm.NewFakeEmbeddingProvider()`). Both satisfy the [Embedder]
// interface structurally — see the godoc on [Embedder] for the
// import-cycle-break rationale.
func WithEmbedder(p Embedder) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		r.embedder = p
	}
}

// WithClock overrides the wall-clock function used to stamp
// [notebook.Entry.ActiveAfter] (= clock() + cooling-off). Defaults to
// [time.Now]. A nil function is a no-op so callers can always pass
// through whatever they have. Tests use this to pin a deterministic
// active_after value.
func WithClock(c func() time.Time) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		if c != nil {
			r.clock = c
		}
	}
}

// WithLogRefFunc overrides the function that produces the
// [notebook.Entry.EvidenceLogRef] value. Defaults to a function that
// returns the empty string ("no evidence ref attached") — M5.6.c will
// supply a real implementation that returns the just-emitted
// `lesson_learned` keepers_log id. A nil function is a no-op.
func WithLogRefFunc(fn func() string) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		if fn != nil {
			r.logRefFunc = fn
		}
	}
}

// WithCoolingOff overrides the cooling-off window added to `clock()`
// to compute [notebook.Entry.ActiveAfter]. Defaults to 24h.
// Non-positive values are ignored so callers can always pass through
// whatever they have without short-circuiting a future row's
// reactivation.
func WithCoolingOff(d time.Duration) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		if d > 0 {
			r.coolingOff = d
		}
	}
}

// NewToolErrorReflector constructs a [ToolErrorReflector] backed by
// the supplied [Rememberer] (typically `*notebook.DB`). Returns
// [ErrEmbedderRequired] when [WithEmbedder] is omitted; the embedder
// is a hard prerequisite since every Reflect call writes a vector and
// there is no sane default that would not silently mis-classify
// errors.
//
// `rememberer` MUST be non-nil; passing a nil rememberer is a
// programmer error and panics with a clear message — matches the
// panic discipline of [keeperslog.New], [lifecycle.New], and the
// other runtime constructors. A reflector with no rememberer cannot
// do anything useful, and silently no-oping every Reflect would mask
// the bug.
//
// Defaults applied by the constructor:
//   - clock      = [time.Now]
//   - logRefFunc = func() string { return "" }
//   - coolingOff = [defaultCoolingOff] (= 24h)
//
// Supplied options override them.
func NewToolErrorReflector(rememberer Rememberer, opts ...ToolErrorReflectorOption) (*ToolErrorReflector, error) {
	if rememberer == nil {
		panic("runtime: NewToolErrorReflector: rememberer must not be nil")
	}
	r := &ToolErrorReflector{
		rememberer: rememberer,
		clock:      time.Now,
		logRefFunc: func() string { return "" },
		coolingOff: defaultCoolingOff,
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.embedder == nil {
		return nil, ErrEmbedderRequired
	}
	return r, nil
}

// Reflect composes a [notebook.Entry] of category `lesson` describing
// the tool-error tuple, computes its embedding via the configured
// [Embedder], and persists it via the [Rememberer].
//
// Field rules (pinned by the M5.6.b acceptance criteria):
//
//   - Subject  = "<toolName>: <errClass>" with the first newline (and
//     anything after it) replaced by an ellipsis so multi-line
//     subjects collapse to a single line — the storage layer treats
//     empty Subject as NULL but a multi-line Subject would be ugly in
//     downstream UIs and Recall hit lists.
//   - Content  = a 4-line block: tool name, version, error class,
//     error message. Newlines inside errMsg are preserved.
//   - ToolVersion is stored verbatim from the `toolVersion` arg.
//   - EvidenceLogRef = result of the configured [WithLogRefFunc]. The
//     M5.6.b cycle pins this to "" by default; M5.6.c will populate
//     it with a freshly-emitted `lesson_learned` keepers_log id.
//   - ActiveAfter = `clock().Add(coolingOff).UnixMilli()`. The
//     cooling-off window keeps a single failure from dominating
//     auto-recall during a retry loop.
//
// Tool-version source rationale (AC6): the wiring layer reads
// `toolVersion` from [ToolCall.ToolVersion] (the field added in this
// cycle to carry the manifest projection's tool version through
// `InvokeTool`'s signature). When the call site has no version yet
// (e.g. a tool not declared in the projection), the wiring passes
// the empty string and the reflector stores SQL NULL — Recall
// queries that filter on version are robust to NULL.
//
// Embedding source: Subject + "\n" + Content. Subsequent Recall
// queries that semantically match either the subject or the body
// land on the row because the embedder folds both into one vector
// (per M5.5.c.d.a / M5.5.d.a.b lesson — the embedder is invoked
// Go-side rather than via a separate index pass).
//
// Best-effort contract (AC5): Reflect surfaces Embed / Remember
// errors as-is so the wiring layer (decorated runtime) can log them
// with full type information. The wiring layer is responsible for
// swallowing those errors and returning the ORIGINAL tool error to
// the caller; the reflector itself does NOT log or swallow.
func (r *ToolErrorReflector) Reflect(ctx context.Context, agentID, toolName, toolVersion, errClass, errMsg string) error {
	subject := composeSubject(toolName, errClass)
	content := composeContent(toolName, toolVersion, errClass, errMsg)

	embedQuery := subject + reflectionEmbedQuerySeparator + content
	vec, err := r.embedder.Embed(ctx, embedQuery)
	if err != nil {
		return fmt.Errorf("runtime: reflector embed: %w", err)
	}

	logRef := r.logRefFunc()
	var logRefPtr *string
	if logRef != "" {
		logRefPtr = &logRef
	}
	var versionPtr *string
	if toolVersion != "" {
		v := toolVersion
		versionPtr = &v
	}

	entry := notebook.Entry{
		Category:       notebook.CategoryLesson,
		Subject:        subject,
		Content:        content,
		ToolVersion:    versionPtr,
		EvidenceLogRef: logRefPtr,
		ActiveAfter:    r.clock().Add(r.coolingOff).UnixMilli(),
		Embedding:      vec,
	}

	// agentID is currently informational at this layer — the per-agent
	// notebook DB the rememberer was constructed against IS the agent
	// scope. Future cycles (M5.6.c keepers_log emit, multi-agent
	// dispatch) will use it. Plumbing it through the signature now
	// keeps the downstream callsite stable.
	_ = agentID

	if _, err := r.rememberer.Remember(ctx, entry); err != nil {
		return fmt.Errorf("runtime: reflector remember: %w", err)
	}
	return nil
}

// composeSubject renders the lesson Subject from `toolName` and
// `errClass`. Multi-line subjects collapse to a single line (first
// newline + ellipsis); the storage layer treats empty as NULL so
// callers MUST supply non-empty toolName + errClass for the row to
// surface in subject-keyed UIs.
func composeSubject(toolName, errClass string) string {
	subj := toolName + ": " + errClass
	if i := strings.IndexByte(subj, '\n'); i >= 0 {
		return subj[:i] + "…"
	}
	return subj
}

// composeContent renders the lesson Content as a 4-line block: tool
// name, version, error class, error message. Newlines inside errMsg
// are preserved verbatim — Content is a TEXT column with no length
// cap at the storage layer, and Phase 1 keeps the templating Go-side
// (no LLM rationale) per the M5.6.b scope.
func composeContent(toolName, toolVersion, errClass, errMsg string) string {
	return fmt.Sprintf(
		"tool: %s\nversion: %s\nerror_class: %s\nerror: %s",
		toolName, toolVersion, errClass, errMsg,
	)
}
