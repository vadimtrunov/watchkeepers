package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// DefaultCoolingOff is the active_after window the reflector adds to
// `clock()` when [WithCoolingOff] is not supplied. Pinned at 24h per
// the M5.6.b acceptance criteria — long enough that a single tool
// failure cannot dominate auto-recall in a tight retry loop, short
// enough that genuinely durable failures resurface within a day.
//
// Exported so verification tests (B5 §M5 Notebook scenarios) can
// reference the production default as a binding-time tie rather than
// re-declaring a coupled literal in test code.
const DefaultCoolingOff = 24 * time.Hour

// reflectionEmbedQuerySeparator is the byte the reflector uses to join
// Subject and Content into a single embedding source. Picked as a
// newline so the embedded text reads naturally; downstream Recall
// queries that match either field can still hit the row because the
// fake / real embedder folds both into one vector.
const reflectionEmbedQuerySeparator = "\n"

// ErrMessageTruncationCap is the byte cap the reflector applies to the
// `error_message` field of the `lesson_learned` keepers_log event
// payload (M5.6.c). Pinned at 4096 bytes per the AC6 acceptance
// criteria so a panic stack does not blow up the persisted row size.
// Messages longer than the cap are sliced to the cap and suffixed with
// the [errMessageTruncationMarker] marker. Exposed so tests can assert
// the cap without re-deriving the constant.
const ErrMessageTruncationCap = 4096

// errMessageTruncationMarker is the literal suffix appended to an
// `error_message` payload field when truncation was applied. The "…"
// is encoded as the UTF-8 horizontal ellipsis (U+2026) so downstream
// readers can detect truncation without ambiguity.
const errMessageTruncationMarker = "…[truncated]"

// keepersLogEventType is the EventType written to keepers_log when the
// reflector emits a tool-error lesson event. Pinned at "lesson_learned"
// per the M5.6.c AC6 schema.
const keepersLogEventType = "lesson_learned"

// keepersLogAppender is the minimal subset of the [keeperslog.Writer]
// surface the [ToolErrorReflector] consumes — only Append. Defined as
// a local interface so tests can stub it without standing up a full
// Writer + LocalKeepClient stack; `*keeperslog.Writer` satisfies it
// structurally. Mirrors the [Embedder] / [Rememberer] / lifecycle
// LocalKeepClient one-way import-cycle-break pattern documented in
// `docs/LESSONS.md`.
type keepersLogAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: the production [*keeperslog.Writer]
// satisfies [keepersLogAppender]. Pinned in production code (rather
// than in `_test.go`) because the assertion uses no test-only
// symbols and locks the integration shape against any future drift
// in the keeperslog package.
var _ keepersLogAppender = (*keeperslog.Writer)(nil)

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
	keepersLog keepersLogAppender
	logger     *slog.Logger
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

// WithKeepersLog wires a [keepersLogAppender] (typically the production
// `*keeperslog.Writer`) onto the returned [*ToolErrorReflector]. When
// set, [ToolErrorReflector.Reflect] composes a `lesson_learned` event,
// calls Append, and uses the returned event id as the
// [notebook.Entry.EvidenceLogRef] — the M5.6.c integration that turns
// the M5.6.b `WithLogRefFunc` seam from "always empty" into a real
// keepers_log row id.
//
// Failure semantics (AC3 best-effort): when Append returns an error,
// the reflector logs the failure via [WithReflectorLogger] (default
// [slog.Default]), falls back to `logRefFunc()` for the
// EvidenceLogRef, and STILL calls Remember. Reflect itself returns
// nil on the Append-failure path so the wired runtime preserves the
// original tool error.
//
// A nil appender is treated identically to "option not set" so callers
// can always pass through whatever they have. Mirrors the
// [WithEmbedder] / [WithLogRefFunc] / [WithClock] no-op-on-nil idiom.
func WithKeepersLog(a keepersLogAppender) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		if a != nil {
			r.keepersLog = a
		}
	}
}

// WithReflectorLogger wires a structured [*slog.Logger] onto the
// reflector for diagnostic emission. Used by the M5.6.c keepers-log
// best-effort path: when Append fails, the reflector logs
// `runtime: reflector keepers_log append failed` carrying the tool
// name, error class, and the err_type — never the err value, mirroring
// the keeperslog redaction discipline established by M3.4.b.
//
// A nil logger is a no-op so callers can always pass through whatever
// they have. The default is [slog.Default] (set in the constructor).
func WithReflectorLogger(l *slog.Logger) ToolErrorReflectorOption {
	return func(r *ToolErrorReflector) {
		if l != nil {
			r.logger = l
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
//   - coolingOff = [DefaultCoolingOff] (= 24h)
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
		coolingOff: DefaultCoolingOff,
		logger:     slog.Default(),
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
// Best-effort contract (AC5 — embed / remember): Reflect surfaces
// Embed / Remember errors as-is so the wiring layer (decorated
// runtime) can log them with full type information. The wiring layer
// is responsible for swallowing those errors and returning the
// ORIGINAL tool error to the caller; the reflector itself does NOT
// log or swallow Embed / Remember failures.
//
// Best-effort contract (AC3 — keepers_log Append): when a
// [WithKeepersLog] appender is configured and Append fails, the
// reflector logs the failure via the configured [*slog.Logger], falls
// back to `logRefFunc()` (or "") for the EvidenceLogRef, and STILL
// calls Remember. Reflect itself returns nil on the Append-failure
// path — the keepers_log row is best-effort by design and its absence
// must not abort the lesson row.
//
// Ordering when a keepers_log appender is configured (AC2):
//  1. compose subject + content + embed
//  2. compose [keeperslog.Event] with EventType="lesson_learned"
//  3. call Append; on success use the returned event id as
//     EvidenceLogRef. On failure, log + fall back to logRefFunc().
//  4. compose the [notebook.Entry] (with the resolved EvidenceLogRef)
//  5. call Remember.
func (r *ToolErrorReflector) Reflect(ctx context.Context, agentID, toolName, toolVersion, errClass, errMsg string) error {
	subject := composeSubject(toolName, errClass)
	content := composeContent(toolName, toolVersion, errClass, errMsg)

	// AC2 ordering: Append keepers_log row BEFORE Embed so that a failed
	// Embed (e.g. cancelled ctx, embedder down) does not prevent the
	// lesson_learned event from being written — the exact failure class
	// the seam exists to capture.
	evidenceLogRef := r.logRefFunc()
	if r.keepersLog != nil {
		evt := composeKeepersLogEvent(agentID, toolName, toolVersion, errClass, errMsg)
		eventID, appendErr := r.keepersLog.Append(ctx, evt)
		if appendErr != nil {
			r.logger.LogAttrs(
				ctx, slog.LevelWarn,
				"runtime: reflector keepers_log append failed",
				slog.String("tool", toolName),
				slog.String("err_class", errClass),
				slog.String("append_err_type", typeName(appendErr)),
			)
			// evidenceLogRef stays at the logRefFunc() default — the
			// fallback was set above so no extra work is needed.
		} else {
			evidenceLogRef = eventID
		}
	}

	embedQuery := subject + reflectionEmbedQuerySeparator + content
	vec, err := r.embedder.Embed(ctx, embedQuery)
	if err != nil {
		return fmt.Errorf("runtime: reflector embed: %w", err)
	}

	var logRefPtr *string
	if evidenceLogRef != "" {
		logRefPtr = &evidenceLogRef
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

	if _, err := r.rememberer.Remember(ctx, entry); err != nil {
		return fmt.Errorf("runtime: reflector remember: %w", err)
	}
	return nil
}

// composeKeepersLogEvent builds the [keeperslog.Event] payload for a
// `lesson_learned` keepers_log row. Schema (AC6): EventType =
// "lesson_learned"; Payload is a `map[string]any` with snake_case keys
// `tool_name`, `tool_version`, `error_class`, `error_message`,
// `agent_id`. The `error_message` field is truncated to
// [ErrMessageTruncationCap] bytes with [errMessageTruncationMarker]
// appended when truncation is applied so the keeperslog row size
// remains bounded even on a panic stack trace.
//
// The notebook Entry's Content field keeps the FULL untruncated
// errMsg — truncation only applies to the keepers_log row. This
// mirrors the redaction discipline of M3.4.b (logs are bounded;
// authoritative storage is not) and lets future Recall queries match
// on the full panic body while keeping the log payload tame.
func composeKeepersLogEvent(agentID, toolName, toolVersion, errClass, errMsg string) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventType,
		Payload: map[string]any{
			"tool_name":     toolName,
			"tool_version":  toolVersion,
			"error_class":   errClass,
			"error_message": truncateErrMsg(errMsg),
			"agent_id":      agentID,
		},
	}
}

// truncateErrMsg returns `s` unchanged when its byte length is at or
// below [ErrMessageTruncationCap]; otherwise it returns the first
// [ErrMessageTruncationCap] bytes followed by [errMessageTruncationMarker].
// Operates on the raw byte slice — fine for the Phase 1 use case where
// `errMsg` is the upstream tool's `error.Error()` string and any
// mid-rune cut is downstream-readable as long as the marker signals
// truncation occurred.
func truncateErrMsg(s string) string {
	if len(s) <= ErrMessageTruncationCap {
		return s
	}
	return s[:ErrMessageTruncationCap] + errMessageTruncationMarker
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
