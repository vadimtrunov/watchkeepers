package coordinator

// forget_dm_handler — Coordinator inbound-DM command dispatcher (M8.3).
//
// The lead replies to the daily briefing's "Pending lessons (24h
// cooling-off)" section with a Slack DM of the shape `forget <uuid>`
// to suppress a pending lesson before its auto-injection window
// closes. Bodies that do NOT match the forget-command prefix are
// classified `Matched=false` so the future Coordinator-binary's DM
// router can fall through to other commands (e.g. an M8.x ack-reply
// dispatcher) without re-parsing.
//
// The handler is intentionally NOT registered against the Slack
// subscribe surface in this PR — production wiring is deferred until
// the Coordinator binary lands (mirrors the M7.1.b
// `approvalwiring.ComposeApprovalDispatcher` deferred-wiring
// precedent). M8.3 ships the parser + dispatcher + tests so the
// shape is pinned; the binary integration is a future commit.
//
// Audit discipline: the handler delegates the underlying audit emit
// to [PendingLessonForgetter.ForgetPendingLesson] (typically
// `*notebook.DB.ForgetPendingLesson`, which writes a
// `notebook_pending_lesson_forgotten` row when wired with a Logger).
// The handler itself does NOT emit; layered redundancy would double-
// count operator-initiated forgets in the keepers_log.
//
// PII discipline: [ForgetCommandResult.Refusal] NEVER echoes the raw
// body or any portion past the matched UUID — only the canonical
// "invalid uuid" / "not found" / transport reason. The matched UUID
// IS echoed in [ForgetCommandResult.EntryID] because (a) it's an
// opaque id, not user-supplied PII; (b) the future Slack-binary
// router echoes it back to the lead in the DM ack ("Forgotten <id>").
//
// Authorisation discipline (iter-1 codex Major): the underlying
// [PendingLessonForgetter] MUST narrow the delete to rows that are
// category='lesson' AND superseded_by IS NULL AND active_after > now.
// Production wiring binds [notebook.DB.ForgetPendingLesson], which
// enforces these predicates inside a single transaction. The broader
// [notebook.DB.Forget] surface is NOT acceptable here — a leaked id
// from a different category (preference, observation, pending_task,
// already-active lesson) would otherwise be erasable via the lead's
// `forget <id>` DM, which is a confused-deputy vector.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// ForgetCommandPrefix is the case-insensitive body prefix the lead
// types to invoke the forget-pending-lesson command. The handler
// matches case-insensitively but echoes the canonical lower-case form
// in error messages so a future operator audit reads consistently
// regardless of how the lead typed it.
const ForgetCommandPrefix = "forget"

// forgetRefusalPrefix is the leading namespace for every
// [ForgetCommandResult.Refusal] string this handler surfaces. Per
// the M8.2.b convention; here the prefix uses the command name
// directly because the handler is not a runtime tool.
const forgetRefusalPrefix = "coordinator: forget: "

// forgetUUIDPattern matches the canonical UUID shape (8-4-4-4-12
// lower- or upper-case hex digits). The handler accepts both cases
// and normalises to lower in [ForgetCommandResult.EntryID] so
// downstream callers can string-compare without re-folding. Mirrors
// `core/pkg/notebook/db.go` UUID validation discipline (notebook
// itself accepts both cases via the canonical regex documented in
// `validate_archive.go`).
var forgetUUIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`,
)

// PendingLessonForgetter is the single-method interface
// [NewForgetDMHandler] consumes for the suppression side effect.
// Mirrors `notebook.DB.ForgetPendingLesson`'s signature exactly so
// production wiring passes a `*notebook.DB` through verbatim; tests
// inject a hand-rolled fake without touching SQLite.
//
// Iter-1 codex Major narrowing: the consumer-side interface MUST be
// scoped to "forget a cooling-off lesson" (not the broader notebook
// `Forget` surface) so a leaked id from a different category cannot
// be erased via the lead's `forget <id>` DM. The `now` arg pushes
// the cooling-off boundary check down to the implementation under
// a single transaction.
type PendingLessonForgetter interface {
	ForgetPendingLesson(ctx context.Context, id string, now time.Time) error
}

// ForgetDMHandler parses + dispatches inbound `forget <uuid>` DMs.
// Single-use construction via [NewForgetDMHandler]; methods are
// safe for concurrent invocation (the embedded
// [PendingLessonForgetter] must itself be safe —
// `*notebook.DB.ForgetPendingLesson` is, via SQLite's per-call
// transaction).
type ForgetDMHandler struct {
	forgetter PendingLessonForgetter
	clock     func() time.Time
}

// ForgetCommandResult is the outcome of [ForgetDMHandler.Handle].
// The four-field shape lets the caller pattern-match without
// type-asserting on errors:
//
//   - body did NOT match the `forget` prefix → `Matched=false`;
//     the caller falls through to other command parsers. EntryID +
//     Forgotten + Refusal are zero-valued.
//
//   - body matched the prefix but the argument did NOT parse as a
//     UUID → `Matched=true`, `EntryID=""`, `Forgotten=false`,
//     `Refusal` carries the canonical refusal string. The future
//     Slack-binary router DMs the refusal back to the lead.
//
//   - body matched + UUID parsed + Forget succeeded → `Matched=true`,
//     `EntryID=<uuid>`, `Forgotten=true`, `Refusal=""`. The caller
//     DMs the ack to the lead ("Forgotten <uuid>").
//
//   - body matched + UUID parsed + Forget returned
//     [notebook.ErrNotFound] / [notebook.ErrInvalidEntry] →
//     `Matched=true`, `EntryID=<uuid>`, `Forgotten=false`, `Refusal`
//     carries the canonical class. The caller DMs the refusal.
//
//   - body matched + UUID parsed + Forget returned any other Go
//     error → [ForgetDMHandler.Handle] surfaces it as a Go-error
//     return (the second return value, NOT inside Refusal) so the
//     caller's logger / retry path can ingest the type information.
//     `ForgetCommandResult` carries `Matched=true`, `EntryID=<uuid>`,
//     `Forgotten=false`, `Refusal=""` so the caller can distinguish
//     "lead-visible refusal" from "internal transport failure".
type ForgetCommandResult struct {
	// Matched is true iff the inbound body started with the
	// case-insensitive [ForgetCommandPrefix] token (after trimming
	// leading whitespace). The token must be followed by whitespace
	// or end-of-string; a body starting with `"forgetfully"` does
	// NOT match.
	Matched bool

	// EntryID carries the canonical lower-case UUID parsed from the
	// body. Set iff the body matched AND the argument parsed as a
	// UUID. Empty otherwise.
	EntryID string

	// Forgotten is true iff [LessonForgetter.Forget] returned nil.
	Forgotten bool

	// Refusal carries a lead-visible explanation when the command
	// matched but did not complete cleanly. Empty on a clean forget
	// AND on a non-match (Matched=false). The string never echoes
	// the raw body past the matched UUID — only the canonical reason.
	Refusal string
}

// defaultForgetDMNow is the production clock the public constructor
// binds. Tests reach for the unexported [newForgetDMHandlerWithClock]
// to substitute a fixed `time.Time` so the per-test override stays
// scoped to one handler instance — no package-level mutable shared
// state, no race under `-parallel`. Mirrors the M8.2.b/c clock-
// injection precedent.
var defaultForgetDMNow = time.Now

// NewForgetDMHandler constructs the [ForgetDMHandler]. Panics on a
// nil `forgetter` per the M*.c.* / M8.2 nil-dep discipline.
//
// The clock used to stamp the cooling-off boundary the
// [PendingLessonForgetter] enforces defaults to [time.Now]; tests use
// [newForgetDMHandlerWithClock].
func NewForgetDMHandler(forgetter PendingLessonForgetter) *ForgetDMHandler {
	return newForgetDMHandlerWithClock(forgetter, defaultForgetDMNow)
}

// newForgetDMHandlerWithClock is the test-internal constructor that
// lets tests inject a fixed clock. Same nil-forgetter panic
// discipline; clock MUST also be non-nil.
func newForgetDMHandlerWithClock(forgetter PendingLessonForgetter, clock func() time.Time) *ForgetDMHandler {
	if forgetter == nil {
		panic("coordinator: NewForgetDMHandler: forgetter must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewForgetDMHandler: clock must not be nil")
	}
	return &ForgetDMHandler{forgetter: forgetter, clock: clock}
}

// Handle parses `body` and dispatches the `forget <uuid>` command
// per the [ForgetCommandResult] docblock. `body` is the raw DM text
// the lead sent; the handler trims surrounding whitespace and
// normalises UUID case before matching.
//
// Returns (result, nil) on every classified outcome (match-or-not,
// success-or-canonical-refusal). Returns (result-with-EntryID-set, err)
// when [LessonForgetter.Forget] surfaces an UNCLASSIFIED error
// (anything that is NOT [notebook.ErrNotFound] /
// [notebook.ErrInvalidEntry]); the caller's logger / retry path
// ingests the error, while the result still carries the matched
// EntryID so the caller can ack the parse alongside the failure.
func (h *ForgetDMHandler) Handle(ctx context.Context, body string) (ForgetCommandResult, error) {
	trimmed := strings.TrimSpace(body)
	rest, matched := matchForgetPrefix(trimmed)
	if !matched {
		// Non-matching bodies fall through to other parsers WITHOUT
		// consulting ctx — the future DM router may want to dispatch
		// to a different command parser even if its inbound queue's
		// ctx is cancelled mid-batch.
		return ForgetCommandResult{Matched: false}, nil
	}

	// ctx.Err pre-check happens AFTER the prefix match so the caller
	// always learns whether the prefix matched (Matched=true). A
	// cancelled context with `forget` body short-circuits the
	// parse + dispatch but still reports the match for caller
	// audit. Mirrors the M8.2 ordering precedent of "classify the
	// caller's intent first; then dispatch under ctx".
	if err := ctx.Err(); err != nil {
		return ForgetCommandResult{Matched: true}, err
	}

	arg := strings.TrimSpace(rest)
	if arg == "" {
		return ForgetCommandResult{
			Matched: true,
			Refusal: forgetRefusalPrefix + "missing entry id argument",
		}, nil
	}
	if !forgetUUIDPattern.MatchString(arg) {
		// Iter-1-style discipline: refusal text does NOT echo the
		// raw arg — only the canonical "invalid uuid" reason. The
		// agent's downstream DM is the surface that re-prompts the
		// lead with a hint, not this layer.
		return ForgetCommandResult{
			Matched: true,
			Refusal: forgetRefusalPrefix + "argument must be a canonical UUID (8-4-4-4-12 hex)",
		}, nil
	}
	id := strings.ToLower(arg)

	err := h.forgetter.ForgetPendingLesson(ctx, id, h.clock())
	if err == nil {
		return ForgetCommandResult{
			Matched:   true,
			EntryID:   id,
			Forgotten: true,
		}, nil
	}
	switch {
	case errors.Is(err, notebook.ErrNotFound):
		return ForgetCommandResult{
			Matched: true,
			EntryID: id,
			Refusal: forgetRefusalPrefix + "no notebook entry matches that id",
		}, nil
	case errors.Is(err, notebook.ErrNotPendingLesson):
		// Iter-1 codex Major: the underlying narrow surface rejects
		// rows that exist but are not cooling-off lessons (wrong
		// category, superseded, already-active). Surface a canonical
		// refusal that does NOT disclose which predicate failed —
		// the lead does not need that detail and disclosing it would
		// turn this surface into a category enumeration oracle for a
		// caller who learned the id from a different scope.
		return ForgetCommandResult{
			Matched: true,
			EntryID: id,
			Refusal: forgetRefusalPrefix + "id does not match a pending lesson (wrong scope or already active)",
		}, nil
	case errors.Is(err, notebook.ErrInvalidEntry):
		// Defensive: forgetUUIDPattern is a superset of notebook's
		// `uuidPattern`. If a future schema change tightens
		// notebook-side validation (e.g. UUID v7 only), the
		// notebook layer rejects with ErrInvalidEntry and we
		// surface a canonical refusal rather than a Go error.
		return ForgetCommandResult{
			Matched: true,
			EntryID: id,
			Refusal: forgetRefusalPrefix + "notebook rejected the id as non-canonical",
		}, nil
	}
	return ForgetCommandResult{
			Matched: true,
			EntryID: id,
		},
		fmt.Errorf("coordinator: forget_dm_handler: %w", err)
}

// matchForgetPrefix returns (rest, true) when `trimmed` starts with
// the case-insensitive [ForgetCommandPrefix] token followed by
// whitespace OR end-of-string. Returns ("", false) otherwise.
//
// Rationale for the trailing-whitespace requirement: the lead's DM
// `"forgetfully ignore this"` MUST NOT route to the forget command.
// The token boundary is the discriminant — mirrors M8.2.c lesson #4
// "discriminant patterns".
func matchForgetPrefix(trimmed string) (string, bool) {
	n := len(ForgetCommandPrefix)
	if len(trimmed) < n {
		return "", false
	}
	if !strings.EqualFold(trimmed[:n], ForgetCommandPrefix) {
		return "", false
	}
	if len(trimmed) == n {
		// Bare `forget` with no argument — match, with empty rest;
		// the argument-parse stage surfaces the missing-arg refusal.
		return "", true
	}
	next := trimmed[n]
	if next != ' ' && next != '\t' && next != '\n' && next != '\r' {
		return "", false
	}
	return trimmed[n+1:], true
}
